package indexer

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/vavallee/bindery/internal/indexer/newznab"
	"github.com/vavallee/bindery/internal/models"
)

// feedOf renders release titles as a newznab RSS response.
func feedOf(titles ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:newznab="http://www.newznab.com/DTD/2010/feeds/attributes/">
  <channel>
    <newznab:response offset="0" total="%d"/>`, len(titles))
	for i, title := range titles {
		fmt.Fprintf(&b, `
    <item>
      <title>%s</title>
      <guid isPermaLink="false">g%d</guid>
      <enclosure url="https://fake/dl/%d" length="1000" type="application/x-nzb"/>
    </item>`, html.EscapeString(title), i, i)
	}
	b.WriteString("\n  </channel>\n</rss>")
	return b.String()
}

// TestRelevanceGuardsReachEveryProductionEntrypoint drives the three real search
// entry points against one fake indexer serving #2502's wrong releases next to
// the right ones.
//
// #2502's guards lived in filterRelevant, and its tests called that function
// directly. But SearchBookWithOutcomes (automatic grabbing, via the scheduler)
// and SearchBookWithDebug (interactive search, via the API) both run
// filterRelevantDebug, which did not have them, so every release #2502 was
// written to reject was still grabbed. A unit test on one filter function
// cannot see that; only running the entry points can.
//
// #2863 later copied the author guard across by hand, which fixed the Coup
// d'Etat case on its own; the title identity gate was never copied, which the
// Power Down, 12 Rules and same spelling "Volume 16" cases still catch.
func TestRelevanceGuardsReachEveryProductionEntrypoint(t *testing.T) {
	cases := []struct {
		title, author string
		right, wrong  []string
	}{
		{
			title: "Coup d'Etat", author: "Ben Coes",
			right: []string{"Coup D'Etat - Ben Coes EPUB"},
			wrong: []string{"Coup D'Etat - Edward Luttwak", "Coup D'Etat by Edward Luttwak EPUB"},
		},
		{
			title: "Power Down", author: "Ben Coes",
			right: []string{"Power Down by Ben Coes EPUB"},
			wrong: []string{"The Power of Writing It Down by Ben Coes EPUB"},
		},
		{
			title: "12 Rules for Life", author: "Jordan B. Peterson",
			right: []string{"12 Rules for Life by Jordan B. Peterson EPUB"},
			wrong: []string{"Beyond Order: 12 More Rules for Life by Jordan B. Peterson EPUB"},
		},
		{
			// #2921's author noted the production path kept this one.
			title: "The Rising of the Shield Hero Volume 17", author: "Aneko Yusagi",
			right: []string{shieldHeroRelease},
			wrong: []string{"Aneko Yusagi - The Rising of the Shield Hero Volume 16 - epub"},
		},
	}
	for _, c := range cases {
		t.Run(c.title, func(t *testing.T) {
			feed := feedOf(append(slices.Clone(c.right), c.wrong...)...)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/xml")
				_, _ = w.Write([]byte(feed))
			}))
			defer srv.Close()

			idxs := []models.Indexer{{ID: 1, Name: "test", URL: srv.URL, Enabled: true, Categories: []int{7020}}}
			crit := MatchCriteria{Title: c.title, Author: c.author}
			ctx := context.Background()

			plain := newTestSearcher().SearchBook(ctx, idxs, crit)
			debugged, _ := newTestSearcher().SearchBookWithDebug(ctx, idxs, crit)
			outcomes, _ := newTestSearcher().SearchBookWithOutcomes(ctx, idxs, crit)

			for name, got := range map[string][]string{
				"SearchBook":             resultTitles(plain),
				"SearchBookWithDebug":    resultTitles(debugged),
				"SearchBookWithOutcomes": resultTitles(outcomes),
			} {
				for _, w := range c.wrong {
					if slices.Contains(got, w) {
						t.Errorf("%s kept the wrong release %q for %q by %s", name, w, c.title, c.author)
					}
				}
				for _, r := range c.right {
					if !slices.Contains(got, r) {
						t.Errorf("%s dropped the right release %q for %q by %s", name, r, c.title, c.author)
					}
				}
			}
		})
	}
}

// TestFilterRelevantDebugDropReasons pins that the interactive panel is told
// which relevance check fired, not only that one did.
func TestFilterRelevantDebugDropReasons(t *testing.T) {
	const splitReason = "title words appear, but with other words or numbers among them"
	cases := []struct {
		title, author, release, reason string
	}{
		{"Coup d'Etat", "Ben Coes", "Coup D'Etat by Edward Luttwak EPUB", "release names a different author for this title"},
		{"12 Rules for Life", "Jordan B. Peterson", "Beyond Order: 12 More Rules for Life by Jordan B. Peterson EPUB", splitReason},
		{"The Rising of the Shield Hero Volume 17", "Aneko Yusagi", "Aneko Yusagi - The Rising of the Shield Hero Volume 16 - epub", splitReason},
		{"Power Down", "Ben Coes", "Something Else Entirely by Ben Coes EPUB", "title/author keywords did not match release name"},
	}
	for _, c := range cases {
		t.Run(c.title, func(t *testing.T) {
			kept, dropped := filterRelevantDebug([]newznab.SearchResult{{Title: c.release}}, c.title, c.author, nil)
			if len(kept) != 0 || len(dropped) != 1 {
				t.Fatalf("kept=%d dropped=%d, want 0 and 1", len(kept), len(dropped))
			}
			if dropped[0].Reason != c.reason || dropped[0].Stage != "relevance" {
				t.Errorf("drop = %s/%q, want relevance/%q", dropped[0].Stage, dropped[0].Reason, c.reason)
			}
		})
	}
}
