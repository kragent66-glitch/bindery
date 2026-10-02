package indexer

import (
	"testing"

	"github.com/vavallee/bindery/internal/indexer/newznab"
)

// shieldHeroRelease is the release from the Discord report, in the
// "{Author} - {Series} - {Position} - {Title}" shape that indexer uses.
const shieldHeroRelease = "Aneko Yusagi - The Rising of the Shield Hero - 17 - The Rising of the Shield Hero Vol 17 - epub"

// TestFilterRelevantVolumeMarkerSpelling pins that the spelling of a volume
// marker does not decide relevance. SigWords keeps "volume" (and "vol") as a
// keyword, so a title saying "Volume 17" demanded the literal word "volume"
// and dropped a release that says "Vol 17", while "Vol. 17" and a bare "17"
// were kept.
//
// Both relevance filters are checked: filterRelevantDebug is the one
// production search goes through. When this test was written the two had
// separate expectations, because only filterRelevant ran the title identity
// gate. They now share one implementation (filterRelevantDetailed, #2812), so
// each case has a single expectation and the test fails if they ever disagree.
func TestFilterRelevantVolumeMarkerSpelling(t *testing.T) {
	const author = "Aneko Yusagi"
	cases := []struct {
		name    string
		title   string
		release string
		want    bool
	}{
		{
			name:    "repro: Volume in the title, Vol in the release",
			title:   "The Rising of the Shield Hero Volume 17",
			release: shieldHeroRelease,
			want:    true,
		},
		{
			name:    "repro with a comma before Volume",
			title:   "The Rising of the Shield Hero, Volume 17",
			release: shieldHeroRelease,
			want:    true,
		},
		{
			name:    "control: Vol. in the title was already kept",
			title:   "The Rising of the Shield Hero, Vol. 17",
			release: shieldHeroRelease,
			want:    true,
		},
		{
			name:    "control: bare number in the title was already kept",
			title:   "The Rising of the Shield Hero 17",
			release: shieldHeroRelease,
			want:    true,
		},
		{
			name:    "reverse: Vol. in the title, Volume in the release",
			title:   "The Rising of the Shield Hero Vol. 17",
			release: "Aneko Yusagi - The Rising of the Shield Hero Volume 17 - epub",
			want:    true,
		},
		{
			name:    "plural and dotted forms are the same marker",
			title:   "The Rising of the Shield Hero Volumes 17",
			release: "Aneko.Yusagi.The.Rising.of.the.Shield.Hero.Vol.17.epub",
			want:    true,
		},
		{
			// The title identity gate used to compare "7" and "07" as
			// different words. A number now matches with any zero padding
			// (keywordPattern), the same as the volume guard.
			name:    "zero padded volume number in the release",
			title:   "The Rising of the Shield Hero Volume 7",
			release: "Aneko Yusagi - The Rising of the Shield Hero Vol 07 - epub",
			want:    true,
		},
		{
			name:    "mid title format qualifier is not a keyword",
			title:   "The Rising of the Shield Hero (Light Novel) Vol. 17",
			release: shieldHeroRelease,
			want:    true,
		},
		{
			name:    "mid title format qualifier plus Volume",
			title:   "The Rising of the Shield Hero (Manga) Volume 17",
			release: shieldHeroRelease,
			want:    true,
		},

		// Negatives.
		{
			name:    "different title that merely contains vol",
			title:   "The Rising of the Shield Hero Volume 17",
			release: "Aneko Yusagi - The Reprise of the Spear Hero Vol 17 - epub",
			want:    false,
		},
		{
			name:    "a word starting with vol is not a volume marker",
			title:   "The Rising of the Shield Hero Volume 17",
			release: "Aneko Yusagi - The Rising of the Shield Hero Volcano 17 - epub",
			want:    false,
		},
		{
			name:    "Vol 16 release for a Volume 17 title stays dropped",
			title:   "The Rising of the Shield Hero Volume 17",
			release: "Aneko Yusagi - The Rising of the Shield Hero - 16 - The Rising of the Shield Hero Vol 16 - epub",
			want:    false,
		},
		{
			name:    "Volume 16 release for a Vol. 17 title stays dropped",
			title:   "The Rising of the Shield Hero Vol. 17",
			release: "Aneko Yusagi - The Rising of the Shield Hero Volume 16 - epub",
			want:    false,
		},
		{
			// Same spelling on both sides. The title identity gate drops it.
			// Production search used to keep it, because the debug filter
			// had no identity gate; since #2812 both paths drop it.
			name:    "Volume 16 release for a Volume 17 title is dropped",
			title:   "The Rising of the Shield Hero Volume 17",
			release: "Aneko Yusagi - The Rising of the Shield Hero Volume 16 - epub",
			want:    false,
		},
		{
			name:    "cross spelling release naming another author is dropped",
			title:   "The Rising of the Shield Hero Volume 17",
			release: "The Rising of the Shield Hero Vol 17 - Some Other Writer",
			want:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := []newznab.SearchResult{{Title: tc.release}}
			gotPlain := len(filterRelevant(in, tc.title, author, nil)) == 1
			kept, _ := filterRelevantDebug(in, tc.title, author, nil)
			gotDebug := len(kept) == 1
			if gotPlain != tc.want {
				t.Errorf("filterRelevant(%q, %q) kept=%v, want %v", tc.title, tc.release, gotPlain, tc.want)
			}
			if gotDebug != tc.want {
				t.Errorf("filterRelevantDebug(%q, %q) kept=%v, want %v", tc.title, tc.release, gotDebug, tc.want)
			}
		})
	}
}

