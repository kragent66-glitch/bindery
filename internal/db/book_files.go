package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vavallee/bindery/internal/models"
)

// BookFileRepo manages the book_files table.
type BookFileRepo struct {
	db *sql.DB
}

// NewBookFileRepo creates a new BookFileRepo backed by the given database.
func NewBookFileRepo(db *sql.DB) *BookFileRepo {
	return &BookFileRepo{db: db}
}

// Fingerprint reads a cheap snapshot of book_files — its row count and its
// highest id — for a cache keyed on the table's actual contents rather than a
// counter this repo maintains itself (the manual-import scan's tracked-file
// index, #2480).
//
// An earlier version of that cache was keyed on an atomic counter bumped by
// this repo's own mutating methods, which went stale: book_files rows also
// disappear through the books(id) ON DELETE CASCADE FK when a book or author
// is deleted (BookRepo.Delete, AuthorRepo.Delete), and through
// BookRepo.UntrackFilePath's rollback DELETE — neither goes through this repo,
// so neither could bump its counter. Reading count+maxID directly from the
// table instead catches every mutation regardless of which code path made it:
// a row removed without a matching insert changes the count, and any insert
// hands out a strictly larger AUTOINCREMENT id, so the pair only repeats when
// nothing actually changed. The query is a single indexed aggregate, cheap
// enough to run on every scan request.
func (r *BookFileRepo) Fingerprint(ctx context.Context) (count int64, maxID int64, err error) {
	err = r.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MAX(id), 0) FROM book_files`).Scan(&count, &maxID)
	if err != nil {
		return 0, 0, fmt.Errorf("book_files fingerprint: %w", err)
	}
	return count, maxID, nil
}

// pathEpochs holds one counter per database, bumped by every in place rewrite
// of book_files.path (UpdatePath). Fingerprint's (count, maxID) pair catches
// inserts and deletes however they happen, including the FK cascade, but an
// UPDATE changes neither number, so the manual-import scan's tracked-file
// cache (#2480) keys on this as well. It is keyed by *sql.DB rather than held
// on the repo because production builds more than one BookFileRepo over the
// same database (BookRepo owns one, the importer wiring another), and the
// rename has to be visible to every reader regardless of which one wrote it.
var pathEpochs sync.Map // *sql.DB -> *atomic.Uint64

func pathEpochFor(db *sql.DB) *atomic.Uint64 {
	v, _ := pathEpochs.LoadOrStore(db, new(atomic.Uint64))
	return v.(*atomic.Uint64)
}

// PathEpoch returns the in place path rewrite counter for this database. Read
// it before Fingerprint and the rows a cache is built from: a rename that
// lands mid rebuild then shows up as a changed epoch on the next read, so the
// cache errs toward rebuilding, never toward serving a stale path.
func (r *BookFileRepo) PathEpoch() uint64 {
	return pathEpochFor(r.db).Load()
}

// Add inserts a book_files row. Duplicate paths are silently ignored (INSERT OR IGNORE).
func (r *BookFileRepo) Add(ctx context.Context, bookID int64, format, path string) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO book_files (book_id, format, path, size_bytes, created_at)
		 VALUES (?, ?, ?, 0, ?)`,
		bookID, format, path, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("book_files add: %w", err)
	}
	return nil
}

// AddIfMissing records a new on-disk file and reports whether this call is the
// one that inserted it. The path column is globally UNIQUE and the insert is
// OR IGNORE, so a row another book (or an earlier run) already owns comes back
// as false rather than being stolen or duplicated.
//
// Callers that need to know what they own use this instead of Add: the Calibre
// importer may only claim, and later roll back, a file row it actually created
// (#1635), mirroring SeriesRepo.LinkBookIfMissing.
func (r *BookFileRepo) AddIfMissing(ctx context.Context, bookID int64, format, path string) (bool, error) {
	res, err := r.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO book_files (book_id, format, path, size_bytes, created_at)
		 VALUES (?, ?, ?, 0, ?)`,
		bookID, format, path, time.Now().UTC())
	if err != nil {
		return false, fmt.Errorf("book_files add if missing: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("book_files add if missing rows: %w", err)
	}
	return n > 0, nil
}

// UpdatePath changes the on-disk path of the book_files row with the given id.
// Used by the library reorganize action (#1181) after a tracked file is moved
// to the location the current naming template computes. The path column is
// globally UNIQUE, so a move onto a path another row already owns fails here
// rather than silently corrupting the index.
func (r *BookFileRepo) UpdatePath(ctx context.Context, id int64, newPath string) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE book_files SET path = ? WHERE id = ?`, newPath, id)
	if err != nil {
		return fmt.Errorf("book_files update path: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("book_files update path rows: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("book_files update path: no row with id %d", id)
	}
	pathEpochFor(r.db).Add(1)
	return nil
}

// BookIDForFile returns the owning book_id for a book_files row id, or 0 when
// no such row exists. Used by the reorganize apply path (#1181) to resolve a
// file the client picked back to its book.
func (r *BookFileRepo) BookIDForFile(ctx context.Context, fileID int64) (int64, error) {
	var bookID int64
	err := r.db.QueryRowContext(ctx,
		`SELECT book_id FROM book_files WHERE id = ?`, fileID).Scan(&bookID)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("book_files book_id lookup: %w", err)
	}
	return bookID, nil
}

