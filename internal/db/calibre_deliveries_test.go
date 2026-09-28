package db

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/vavallee/bindery/internal/models"
)

type deliveryFixture struct {
	db    *sql.DB
	repo  *CalibreDeliveryRepo
	clock time.Time
	books *BookRepo
	book  int64
	files []int64
}

// newDeliveryFixture opens a database with one book and n ebook files, and a
// repo whose clock the test moves by hand.
func newDeliveryFixture(t *testing.T, n int) *deliveryFixture {
	t.Helper()
	database, err := OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	f := &deliveryFixture{
		db:    database,
		books: NewBookRepo(database),
		clock: time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC),
	}
	f.repo = NewCalibreDeliveryRepo(database)
	f.repo.now = func() time.Time { return f.clock }
	author := seedAuthor(t, NewAuthorRepo(database), "OL1A", "Author")
	f.book = seedBook(t, f.books, author.ID, "OL1W", "Book").ID
	for i := 0; i < n; i++ {
		f.files = append(f.files, f.addFile(t, f.book, "/lib/"+string(rune('a'+i))+".epub"))
	}
	return f
}

func (f *deliveryFixture) addFile(t *testing.T, bookID int64, path string) int64 {
	t.Helper()
	res, err := f.db.Exec(`INSERT INTO book_files (book_id, format, path) VALUES (?, 'ebook', ?)`, bookID, path)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func (f *deliveryFixture) enqueue(t *testing.T, i int) *models.CalibreDelivery {
	t.Helper()
	created, err := f.repo.Enqueue(context.Background(), f.book, f.files[i], nil, "/lib//x.epub", ".EPUB")
	if err != nil || !created {
		t.Fatalf("Enqueue file %d = %v, %v; want created", i, created, err)
	}
	d, err := f.repo.GetByBookFile(context.Background(), f.files[i])
	if err != nil || d == nil {
		t.Fatalf("GetByBookFile %d = %v, %v", i, d, err)
	}
	return d
}

func TestCalibreDeliveryEnqueue(t *testing.T) {
	f := newDeliveryFixture(t, 1)
	ctx := context.Background()
	edition := int64(42)

	created, err := f.repo.Enqueue(ctx, f.book, f.files[0], &edition, "/lib//a.epub", ".EPUB")
	if err != nil || !created {
		t.Fatalf("Enqueue = %v, %v; want true, nil", created, err)
	}
	d, err := f.repo.GetByBookFile(ctx, f.files[0])
	if err != nil || d == nil {
		t.Fatalf("GetByBookFile = %v, %v", d, err)
	}
	if d.State != models.CalibreDeliveryPending || d.Attempts != 0 || d.BookID != f.book ||
		d.FilePath != "/lib/a.epub" || d.Format != "epub" || d.EditionID == nil || *d.EditionID != 42 ||
		d.CalibreID != nil || d.DeliveredAt != nil || !d.NextAttemptAt.Equal(f.clock) || !d.CreatedAt.Equal(f.clock) {
		t.Fatalf("enqueued row = %+v", d)
	}
	if byID, err := f.repo.Get(ctx, d.ID); err != nil || byID == nil || byID.BookFileID != f.files[0] {
		t.Fatalf("Get = %+v, %v", byID, err)
	}
	if missing, err := f.repo.Get(ctx, 9999); err != nil || missing != nil {
		t.Fatalf("Get on a missing id = %+v, %v; want nil, nil", missing, err)
	}
}

// TestCalibreDeliveryEnqueueLeavesExistingRows pins the conflict rule: a
// second Enqueue for the same file changes nothing, in any state. Only Retry
// re-arms a failed or skipped row.
func TestCalibreDeliveryEnqueueLeavesExistingRows(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		move func(f *deliveryFixture, id int64) error
	}{
		{"pending", func(*deliveryFixture, int64) error { return nil }},
		{"delivered", func(f *deliveryFixture, id int64) error {
			return f.repo.MarkDelivered(ctx, id, 7, "added", "/target")
		}},
		{"failed", func(f *deliveryFixture, id int64) error {
			return f.repo.MarkFailed(ctx, id, "rejected", "bad file", f.clock.Add(time.Hour), true)
		}},
		{"skipped", func(f *deliveryFixture, id int64) error { return f.repo.MarkSkipped(ctx, id, "unsupported") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDeliveryFixture(t, 1)
			d := f.enqueue(t, 0)
			if err := tc.move(f, d.ID); err != nil {
				t.Fatal(err)
			}
			before, _ := f.repo.Get(ctx, d.ID)
			f.clock = f.clock.Add(24 * time.Hour)
			created, err := f.repo.Enqueue(ctx, f.book, f.files[0], nil, "/elsewhere/b.mobi", "mobi")
			if err != nil || created {
				t.Fatalf("second Enqueue = %v, %v; want false, nil", created, err)
			}
			after, _ := f.repo.Get(ctx, d.ID)
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("second Enqueue changed the row:\n before %+v\n after  %+v", before, after)
			}
		})
	}
}

