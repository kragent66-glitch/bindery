package db

import (
	"context"
	"testing"

	"github.com/vavallee/bindery/internal/models"
)

// UnmonitoredAuthorIDs backs the author monitoring rule (#2742): the wanted
// sweep asks it once per tick instead of loading an author per book, and the
// Wanted list asks it once per response instead of per row.
func TestAuthorRepo_UnmonitoredAuthorIDs(t *testing.T) {
	database, err := OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	repo := NewAuthorRepo(database)
	ctx := context.Background()

	// An empty table is "everyone is monitored", not an error, and must come
	// back as a usable empty set rather than a nil map the caller has to guard.
	empty, err := repo.UnmonitoredAuthorIDs(ctx)
	if err != nil {
		t.Fatalf("UnmonitoredAuthorIDs on an empty table: %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Fatalf("empty table returned %v, want an empty set", empty)
	}

	watched := &models.Author{ForeignID: "OL-M1", Name: "Andy Weir", SortName: "Weir, Andy", Monitored: true}
	quiet := &models.Author{ForeignID: "OL-U1", Name: "P. G. Wodehouse", SortName: "Wodehouse, P. G.", Monitored: false}
	alsoQuiet := &models.Author{ForeignID: "OL-U2", Name: "Dorothy Sayers", SortName: "Sayers, Dorothy", Monitored: false}
	for _, a := range []*models.Author{watched, quiet, alsoQuiet} {
		if err := repo.Create(ctx, a); err != nil {
			t.Fatalf("seed %s: %v", a.Name, err)
		}
	}

	got, err := repo.UnmonitoredAuthorIDs(ctx)
	if err != nil {
		t.Fatalf("UnmonitoredAuthorIDs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d unmonitored ids, want 2: %v", len(got), got)
	}
	if !got[quiet.ID] || !got[alsoQuiet.ID] {
		t.Errorf("unmonitored authors missing from the set: %v", got)
	}
	if got[watched.ID] {
		t.Errorf("a monitored author (%d) is in the unmonitored set: %v", watched.ID, got)
	}

	// The set follows the flag, so re-monitoring an author removes them.
	quiet.Monitored = true
	if err := repo.Update(ctx, quiet); err != nil {
		t.Fatalf("re-monitor: %v", err)
	}
	got, err = repo.UnmonitoredAuthorIDs(ctx)
	if err != nil {
		t.Fatalf("UnmonitoredAuthorIDs after update: %v", err)
	}
	if got[quiet.ID] || len(got) != 1 {
		t.Errorf("re-monitored author still reported as unmonitored: %v", got)
	}
}
