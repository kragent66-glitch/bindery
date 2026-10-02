package calibre

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vavallee/bindery/internal/jobs"
	"github.com/vavallee/bindery/internal/models"
)

// SyncError is a per-book failure entry returned to the UI.
type SyncError struct {
	BookID int64  `json:"bookId"`
	Title  string `json:"title"`
	Path   string `json:"path,omitempty"`
	Reason string `json:"reason"`
}

// SyncSkip is a per-book record of a book the bulk push did not attempt.
// Before these existed, a skipped book appeared in no counter, no error row
// and no log line, so "Pushed 0, already in Calibre 0, failed 0" could mean
// either "your library is already in Calibre" or "every book was dropped by a
// filter you cannot see" (discussion #1592).
type SyncSkip struct {
	BookID int64  `json:"bookId"`
	Title  string `json:"title"`
	Reason string `json:"reason"`
}

// Skip reasons. A book that has not been imported is not among them: it has
// no file, so it was never a candidate, and listing it drowned the reasons a
// user can act on (a 3,000 book wanted list filled the 50 row sample before
// any unmonitored or missing file book was reached). Kept free of commas so they read correctly when the zero-case
// message joins several of them into one sentence.
const (
	// SkipReasonNotMonitored is an imported book that is unmonitored. The
	// bulk push has always inherited this filter from ListByStatus; it was
	// simply invisible.
	SkipReasonNotMonitored = "not monitored"
	// SkipReasonNoFile is an imported book with no file path at all, which
	// means the importer crashed or the file was deleted underneath Bindery.
	SkipReasonNoFile = "no file on disk"
	// SkipReasonAudiobookOnly is an imported book that only has an audiobook.
	// The plugin endpoint takes one ebook file, so there is nothing to send.
	SkipReasonAudiobookOnly = "audiobook only with no ebook"
	// SkipReasonUntracked is a book whose only file path is the legacy
	// books.file_path column, with no ebook row in book_files behind it. The
	// delivery ledger is keyed on book_files, so there is nothing to queue.
	// A library rescan tracks the file and clears this.
	SkipReasonUntracked = "ebook file not tracked"
)

// maxSyncErrors caps the per-book error and skip lists returned on every
// status poll. A run that fails on every book (a library path the Calibre
// container cannot see, #1346) would otherwise re-serialize thousands of
// near identical entries on each poll. The full counts are always in Stats;
// the lists are a sample for display.
const maxSyncErrors = 50

// syncRemovedReason is the failure shown for a row this run queued that was
// gone from the ledger before it was delivered: cleared from the queue, reset,
// or removed with its file.
const syncRemovedReason = "removed from the queue before it was delivered"

// SyncStats summarises one Push all run, read from the delivery ledger.
// Counts are per ebook file. Pushed counts the run's files Calibre newly took,
// as a new record or as a format added to one; AlreadyInCalibre
// counts the ones Calibre already had, including files delivered before this
// run began; Failed counts the ones that failed for good (a retrying row is
// still pending).
type SyncStats struct {
	Total            int `json:"total"`
	Processed        int `json:"processed"`
	Pushed           int `json:"pushed"`
	AlreadyInCalibre int `json:"alreadyInCalibre"`
	Failed           int `json:"failed"`
	// Skipped counts books the run never queued, plus queued books the worker
	// then skipped because the file had gone. It is deliberately not part of
	// Total: Total is the denominator of the progress bar and only counts
	// work the run is actually going to do.
	Skipped int `json:"skipped"`
}

// SyncProgress is the polled shape for /calibre/sync/status. Running=false
// with a non-nil FinishedAt means the last run is complete; Running=false
// with StartedAt zero means nothing has been run yet this process.
type SyncProgress struct {
	// Running is true while the run is queueing or any book it queued is
	// still waiting for Calibre, which lasts as long as Calibre is closed.
	Running bool `json:"running"`
	// Queueing is the part of Running that is Push all itself walking the
	// library. Only this part blocks a second Push all.
	Queueing   bool        `json:"queueing,omitempty"`
	StartedAt  time.Time   `json:"startedAt"`
	FinishedAt *time.Time  `json:"finishedAt,omitempty"`
	Message    string      `json:"message,omitempty"`
	Error      string      `json:"error,omitempty"`
	Stats      SyncStats   `json:"stats"`
	Errors     []SyncError `json:"errors"`
	// Skips samples the books the run did not attempt, capped the same way
	// Errors is. Stats.Skipped always holds the full count.
	Skips []SyncSkip `json:"skips"`
}

