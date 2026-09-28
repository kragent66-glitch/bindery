package calibre

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/vavallee/bindery/internal/db"
	"github.com/vavallee/bindery/internal/jobs"
	"github.com/vavallee/bindery/internal/models"
)

// DeliveryStore is the subset of *db.CalibreDeliveryRepo the worker uses.
type DeliveryStore interface {
	Enqueue(ctx context.Context, bookID, bookFileID int64, editionID *int64, filePath, format string) (bool, error)
	DueBatch(ctx context.Context, now time.Time, limit int) ([]models.CalibreDelivery, error)
	MarkDelivered(ctx context.Context, id, calibreID int64, outcome, targetLibrary string) error
	MarkFailed(ctx context.Context, id int64, code, msg string, nextAttemptAt time.Time, terminal bool) error
	MarkSkipped(ctx context.Context, id int64, reason string) error
	DeliveredByCalibreID(ctx context.Context, calibreID int64) ([]models.CalibreDelivery, error)
}

// DeliveryBooks is the subset of *db.BookRepo the worker uses.
type DeliveryBooks interface {
	GetByID(ctx context.Context, id int64) (*models.Book, error)
	ListFiles(ctx context.Context, bookID int64) ([]models.BookFile, error)
	SetCalibreIDIfUnset(ctx context.Context, id, calibreID int64) (bool, error)
}

// deliveryHealthProber is what the worker asks before a pass. Only the plugin
// client implements it; calibredb runs locally and has nothing to probe.
type deliveryHealthProber interface {
	HealthDetail(ctx context.Context) (HealthState, error)
}

// metadataUpdater writes to a Calibre row that already exists
// (PATCH /v1/books/{id}). It is what turns a 409 into a correction.
type metadataUpdater interface {
	SupportsMetadataUpdate(ctx context.Context) bool
	UpdateMetadata(ctx context.Context, id int64, meta Metadata) ([]string, error)
}

// Delivery outcomes recorded on the ledger row.
const (
	DeliveryOutcomeAdded   = "added"
	DeliveryOutcomeAlready = "already"
)

// Skip reasons recorded on the ledger row.
const (
	deliverySkipBookGone = "book removed"
	deliverySkipFileGone = "file removed"
	deliverySkipNoFile   = "file missing on disk"
)

const (
	// deliveryBatchSize bounds one pass. The next tick picks up the rest.
	deliveryBatchSize = 50
	// deliveryHealthTimeout bounds the reachability probe at the start of a
	// pass, so a Calibre host that does not answer costs five seconds a
	// minute rather than the plugin client's thirty second request timeout.
	deliveryHealthTimeout = 5 * time.Second
	// deliveryMaxAttempts is the attempt that gives up for good.
	deliveryMaxAttempts = 8
)

// deliveryBackoff is the wait after the Nth failed attempt (index N-1). Past
// the end of the list the last step repeats. A var so tests can read it.
var deliveryBackoff = []time.Duration{
	time.Minute,
	5 * time.Minute,
	15 * time.Minute,
	time.Hour,
	6 * time.Hour,
	24 * time.Hour,
}

// deliveryTerminalCodes are plugin error codes no retry can fix: the file is
// a format Calibre will not take, or the plugin refuses the path outright.
var deliveryTerminalCodes = map[string]bool{
	"bad_format":     true,
	"path_forbidden": true,
}

// nextDeliveryAttempt returns when a row that has now failed attempts times
// should be tried again, and whether it should instead give up.
func nextDeliveryAttempt(now time.Time, attempts int) (time.Time, bool) {
	if attempts >= deliveryMaxAttempts {
		return now, true
	}
	i := attempts - 1
	if i < 0 {
		i = 0
	}
	if i >= len(deliveryBackoff) {
		i = len(deliveryBackoff) - 1
	}
	return now.Add(deliveryBackoff[i]), false
}

// Deliverer sends queued ebook files to Calibre (#2832). Imports enqueue a
// calibre_deliveries row and kick it; a scheduler job runs it every minute as
// well, so a book imported while Calibre was closed is delivered once Calibre
// is back.
//
// Passes never overlap: the tick and a kick share one mutex taken with
// TryLock, and whichever arrives second simply returns. Nothing holds a
// transaction across a Calibre call; every ledger write is one statement.
type Deliverer struct {
	store    DeliveryStore
	books    DeliveryBooks
	mode     func() Mode
	config   func() Config
	adderFor func(Mode) Adder

	authors  AuthorGetter
	editions EditionLister
	series   SeriesGetter
	covers   CoverSource
	jobs     *jobs.Group
	now      func() time.Time

	mu sync.Mutex

	healthMu sync.Mutex
	health   DeliveryHealth
}