func TestCalibreDeliveryDueBatch(t *testing.T) {
	f := newDeliveryFixture(t, 4)
	ctx := context.Background()
	a := f.enqueue(t, 0)
	f.clock = f.clock.Add(time.Second)
	b := f.enqueue(t, 1)
	// Sub-second precision with trailing zeros is where RFC3339Nano text
	// stops sorting: .1 would sort after .123 as text.
	f.clock = f.clock.Add(100 * time.Millisecond)
	c := f.enqueue(t, 2)
	d := f.enqueue(t, 3)
	if err := f.repo.MarkDelivered(ctx, d.ID, 1, "added", ""); err != nil {
		t.Fatal(err)
	}
	// a fails transiently and comes due 23ms after c: as RFC3339Nano text
	// "…01.123Z" would sort before "…01.1Z" and already be due.
	if err := f.repo.MarkFailed(ctx, a.ID, "timeout", "slow", f.clock.Add(23*time.Millisecond), false); err != nil {
		t.Fatal(err)
	}

	ids := func(ds []models.CalibreDelivery) []int64 {
		out := make([]int64, len(ds))
		for i, d := range ds {
			out[i] = d.ID
		}
		return out
	}
	due, err := f.repo.DueBatch(ctx, f.clock, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(due); len(got) != 2 || got[0] != b.ID || got[1] != c.ID {
		t.Fatalf("DueBatch at enqueue time = %v, want [%d %d]", got, b.ID, c.ID)
	}
	due, err = f.repo.DueBatch(ctx, f.clock.Add(time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(due); len(got) != 3 || got[0] != b.ID || got[1] != c.ID || got[2] != a.ID {
		t.Fatalf("DueBatch later = %v, want [%d %d %d]", got, b.ID, c.ID, a.ID)
	}
	due, err = f.repo.DueBatch(ctx, f.clock.Add(time.Hour), 1)
	if err != nil || len(due) != 1 || due[0].ID != b.ID {
		t.Fatalf("DueBatch limit 1 = %v, %v", ids(due), err)
	}
}

func TestCalibreDeliveryMarkMethods(t *testing.T) {
	f := newDeliveryFixture(t, 3)
	ctx := context.Background()
	a, b, c := f.enqueue(t, 0), f.enqueue(t, 1), f.enqueue(t, 2)

	next := f.clock.Add(5 * time.Minute)
	if err := f.repo.MarkFailed(ctx, a.ID, "timeout", "first", next, false); err != nil {
		t.Fatal(err)
	}
	got, _ := f.repo.Get(ctx, a.ID)
	if got.State != models.CalibreDeliveryPending || got.Attempts != 1 || got.LastErrorCode != "timeout" ||
		got.LastError != "first" || !got.NextAttemptAt.Equal(next) {
		t.Fatalf("after a transient failure = %+v", got)
	}
	if err := f.repo.MarkFailed(ctx, a.ID, "rejected", "second", next, true); err != nil {
		t.Fatal(err)
	}
	got, _ = f.repo.Get(ctx, a.ID)
	if got.State != models.CalibreDeliveryFailed || got.Attempts != 2 || got.LastErrorCode != "rejected" {
		t.Fatalf("after a terminal failure = %+v", got)
	}

	// A delivery after a transient failure clears the error.
	if err := f.repo.MarkFailed(ctx, b.ID, "timeout", "slow", next, false); err != nil {
		t.Fatal(err)
	}
	f.clock = f.clock.Add(time.Minute)
	if err := f.repo.MarkDelivered(ctx, b.ID, 77, "added", "/target/lib"); err != nil {
		t.Fatal(err)
	}
	got, _ = f.repo.Get(ctx, b.ID)
	if got.State != models.CalibreDeliveryDelivered || got.CalibreID == nil || *got.CalibreID != 77 ||
		got.Outcome != "added" || got.TargetLibrary != "/target/lib" || got.LastError != "" ||
		got.LastErrorCode != "" || got.DeliveredAt == nil || !got.DeliveredAt.Equal(f.clock) || got.Attempts != 1 {
		t.Fatalf("after delivery = %+v", got)
	}

	if err := f.repo.MarkSkipped(ctx, c.ID, "audiobook"); err != nil {
		t.Fatal(err)
	}
	got, _ = f.repo.Get(ctx, c.ID)
	if got.State != models.CalibreDeliverySkipped || got.Outcome != "audiobook" {
		t.Fatalf("after skip = %+v", got)
	}

	for name, err := range map[string]error{
		"MarkDelivered": f.repo.MarkDelivered(ctx, 9999, 1, "added", ""),
		"MarkFailed":    f.repo.MarkFailed(ctx, 9999, "x", "y", next, false),
		"MarkSkipped":   f.repo.MarkSkipped(ctx, 9999, "z"),
	} {
		if !errors.Is(err, ErrCalibreDeliveryNotFound) {
			t.Errorf("%s on a missing row = %v, want ErrCalibreDeliveryNotFound", name, err)
		}
	}
}

func TestCalibreDeliveryRetry(t *testing.T) {
	f := newDeliveryFixture(t, 4)
	ctx := context.Background()
	failed, skipped, delivered, pending := f.enqueue(t, 0), f.enqueue(t, 1), f.enqueue(t, 2), f.enqueue(t, 3)
	later := f.clock.Add(time.Hour)
	if err := f.repo.MarkFailed(ctx, failed.ID, "rejected", "bad", later, true); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.MarkSkipped(ctx, skipped.ID, "why"); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.MarkDelivered(ctx, delivered.ID, 5, "added", ""); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.MarkFailed(ctx, pending.ID, "timeout", "slow", later, false); err != nil {
		t.Fatal(err)
	}

	if _, err := f.repo.Retry(ctx, models.CalibreDeliveryDelivered); err == nil {
		t.Fatal("Retry of delivered rows must be refused")
	}

	f.clock = f.clock.Add(time.Minute)
	n, err := f.repo.Retry(ctx, models.CalibreDeliveryFailed)
	if err != nil || n != 1 {
		t.Fatalf("Retry(failed) = %d, %v; want 1", n, err)
	}
	got, _ := f.repo.Get(ctx, failed.ID)
	if got.State != models.CalibreDeliveryPending || got.Attempts != 0 || got.LastError != "" ||
		got.LastErrorCode != "" || !got.NextAttemptAt.Equal(f.clock) {
		t.Fatalf("retried row = %+v", got)
	}
	if got, _ := f.repo.Get(ctx, skipped.ID); got.State != models.CalibreDeliverySkipped {
		t.Fatalf("Retry(failed) touched a skipped row: %+v", got)
	}
	// A pending row with a transient failure keeps its attempts and backoff.
	if got, _ := f.repo.Get(ctx, pending.ID); got.Attempts != 1 || !got.NextAttemptAt.Equal(later) {
		t.Fatalf("Retry touched a pending row: %+v", got)
	}

	if err := f.repo.MarkFailed(ctx, failed.ID, "rejected", "again", later, true); err != nil {
		t.Fatal(err)
	}
	n, err = f.repo.Retry(ctx, "")
	if err != nil || n != 2 {
		t.Fatalf(`Retry("") = %d, %v; want 2 (the failed and the skipped row)`, n, err)
	}
	if got, _ := f.repo.Get(ctx, delivered.ID); got.State != models.CalibreDeliveryDelivered {
		t.Fatalf("Retry touched a delivered row: %+v", got)
	}
}

func TestCalibreDeliverySummaryListClearReset(t *testing.T) {
	f := newDeliveryFixture(t, 5)
	ctx := context.Background()

	s, err := f.repo.Summary(ctx)
	if err != nil || s != (models.CalibreDeliverySummary{}) {
		t.Fatalf("Summary on an empty ledger = %+v, %v", s, err)
	}

	var ds []*models.CalibreDelivery
	for i := range f.files {
		ds = append(ds, f.enqueue(t, i))
	}
	f.clock = f.clock.Add(time.Minute)
	if err := f.repo.MarkDelivered(ctx, ds[0].ID, 1, "added", ""); err != nil {
		t.Fatal(err)
	}
	f.clock = f.clock.Add(time.Minute)
	lastDelivered := f.clock
	if err := f.repo.MarkDelivered(ctx, ds[1].ID, 2, "linked", ""); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.MarkFailed(ctx, ds[2].ID, "rejected", "bad", f.clock, true); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.MarkSkipped(ctx, ds[3].ID, "why"); err != nil {
		t.Fatal(err)
	}

	s, err = f.repo.Summary(ctx)
	if err != nil || s.Pending != 1 || s.Delivered != 2 || s.Failed != 1 || s.Skipped != 1 ||
		s.LastDeliveredAt == nil || !s.LastDeliveredAt.Equal(lastDelivered) {
		t.Fatalf("Summary = %+v, %v", s, err)
	}

	all, err := f.repo.List(ctx, "", 10, 0)
	if err != nil || len(all) != 5 {
		t.Fatalf("List(all) = %d rows, %v", len(all), err)
	}
	// Most recently updated first; ties broken by the newer id.
	if all[0].UpdatedAt.Before(all[len(all)-1].UpdatedAt) {
		t.Fatalf("List is not newest first: %v then %v", all[0].UpdatedAt, all[len(all)-1].UpdatedAt)
	}
	page, err := f.repo.List(ctx, "", 2, 1)
	if err != nil || len(page) != 2 || page[0].ID != all[1].ID {
		t.Fatalf("List page = %+v, %v", page, err)
	}
	delivered, err := f.repo.List(ctx, models.CalibreDeliveryDelivered, 10, 0)
	if err != nil || len(delivered) != 2 {
		t.Fatalf("List(delivered) = %d rows, %v", len(delivered), err)
	}
	for _, d := range delivered {
		if d.State != models.CalibreDeliveryDelivered {
			t.Fatalf("List(delivered) returned %+v", d)
		}
	}

	n, err := f.repo.ClearPending(ctx)
	if err != nil || n != 1 {
		t.Fatalf("ClearPending = %d, %v; want 1", n, err)
	}
	if got, _ := f.repo.Get(ctx, ds[4].ID); got != nil {
		t.Fatalf("ClearPending left the pending row: %+v", got)
	}
	s, _ = f.repo.Summary(ctx)
	if s.Pending != 0 || s.Delivered != 2 || s.Failed != 1 || s.Skipped != 1 {
		t.Fatalf("ClearPending touched a settled row: %+v", s)
	}

	n, err = f.repo.ResetAll(ctx)
	if err != nil || n != 4 {
		t.Fatalf("ResetAll = %d, %v; want 4", n, err)
	}
	if s, _ := f.repo.Summary(ctx); s != (models.CalibreDeliverySummary{}) {
		t.Fatalf("ResetAll left %+v", s)
	}
}

func TestCalibreDeliveryDeliveredByCalibreID(t *testing.T) {
	f := newDeliveryFixture(t, 3)
	ctx := context.Background()
	a, b, c := f.enqueue(t, 0), f.enqueue(t, 1), f.enqueue(t, 2)
	if err := f.repo.MarkDelivered(ctx, a.ID, 50, "added", "/t"); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.MarkDelivered(ctx, b.ID, 51, "added", "/t"); err != nil {
		t.Fatal(err)
	}
	// c carries calibre id 50 but is not delivered, so it does not prove
	// ownership.
	if _, err := f.db.Exec(`UPDATE calibre_deliveries SET calibre_id = 50 WHERE id = ?`, c.ID); err != nil {
		t.Fatal(err)
	}
	got, err := f.repo.DeliveredByCalibreID(ctx, 50)
	if err != nil || len(got) != 1 || got[0].ID != a.ID || got[0].TargetLibrary != "/t" {
		t.Fatalf("DeliveredByCalibreID(50) = %+v, %v", got, err)
	}
	if got, err := f.repo.DeliveredByCalibreID(ctx, 999); err != nil || len(got) != 0 {
		t.Fatalf("DeliveredByCalibreID(999) = %+v, %v", got, err)
	}
}

// TestCalibreDeliveryCascades checks the ledger follows the rows it points
// at: deleting a book_files row or the whole book removes its deliveries.
func TestCalibreDeliveryCascades(t *testing.T) {
	f := newDeliveryFixture(t, 2)
	ctx := context.Background()
	a, b := f.enqueue(t, 0), f.enqueue(t, 1)

	if _, err := f.db.Exec(`DELETE FROM book_files WHERE id = ?`, f.files[0]); err != nil {
		t.Fatal(err)
	}
	if got, err := f.repo.Get(ctx, a.ID); err != nil || got != nil {
		t.Fatalf("delivery for a deleted book file survived: %+v, %v", got, err)
	}
	if got, _ := f.repo.Get(ctx, b.ID); got == nil {
		t.Fatal("deleting one book file removed another file's delivery")
	}
	if _, err := f.db.Exec(`DELETE FROM books WHERE id = ?`, f.book); err != nil {
		t.Fatal(err)
	}
	if got, err := f.repo.Get(ctx, b.ID); err != nil || got != nil {
		t.Fatalf("delivery for a deleted book survived: %+v, %v", got, err)
	}

	// And the foreign keys hold on insert: a file that does not exist cannot
	// be queued.
	if _, err := f.repo.Enqueue(ctx, f.book, 9999, nil, "/x.epub", "epub"); err == nil {
		t.Fatal("Enqueue for a missing book file must fail the foreign key")
	}
}

// TestCalibreDeliveryListAllByBookAndWithBooks covers the reads Push all and
// the settings queue view use.
func TestCalibreDeliveryListAllByBookAndWithBooks(t *testing.T) {
	ctx := context.Background()
	f := newDeliveryFixture(t, 2)
	other := seedBook(t, f.books, seedAuthor(t, NewAuthorRepo(f.db), "OL2A", "Second Author").ID, "OL2W", "Other").ID
	otherFile := f.addFile(t, other, "/lib/other.epub")

	first := f.enqueue(t, 0)
	f.clock = f.clock.Add(time.Minute)
	f.enqueue(t, 1)
	if _, err := f.repo.Enqueue(ctx, other, otherFile, nil, "/lib/other.epub", "epub"); err != nil {
		t.Fatal(err)
	}
	f.clock = f.clock.Add(time.Minute)
	if err := f.repo.MarkFailed(ctx, first.ID, "bad_format", "nope", f.clock, true); err != nil {
		t.Fatal(err)
	}

	all, err := f.repo.ListAll(ctx)
	if err != nil || len(all) != 3 || all[0].ID != first.ID {
		t.Fatalf("ListAll = %d rows, %v; want 3 oldest first", len(all), err)
	}
	mine, err := f.repo.ListByBook(ctx, f.book)
	if err != nil || len(mine) != 2 {
		t.Fatalf("ListByBook = %d rows, %v; want 2", len(mine), err)
	}

	items, err := f.repo.ListWithBooks(ctx, models.CalibreDeliveryFailed, 10, 0)
	if err != nil || len(items) != 1 {
		t.Fatalf("ListWithBooks failed = %d, %v; want 1", len(items), err)
	}
	if items[0].ID != first.ID || items[0].BookTitle != "Book" || items[0].AuthorName != "Author" || items[0].LastErrorCode != "bad_format" {
		t.Errorf("item = %+v", items[0])
	}
	items, err = f.repo.ListWithBooks(ctx, "", 10, 0)
	if err != nil || len(items) != 3 || items[0].ID != first.ID {
		t.Fatalf("ListWithBooks all = %d, %v; want 3 most recently updated first", len(items), err)
	}
	names := map[string]bool{}
	for _, it := range items {
		names[it.AuthorName] = true
	}
	if !names["Second Author"] {
		t.Errorf("author names = %v", names)
	}
	if page, err := f.repo.ListWithBooks(ctx, "", 1, 1); err != nil || len(page) != 1 || page[0].ID == first.ID {
		t.Errorf("second page = %+v, %v", page, err)
	}
	if empty, err := f.repo.ListWithBooks(ctx, models.CalibreDeliveryDelivered, 10, 0); err != nil || empty == nil || len(empty) != 0 {
		t.Errorf("empty list = %#v, %v; want a non-nil empty slice", empty, err)
	}
}

// RearmSkipped re-queues only skipped rows with exactly the given reason.
func TestCalibreDeliveryRearmSkipped(t *testing.T) {
	f := newDeliveryFixture(t, 4)
	ctx := context.Background()
	const reason = "bridge cannot add a second format; update the Calibre plugin to 0.7.0"
	held := f.enqueue(t, 0)
	other := f.enqueue(t, 1)
	failed := f.enqueue(t, 2)
	pending := f.enqueue(t, 3)
	if err := f.repo.MarkSkipped(ctx, held.ID, reason); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.MarkSkipped(ctx, other.ID, "file missing on disk"); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.MarkFailed(ctx, failed.ID, "bad_format", "nope", f.clock, true); err != nil {
		t.Fatal(err)
	}

	has, err := f.repo.HasSkipped(ctx, reason)
	if err != nil || !has {
		t.Fatalf("HasSkipped = %v, %v; want true", has, err)
	}
	if has, _ := f.repo.HasSkipped(ctx, "no such reason"); has {
		t.Error("HasSkipped matched a reason no row has")
	}

	f.clock = f.clock.Add(time.Hour)
	n, err := f.repo.RearmSkipped(ctx, reason)
	if err != nil || n != 1 {
		t.Fatalf("RearmSkipped = %d, %v; want 1", n, err)
	}
	got, _ := f.repo.Get(ctx, held.ID)
	if got.State != models.CalibreDeliveryPending || got.Outcome != "" || got.Attempts != 0 || !got.NextAttemptAt.Equal(f.clock) {
		t.Errorf("re-armed row = %+v, want pending, due now, no outcome", got)
	}
	for id, want := range map[int64]models.CalibreDeliveryState{
		other.ID: models.CalibreDeliverySkipped, failed.ID: models.CalibreDeliveryFailed, pending.ID: models.CalibreDeliveryPending,
	} {
		if got, _ := f.repo.Get(ctx, id); got.State != want {
			t.Errorf("row %d = %s, want %s untouched", id, got.State, want)
		}
	}
	if has, _ := f.repo.HasSkipped(ctx, reason); has {
		t.Error("HasSkipped still true after the rearm")
	}
	if n, err := f.repo.RearmSkipped(ctx, reason); err != nil || n != 0 {
		t.Errorf("second RearmSkipped = %d, %v; want 0", n, err)
	}
}
