package abs

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/vavallee/bindery/internal/db"
	"github.com/vavallee/bindery/internal/metadata"
	"github.com/vavallee/bindery/internal/models"
)

// mergeRaceUpstreamID is the upstream id the ISBN lookup relinks the book to.
const mergeRaceUpstreamID = "OL-RACE-1W"

// editingISBNProvider answers the ISBN lookup and, while it handles the
// request, runs edit: a user change committed between the importer's read of
// the book and its merge write (#2926).
type editingISBNProvider struct {
	bindingStubProvider
	edit func()
}

func (p *editingISBNProvider) GetBookByISBN(ctx context.Context, isbn string) (*models.Book, error) {
	if p.edit != nil {
		p.edit()
	}
	return p.bindingStubProvider.GetBookByISBN(ctx, isbn)
}

type mergeRaceFixture struct {
	importer  *Importer
	database  *sql.DB
	books     *db.BookRepo
	conflicts *db.ABSMetadataConflictRepo
	provider  *editingISBNProvider
	item      NormalizedLibraryItem
	book      *models.Book
}

func newMergeRaceFixture(t *testing.T) *mergeRaceFixture {
	t.Helper()
	database, err := db.OpenMemory()
	if err != nil {
		t.Fatalf("open memory db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	books := db.NewBookRepo(database)
	conflicts := db.NewABSMetadataConflictRepo(database)
	importer := NewImporter(
		db.NewAuthorRepo(database),
		db.NewAuthorAliasRepo(database),
		books,
		db.NewEditionRepo(database),
		db.NewSeriesRepo(database),
		db.NewSettingsRepo(database),
		db.NewABSImportRunRepo(database),
		db.NewABSImportRunEntityRepo(database),
		db.NewABSProvenanceRepo(database),
		db.NewABSReviewItemRepo(database),
		conflicts,
	)
	author := asinTestAuthor(t, importer)
	provider := &editingISBNProvider{bindingStubProvider: bindingStubProvider{
		name: "openlibrary",
		booksByISBN: map[string]*models.Book{bookBindingISBN: {
			ForeignID:        mergeRaceUpstreamID,
			Title:            "Project Hail Mary",
			Description:      "Upstream description of the book.",
			MetadataProvider: "openlibrary",
		}},
	}}
	importer.meta = metadata.NewAggregator(provider)

	item := sampleABSItem()
	item.ISBN = bookBindingISBN
	book := &models.Book{
		ForeignID:        "abs:book:" + item.LibraryID + ":" + item.ItemID,
		AuthorID:         author.ID,
		Title:            item.Title,
		SortTitle:        item.Title,
		Status:           models.BookStatusWanted,
		Monitored:        true,
		Genres:           []string{},
		MetadataProvider: providerAudiobookshelf,
	}
	if err := books.Create(context.Background(), book); err != nil {
		t.Fatalf("create book: %v", err)
	}
	// Create records the ABS id itself now; a row from before that has no
	// identifier, which is the case the relink's own record exists for, and
	// it makes that record observable.
	if err := books.DeleteBookIdentifier(context.Background(), book.ID, book.ForeignID); err != nil {
		t.Fatalf("drop identifier: %v", err)
	}
	return &mergeRaceFixture{importer: importer, database: database, books: books, conflicts: conflicts,
		provider: provider, item: item, book: book}
}

// userEdit unmonitors the book and sets a narrator, the kind of change a
// user saves from the book page, through the ordinary full row Update.
func (f *mergeRaceFixture) userEdit(t *testing.T) func() {
	return func() {
		current, err := f.books.GetByID(context.Background(), f.book.ID)
		if err != nil || current == nil {
			t.Errorf("load book for concurrent edit: %v", err)
			return
		}
		current.Monitored = false
		current.Narrator = "User Narrator"
		if err := f.books.Update(context.Background(), current); err != nil {
			t.Errorf("concurrent edit: %v", err)
		}
	}
}

// TestMergeUpstreamBookRetriesOnTopOfConcurrentEdit covers #2926 for the ABS
// metadata merge: an edit committed while the upstream lookup is in flight
// survives, and the upstream record already in hand is merged once more onto
// the edited row, so the relink and the conflict bookkeeping still land.
func TestMergeUpstreamBookRetriesOnTopOfConcurrentEdit(t *testing.T) {
	t.Parallel()
	f := newMergeRaceFixture(t)
	ctx := context.Background()
	absForeignID := f.book.ForeignID
	f.provider.edit = f.userEdit(t)

	result, err := f.importer.enrichBook(ctx, asinTestConfig(), f.item, nil, f.book)
	if err != nil {
		t.Fatalf("enrichBook: %v", err)
	}

	stored, err := f.books.GetByID(ctx, f.book.ID)
	if err != nil || stored == nil {
		t.Fatalf("reload book: %v", err)
	}
	if stored.Monitored || stored.Narrator != "User Narrator" {
		t.Fatalf("concurrent edit overwritten by the metadata merge: monitored=%v narrator=%q", stored.Monitored, stored.Narrator)
	}
	if stored.ForeignID != mergeRaceUpstreamID || stored.Description != "Upstream description of the book." {
		t.Fatalf("merge not applied on the retry: foreignID=%q description=%q", stored.ForeignID, stored.Description)
	}
	if f.book.ForeignID != mergeRaceUpstreamID || f.book.Monitored {
		t.Fatalf("caller row not refreshed: foreignID=%q monitored=%v", f.book.ForeignID, f.book.Monitored)
	}
	if result.Relinked != 1 {
		t.Errorf("Relinked = %d, want 1", result.Relinked)
	}
	if ident, err := f.books.GetBookIdentifier(ctx, absForeignID); err != nil || ident == nil || ident.BookID != f.book.ID {
		t.Errorf("ABS identifier not kept across the relink: %+v err=%v", ident, err)
	}
	if conflict, err := f.conflicts.GetByEntityField(ctx, entityTypeBook, f.book.ID, "description"); err != nil || conflict == nil {
		t.Errorf("description conflict not recorded: %+v err=%v", conflict, err)
	}
}

// TestMergeUpstreamBookSkipsWhenTheBookKeepsChanging is the second loss: the
// merge is dropped for this import, and nothing it would have recorded (the
// relink count, the ABS identifier, the conflict rows) is written for a merge
// that never landed. A trigger stands in for an edit that lands on every
// attempt by silently ignoring the relink write.
func TestMergeUpstreamBookSkipsWhenTheBookKeepsChanging(t *testing.T) {
	t.Parallel()
	f := newMergeRaceFixture(t)
	ctx := context.Background()
	absForeignID := f.book.ForeignID
	if _, err := f.database.ExecContext(ctx, `
		CREATE TRIGGER test_ignore_relink BEFORE UPDATE OF foreign_id ON books
		WHEN NEW.foreign_id = '`+mergeRaceUpstreamID+`'
		BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}
	f.provider.edit = f.userEdit(t)

	result, err := f.importer.enrichBook(ctx, asinTestConfig(), f.item, nil, f.book)
	if err != nil {
		t.Fatalf("enrichBook: %v", err)
	}

	stored, err := f.books.GetByID(ctx, f.book.ID)
	if err != nil || stored == nil {
		t.Fatalf("reload book: %v", err)
	}
	if stored.Monitored || stored.Narrator != "User Narrator" || stored.ForeignID != absForeignID {
		t.Fatalf("stored row = monitored=%v narrator=%q foreignID=%q, want the user's edit and no relink",
			stored.Monitored, stored.Narrator, stored.ForeignID)
	}
	if f.book.Monitored || f.book.ForeignID != absForeignID {
		t.Fatalf("caller row not refreshed: foreignID=%q monitored=%v", f.book.ForeignID, f.book.Monitored)
	}
	if result.Relinked != 0 || result.Conflicts != 0 || result.AutoResolved != 0 {
		t.Errorf("result counts a merge that was not written: %+v", result)
	}
	if msg := strings.Join(result.Messages, "; "); !strings.Contains(msg, "changed") {
		t.Errorf("messages = %q, want a skipped merge reason", msg)
	}
	if ident, err := f.books.GetBookIdentifier(ctx, absForeignID); err != nil || ident != nil {
		t.Errorf("ABS identifier recorded for a relink that did not happen: %+v err=%v", ident, err)
	}
	if conflict, err := f.conflicts.GetByEntityField(ctx, entityTypeBook, f.book.ID, "description"); err != nil || conflict != nil {
		t.Errorf("conflict recorded for a merge that did not happen: %+v err=%v", conflict, err)
	}
}

// TestMergeUpstreamBookPersistsWithoutConcurrentEdit is the control: with
// nothing racing it, the guarded merge lands on the first attempt.
func TestMergeUpstreamBookPersistsWithoutConcurrentEdit(t *testing.T) {
	t.Parallel()
	f := newMergeRaceFixture(t)
	ctx := context.Background()

	result, err := f.importer.enrichBook(ctx, asinTestConfig(), f.item, nil, f.book)
	if err != nil {
		t.Fatalf("enrichBook: %v", err)
	}
	stored, err := f.books.GetByID(ctx, f.book.ID)
	if err != nil || stored == nil {
		t.Fatalf("reload book: %v", err)
	}
	if !stored.Monitored || stored.ForeignID != mergeRaceUpstreamID || stored.LastMetadataRefreshAt == nil {
		t.Fatalf("merge not persisted: monitored=%v foreignID=%q refreshed=%v", stored.Monitored, stored.ForeignID, stored.LastMetadataRefreshAt)
	}
	if result.Relinked != 1 {
		t.Errorf("Relinked = %d, want 1", result.Relinked)
	}
}
