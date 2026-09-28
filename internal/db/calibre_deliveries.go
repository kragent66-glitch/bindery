package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/vavallee/bindery/internal/models"
)

// calibreDeliveryTimeLayout is the one shape every calibre_deliveries time
// column is written in. It is RFC3339 with a fixed nine digit fraction, so it
// parses with the RFC3339Nano layout while also comparing and sorting as
// text, which the due query needs. See migration 095 for why neither
// timeValueArg (variable width fraction) nor CURRENT_TIMESTAMP will do.
const calibreDeliveryTimeLayout = "2006-01-02T15:04:05.000000000Z"

func calibreDeliveryTime(t time.Time) string {
	return t.UTC().Format(calibreDeliveryTimeLayout)
}

// CalibreDeliveryRepo is the calibre_deliveries ledger (#2832): one row per
// ebook file, recording whether it reached the Calibre push target. Every
// method is a single statement, so none of them holds a transaction across
// a Calibre call.
type CalibreDeliveryRepo struct {
	db  *sql.DB
	now func() time.Time
}

func NewCalibreDeliveryRepo(db *sql.DB) *CalibreDeliveryRepo {
	return &CalibreDeliveryRepo{db: db, now: time.Now}
}

const calibreDeliveryColumns = `id, book_id, book_file_id, edition_id, file_path, format, state,
	outcome, attempts, last_error, last_error_code, calibre_id, target_library,
	next_attempt_at, created_at, updated_at, delivered_at`

// prefixedCalibreDeliveryColumns is calibreDeliveryColumns qualified with
// the alias d, for queries that join other tables.
const prefixedCalibreDeliveryColumns = `d.id, d.book_id, d.book_file_id, d.edition_id, d.file_path, d.format, d.state,
	d.outcome, d.attempts, d.last_error, d.last_error_code, d.calibre_id, d.target_library,
	d.next_attempt_at, d.created_at, d.updated_at, d.delivered_at`

// normalizeCalibreFormat stores a format as the ledger expects it: the file
// extension, lower case, without the dot.
func normalizeCalibreFormat(format string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(format), "."))
}

