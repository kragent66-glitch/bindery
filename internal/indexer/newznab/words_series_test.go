package newznab

import "testing"

func TestSeriesPositionTitle(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Feral Mage 4: The Mining Company Contract", "The Mining Company Contract"},
		{"Cradle Book 8: Wintersteel", "Wintersteel"},
		{"Discworld #3: Equal Rites", "Equal Rites"},
		{"Dungeon Crawler Carl 07: This Inevitable Ruin", "This Inevitable Ruin"},
		{"The Wandering Inn Vol. 2.: Volume Two", "Volume Two"},
		// No series position before the colon: an ordinary subtitle, left alone.
		{"12 Rules for Life: An Antidote to Chaos", ""},
		{"Dune: Messiah", ""},
		{"The Lord of the Rings: The Fellowship of the Ring", ""},
		// A bare number is a title, not a series with a position.
		{"1984: A Novel", ""},
		// Roman numerals are ambiguous with words ("I"), so they do not count.
		{"Rocky IV: The Novel", ""},
		{"Feral Mage 4:", ""},
		{"No colon here 4", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := SeriesPositionTitle(c.in); got != c.want {
			t.Errorf("SeriesPositionTitle(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestTitleHasRelevantResult_SeriesPositionTitle covers the tier gate. A tier
// whose only results name the book's own title is relevant; without this it is
// read as a canned feed and the cascade walks on past it.
func TestTitleHasRelevantResult_SeriesPositionTitle(t *testing.T) {
	q := "Feral Mage 4: The Mining Company Contract"
	if !titleHasRelevantResult(q, []SearchResult{{Title: "The Mining Company Contract by Chase Kilgore [ENG / EPUB]"}}) {
		t.Error("a release naming the book's own title should make the tier relevant")
	}
	if titleHasRelevantResult(q, []SearchResult{{Title: "Wulfe Untamed: Feral Warriors, Book 8 by Pamela Palmer"}}) {
		t.Error("a release naming neither the series nor the book's own title must not")
	}
	// A one word book title is below the floor, so it does not open the gate.
	if titleHasRelevantResult("Cradle Book 8: Wintersteel", []SearchResult{{Title: "Wintersteel by Someone"}}) {
		t.Error("a one word series position title must not be enough on its own")
	}
}