// DeliveryHealth is what the worker last learned about the push target, for
// the settings queue view. It is only as fresh as the last pass that had
// something to deliver: an idle queue does not probe Calibre, so CheckedAt
// can be old while nothing is waiting.
type DeliveryHealth struct {
	// LastPassAt is when a pass last looked at the queue.
	LastPassAt *time.Time `json:"lastPassAt,omitempty"`
	// CheckedAt is when the worker last learned whether Calibre answers.
	CheckedAt *time.Time `json:"checkedAt,omitempty"`
	// Reachable is nil until the worker has had a reason to find out.
	Reachable *bool `json:"reachable,omitempty"`
	// LastError is why Calibre could not be reached, when it could not.
	LastError string `json:"lastError,omitempty"`
}

// Health returns a copy of what the worker last learned about the target.
func (d *Deliverer) Health() DeliveryHealth {
	d.healthMu.Lock()
	defer d.healthMu.Unlock()
	h := d.health
	if h.LastPassAt != nil {
		t := *h.LastPassAt
		h.LastPassAt = &t
	}
	if h.CheckedAt != nil {
		t := *h.CheckedAt
		h.CheckedAt = &t
	}
	if h.Reachable != nil {
		v := *h.Reachable
		h.Reachable = &v
	}
	return h
}

func (d *Deliverer) notePass() {
	now := d.now().UTC()
	d.healthMu.Lock()
	d.health.LastPassAt = &now
	d.healthMu.Unlock()
}

// noteReachable records whether Calibre answered, and why not when it did not.
func (d *Deliverer) noteReachable(ok bool, cause string) {
	now := d.now().UTC()
	d.healthMu.Lock()
	d.health.CheckedAt = &now
	d.health.Reachable = &ok
	d.health.LastError = cause
	d.healthMu.Unlock()
}

// NewDeliverer builds a worker. mode and config are read at the start of each
// pass and adderFor resolves the client for the mode, so a settings change
// takes effect on the next pass.
func NewDeliverer(store DeliveryStore, books DeliveryBooks, mode func() Mode, config func() Config, adderFor func(Mode) Adder) *Deliverer {
	return &Deliverer{
		store:    store,
		books:    books,
		mode:     mode,
		config:   config,
		adderFor: adderFor,
		now:      time.Now,
	}
}

// WithMetadata attaches the lookups that fill the author, edition and series
// fields of the payload. Each is optional.
func (d *Deliverer) WithMetadata(authors AuthorGetter, editions EditionLister, series SeriesGetter) *Deliverer {
	d.authors = authors
	d.editions = editions
	d.series = series
	return d
}

// WithCovers lets deliveries carry the book's cover.
func (d *Deliverer) WithCovers(src CoverSource) *Deliverer {
	d.covers = src
	return d
}

// WithJobs tracks passes in the process-wide jobs group, so shutdown waits
// for an in-flight delivery before it closes the database.
func (d *Deliverer) WithJobs(g *jobs.Group) *Deliverer {
	d.jobs = g
	return d
}

// Enqueue queues one imported ebook file. The format is the file extension,
// lower case. editionID is the edition the download was grabbed for, when
// known; otherwise the worker matches one by format at delivery time.
func (d *Deliverer) Enqueue(ctx context.Context, bookID, bookFileID int64, editionID *int64, path string) (bool, error) {
	format := strings.ToLower(strings.TrimPrefix(filepath.Ext(path), "."))
	return d.store.Enqueue(ctx, bookID, bookFileID, editionID, path, format)
}

// Kick starts a pass in the background and returns at once. If a pass is
// already running the kick is dropped; the running pass or the next tick
// picks the new row up.
func (d *Deliverer) Kick() {
	if d.jobs != nil {
		d.jobs.Go("calibre-deliver-kick", d.runLocked)
		return
	}
	go d.runLocked(context.Background())
}

// RunDeliveries runs one pass on the calling goroutine. It is the scheduler
// job; it returns at once when a pass is already running.
func (d *Deliverer) RunDeliveries(ctx context.Context) {
	if d.jobs != nil {
		d.jobs.Run("calibre-deliver", d.runLocked)
		return
	}
	d.runLocked(ctx)
}

func (d *Deliverer) runLocked(ctx context.Context) {
	if !d.mu.TryLock() {
		return
	}
	defer d.mu.Unlock()
	d.pass(ctx)
}

// deliveryResult is what one row came to.
type deliveryResult int

