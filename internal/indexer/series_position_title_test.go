package indexer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vavallee/bindery/internal/indexer/newznab"
	"github.com/vavallee/bindery/internal/models"
)

// A book stored as "Series N: Title" was only ever matched on the whole name or
// on the series half, so a release naming the book itself was dropped. This
// release is the one from the Discord report.
func TestFilterRelevant_SeriesPositionTitle(t *testing.T) {
	const title, author = "Feral Mage 4: The Mining Company Contract", "Chase Kilgore"
	cases := []struct {
		release string
		want    bool
	}{
		{"The Mining Company Contract by Chase Kilgore [ENG / EPUB] [VIP]", true},
		{"Chase Kilgore - The Mining Company Contract (epub)", true},
		// Already matched before this change, and still does.
		{"Chase Kilgore - Feral Mage 4 - The Mining Company Contract (epub)", true},
		// The author is still required.
		{"The Mining Company Contract by Someone Else [ENG / EPUB]", false},
		{"The Mining Company Contract [ENG / EPUB]", true},
		// A different book by the same author.
		{"The Silver Company Contract by Chase Kilgore [ENG / EPUB]", false},
		// Words present but split by words the title does not have.
		{"The Mining Company Breach of Contract by Chase Kilgore", false},
		{"Wulfe Untamed: Feral Warriors, Book 8 by Pamela Palmer [ENG / MP3]", false},
	}
	for _, f := range relevanceFilters {
		for _, c := range cases {
			t.Run(f.name+"/"+c.release, func(t *testing.T) {
				got := f.fn([]newznab.SearchResult{{Title: c.release}}, title, author, nil)
				if (len(got) == 1) != c.want {
					t.Fatalf("%s kept=%v, want %v for %q", f.name, len(got) == 1, c.want, c.release)
				}
			})
		}
	}
}

// A one word book title after a series position is below the floor, so the
// fallback reading does not open up every release by the author that contains
// that word.
func TestFilterRelevant_SeriesPositionTitleNeedsTwoWords(t *testing.T) {
	got := filterRelevant([]newznab.SearchResult{{Title: "Wintersteel Anthology by Will Wight"}},
		"Cradle Book 8: Wintersteel", "Will Wight", nil)
	for _, r := range got {
		t.Errorf("kept %q through a one word series position title", r.Title)
	}
}

// Drives the production entry points so the reading is proven on the paths
// that search and grab, not only on the filter function.
func TestSeriesPositionTitle_ReachesProductionEntrypoints(t *testing.T) {
	const right = "The Mining Company Contract by Chase Kilgore [ENG / EPUB] [VIP]"
	const wrong = "Wulfe Untamed: Feral Warriors, Book 8 by Pamela Palmer [ENG / MP3]"
	feed := feedOf(right, wrong)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(feed))
	}))
	defer srv.Close()

	idxs := []models.Indexer{{ID: 1, Name: "test", URL: srv.URL, Enabled: true, Categories: []int{7020}}}
	crit := MatchCriteria{Title: "Feral Mage 4: The Mining Company Contract", Author: "Chase Kilgore"}
	ctx := context.Background()
	debugged, _ := newTestSearcher().SearchBookWithDebug(ctx, idxs, crit)
	outcomes, _ := newTestSearcher().SearchBookWithOutcomes(ctx, idxs, crit)
	for name, got := range map[string][]string{
		"SearchBookWithDebug":    resultTitles(debugged),
		"SearchBookWithOutcomes": resultTitles(outcomes),
	} {
		if len(got) != 1 || got[0] != right {
			t.Errorf("%s kept %v, want only %q", name, got, right)
		}
	}
}
