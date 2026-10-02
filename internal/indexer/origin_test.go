package indexer

import (
	"context"
	"testing"
)

// TestSearchOrigin_Automatic is the classification the author monitoring rule
// reads (#2742). Unmonitoring an author suppresses the automatic origins and
// must leave every search a user asked for alone, so the table is pinned here,
// next to the taxonomy it classifies, rather than only in the scheduler that
// consumes it.
func TestSearchOrigin_Automatic(t *testing.T) {
	cases := []struct {
		origin SearchOrigin
		want   bool
		why    string
	}{
		{OriginScheduled, true, "the periodic wanted sweep"},
		{OriginRequeue, true, "the re search after a stalled download was removed"},
		{OriginAuthor, true, "a catalogue sync fanning out over works it just discovered"},
		{OriginUnknown, true, "a caller that never tagged itself is not evidence of user intent"},
		{OriginBook, false, "the Search button on the book page"},
		{OriginAdd, false, "an explicit add that asked to search on add"},
		{OriginBulk, false, "a bulk Search over a selection the user made"},
		{OriginSeriesFill, false, "series Fill"},
		{OriginRecommendation, false, "a recommendation the user accepted"},
	}
	for _, tc := range cases {
		if got := tc.origin.Automatic(); got != tc.want {
			t.Errorf("SearchOrigin(%q).Automatic() = %v, want %v: %s", tc.origin, got, tc.want, tc.why)
		}
	}
}

// An origin nobody has classified yet must read as automatic. That default is
// the safe half of the rule: a new dispatch site is guarded until somebody
// decides it carries user intent, rather than grabbing until somebody notices.
func TestSearchOrigin_UnclassifiedDefaultsToAutomatic(t *testing.T) {
	if !SearchOrigin("some-future-caller").Automatic() {
		t.Error("an unclassified origin reads as user initiated; it must default to automatic")
	}
}

// SearchOriginFrom is what the guard actually calls, so the untagged and empty
// context paths are pinned to the same answer.
func TestSearchOriginFrom_UntaggedIsUnknownAndAutomatic(t *testing.T) {
	if got := SearchOriginFrom(context.Background()); got != OriginUnknown {
		t.Fatalf("SearchOriginFrom(untagged) = %q, want %q", got, OriginUnknown)
	}
	if got := SearchOriginFrom(WithSearchOrigin(context.Background(), "")); got != OriginUnknown {
		t.Fatalf("SearchOriginFrom(empty origin) = %q, want %q", got, OriginUnknown)
	}
	if got := SearchOriginFrom(WithSearchOrigin(context.Background(), OriginBook)); got != OriginBook {
		t.Fatalf("SearchOriginFrom(book) = %q, want %q", got, OriginBook)
	}
	if !SearchOriginFrom(context.Background()).Automatic() {
		t.Error("an untagged context must classify as automatic")
	}
}