const (
	deliveryDelivered deliveryResult = iota
	deliveryFailed
	deliveryGaveUp
	deliverySkipped
	// deliveryUntouched left the row as it was: Calibre went away mid
	// batch, or a local read failed. Not an attempt.
	deliveryUntouched
	// deliveryStop is deliveryUntouched and ends the pass too.
	deliveryStop
)

// deliveryTarget is what a pass knows about where it is delivering.
type deliveryTarget struct {
	mode    Mode
	adder   Adder
	cfg     Config
	library string
}

func (d *Deliverer) pass(ctx context.Context) {
	mode := d.mode()
	if mode != ModePlugin && mode != ModeCalibredb {
		return
	}
	// The due query is a local read, so it goes first: an idle queue must
	// not cost a request to the Calibre host every minute.
	d.notePass()
	rows, err := d.store.DueBatch(ctx, d.now(), deliveryBatchSize)
	if err != nil {
		slog.Warn("calibre delivery: reading the queue failed", "error", err)
		return
	}
	if len(rows) == 0 {
		return
	}
	adder := d.adderFor(mode)
	if adder == nil {
		return
	}
	target := deliveryTarget{mode: mode, adder: adder, cfg: d.config()}
	if mode == ModeCalibredb {
		target.library = target.cfg.LibraryPath
	}
	if prober, ok := adder.(deliveryHealthProber); ok && mode == ModePlugin {
		pctx, cancel := context.WithTimeout(ctx, deliveryHealthTimeout)
		state, herr := prober.HealthDetail(pctx)
		cancel()
		if herr != nil {
			// Calibre is closed or the host is down. That is not an
			// attempt, so no row is touched; the next tick asks again.
			slog.Debug("calibre delivery: bridge unreachable, waiting", "pending", len(rows), "error", herr)
			d.noteReachable(false, herr.Error())
			return
		}
		if state.Degraded {
			slog.Debug("calibre delivery: bridge degraded, waiting", "pending", len(rows), "reason", state.Reason)
			d.noteReachable(false, "bridge degraded: "+state.Reason)
			return
		}
		d.noteReachable(true, "")
		target.library = state.Library
	}

	var delivered, failed, gaveUp, skipped int
	for i := range rows {
		if ctx.Err() != nil {
			break
		}
		res := d.deliver(ctx, &rows[i], target)
		switch res {
		case deliveryDelivered:
			delivered++
		case deliveryFailed:
			failed++
		case deliveryGaveUp:
			gaveUp++
		case deliverySkipped:
			skipped++
		}
		if res == deliveryStop {
			break
		}
	}
	if delivered+failed+gaveUp+skipped > 0 {
		slog.Info("calibre delivery pass",
			"mode", mode, "delivered", delivered, "failed", failed+gaveUp, "gaveUp", gaveUp, "skipped", skipped)
	}
}

// deliver sends one row and records how it went.
func (d *Deliverer) deliver(ctx context.Context, row *models.CalibreDelivery, target deliveryTarget) deliveryResult {
	book, err := d.books.GetByID(ctx, row.BookID)
	if err != nil {
		if ctx.Err() != nil {
			return deliveryStop
		}
		slog.Warn("calibre delivery: loading the book failed", "bookId", row.BookID, "error", err)
		return deliveryUntouched
	}
	if book == nil {
		return d.skip(ctx, row, deliverySkipBookGone)
	}
	files, err := d.books.ListFiles(ctx, book.ID)
	if err != nil {
		if ctx.Err() != nil {
			return deliveryStop
		}
		slog.Warn("calibre delivery: listing the book's files failed", "bookId", book.ID, "error", err)
		return deliveryUntouched
	}
	path := ""
	for _, f := range files {
		if f.ID == row.BookFileID {
			path = f.Path
			break
		}
	}
	if path == "" {
		return d.skip(ctx, row, deliverySkipFileGone)
	}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return d.skip(ctx, row, deliverySkipNoFile)
		}
		return d.fail(ctx, row, book, "file_unreadable", err)
	}

	meta := d.metadata(ctx, book, row, path, target)
	id, addErr := target.adder.Add(ctx, path, meta)
	// Once Calibre has the book, the result is recorded even if shutdown
	// cancelled ctx while the add was in flight. Losing it would only cost a
	// 409 on the next start, but there is no reason to lose it.
	record := context.WithoutCancel(ctx)
	if addErr == nil || errors.Is(addErr, ErrAlreadyInCalibre) {
		d.noteReachable(true, "")
	}
	switch {
	case addErr == nil:
		if err := d.store.MarkDelivered(record, row.ID, id, DeliveryOutcomeAdded, target.library); err != nil {
			return d.lostRow(row, err)
		}
		d.recordSourceID(record, book, id, target)
		slog.Debug("calibre delivery: book added", "bookId", book.ID, "calibreId", id, "path", path)
		return deliveryDelivered
	case errors.Is(addErr, ErrAlreadyInCalibre):
		// Ownership is read from the ledger before this row joins it, so
		// only an earlier delivery of this book to this id counts.
		owned := id > 0 && d.owns(ctx, book.ID, id, target.library)
		if err := d.store.MarkDelivered(record, row.ID, id, DeliveryOutcomeAlready, target.library); err != nil {
			return d.lostRow(row, err)
		}
		d.recordSourceID(record, book, id, target)
		slog.Info("calibre delivery: book already in Calibre", "bookId", book.ID, "calibreId", id, "owned", owned)
		if owned {
			d.refreshMetadata(ctx, book.ID, id, meta, target.adder)
		}
		return deliveryDelivered
	case errors.Is(addErr, ErrDisabled):
		// The adder was built from the same settings read as the mode, so
		// this can only be a wiring bug. Stop without counting an attempt.
		slog.Warn("calibre delivery: the adder reports the integration disabled while the configured mode is on; this is a wiring bug, please report it",
			"mode", target.mode, "bookId", book.ID)
		return deliveryStop
	case isDeliveryUnreachable(ctx, addErr):
		// Calibre went away mid batch. Not the book's fault and not an
		// attempt: leave the row and stop the pass.
		slog.Debug("calibre delivery: Calibre became unreachable, stopping the pass", "bookId", book.ID, "error", addErr)
		if ctx.Err() == nil {
			d.noteReachable(false, addErr.Error())
		}
		return deliveryStop
	default:
		return d.fail(ctx, row, book, PluginErrorCode(addErr), addErr)
	}
}

