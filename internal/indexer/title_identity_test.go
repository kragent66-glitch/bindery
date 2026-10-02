package indexer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/vavallee/bindery/internal/indexer/newznab"
	"github.com/vavallee/bindery/internal/models"
)

// These releases were auto-grabbed and imported as three different books.
func TestFilterRelevantConflictingBookIdentity(t *testing.T) {
	cases := []struct{ title, author, release string }{
		{"Power down", "Ben Coes", "The Power of Writing It Down by Allison Fallon EPUB"},
		{"12 Rules for Life", "Jordan B. Peterson", "Beyond Order: 12 More Rules for Life by Jordan B. Peterson EPUB"},
		{"Coup d'Etat", "Ben Coes", "Coup D'Etat - Edward Luttwak"},
	}
	for _, c := range cases {
		t.Run(c.title, func(t *testing.T) {
			got := filterRelevant([]newznab.SearchResult{{Title: c.release}}, c.title, c.author, nil)
			if len(got) != 0 {
				t.Fatalf("wrong release accepted: %q for %q by %s", c.release, c.title, c.author)
			}
		})
	}
}

func TestFilterRelevantIdentityCompatibility(t *testing.T) {
	cases := []struct {
		title, author, release string
		want                   bool
	}{
		{"Coup d'Etat", "Ben Coes", "Coup D'Etat - Ben Coes", true},
		{"Coup d'Etat", "Ben Coes", "Coup D'Etat by Ben Coes EPUB", true},
		{"Coup d'Etat", "Ben Coes", "Coup D'Etat by Coes EPUB", true},
		{"Coup d'Etat", "Ben Coes", "Coup D'Etat EPUB", true},
		{"Coup d'Etat", "Ben Coes", "Coup D'Etat - Unabridged MP3", true},
		{"Coup d'Etat", "Ben Coes", "Coup D'Etat - narrated by John Doe", true},
		{"Coup d'Etat", "Ben Coes", "Coup D'Etat — Edward Luttwak", false},
		{"Coup d'Etat", "Ben Coes", "Coup D'Etat by Edward Luttwak EPUB", false},
		{"Coup d'Etat", "", "Coup D'Etat - Edward Luttwak", true},
		{"Power Down", "Ben Coes", "Power Down by Ben Coes EPUB", true},
		{"Power Down", "Ben Coes", "Ben Coes - Power Down EPUB", true},
		{"Power Down", "Ben Coes", "Power Down EPUB", true},
		{"Power Down", "Ben Coes", "The Power of Writing It Down by Ben Coes EPUB", false},
		{"12 Rules for Life", "Jordan B. Peterson", "12 Rules for Life by Jordan B. Peterson EPUB", true},
		{"12 Rules for Life", "Jordan B. Peterson", "Jordan B Peterson - 12 Rules for Life EPUB", true},
		{"12 Rules for Life", "Jordan B. Peterson", "12 Rules for Life EPUB", true},
		{"12 Rules for Life: An Antidote to Chaos", "Jordan B. Peterson", "12 Rules for Life EPUB", true},
		{"12 Rules for Life: An Antidote to Chaos", "Jordan B. Peterson", "12 More Rules for Life by Jordan B. Peterson EPUB", false},
		{"The Lord of the Rings", "J.R.R. Tolkien", "The Lord of the Rings EPUB", true},
		{"The Lord of the Rings", "J.R.R. Tolkien", "Lord Rings EPUB", true},
		{"The Lord of the Rings", "J.R.R. Tolkien", "The Lord of the Rings by J. R. R. Tolkien EPUB", true},
		{"Death by Black Hole", "Neil deGrasse Tyson", "Death by Black Hole EPUB", true},
		{"Death by Black Hole", "Neil deGrasse Tyson", "Death by Black Hole by Neil deGrasse Tyson EPUB", true},
		{"Death by Black Hole", "Neil deGrasse Tyson", "Death by Black Hole by Edward Luttwak EPUB", false},
	}
	for _, c := range cases {
		t.Run(c.release, func(t *testing.T) {
			got := filterRelevant(toResults(c.release), c.title, c.author, nil)
			if (len(got) == 1) != c.want {
				t.Fatalf("filterRelevant(%q, %q, %q): kept=%v, want %v", c.release, c.title, c.author, len(got) == 1, c.want)
			}
		})
	}
}