// BookLister is the subset of *db.BookRepo the syncer uses. Keeps the
// dependency narrow for tests.
type BookLister interface {
	// List returns the whole visible catalogue. It exists only so the run can
	// say why a book was left out; eligibility still comes from ListByStatus.
	List(ctx context.Context) ([]models.Book, error)
	ListByStatus(ctx context.Context, status string) ([]models.Book, error)
	ListFiles(ctx context.Context, bookID int64) ([]models.BookFile, error)
}

// SyncQueue is where Push all puts books: the delivery worker's queue.
// *Deliverer implements it.
type SyncQueue interface {
	Enqueue(ctx context.Context, bookID, bookFileID int64, editionID *int64, path string) (bool, error)
	Kick()
}

// SyncLedger reads the delivery ledger. *db.CalibreDeliveryRepo implements it.
type SyncLedger interface {
	ListAll(ctx context.Context) ([]models.CalibreDelivery, error)
}

// SeriesGetter is the subset of *db.SeriesRepo used to attach series and
// series index to a delivered book.
type SeriesGetter interface {
	GetPrimarySeriesForBook(ctx context.Context, bookID int64) (string, string, error)
}

// AuthorGetter is the subset of *db.AuthorRepo used to add author metadata
// to a delivery.
type AuthorGetter interface {
	GetByID(ctx context.Context, id int64) (*models.Author, error)
}

// EditionLister is the subset of *db.EditionRepo used to identify the
// specific edition for the file being delivered.
type EditionLister interface {
	ListByBook(ctx context.Context, bookID int64) ([]models.Edition, error)
}

// Syncer is "Push all to Calibre" (#2832). It no longer talks to Calibre: it
// queues every ebook file of every eligible book that the delivery ledger does not
// already hold and kicks the delivery worker, which does the sending,
// retrying and recording. Status is read back from the ledger, so what the
// modal reports is what the worker actually did.
//
// Only the queueing phase is exclusive. Once it is done a second Push all may
// start, and simply finds the first run's rows already in the ledger.
type Syncer struct {
	books  BookLister
	ledger SyncLedger
	queue  SyncQueue
	jobs   *jobs.Group
	now    func() time.Time

	mu  sync.Mutex
	run *syncRun
}

// trackedFile is one ebook file the run follows through the ledger. A book
// with several formats has one per file.
type trackedFile struct {
	bookID int64
	title  string
	path   string
	// before is true when the book's row was already delivered when the run
	// began. That book was in Calibre before this Push all, whatever outcome
	// its row records, so it counts as already in Calibre and not as pushed.
	before bool
}

// syncRun is the in-memory record of the latest Push all. It holds what the
// ledger cannot say: which rows belong to the run and why books were left
// out. Everything else is read from the ledger on each poll.
type syncRun struct {
	startedAt  time.Time
	finishedAt *time.Time
	queueing   bool
	err        string
	// nothing is the zero case message naming why nothing was queued.
	nothing string
	tracked map[int64]trackedFile // by book_file_id
	skipped int
	skips   []SyncSkip
	// failed and errors are books the run could not queue at all.
	failed int
	errors []SyncError
}

// NewSyncer wires Push all against the books repo, the delivery ledger and
// the worker's queue.
func NewSyncer(books BookLister, ledger SyncLedger, queue SyncQueue) *Syncer {
	return &Syncer{books: books, ledger: ledger, queue: queue, now: time.Now}
}

// WithJobs runs the queueing phase in the process-wide jobs group, so it is
// cancelled at shutdown and the database is not closed under it.
func (s *Syncer) WithJobs(g *jobs.Group) *Syncer {
	s.jobs = g
	return s
}

// ErrSyncAlreadyRunning is returned when Start is called while a previous
// run is still queueing. Maps to 409 Conflict at the API layer.
var ErrSyncAlreadyRunning = errors.New("calibre sync already running")

// ErrSyncModeNotPlugin is returned when Start is called while the Calibre
// integration is not in plugin mode. The delivery worker serves calibredb
// mode too, but calibredb has no "already in the library" answer: its add
// refuses a duplicate with an error, so a bulk run would turn every book the
// library already holds into a failed row. The plugin answers 409 instead.
var ErrSyncModeNotPlugin = errors.New("calibre sync requires mode=plugin")