// fail records one failed attempt with backoff, or gives up for good when the
// error cannot be fixed by waiting or the row is out of attempts.
func (d *Deliverer) fail(ctx context.Context, row *models.CalibreDelivery, book *models.Book, code string, cause error) deliveryResult {
	attempts := row.Attempts + 1
	next, terminal := nextDeliveryAttempt(d.now(), attempts)
	if deliveryTerminalCodes[code] {
		terminal = true
	}
	if err := d.store.MarkFailed(ctx, row.ID, code, cause.Error(), next, terminal); err != nil {
		return d.lostRow(row, err)
	}
	if terminal {
		slog.Warn("calibre delivery: giving up on a book",
			"bookId", book.ID, "path", row.FilePath, "code", code, "attempts", attempts, "error", cause)
		return deliveryGaveUp
	}
	// The first failure is a WARN so the cause is visible at the default
	// log level long before the row gives up; the repeats are not.
	level := slog.LevelDebug
	if attempts == 1 {
		level = slog.LevelWarn
	}
	slog.Log(ctx, level, "calibre delivery: add failed, will retry",
		"bookId", book.ID, "path", row.FilePath, "code", code, "attempts", attempts, "next", next, "error", cause)
	return deliveryFailed
}

func (d *Deliverer) skip(ctx context.Context, row *models.CalibreDelivery, reason string) deliveryResult {
	if err := d.store.MarkSkipped(ctx, row.ID, reason); err != nil {
		return d.lostRow(row, err)
	}
	slog.Info("calibre delivery: skipped", "bookId", row.BookID, "path", row.FilePath, "reason", reason)
	return deliverySkipped
}

// lostRow handles a ledger write that failed. A row that vanished (cleared,
// or cascaded with its file) is not a problem; anything else is logged.
func (d *Deliverer) lostRow(row *models.CalibreDelivery, err error) deliveryResult {
	if !errors.Is(err, db.ErrCalibreDeliveryNotFound) {
		slog.Warn("calibre delivery: recording the result failed", "deliveryId", row.ID, "bookId", row.BookID, "error", err)
	}
	return deliveryUntouched
}

// owns reports whether Bindery put calibreID into the target library for
// this book itself: the ledger holds an earlier delivery of it. A backfilled
// row has no target library recorded and counts for any target.
func (d *Deliverer) owns(ctx context.Context, bookID, calibreID int64, library string) bool {
	rows, err := d.store.DeliveredByCalibreID(ctx, calibreID)
	if err != nil {
		slog.Warn("calibre delivery: ownership lookup failed", "bookId", bookID, "calibreId", calibreID, "error", err)
		return false
	}
	for _, r := range rows {
		if r.BookID != bookID {
			continue
		}
		if strings.TrimSpace(r.TargetLibrary) == "" || cleanLibraryPath(r.TargetLibrary) == cleanLibraryPath(library) {
			return true
		}
	}
	return false
}

