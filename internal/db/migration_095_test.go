package db

import (
	"context"
	"database/sql"
	"testing"
)

// TestMigrate095BackfillsPushedBooks covers the calibre_deliveries backfill
// (#2832). A book whose calibre_id came from a push gets one delivered row for
// the ebook file it reports; a book that came from a Calibre library import
// gets nothing, however that origin is recorded; a book with no ebook file
// gets nothing; and running the migration again adds and changes nothing.
func TestMigrate095BackfillsPushedBooks(t *testing.T) {
	database, err := OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()

	// Put the database back at version 94: no ledger table, no marker.
	v095 := migrationVersionForTest(t, "095_calibre_deliveries.sql")
	mustExec(t, database, `DROP TABLE calibre_deliveries`)
	mustExec(t, database, `DELETE FROM schema_migrations WHERE version = ?`, v095)

	authors := NewAuthorRepo(database)
	books := NewBookRepo(database)
	author := seedAuthor(t, authors, "OL1A", "Author")

	type seeded struct {
		id       int64
		files    map[string]int64
		wantFile string // path of the file that must be backfilled, "" for none
	}
	seed := func(foreignID, provider string, calibreID any, ebookPath string, files [][2]string) seeded {
		t.Helper()
		b := seedBook(t, books, author.ID, foreignID, foreignID)
		mustExec(t, database, `UPDATE books SET metadata_provider = ?, calibre_id = ?, ebook_file_path = ? WHERE id = ?`,
			provider, calibreID, ebookPath, b.ID)
		s := seeded{id: b.ID, files: map[string]int64{}}
		for _, f := range files {
			res, err := database.ExecContext(ctx, `INSERT INTO book_files (book_id, format, path) VALUES (?, ?, ?)`, b.ID, f[0], f[1])
			if err != nil {
				t.Fatalf("seed file %s: %v", f[1], err)
			}
			id, _ := res.LastInsertId()
			s.files[f[1]] = id
		}
		return s
	}

	// Pushed, reporting its second ebook file as the ebook path: that one wins
	// over the lower id.
	pushedPath := seed("OL1W", "openlibrary", 10, "/lib/a/A.MOBI", [][2]string{
		{"ebook", "/lib/a/A.epub"},
		{"ebook", "/lib/a/A.MOBI"},
	})
	pushedPath.wantFile = "/lib/a/A.MOBI"

	// Pushed, no ebook path stored: the lowest id ebook row, not the lower id
	// audiobook row. The directory has a dot and the file has an extension.
	pushedFirst := seed("OL2W", "hardcover", 11, "", [][2]string{
		{"audiobook", "/lib/b/B (m4b)"},
		{"ebook", "/lib/v1.2/B.azw3"},
		{"ebook", "/lib/v1.2/B.pdf"},
	})
	pushedFirst.wantFile = "/lib/v1.2/B.azw3"

	// Pushed, file with no extension under a dotted directory: format stays empty.
	pushedNoExt := seed("OL3W", "openlibrary", 12, "", [][2]string{{"ebook", "/lib/c.d/book"}})
	pushedNoExt.wantFile = "/lib/c.d/book"

	// Calibre origin, each marker on its own.
	byForeignID := seed("calibre:book:5", "openlibrary", 5, "", [][2]string{{"ebook", "/cal/5.epub"}})
	byProvider := seed("OL4W", " Calibre ", 6, "", [][2]string{{"ebook", "/cal/6.epub"}})
	byProvenance := seed("OL5W", "openlibrary", 7, "", [][2]string{{"ebook", "/cal/7.epub"}})
	mustExec(t, database, `INSERT INTO calibre_provenance (entity_type, external_id, local_id) VALUES ('book', 'calibre:book:7', ?)`, byProvenance.id)
	// A provenance row for another entity type with the same local id is not
	// a book import and must not exclude anything.
	pushedOtherProv := seed("OL6W", "openlibrary", 13, "", [][2]string{{"ebook", "/lib/e/E.epub"}})
	pushedOtherProv.wantFile = "/lib/e/E.epub"
	mustExec(t, database, `INSERT INTO calibre_provenance (entity_type, external_id, local_id) VALUES ('author', 'calibre:author:1', ?)`, pushedOtherProv.id)

	// Pushed but only an audiobook file, and never pushed at all.
	audioOnly := seed("OL7W", "openlibrary", 14, "", [][2]string{{"audiobook", "/lib/f/F.m4b"}})
	notPushed := seed("OL8W", "openlibrary", nil, "", [][2]string{{"ebook", "/lib/g/G.epub"}})

	if err := migrate(database); err != nil {
		t.Fatalf("migrate to 095: %v", err)
	}

	type row struct {
		bookID, fileID, calibreID          int64
		path, format, state, outcome, next string
		attempts                           int
		delivered                          sql.NullString
	}
	readRows := func() map[int64]row {
		t.Helper()
		rs, err := database.QueryContext(ctx, `SELECT book_id, book_file_id, calibre_id, file_path, format, state, outcome,
			CAST(next_attempt_at AS TEXT), attempts, CAST(delivered_at AS TEXT) FROM calibre_deliveries`)
		if err != nil {
			t.Fatal(err)
		}
		defer rs.Close()
		out := map[int64]row{}
		for rs.Next() {
			var r row
			if err := rs.Scan(&r.bookID, &r.fileID, &r.calibreID, &r.path, &r.format, &r.state, &r.outcome, &r.next, &r.attempts, &r.delivered); err != nil {
				t.Fatal(err)
			}
			if _, dup := out[r.bookID]; dup {
				t.Errorf("book %d has more than one backfilled row", r.bookID)
			}
			out[r.bookID] = r
		}
		if err := rs.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}

	got := readRows()
	want := map[int64]struct {
		s         seeded
		calibreID int64
		format    string
	}{
		pushedPath.id:      {pushedPath, 10, "mobi"},
		pushedFirst.id:     {pushedFirst, 11, "azw3"},
		pushedNoExt.id:     {pushedNoExt, 12, ""},
		pushedOtherProv.id: {pushedOtherProv, 13, "epub"},
	}
	for bookID, w := range want {
		r, ok := got[bookID]
		if !ok {
			t.Errorf("pushed book %d got no delivered row", bookID)
			continue
		}
		if r.fileID != w.s.files[w.s.wantFile] || r.path != w.s.wantFile {
			t.Errorf("book %d backfilled file %d %q, want %d %q", bookID, r.fileID, r.path, w.s.files[w.s.wantFile], w.s.wantFile)
		}
		if r.state != "delivered" || r.outcome != "backfilled" || r.attempts != 0 {
			t.Errorf("book %d row = %s/%s/%d, want delivered/backfilled/0", bookID, r.state, r.outcome, r.attempts)
		}
		if r.calibreID != w.calibreID {
			t.Errorf("book %d calibre_id = %d, want %d", bookID, r.calibreID, w.calibreID)
		}
		if r.format != w.format {
			t.Errorf("book %d format = %q, want %q", bookID, r.format, w.format)
		}
		if !r.delivered.Valid || len(r.delivered.String) != len(calibreDeliveryTimeLayout) || len(r.next) != len(calibreDeliveryTimeLayout) {
			t.Errorf("book %d times = %q / %q, want the fixed width ledger shape", bookID, r.next, r.delivered.String)
		}
	}
	for name, s := range map[string]seeded{
		"calibre foreign_id": byForeignID,
		"calibre provider":   byProvider,
		"calibre provenance": byProvenance,
		"audiobook only":     audioOnly,
		"never pushed":       notPushed,
	} {
		if _, ok := got[s.id]; ok {
			t.Errorf("%s book %d must not be backfilled", name, s.id)
		}
	}
	if len(got) != len(want) {
		t.Errorf("backfill wrote %d rows, want %d", len(got), len(want))
	}

	// The repo must read a backfilled row back with its times intact.
	d, err := NewCalibreDeliveryRepo(database).GetByBookFile(ctx, pushedPath.files["/lib/a/A.MOBI"])
	if err != nil || d == nil || d.DeliveredAt == nil || d.NextAttemptAt.IsZero() {
		t.Fatalf("GetByBookFile on a backfilled row = %+v, %v", d, err)
	}

	// Running the migration again adds nothing and changes nothing, even for a
	// row whose state has moved on since.
	mustExec(t, database, `UPDATE calibre_deliveries SET state = 'failed', calibre_id = 99 WHERE book_id = ?`, pushedFirst.id)
	before := readRows()
	mustExec(t, database, `DELETE FROM schema_migrations WHERE version = ?`, v095)
	if err := migrate(database); err != nil {
		t.Fatalf("rerun migration 095: %v", err)
	}
	after := readRows()
	if len(after) != len(before) {
		t.Fatalf("rerun changed the row count from %d to %d", len(before), len(after))
	}
	for id, r := range before {
		if after[id] != r {
			t.Errorf("rerun changed book %d from %+v to %+v", id, r, after[id])
		}
	}
}

func mustExec(t *testing.T, database *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := database.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}