// Start begins a Push all: the queueing runs in the background and the
// caller polls Progress. It returns at once.
func (s *Syncer) Start(mode Mode) error {
	if mode != ModePlugin {
		return ErrSyncModeNotPlugin
	}
	s.mu.Lock()
	if s.run != nil && s.run.queueing {
		s.mu.Unlock()
		return ErrSyncAlreadyRunning
	}
	run := &syncRun{
		startedAt: s.now().UTC(),
		queueing:  true,
		tracked:   map[int64]trackedFile{},
		skips:     []SyncSkip{},
		errors:    []SyncError{},
	}
	s.run = run
	s.mu.Unlock()

	work := func(ctx context.Context) { s.queueAll(ctx, run) }
	if s.jobs == nil {
		go work(context.Background())
		return nil
	}
	if !s.jobs.Go("calibre-push-all", work) {
		s.finishQueueing(run, "Bindery is shutting down")
	}
	return nil
}

func (s *Syncer) finishQueueing(run *syncRun, errMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run.queueing = false
	if errMsg != "" {
		run.err = errMsg
		now := s.now().UTC()
		run.finishedAt = &now
	}
}

// queueAll walks the imported books and queues each eligible one's ebook
// file, unless the ledger already holds a row for one of its ebook files.
func (s *Syncer) queueAll(ctx context.Context, run *syncRun) {
	failMsg := ""
	defer func() { s.finishQueueing(run, failMsg) }()
	fail := func(msg string) {
		failMsg = msg
		slog.Error("calibre sync failed", "error", msg)
	}

	books, err := s.books.ListByStatus(ctx, models.BookStatusImported)
	if err != nil {
		fail("list imported books: " + err.Error())
		return
	}
	ledger, err := s.ledger.ListAll(ctx)
	if err != nil {
		fail("read the delivery queue: " + err.Error())
		return
	}
	byFile := make(map[int64]models.CalibreDelivery, len(ledger))
	for _, row := range ledger {
		byFile[row.BookFileID] = row
	}

	skipCounts := map[string]int{}
	recordSkip := func(b *models.Book, reason string) {
		skipCounts[reason]++
		s.mu.Lock()
		run.skipped++
		if len(run.skips) < maxSyncErrors {
			run.skips = append(run.skips, SyncSkip{BookID: b.ID, Title: b.Title, Reason: reason})
		}
		s.mu.Unlock()
	}
	recordFailure := func(b *models.Book, reason string) {
		s.mu.Lock()
		run.failed++
		if len(run.errors) < maxSyncErrors {
			run.errors = append(run.errors, SyncError{BookID: b.ID, Title: b.Title, Reason: reason})
		}
		s.mu.Unlock()
		slog.Warn("calibre sync: could not queue a book", "bookId", b.ID, "title", b.Title, "reason", reason)
	}
	track := func(b *models.Book, file models.BookFile, before bool) {
		s.mu.Lock()
		run.tracked[file.ID] = trackedFile{bookID: b.ID, title: b.Title, path: file.Path, before: before}
		s.mu.Unlock()
	}

	queued := 0
	for i := range books {
		if err := ctx.Err(); err != nil {
			fail("cancelled: " + err.Error())
			return
		}
		b := &books[i]
		files, err := s.books.ListFiles(ctx, b.ID)
		if err != nil {
			recordFailure(b, "listing the book's files failed: "+err.Error())
			continue
		}
		ebooks := make([]models.BookFile, 0, len(files))
		for _, f := range files {
			if f.Format == models.MediaTypeEbook {
				ebooks = append(ebooks, f)
			}
		}
		if len(ebooks) == 0 {
			switch {
			case strings.TrimSpace(b.AudiobookFilePath) != "":
				recordSkip(b, SkipReasonAudiobookOnly)
			case pushPath(b) != "":
				recordSkip(b, SkipReasonUntracked)
			default:
				recordSkip(b, SkipReasonNoFile)
			}
			continue
		}
		// Every ebook file is queued, in format preference order, so the
		// preferred one makes the Calibre record and the worker adds the
		// rest to it as further formats. A file the ledger already holds is
		// never queued again, in any state: a delivered one is in Calibre,
		// a pending one is queued, and a failed or skipped one waits for
		// Retry.
		sortEbooksByPreference(ebooks)
		for _, file := range ebooks {
			if row, ok := byFile[file.ID]; ok {
				track(b, file, row.State == models.CalibreDeliveryDelivered)
				continue
			}
			if _, err := s.queue.Enqueue(ctx, b.ID, file.ID, nil, file.Path); err != nil {
				recordFailure(b, "queueing failed: "+err.Error())
				continue
			}
			track(b, file, false)
			queued++
		}
	}

	// Books that never reached the eligible list at all. ListByStatus filters
	// on status AND monitored, so both are reported separately rather than as
	// one undifferentiated "not eligible".
	if all, listErr := s.books.List(ctx); listErr != nil {
		slog.Warn("calibre sync: full catalogue unavailable; skip reasons limited to the imported set", "error", listErr)
	} else {
		considered := make(map[int64]struct{}, len(books))
		for i := range books {
			considered[books[i].ID] = struct{}{}
		}
		for i := range all {
			b := &all[i]
			if _, ok := considered[b.ID]; ok {
				continue
			}
			// A book with no import has no file and was never a candidate,
			// so it is left out of the skips rather than reported as one.
			if b.Status == models.BookStatusImported {
				recordSkip(b, SkipReasonNotMonitored)
			}
		}
	}

	s.mu.Lock()
	tracked := len(run.tracked)
	if tracked == 0 && run.failed == 0 {
		run.nothing = nothingToPushMessage(skipCounts)
	}
	s.mu.Unlock()
	if len(skipCounts) > 0 {
		slog.Info("calibre sync: books skipped", "reasons", skipSummary(skipCounts))
	}
	slog.Info("calibre sync: books queued for Calibre", "queued", queued, "alreadyRecorded", tracked-queued)
	if tracked > 0 {
		s.queue.Kick()
	}
}