// TestFilterRelevantNumberPadding pins that a number in a title matches the
// same number with zero padding in a release, and the other way round, and
// that the padding rule cannot make one number match another. Release names
// pad series and volume positions ("Vol 07", "Book 07"); titles usually do
// not. Every case runs through both filter wrappers, which share one
// implementation.
func TestFilterRelevantNumberPadding(t *testing.T) {
	cases := []struct {
		name, title, author, release string
		want                         bool
	}{
		{"Volume 7 title, Vol 07 release", "The Rising of the Shield Hero Volume 7", "Aneko Yusagi",
			"Aneko Yusagi - The Rising of the Shield Hero Vol 07 - epub", true},
		{"bare 7 title, 07 release", "The Rising of the Shield Hero 7", "Aneko Yusagi",
			"Aneko Yusagi - The Rising of the Shield Hero 07 - epub", true},
		{"Book 7 title, Book 07 release", "Dungeon Crawler Carl Book 7", "Matt Dinniman",
			"Matt Dinniman - Dungeon Crawler Carl Book 07 - This Inevitable Ruin", true},
		{"padded title, bare release", "Dungeon Crawler Carl Book 07", "Matt Dinniman",
			"Matt Dinniman - Dungeon Crawler Carl Book 7 - This Inevitable Ruin", true},
		{"triple padded release", "The Rising of the Shield Hero 7", "Aneko Yusagi",
			"Aneko Yusagi - The Rising of the Shield Hero 007 - epub", true},

		// Padding never makes one number match another.
		{"Volume 16 release for Volume 17 title", "The Rising of the Shield Hero Volume 17", "Aneko Yusagi",
			"Aneko Yusagi - The Rising of the Shield Hero Volume 16 - epub", false},
		{"7 does not match 17", "The Rising of the Shield Hero 7", "Aneko Yusagi",
			"Aneko Yusagi - The Rising of the Shield Hero 17 - epub", false},
		{"7 does not match 70", "The Rising of the Shield Hero 7", "Aneko Yusagi",
			"Aneko Yusagi - The Rising of the Shield Hero 70 - epub", false},
		{"Book 7 does not match Book 017", "Dungeon Crawler Carl Book 7", "Matt Dinniman",
			"Matt Dinniman - Dungeon Crawler Carl Book 017", false},
		{"padded release for another author is still caught", "Dungeon Crawler Carl Book 7", "Matt Dinniman",
			"Dungeon Crawler Carl Book 07 - Some Other Writer", false},

		// KNOWN GAP, not a goal: this is the right release and it is
		// dropped. A digit and a spelled out number ("7" and "Seven") are
		// different words to the title identity gate, and only zero padding
		// is treated as equal. Production search kept it until #2812 put the
		// identity gate on that path.
		{"7 does not match Seven", "The 7 Habits of Highly Effective People", "Stephen R. Covey",
			"Stephen R. Covey - The Seven Habits of Highly Effective People", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := []newznab.SearchResult{{Title: tc.release}}
			gotPlain := len(filterRelevant(in, tc.title, tc.author, nil)) == 1
			kept, _ := filterRelevantDebug(in, tc.title, tc.author, nil)
			gotDebug := len(kept) == 1
			if gotPlain != tc.want {
				t.Errorf("filterRelevant(%q, %q) kept=%v, want %v", tc.title, tc.release, gotPlain, tc.want)
			}
			if gotDebug != tc.want {
				t.Errorf("filterRelevantDebug(%q, %q) kept=%v, want %v", tc.title, tc.release, gotDebug, tc.want)
			}
		})
	}
}

// TestContainsPhraseNumberPadding pins the number rule in keywordPattern at
// the matcher level, where the word boundaries do the work.
func TestContainsPhraseNumberPadding(t *testing.T) {
	cases := []struct {
		haystack string
		phrase   []string
		want     bool
	}{
		{"book 07", []string{"book", "7"}, true},
		{"book 7", []string{"book", "07"}, true},
		{"book 007", []string{"book", "7"}, true},
		{"book 0", []string{"book", "00"}, true},
		{"book 17", []string{"book", "7"}, false},
		{"book 70", []string{"book", "7"}, false},
		{"book 107", []string{"book", "07"}, false},
		{"book 0", []string{"book", "7"}, false},
	}
	for _, c := range cases {
		if got := ContainsPhrase(c.haystack, c.phrase); got != c.want {
			t.Errorf("ContainsPhrase(%q, %q) = %v, want %v", c.haystack, c.phrase, got, c.want)
		}
	}
}
