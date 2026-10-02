package calibre

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// mutateFixture runs statements against a fixture library's metadata.db after
// buildFixtureLibrary has closed it.
func mutateFixture(t *testing.T, root string, stmts ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(root, metadataDB))
	if err != nil {
		t.Fatalf("reopen fixture db: %v", err)
	}
	defer db.Close()
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("exec %q: %v", s, err)
		}
	}
}

// TestBooks_NonNumericSeriesIndex covers a library whose series_index holds
// text rather than a number. Calibre declares the column REAL NOT NULL, but
// SQLite's storage class is per value, so the empty string survives there and
// used to abort the entire import with "converting driver.Value type string
// (\"\") to a float64" before a single book was read (#2720).
func TestBooks_NonNumericSeriesIndex(t *testing.T) {
	root := buildFixtureLibrary(t)
	mutateFixture(t, root,
		`UPDATE books SET series_index = '' WHERE id = 1`,
		`UPDATE books SET series_index = 'two' WHERE id = 2`,
	)

	r, err := OpenReader(root)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer r.Close()

	got := map[int64]CalibreBook{}
	if err := r.Books(context.Background(), func(cb CalibreBook) error {
		got[cb.CalibreID] = cb
		return nil
	}); err != nil {
		t.Fatalf("Books: %v", err)
	}

	if len(got) != 3 {
		t.Fatalf("read %d books, want all 3", len(got))
	}
	// Both books keep their series; only the unreadable position is dropped,
	// and position 0 is what attachBookToSeries already treats as "no
	// position" for a book with no series_index.
	for _, id := range []int64{1, 2} {
		cb := got[id]
		if cb.Series == nil {
			t.Fatalf("book %d lost its series", id)
		}
		if cb.Series.Name != "Example Saga" {
			t.Errorf("book %d series = %q, want %q", id, cb.Series.Name, "Example Saga")
		}
		if cb.Series.Position != 0 {
			t.Errorf("book %d position = %v, want 0", id, cb.Series.Position)
		}
	}
}

// TestBooks_NullTitleSortAndPath covers the sibling columns on the same SELECT.
// Calibre's `sort` column is nullable, and a library written by a third-party
// tool can leave title or path NULL; each would have failed the same Scan.
func TestBooks_NullTitleSortAndPath(t *testing.T) {
	root := buildFixtureLibrary(t)
	mutateFixture(t, root, `UPDATE books SET sort = NULL WHERE id = 1`)

	r, err := OpenReader(root)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer r.Close()

	var first CalibreBook
	if err := r.Books(context.Background(), func(cb CalibreBook) error {
		if cb.CalibreID == 1 {
			first = cb
		}
		return nil
	}); err != nil {
		t.Fatalf("Books: %v", err)
	}
	if first.CalibreID != 1 {
		t.Fatal("book 1 was not read")
	}
	if first.SortTitle != "" {
		t.Errorf("SortTitle = %q, want empty", first.SortTitle)
	}
	if first.Title != "Book One" {
		t.Errorf("Title = %q, want %q", first.Title, "Book One")
	}
}

func TestParseSeriesIndex(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want float64
	}{
		{"1", 1},
		{"1.5", 1.5},
		{" 2.0 ", 2},
		{"", 0},
		{"two", 0},
		{"1,5", 0},
	} {
		if got := parseSeriesIndex(tc.raw); got != tc.want {
			t.Errorf("parseSeriesIndex(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}