// sortEbooksByPreference orders a book's ebook files the way they should
// reach Calibre: by format preference, then by id within a format.
func sortEbooksByPreference(ebooks []models.BookFile) {
	sort.SliceStable(ebooks, func(i, j int) bool {
		a, b := ebooks[i], ebooks[j]
		if lessByFormatPreference(a.Path, b.Path) {
			return true
		}
		if lessByFormatPreference(b.Path, a.Path) {
			return false
		}
		return a.ID < b.ID
	})
}

// Progress reports the latest run, reading each of its rows from the ledger.
func (s *Syncer) Progress(ctx context.Context) (SyncProgress, error) {
	s.mu.Lock()
	run := s.run
	if run == nil {
		s.mu.Unlock()
		return SyncProgress{Errors: []SyncError{}, Skips: []SyncSkip{}}, nil
	}
	p := SyncProgress{
		StartedAt: run.startedAt,
		Error:     run.err,
		Errors:    append([]SyncError{}, run.errors...),
		Skips:     append([]SyncSkip{}, run.skips...),
		Stats:     SyncStats{Failed: run.failed, Skipped: run.skipped},
	}
	queueing := run.queueing
	nothing := run.nothing
	queueFailed := run.failed
	tracked := make([]struct {
		fileID int64
		f      trackedFile
	}, 0, len(run.tracked))
	for id, f := range run.tracked {
		tracked = append(tracked, struct {
			fileID int64
			f      trackedFile
		}{id, f})
	}
	if run.finishedAt != nil {
		t := *run.finishedAt
		p.FinishedAt = &t
	}
	s.mu.Unlock()

	if queueing {
		p.Running = true
		p.Queueing = true
		p.Message = "queueing books for Calibre…"
		p.Stats.Total = len(tracked) + p.Stats.Failed
		return p, nil
	}
	if p.Error != "" {
		p.Message = "failed"
		return p, nil
	}

	rows, err := s.ledger.ListAll(ctx)
	if err != nil {
		return p, err
	}
	byFile := make(map[int64]models.CalibreDelivery, len(rows))
	for _, r := range rows {
		byFile[r.BookFileID] = r
	}
	sort.Slice(tracked, func(i, j int) bool { return tracked[i].f.bookID < tracked[j].f.bookID })

	pending, workerSkipped := 0, 0
	addError := func(t trackedFile, reason string) {
		p.Stats.Failed++
		if len(p.Errors) < maxSyncErrors {
			p.Errors = append(p.Errors, SyncError{BookID: t.bookID, Title: t.title, Path: t.path, Reason: reason})
		}
	}
	for _, tf := range tracked {
		row, ok := byFile[tf.fileID]
		switch {
		case !ok:
			addError(tf.f, syncRemovedReason)
		case row.State == models.CalibreDeliveryPending:
			pending++
		case row.State == models.CalibreDeliveryDelivered:
			if tf.f.before || (row.Outcome != DeliveryOutcomeAdded && row.Outcome != DeliveryOutcomeFormatAdded) {
				p.Stats.AlreadyInCalibre++
			} else {
				p.Stats.Pushed++
			}
		case row.State == models.CalibreDeliveryFailed:
			reason := row.LastError
			if reason == "" {
				reason = row.LastErrorCode
			}
			addError(tf.f, reason)
		case row.State == models.CalibreDeliverySkipped:
			workerSkipped++
			p.Stats.Skipped++
			if len(p.Skips) < maxSyncErrors {
				p.Skips = append(p.Skips, SyncSkip{BookID: tf.f.bookID, Title: tf.f.title, Reason: row.Outcome})
			}
		}
	}
	// Total is every book the run took on: the rows it follows, less the
	// ones the worker skipped (those count under Skipped), plus the books it
	// could not queue at all.
	p.Stats.Total = len(tracked) - workerSkipped + queueFailed
	p.Stats.Processed = p.Stats.Total - pending
	p.Running = pending > 0
	switch {
	case p.Running:
		p.Message = strconv.Itoa(pending) + " waiting for Calibre"
	case p.Stats.Total == 0 && nothing != "":
		p.Message = nothing
	default:
		p.Message = "done"
	}
	if p.Running && p.FinishedAt != nil {
		// A retry put one of the run's rows back in the queue after the run
		// had settled, so it is running again.
		p.FinishedAt = nil
		s.mu.Lock()
		if s.run == run {
			run.finishedAt = nil
		}
		s.mu.Unlock()
	}
	if !p.Running && p.FinishedAt == nil {
		s.mu.Lock()
		if s.run == run && run.finishedAt == nil {
			now := s.now().UTC()
			run.finishedAt = &now
		}
		if run.finishedAt != nil {
			t := *run.finishedAt
			p.FinishedAt = &t
		}
		s.mu.Unlock()
	}
	return p, nil
}

