package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vavallee/bindery/internal/models"
)

// A book the sweep will never grab, because its author is unmonitored, sits on
// the Wanted page looking exactly like one whose grab is merely slow (#2742).
// The list says so per row, so the page explains itself instead of the user
// having to read the server log to find out why nothing is happening.
func TestListWanted_FlagsBooksOfUnmonitoredAuthors(t *testing.T) {
	h, books, authors, monitoredAuthor, ctx := bookFixture(t)
	h = h.WithAuthors(authors)

	quiet := &models.Author{
		ForeignID: "OL2A", Name: "P. G. Wodehouse", SortName: "Wodehouse, P. G.",
		MetadataProvider: "openlibrary", Monitored: false,
	}
	if err := authors.Create(ctx, quiet); err != nil {
		t.Fatal(err)
	}
	for _, b := range []*models.Book{
		{ForeignID: "WM1", AuthorID: monitoredAuthor.ID, Title: "Watched", SortTitle: "watched", Status: models.BookStatusWanted, Genres: []string{}, MetadataProvider: "openlibrary", Monitored: true},
		{ForeignID: "WM2", AuthorID: quiet.ID, Title: "Old Reliable", SortTitle: "old reliable", Status: models.BookStatusWanted, Genres: []string{}, MetadataProvider: "openlibrary", Monitored: true},
	} {
		if err := books.Create(ctx, b); err != nil {
			t.Fatal(err)
		}
	}

	rec := httptest.NewRecorder()
	h.ListWanted(rec, httptest.NewRequest(http.MethodGet, "/api/v1/wanted/missing", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got []models.Book
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 wanted books, got %d", len(got))
	}
	want := map[string]bool{"Watched": false, "Old Reliable": true}
	for _, b := range got {
		if b.AuthorUnmonitored != want[b.Title] {
			t.Errorf("%s authorUnmonitored = %v, want %v", b.Title, b.AuthorUnmonitored, want[b.Title])
		}
	}
}

// The flag is computed, not stored, so a handler with no authors repo must
// simply not claim anything rather than reporting every row as unmonitored.
func TestListWanted_WithoutAuthorsRepoClaimsNothing(t *testing.T) {
	h, books, _, author, ctx := bookFixture(t)

	if err := books.Create(ctx, &models.Book{
		ForeignID: "WM3", AuthorID: author.ID, Title: "Lone", SortTitle: "lone",
		Status: models.BookStatusWanted, Genres: []string{}, MetadataProvider: "openlibrary", Monitored: true,
	}); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	h.ListWanted(rec, httptest.NewRequest(http.MethodGet, "/api/v1/wanted/missing", nil))
	var got []models.Book
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].AuthorUnmonitored {
		t.Errorf("authorUnmonitored = %v with no authors repo, want false", got[0].AuthorUnmonitored)
	}
}