// ListByBook returns all book_files rows for the given book, ordered by id.
func (r *BookFileRepo) ListByBook(ctx context.Context, bookID int64) ([]models.BookFile, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, book_id, format, path, size_bytes, created_at
		 FROM book_files WHERE book_id = ? ORDER BY id`,
		bookID)
	if err != nil {
		return nil, fmt.Errorf("book_files list: %w", err)
	}
	defer rows.Close()

	var files []models.BookFile
	for rows.Next() {
		var f models.BookFile
		if err := rows.Scan(&f.ID, &f.BookID, &f.Format, &f.Path, &f.SizeBytes, &f.CreatedAt); err != nil {
			return nil, fmt.Errorf("book_files scan: %w", err)
		}
		files = append(files, f)
	}
	return files, rows.Err()
}

// ListByBooks returns every book_files row for any of the given book IDs, in
// one query, grouped by book_id. Used by the manual-import scan
// (ManualImportHandler.Scan, #2480) to replace an N+1 ListByBook call per
// confident catalogue match with a single round trip. Duplicate IDs collapse
// naturally (IN ignores repeats); an empty bookIDs returns an empty map.
func (r *BookFileRepo) ListByBooks(ctx context.Context, bookIDs []int64) (map[int64][]models.BookFile, error) {
	result := make(map[int64][]models.BookFile, len(bookIDs))
	if len(bookIDs) == 0 {
		return result, nil
	}
	placeholders := make([]string, len(bookIDs))
	args := make([]any, len(bookIDs))
	for i, id := range bookIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	query := `SELECT id, book_id, format, path, size_bytes, created_at
		FROM book_files WHERE book_id IN (` + strings.Join(placeholders, ",") + `) ORDER BY id` // #nosec G202 -- placeholders are generated from fixed ? tokens; book IDs remain bound args
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("book_files list by books: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var f models.BookFile
		if err := rows.Scan(&f.ID, &f.BookID, &f.Format, &f.Path, &f.SizeBytes, &f.CreatedAt); err != nil {
			return nil, fmt.Errorf("book_files scan: %w", err)
		}
		result[f.BookID] = append(result[f.BookID], f)
	}
	return result, rows.Err()
}

// DeleteByBook removes all book_files rows for the given book.
func (r *BookFileRepo) DeleteByBook(ctx context.Context, bookID int64) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM book_files WHERE book_id = ?`, bookID)
	if err != nil {
		return fmt.Errorf("book_files delete by book: %w", err)
	}
	return nil
}

// DeleteByPath removes the book_files row matching the given path and returns
// the book_id of the deleted row (0 if no row matched).
func (r *BookFileRepo) DeleteByPath(ctx context.Context, path string) (int64, error) {
	var bookID int64
	err := r.db.QueryRowContext(ctx,
		`SELECT book_id FROM book_files WHERE path = ?`, path).Scan(&bookID)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("book_files lookup by path: %w", err)
	}
	if _, err := r.db.ExecContext(ctx, `DELETE FROM book_files WHERE path = ?`, path); err != nil {
		return 0, fmt.Errorf("book_files delete by path: %w", err)
	}
	return bookID, nil
}

// PathOwnedByOtherBook reports whether the given on-disk path is present in
// book_files under a book other than excludeBookID. The path column is globally
// UNIQUE, so there is at most one owner. Pass excludeBookID=0 to treat ANY
// registered owner as "another book" (e.g. when the current book's rows have
// already been cascade-deleted). The delete and reassign-cleanup paths use this
// to avoid os.Remove-ing a file another book still owns (#1368).
func (r *BookFileRepo) PathOwnedByOtherBook(ctx context.Context, path string, excludeBookID int64) (bool, error) {
	var owner int64
	err := r.db.QueryRowContext(ctx, `SELECT book_id FROM book_files WHERE path = ? LIMIT 1`, path).Scan(&owner)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("book_files owner lookup: %w", err)
	}
	return owner != excludeBookID, nil
}

// ListAllPaths returns every path currently registered in book_files.
// Used by ScanLibrary to build the set of already-tracked files.
func (r *BookFileRepo) ListAllPaths(ctx context.Context) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT path FROM book_files`)
	if err != nil {
		return nil, fmt.Errorf("book_files list all paths: %w", err)
	}
	defer rows.Close()

	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("book_files scan path: %w", err)
		}
		paths = append(paths, p)
	}
	return paths, rows.Err()
}

// RecentEbookPaths returns up to limit ebook paths, newest first. The Calibre
// Test connection walks them for one that exists on Bindery's side and asks
// the plugin to open it through the push remap, because the library root on
// its own is an exact prefix match that never exercises the remap's join
// (#2831).
func (r *BookFileRepo) RecentEbookPaths(ctx context.Context, limit int) ([]string, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT path FROM book_files WHERE format = 'ebook' ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("book_files recent ebook paths: %w", err)
	}
	defer rows.Close()

	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("book_files scan recent ebook path: %w", err)
		}
		paths = append(paths, p)
	}
	return paths, rows.Err()
}