// pushPath returns the on-disk path to send to Calibre for the given
// book, preferring the ebook-specific column (populated by dual-format
// imports) and falling back to the legacy single file_path.
func pushPath(b *models.Book) string {
	if b.EbookFilePath != "" {
		return b.EbookFilePath
	}
	return b.FilePath
}

// nothingToPushMessage replaces the old generic "no imported books with files
// to push" with the reasons the run actually found, so the #1592 report is
// self-diagnosing.
func nothingToPushMessage(counts map[string]int) string {
	summary := skipSummary(counts)
	if summary == "" {
		return "no books to push: no imported book has an ebook file yet"
	}
	return "no books to push: " + summary
}

// skipSummary renders the skip tally in a stable order, most common first.
func skipSummary(counts map[string]int) string {
	if len(counts) == 0 {
		return ""
	}
	type entry struct {
		reason string
		n      int
	}
	entries := make([]entry, 0, len(counts))
	for reason, n := range counts {
		entries = append(entries, entry{reason, n})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].n != entries[j].n {
			return entries[i].n > entries[j].n
		}
		return entries[i].reason < entries[j].reason
	})
	parts := make([]string, 0, len(entries))
	for _, e := range entries {
		parts = append(parts, strconv.Itoa(e.n)+" "+e.reason)
	}
	return strings.Join(parts, "; ")
}

// editionForFile picks the edition that describes the file at path: one whose
// format matches the file's extension, preferring the book's selected edition
// among those, then the selected edition, then the first. nil when there are
// no editions.
func editionForFile(editions []models.Edition, b *models.Book, path string) *models.Edition {
	if len(editions) == 0 {
		return nil
	}

	ext := strings.TrimPrefix(filepath.Ext(path), ".")
	if ext != "" {
		format := strings.ToUpper(strings.TrimSpace(ext))
		var firstFormatMatch *models.Edition
		for i := range editions {
			if strings.ToUpper(strings.TrimSpace(editions[i].Format)) != format {
				continue
			}
			if b.SelectedEditionID != nil && editions[i].ID == *b.SelectedEditionID {
				return &editions[i]
			}
			if firstFormatMatch == nil {
				firstFormatMatch = &editions[i]
			}
		}
		if firstFormatMatch != nil {
			return firstFormatMatch
		}
	}

	if b.SelectedEditionID != nil {
		for i := range editions {
			if editions[i].ID == *b.SelectedEditionID {
				return &editions[i]
			}
		}
	}
	return &editions[0]
}

// sameLibraryPath reports whether two library paths name the same library.
// An empty path on either side is unknown, never a match.
func sameLibraryPath(a, b string) bool {
	a, b = cleanLibraryPath(a), cleanLibraryPath(b)
	return a != "" && a == b
}

func cleanLibraryPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	return filepath.Clean(path)
}

func isCalibreOrigin(b *models.Book) bool {
	if b == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(b.MetadataProvider), "calibre") ||
		strings.HasPrefix(strings.ToLower(strings.TrimSpace(b.ForeignID)), "calibre:book:")
}