// seriesLabelRelevanceFilters runs the #2863 table through both filter entry
// points. filterRelevantDebug is the one production runs (automatic search via
// SearchBookWithOutcomes, interactive search via SearchBookWithDebug);
// filterRelevant is the fallback for searchers that cannot report outcomes.
var seriesLabelRelevanceFilters = []struct {
	name string
	fn   func([]newznab.SearchResult, string, string, []string) []newznab.SearchResult
}{
	{"filterRelevant", filterRelevant},
	{"filterRelevantDebug", func(r []newznab.SearchResult, title, author string, aliases []string) []newznab.SearchResult {
		kept, _ := filterRelevantDebug(r, title, author, aliases)
		return kept
	}},
}

// A series label between the title and "by <author>" used to hide the
// attribution, so a same titled book by someone else was grabbed (#2863).
func TestFilterRelevantSeriesLabelAttribution(t *testing.T) {
	cases := []struct {
		title, author, release string
		want                   bool
	}{
		// The reported grab.
		{"Heart of Glass", "Jennifer Hillier", "Heart of Glass, Skye Druids (03) by Donna Grant EPUB", false},
		{"Heart of Glass", "Jennifer Hillier", "Heart of Glass, Skye Druids (03) by Jennifer Hillier EPUB", true},
		{"Heart of Glass", "Jennifer Hillier", "Heart of Glass, Skye Druids (03) by J. Hillier EPUB", true},
		{"Heart of Glass", "Jennifer Hillier", "Heart of Glass, Skye Druids (03) by Hillier, Jennifer EPUB", true},
		{"Heart of Glass", "Jennifer Hillier", "Heart of Glass, Skye Druids (03) by Hillier EPUB", true},
		{"Heart of Glass", "Jennifer Hillier", "Heart of Glass, Skye Druids (03) by Donna Grant and Jennifer Hillier EPUB", true},
		{"Heart of Glass", "Jennifer Hillier", "Heart of Glass, Skye Druids (03) narrated by Donna Grant MP3", true},
		{"Heart of Glass", "Jennifer Hillier", "Heart of Glass, Skye Druids (03) EPUB", true},
		{"Heart of Glass", "Jennifer Hillier", "Heart of Glass EPUB", true},
		// Other index shapes.
		{"Heart of Glass", "Jennifer Hillier", "Heart of Glass, Skye Druids #3 by Jennifer Hillier EPUB", true},
		{"Heart of Glass", "Jennifer Hillier", "Heart of Glass, Skye Druids #3 by Donna Grant EPUB", false},
		{"Heart of Glass", "Jennifer Hillier", "Heart of Glass (Skye Druids Book 2) by Donna Grant", false},
		{"Heart of Glass", "Jennifer Hillier", "Heart of Glass (Skye Druids Book 2) by Jennifer Hillier", true},
		{"Heart of Glass", "Jennifer Hillier", "Heart of Glass, Skye Druids 3 by Donna Grant [retail] 2019", false},
		// The attribution ends at the first file token; a name after it is not the author.
		{"Heart of Glass", "Jennifer Hillier", "Heart of Glass, Skye Druids 3 by Donna Grant (2019) [for fans of Jennifer Hillier]", false},
		// Unchanged: the separator form was already read as an attribution.
		{"Heart of Glass", "Jennifer Hillier", "Heart of Glass - Skye Druids 03 - Donna Grant", false},
		// A title that contains "by" itself.
		{"Stand by Me", "Neil Jones", "Stand by Me by Neil Jones EPUB", true},
		{"Stand by Me", "Neil Jones", "Stand by Me by Donna Grant EPUB", false},
		{"Stand by Me", "Neil Jones", "Stand by Me, Heartland #2 by Neil Jones EPUB", true},
		{"Stand by Me", "Neil Jones", "Stand by Me, Heartland #2 by Donna Grant EPUB", false},
		{"Death by Chocolate", "Sarah Graves", "Death by Chocolate, Home Repair Is Homicide 12 by Sarah Graves", true},
		{"Death by Chocolate", "Sarah Graves", "Death by Chocolate, Home Repair Is Homicide 12 by Donna Grant", false},
		{"Death by Chocolate", "Sarah Graves", "Death by Chocolate EPUB", true},
		// No author requested: nothing to conflict with.
		{"Heart of Glass", "", "Heart of Glass, Skye Druids (03) by Donna Grant EPUB", true},
	}
	for _, f := range seriesLabelRelevanceFilters {
		for _, c := range cases {
			t.Run(f.name+"/"+c.release, func(t *testing.T) {
				got := f.fn(toResults(c.release), c.title, c.author, nil)
				if (len(got) == 1) != c.want {
					t.Fatalf("%s(%q, %q, %q): kept=%v, want %v", f.name, c.release, c.title, c.author, len(got) == 1, c.want)
				}
			})
		}
	}
}