// Enqueue queues bookFileID for delivery, due now. It reports whether this
// call created the row.
//
// An existing row for the file is left exactly as it is, whatever its state:
// a pending row is already queued, a delivered one must not be sent twice,
// and a failed or skipped one stays put until someone asks for it through
// Retry. Re-importing a file therefore never re-arms a delivery that gave up.
func (r *CalibreDeliveryRepo) Enqueue(ctx context.Context, bookID, bookFileID int64, editionID *int64, filePath, format string) (bool, error) {
	now := calibreDeliveryTime(r.now())
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO calibre_deliveries
			(book_id, book_file_id, edition_id, file_path, format, state,
			 next_attempt_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 'pending', ?, ?, ?)
		ON CONFLICT(book_file_id) DO NOTHING`,
		bookID, bookFileID, editionID, filepath.Clean(filePath), normalizeCalibreFormat(format), now, now, now)
	if err != nil {
		return false, fmt.Errorf("calibre deliveries enqueue: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("calibre deliveries enqueue rows: %w", err)
	}
	return n > 0, nil
}

// Get returns one row by id, or nil when there is none.
func (r *CalibreDeliveryRepo) Get(ctx context.Context, id int64) (*models.CalibreDelivery, error) {
	return r.getOne(ctx, `SELECT `+calibreDeliveryColumns+` FROM calibre_deliveries WHERE id = ?`, id)
}

// GetByBookFile returns the row for a book_files id, or nil when there is none.
func (r *CalibreDeliveryRepo) GetByBookFile(ctx context.Context, bookFileID int64) (*models.CalibreDelivery, error) {
	return r.getOne(ctx, `SELECT `+calibreDeliveryColumns+` FROM calibre_deliveries WHERE book_file_id = ?`, bookFileID)
}

func (r *CalibreDeliveryRepo) getOne(ctx context.Context, query string, arg any) (*models.CalibreDelivery, error) {
	rows, err := r.db.QueryContext(ctx, query, arg)
	if err != nil {
		return nil, fmt.Errorf("calibre deliveries get: %w", err)
	}
	out, err := scanCalibreDeliveries(rows)
	if err != nil || len(out) == 0 {
		return nil, err
	}
	return &out[0], nil
}

// DueBatch returns up to limit pending rows whose next_attempt_at is at or
// before now, oldest due first.
func (r *CalibreDeliveryRepo) DueBatch(ctx context.Context, now time.Time, limit int) ([]models.CalibreDelivery, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+calibreDeliveryColumns+` FROM calibre_deliveries
		WHERE state = 'pending' AND next_attempt_at <= ?
		ORDER BY next_attempt_at, id
		LIMIT ?`, calibreDeliveryTime(now), limit)
	if err != nil {
		return nil, fmt.Errorf("calibre deliveries due: %w", err)
	}
	return scanCalibreDeliveries(rows)
}

// MarkDelivered records a successful delivery. outcome says how it landed
// (for example "added" or "linked"); targetLibrary identifies the library
// calibreID belongs to. The last error is cleared.
func (r *CalibreDeliveryRepo) MarkDelivered(ctx context.Context, id, calibreID int64, outcome, targetLibrary string) error {
	now := calibreDeliveryTime(r.now())
	return r.execOne(ctx, "mark delivered", `
		UPDATE calibre_deliveries
		SET state = 'delivered', calibre_id = ?, outcome = ?, target_library = ?,
		    last_error = '', last_error_code = '', delivered_at = ?, updated_at = ?
		WHERE id = ?`,
		calibreID, outcome, targetLibrary, now, now, id)
}

// MarkFailed records one failed attempt and increments attempts. A terminal
// failure moves the row to failed, out of the queue until Retry; otherwise
// it stays pending and comes due again at nextAttemptAt.
//
// Only call this for an attempt that actually reached Calibre. An unreachable
// Calibre is not an attempt and should leave the row alone.
func (r *CalibreDeliveryRepo) MarkFailed(ctx context.Context, id int64, code, msg string, nextAttemptAt time.Time, terminal bool) error {
	state := models.CalibreDeliveryPending
	if terminal {
		state = models.CalibreDeliveryFailed
	}
	return r.execOne(ctx, "mark failed", `
		UPDATE calibre_deliveries
		SET state = ?, attempts = attempts + 1, last_error_code = ?, last_error = ?,
		    next_attempt_at = ?, updated_at = ?
		WHERE id = ?`,
		string(state), code, msg, calibreDeliveryTime(nextAttemptAt), calibreDeliveryTime(r.now()), id)
}

// MarkSkipped takes the row out of the queue on purpose. reason lands in
// outcome.
func (r *CalibreDeliveryRepo) MarkSkipped(ctx context.Context, id int64, reason string) error {
	return r.execOne(ctx, "mark skipped", `
		UPDATE calibre_deliveries
		SET state = 'skipped', outcome = ?, updated_at = ?
		WHERE id = ?`,
		reason, calibreDeliveryTime(r.now()), id)
}

// Retry puts failed or skipped rows back in the queue, due now, with attempts
// and the last error reset. state picks which: failed, skipped, or "" for
// both. Pending and delivered rows are never touched. It returns the number
// of rows re-queued.
func (r *CalibreDeliveryRepo) Retry(ctx context.Context, state models.CalibreDeliveryState) (int64, error) {
	// Two placeholders either way: a single state is bound twice.
	var a, b models.CalibreDeliveryState
	switch state {
	case models.CalibreDeliveryFailed, models.CalibreDeliverySkipped:
		a, b = state, state
	case "":
		a, b = models.CalibreDeliveryFailed, models.CalibreDeliverySkipped
	default:
		return 0, fmt.Errorf("calibre deliveries retry: cannot retry %q rows", state)
	}
	now := calibreDeliveryTime(r.now())
	res, err := r.db.ExecContext(ctx, `
		UPDATE calibre_deliveries
		SET state = 'pending', attempts = 0, last_error = '', last_error_code = '',
		    outcome = '', next_attempt_at = ?, updated_at = ?
		WHERE state IN (?, ?)`, now, now, string(a), string(b))
	if err != nil {
		return 0, fmt.Errorf("calibre deliveries retry: %w", err)
	}
	return res.RowsAffected()
}

// RearmSkipped puts the skipped rows whose recorded reason is exactly reason
// back in the queue, due now, with attempts and the last error reset. It is
// for a skip that a change on the Calibre side undoes, such as the bridge
// gaining a capability it lacked. Every other skipped row is left alone. It
// returns the number of rows re-queued.
func (r *CalibreDeliveryRepo) RearmSkipped(ctx context.Context, reason string) (int64, error) {
	now := calibreDeliveryTime(r.now())
	res, err := r.db.ExecContext(ctx, `
		UPDATE calibre_deliveries
		SET state = 'pending', attempts = 0, last_error = '', last_error_code = '',
		    outcome = '', next_attempt_at = ?, updated_at = ?
		WHERE state = 'skipped' AND outcome = ?`, now, now, reason)
	if err != nil {
		return 0, fmt.Errorf("calibre deliveries rearm skipped: %w", err)
	}
	return res.RowsAffected()
}

// HasSkipped reports whether any skipped row records exactly reason. The
// worker asks it while the queue is idle, to decide whether a rearm is worth
// a request to Calibre.
func (r *CalibreDeliveryRepo) HasSkipped(ctx context.Context, reason string) (bool, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM calibre_deliveries WHERE state = 'skipped' AND outcome = ?)`, reason).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("calibre deliveries has skipped: %w", err)
	}
	return n != 0, nil
}

