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