// Every author token set (the primary name and each bound alias) is tried
// against the attribution read after a series label.
func TestConflictingTitleAuthorSeriesLabelAlias(t *testing.T) {
	const release = "Heart of Glass, Skye Druids (03) by Jenny Hill EPUB"
	primary := authorTokens("Jennifer Hillier")
	alias := authorTokens("Jenny Hill")
	if !conflictingTitleAuthor(release, "Heart of Glass", [][]string{primary}) {
		t.Fatalf("%q not read as a conflicting attribution without the alias", release)
	}
	if conflictingTitleAuthor(release, "Heart of Glass", [][]string{primary, alias}) {
		t.Fatalf("%q rejected although its attribution matches an alias", release)
	}
}

// The #2863 release must be dropped by the entry points that actually grab:
// SearchBookWithOutcomes (scheduler) and SearchBookWithDebug (interactive).
func TestSeriesLabelAttributionReachesSearchEntrypoints(t *testing.T) {
	const wrong = "Heart of Glass, Skye Druids (03) by Donna Grant EPUB"
	const right = "Heart of Glass by Jennifer Hillier EPUB"
	feed := `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:newznab="http://www.newznab.com/DTD/2010/feeds/attributes/">
  <channel>
    <newznab:response offset="0" total="2"/>
    <item><title>` + wrong + `</title><guid isPermaLink="false">g0</guid><enclosure url="https://fake/dl/0" length="1000" type="application/x-nzb"/></item>
    <item><title>` + right + `</title><guid isPermaLink="false">g1</guid><enclosure url="https://fake/dl/1" length="1000" type="application/x-nzb"/></item>
  </channel>
</rss>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(feed))
	}))
	defer srv.Close()

	idxs := []models.Indexer{{ID: 1, Name: "test", URL: srv.URL, Enabled: true, Categories: []int{7020}}}
	crit := MatchCriteria{Title: "Heart of Glass", Author: "Jennifer Hillier"}
	ctx := context.Background()
	debugged, _ := newTestSearcher().SearchBookWithDebug(ctx, idxs, crit)
	outcomes, _ := newTestSearcher().SearchBookWithOutcomes(ctx, idxs, crit)
	for name, got := range map[string][]string{
		"SearchBookWithDebug":    resultTitles(debugged),
		"SearchBookWithOutcomes": resultTitles(outcomes),
	} {
		if slices.Contains(got, wrong) {
			t.Errorf("%s kept the wrong author release %q", name, wrong)
		}
		if !slices.Contains(got, right) {
			t.Errorf("%s dropped the right release %q (got %q)", name, right, got)
		}
	}
}