// ClearPending drops every pending row, emptying the queue without touching
// the record of what was delivered, failed or skipped.
func (r *CalibreDeliveryRepo) ClearPending(ctx context.Context) (int64, error) {
	res, err := r.db.ExecContext(ctx, `DELETE FROM calibre_deliveries WHERE state = 'pending'`)
	if err != nil {
		return 0, fmt.Errorf("calibre deliveries clear pending: %w", err)
	}
	return res.RowsAffected()
}

// ResetAll forgets the whole ledger. It is for a change of push target: every
// row describes the old target, so a delivered row would wrongly claim the
// file is in the new library and its calibre_id would point into the wrong
// one. Deleting rather than re-queueing means nothing is sent to the new
// target until something enqueues it again.
func (r *CalibreDeliveryRepo) ResetAll(ctx context.Context) (int64, error) {
	res, err := r.db.ExecContext(ctx, `DELETE FROM calibre_deliveries`)
	if err != nil {
		return 0, fmt.Errorf("calibre deliveries reset: %w", err)
	}
	return res.RowsAffected()
}

// Summary counts rows per state and reports the most recent delivery.
func (r *CalibreDeliveryRepo) Summary(ctx context.Context) (models.CalibreDeliverySummary, error) {
	var s models.CalibreDeliverySummary
	var last sql.NullString
	err := r.db.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(state = 'pending'), 0),
			COALESCE(SUM(state = 'delivered'), 0),
			COALESCE(SUM(state = 'failed'), 0),
			COALESCE(SUM(state = 'skipped'), 0),
			MAX(CASE WHEN state = 'delivered' THEN delivered_at END)
		FROM calibre_deliveries`).Scan(&s.Pending, &s.Delivered, &s.Failed, &s.Skipped, &last)
	if err != nil {
		return s, fmt.Errorf("calibre deliveries summary: %w", err)
	}
	s.LastDeliveredAt, err = parseFlexibleTime(last)
	if err != nil {
		return s, fmt.Errorf("calibre deliveries summary: %w", err)
	}
	return s, nil
}

// List pages through the ledger, most recently updated first. An empty state
// lists every row.
func (r *CalibreDeliveryRepo) List(ctx context.Context, state models.CalibreDeliveryState, limit, offset int) ([]models.CalibreDelivery, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+calibreDeliveryColumns+` FROM calibre_deliveries
		WHERE (? = '' OR state = ?)
		ORDER BY updated_at DESC, id DESC
		LIMIT ? OFFSET ?`, string(state), string(state), limit, offset)
	if err != nil {
		return nil, fmt.Errorf("calibre deliveries list: %w", err)
	}
	return scanCalibreDeliveries(rows)
}

// ListAll returns every row, oldest first. The ledger holds one row per
// ebook file, so it is about the size of the library. Push all reads it once
// to see what is already recorded, and its status reads it on each poll.
func (r *CalibreDeliveryRepo) ListAll(ctx context.Context) ([]models.CalibreDelivery, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+calibreDeliveryColumns+` FROM calibre_deliveries ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("calibre deliveries list all: %w", err)
	}
	return scanCalibreDeliveries(rows)
}

// ListByBook returns the rows for one book, oldest first.
func (r *CalibreDeliveryRepo) ListByBook(ctx context.Context, bookID int64) ([]models.CalibreDelivery, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+calibreDeliveryColumns+` FROM calibre_deliveries
		WHERE book_id = ?
		ORDER BY id`, bookID)
	if err != nil {
		return nil, fmt.Errorf("calibre deliveries by book: %w", err)
	}
	return scanCalibreDeliveries(rows)
}

