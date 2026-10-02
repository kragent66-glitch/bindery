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
			// KNOWN GAP, not a goal: this is the right release and it is
			// dropped. The title identity gate compares "7" and "07" as
			// different words (the volume guard itself ignores zero padding).
			// Production search kept it until #2812 put the identity gate on
			// that path too; flip this to true when the gate learns to
			// ignore leading zeros.
			name:    "zero padded volume number in the release",
			title:   "The Rising of the Shield Hero Volume 7",
			release: "Aneko Yusagi - The Rising of the Shield Hero Vol 07 - epub",
			want:    false,
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