// refreshMetadata fills empty fields on a Calibre row Bindery created itself.
// The first time Bindery meets a row it did not create, it writes nothing:
// that row may be one the user curated in Calibre.
func (d *Deliverer) refreshMetadata(ctx context.Context, bookID, calibreID int64, meta Metadata, adder Adder) {
	if meta.IsEmpty() {
		return
	}
	updater, ok := adder.(metadataUpdater)
	if !ok || !updater.SupportsMetadataUpdate(ctx) {
		return
	}
	fields, err := updater.UpdateMetadata(ctx, calibreID, meta)
	if err != nil {
		slog.Warn("calibre delivery: metadata update failed", "bookId", bookID, "calibreId", calibreID, "error", err)
		return
	}
	if len(fields) > 0 {
		slog.Info("calibre delivery: filled empty fields on a book Bindery had already pushed",
			"bookId", bookID, "calibreId", calibreID, "fields", strings.Join(fields, ","))
	}
}

// recordSourceID fills books.calibre_id from a delivery, which is only right
// when the id belongs to the library at calibre.library_path. So: the book
// has no id yet, it did not come from a Calibre import (its id is a source
// id and a push must not replace it), and either no library path is set or
// the target is that same library.
func (d *Deliverer) recordSourceID(ctx context.Context, book *models.Book, calibreID int64, target deliveryTarget) {
	if calibreID <= 0 || book.CalibreID != nil || isCalibreOrigin(book) {
		return
	}
	if cleanLibraryPath(target.cfg.LibraryPath) != "" && !sameLibraryPath(target.cfg.LibraryPath, target.library) {
		return
	}
	if _, err := d.books.SetCalibreIDIfUnset(ctx, book.ID, calibreID); err != nil {
		slog.Warn("calibre delivery: persist calibre_id failed", "bookId", book.ID, "calibreId", calibreID, "error", err)
	}
}

// metadata builds the payload at delivery time, from the book as it is now.
func (d *Deliverer) metadata(ctx context.Context, book *models.Book, row *models.CalibreDelivery, path string, target deliveryTarget) Metadata {
	edition := d.edition(ctx, book, row, path)
	var author *models.Author
	if d.authors != nil && book.AuthorID != 0 {
		a, err := d.authors.GetByID(ctx, book.AuthorID)
		if err != nil {
			slog.Debug("calibre delivery: author lookup failed", "bookId", book.ID, "error", err)
		}
		author = a
	}
	if author == nil && book.Author != nil {
		author = book.Author
	}
	var seriesTitle, seriesIndex string
	if d.series != nil {
		t, i, err := d.series.GetPrimarySeriesForBook(ctx, book.ID)
		if err != nil {
			slog.Debug("calibre delivery: primary series lookup failed", "bookId", book.ID, "error", err)
		} else {
			seriesTitle, seriesIndex = t, i
		}
	}
	identifiers := IdentifiersForBook(book, edition)
	if isCalibreOrigin(book) && !sameLibraryPath(target.cfg.LibraryPath, target.library) {
		// A source library id means nothing in another library.
		delete(identifiers, "calibre")
	}
	meta := BuildMetadata(MetadataSource{
		Book:        book,
		Author:      author,
		Edition:     edition,
		SeriesTitle: seriesTitle,
		SeriesIndex: seriesIndex,
		Identifiers: identifiers,
	})
	meta.CoverPath = d.covers.PathFor(ctx, CoverSourceFor(book, edition), target.adder)
	return meta
}

// edition is the row's edition when it still exists, else one matched to the
// file the way the bulk push matches it.
func (d *Deliverer) edition(ctx context.Context, book *models.Book, row *models.CalibreDelivery, path string) *models.Edition {
	if d.editions == nil {
		return nil
	}
	editions, err := d.editions.ListByBook(ctx, book.ID)
	if err != nil {
		slog.Debug("calibre delivery: edition lookup failed", "bookId", book.ID, "error", err)
		return nil
	}
	if row.EditionID != nil {
		for i := range editions {
			if editions[i].ID == *row.EditionID {
				return &editions[i]
			}
		}
	}
	return editionForFile(editions, book, path)
}

// isDeliveryUnreachable reports whether err means Calibre could not be
// reached or is busy, rather than that it looked at this book and refused.
func isDeliveryUnreachable(ctx context.Context, err error) bool {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	// 503 is the plugin saying Calibre is mid library swap, and the client
	// has already retried it for about thirty seconds. That is Calibre
	// being busy, not the book being bad.
	var pe *PluginError
	return errors.As(err, &pe) && pe.Status == http.StatusServiceUnavailable
}