// ListWithBooks is List with each row's book title and author name, for the
// settings queue view. An empty state lists every row.
func (r *CalibreDeliveryRepo) ListWithBooks(ctx context.Context, state models.CalibreDeliveryState, limit, offset int) ([]models.CalibreDeliveryListItem, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+prefixedCalibreDeliveryColumns+`, COALESCE(b.title, ''), COALESCE(a.name, '')
		FROM calibre_deliveries d
		LEFT JOIN books   b ON b.id = d.book_id
		LEFT JOIN authors a ON a.id = b.author_id
		WHERE (? = '' OR d.state = ?)
		ORDER BY d.updated_at DESC, d.id DESC
		LIMIT ? OFFSET ?`, string(state), string(state), limit, offset)
	if err != nil {
		return nil, fmt.Errorf("calibre deliveries list with books: %w", err)
	}
	defer rows.Close()
	out := []models.CalibreDeliveryListItem{}
	for rows.Next() {
		var item models.CalibreDeliveryListItem
		d, err := scanCalibreDelivery(rows, &item.BookTitle, &item.AuthorName)
		if err != nil {
			return nil, err
		}
		item.CalibreDelivery = d
		out = append(out, item)
	}
	return out, rows.Err()
}

// DeliveredByCalibreID returns the delivered rows that point at calibreID in
// the push target, oldest first. Bindery owns a Calibre record exactly when
// one of these exists for the current target library. Callers compare
// TargetLibrary themselves: a backfilled row has it empty, because the
// library a pre-ledger push went to was never recorded.
func (r *CalibreDeliveryRepo) DeliveredByCalibreID(ctx context.Context, calibreID int64) ([]models.CalibreDelivery, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+calibreDeliveryColumns+` FROM calibre_deliveries
		WHERE state = 'delivered' AND calibre_id = ?
		ORDER BY id`, calibreID)
	if err != nil {
		return nil, fmt.Errorf("calibre deliveries by calibre id: %w", err)
	}
	return scanCalibreDeliveries(rows)
}

// ErrCalibreDeliveryNotFound is returned by the Mark methods when the row is
// gone, for example cleared by ClearPending or cascaded away with its
// book_files row while a delivery was in flight.
var ErrCalibreDeliveryNotFound = errors.New("calibre delivery not found")

func (r *CalibreDeliveryRepo) execOne(ctx context.Context, what, query string, args ...any) error {
	res, err := r.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("calibre deliveries %s: %w", what, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("calibre deliveries %s rows: %w", what, err)
	}
	if n == 0 {
		return fmt.Errorf("calibre deliveries %s: %w", what, ErrCalibreDeliveryNotFound)
	}
	return nil
}

func scanCalibreDeliveries(rows *sql.Rows) ([]models.CalibreDelivery, error) {
	defer rows.Close()
	var out []models.CalibreDelivery
	for rows.Next() {
		d, err := scanCalibreDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// scanCalibreDelivery reads calibreDeliveryColumns, then any extra columns
// the query selected after them into extra.
func scanCalibreDelivery(rows *sql.Rows, extra ...any) (models.CalibreDelivery, error) {
	var d models.CalibreDelivery
	var state string
	var edition, calibreID sql.NullInt64
	var next, created, updated, delivered sql.NullString
	dest := []any{&d.ID, &d.BookID, &d.BookFileID, &edition, &d.FilePath, &d.Format, &state,
		&d.Outcome, &d.Attempts, &d.LastError, &d.LastErrorCode, &calibreID, &d.TargetLibrary,
		&next, &created, &updated, &delivered}
	if err := rows.Scan(append(dest, extra...)...); err != nil {
		return d, fmt.Errorf("scan calibre delivery: %w", err)
	}
	d.State = models.CalibreDeliveryState(state)
	if edition.Valid {
		v := edition.Int64
		d.EditionID = &v
	}
	if calibreID.Valid {
		v := calibreID.Int64
		d.CalibreID = &v
	}
	d.NextAttemptAt = parseFlexibleTimeValue(next, "calibre_deliveries.next_attempt_at")
	d.CreatedAt = parseFlexibleTimeValue(created, "calibre_deliveries.created_at")
	d.UpdatedAt = parseFlexibleTimeValue(updated, "calibre_deliveries.updated_at")
	if t, err := parseFlexibleTime(delivered); err == nil {
		d.DeliveredAt = t
	}
	return d, nil
}
