package api

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/vavallee/bindery/internal/models"
)

// The Authors page bulk Unmonitor wrote only the author flag and stopped
// there, so the books under it stayed monitored. That is the state #2742 was
// reported from. The scheduler no longer grabs for those books either way, but
// the library still reads as "200 authors off, several thousand books on",
// which is the state the reporter was trying to get out of. Bulk unmonitor now
// takes the same apply-to-existing flag the single author path has, defaulting
// to off so the action keeps its old shape for callers that predate it.

func TestAuthorsBulk_Unmonitor_AppliesToExistingBooks(t *testing.T) {
	h, authors, books, author, ctx := bulkFixture(t)

	book := mustCreateBook(t, books, ctx, &models.Book{
		ForeignID: "OL_UNMON_CASCADE", AuthorID: author.ID, Title: "Old Reliable",
		SortTitle: "old reliable", Status: models.BookStatusWanted,
		Genres: []string{}, MetadataProvider: "openlibrary", Monitored: true,
	})

	body := fmt.Sprintf(`{"ids":[%d],"action":"unmonitor","applyMonitorModeToExisting":true}`, author.ID)
	rec := postBulk(t, h.AuthorsBulk, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	gotAuthor, _ := authors.GetByID(ctx, author.ID)
	if gotAuthor.Monitored {
		t.Fatal("author should be unmonitored")
	}
	gotBook, _ := books.GetByID(ctx, book.ID)
	if gotBook.Monitored {
		t.Error("book stayed monitored after a bulk unmonitor that asked to apply to existing books")
	}
}

func TestAuthorsBulk_Unmonitor_LeavesBooksAloneByDefault(t *testing.T) {
	h, _, books, author, ctx := bulkFixture(t)

	book := mustCreateBook(t, books, ctx, &models.Book{
		ForeignID: "OL_UNMON_DEFAULT", AuthorID: author.ID, Title: "Summer Lightning",
		SortTitle: "summer lightning", Status: models.BookStatusWanted,
		Genres: []string{}, MetadataProvider: "openlibrary", Monitored: true,
	})

	body := fmt.Sprintf(`{"ids":[%d],"action":"unmonitor"}`, author.ID)
	rec := postBulk(t, h.AuthorsBulk, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	gotBook, _ := books.GetByID(ctx, book.ID)
	if !gotBook.Monitored {
		t.Error("bulk unmonitor cascaded without being asked to; the flag defaults to off")
	}
}

// TestAuthorsBulk_Monitor_AppliesToExistingBooks: the same flag on the way
// back in, so a user who unmonitored a batch by mistake can put it back in one
// action instead of visiting each author.
func TestAuthorsBulk_Monitor_AppliesToExistingBooks(t *testing.T) {
	h, _, books, author, ctx := bulkFixture(t)

	book := mustCreateBook(t, books, ctx, &models.Book{
		ForeignID: "OL_MON_CASCADE", AuthorID: author.ID, Title: "Heavy Weather",
		SortTitle: "heavy weather", Status: models.BookStatusWanted,
		Genres: []string{}, MetadataProvider: "openlibrary", Monitored: false,
	})

	body := fmt.Sprintf(`{"ids":[%d],"action":"monitor","applyMonitorModeToExisting":true}`, author.ID)
	rec := postBulk(t, h.AuthorsBulk, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	gotBook, _ := books.GetByID(ctx, book.ID)
	if !gotBook.Monitored {
		t.Error("book stayed unmonitored after a bulk monitor that asked to apply to existing books")
	}
}

// TestApplyMonitorModeToExisting_UnmonitoredAuthorInSeriesMode covers the one
// branch of applyMonitorModeToExistingBooks that did not ask whether the author
// is monitored at all: series mode recomputed `next` purely from series
// membership, so cascading an unmonitored author in that mode monitored their
// books. shouldMonitorBookForAuthor has always said an unmonitored author
// monitors nothing; series mode is now held to the same rule.
func TestApplyMonitorModeToExisting_UnmonitoredAuthorInSeriesMode(t *testing.T) {
	h, authors, books, author, ctx := bulkFixture(t)

	author.MonitorMode = models.AuthorMonitorModeSeries
	if err := authors.Update(ctx, author); err != nil {
		t.Fatal(err)
	}
	book := mustCreateBook(t, books, ctx, &models.Book{
		ForeignID: "OL_SERIES_UNMON", AuthorID: author.ID, Title: "Blandings",
		SortTitle: "blandings", Status: models.BookStatusWanted,
		Genres: []string{}, MetadataProvider: "openlibrary", Monitored: true,
	})

	body := fmt.Sprintf(`{"ids":[%d],"action":"unmonitor","applyMonitorModeToExisting":true}`, author.ID)
	rec := postBulk(t, h.AuthorsBulk, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	gotBook, _ := books.GetByID(ctx, book.ID)
	if gotBook.Monitored {
		t.Error("series mode cascaded an unmonitored author's book back to monitored")
	}
}
