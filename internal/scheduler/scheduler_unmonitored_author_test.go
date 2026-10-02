package scheduler

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vavallee/bindery/internal/db"
	"github.com/vavallee/bindery/internal/indexer"
	"github.com/vavallee/bindery/internal/models"
)

// unmonitoredAuthorFixture builds a scheduler over a real in memory database
// holding one author and one wanted, monitored book by that author. The
// author's own monitored flag is the variable under test, and the book is
// always left monitored: that is the reporter's state in #2742, where a bulk
// unmonitor on the Authors page wrote the author flag and never touched the
// books.
func unmonitoredAuthorFixture(t *testing.T, authorMonitored bool) (*Scheduler, *stubSearcher, models.Book) {
	t.Helper()
	database, err := db.OpenMemory()
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	ctx := context.Background()

	authors := db.NewAuthorRepo(database)
	author := &models.Author{
		ForeignID: "OL1A",
		Name:      "P. G. Wodehouse",
		SortName:  "Wodehouse, P. G.",
		Monitored: authorMonitored,
	}
	if err := authors.Create(ctx, author); err != nil {
		t.Fatalf("create author: %v", err)
	}

	books := db.NewBookRepo(database)
	book := &models.Book{
		ForeignID: "OL1M",
		AuthorID:  author.ID,
		Title:     "The Old Reliable",
		Monitored: true,
		Status:    models.BookStatusWanted,
		MediaType: models.MediaTypeEbook,
	}
	if err := books.Create(ctx, book); err != nil {
		t.Fatalf("create book: %v", err)
	}

	ss := &stubSearcher{}
	s := &Scheduler{searcher: ss, authors: authors, books: books}
	return s, ss, *book
}

// TestScheduledSweep_SkipsBooksOfAnUnmonitoredAuthor is the #2742 report.
//
// The reporter imported 200+ authors with every book monitored, bulk
// unmonitored most of them on the Authors page so they could work through the
// wanted lists one at a time, and the sweep kept grabbing. The author flag was
// consulted nowhere in the grab path: it only fed a cascade that rewrites each
// book's own monitored flag, and the bulk path never ran that cascade.
func TestScheduledSweep_SkipsBooksOfAnUnmonitoredAuthor(t *testing.T) {
	s, ss, book := unmonitoredAuthorFixture(t, false)

	ctx := indexer.WithSearchOrigin(context.Background(), indexer.OriginScheduled)
	s.searchAndGrabFormats(ctx, book, neededFormats(&book), s.newSweepContext(ctx))

	if got := int(ss.calls.Load()); got != 0 {
		t.Fatalf("scheduled sweep searched a book whose author is unmonitored: got %d searches, want 0", got)
	}
}

// TestScheduledSweep_StillSearchesAMonitoredAuthor is the other half: the
// guard must not stop the sweep doing its job for everyone else.
func TestScheduledSweep_StillSearchesAMonitoredAuthor(t *testing.T) {
	s, ss, book := unmonitoredAuthorFixture(t, true)

	ctx := indexer.WithSearchOrigin(context.Background(), indexer.OriginScheduled)
	s.searchAndGrabFormats(ctx, book, neededFormats(&book), s.newSweepContext(ctx))

	if got := int(ss.calls.Load()); got != 1 {
		t.Fatalf("scheduled sweep skipped a book whose author is monitored: got %d searches, want 1", got)
	}
}

// TestManualBookSearch_RunsForAnUnmonitoredAuthor pins the second half of the
// rule. Unmonitoring an author means "stop reaching for their books on your
// own", not "refuse to search when I press the button".
func TestManualBookSearch_RunsForAnUnmonitoredAuthor(t *testing.T) {
	s, ss, book := unmonitoredAuthorFixture(t, false)

	s.SearchAndGrabBook(indexer.WithSearchOrigin(context.Background(), indexer.OriginBook), book)

	if got := int(ss.calls.Load()); got != 1 {
		t.Fatalf("a book page search was refused for an unmonitored author: got %d searches, want 1", got)
	}
}

// TestSearchOriginGuard_PerOrigin is the table the PR body carries: every
// origin in the taxonomy, and whether an unmonitored author stops it.
func TestSearchOriginGuard_PerOrigin(t *testing.T) {
	cases := []struct {
		origin    indexer.SearchOrigin
		wantCalls int
	}{
		{indexer.OriginScheduled, 0},
		{indexer.OriginRequeue, 0},
		{indexer.OriginAuthor, 0},
		{indexer.OriginUnknown, 0},
		{indexer.OriginBook, 1},
		{indexer.OriginBulk, 1},
		{indexer.OriginSeriesFill, 1},
		{indexer.OriginRecommendation, 1},
		{indexer.OriginAdd, 1},
	}
	for _, tc := range cases {
		t.Run(string(tc.origin), func(t *testing.T) {
			s, ss, book := unmonitoredAuthorFixture(t, false)
			s.SearchAndGrabBook(indexer.WithSearchOrigin(context.Background(), tc.origin), book)
			if got := int(ss.calls.Load()); got != tc.wantCalls {
				t.Fatalf("origin %q under an unmonitored author: got %d searches, want %d", tc.origin, got, tc.wantCalls)
			}
		})
	}
}

// TestUntaggedSearchIsTreatedAsAutomatic covers a caller that never tagged its
// context at all, which is what SearchOriginFrom reports as OriginUnknown. A
// caller forgetting to say who it is is not evidence of user intent.
func TestUntaggedSearchIsTreatedAsAutomatic(t *testing.T) {
	s, ss, book := unmonitoredAuthorFixture(t, false)

	s.SearchAndGrabBook(context.Background(), book)

	if got := int(ss.calls.Load()); got != 0 {
		t.Fatalf("an untagged search ran for an unmonitored author: got %d searches, want 0", got)
	}
}

// TestUnmonitoredAuthorGuard_FailsOpenWithoutAuthors pins the nil repo
// behaviour, matching autoGrabEnabled: a scheduler with no authors repo, or a
// book whose author row has gone, keeps searching rather than silently
// stopping every grab.
func TestUnmonitoredAuthorGuard_FailsOpenWithoutAuthors(t *testing.T) {
	ss := &stubSearcher{}
	s := &Scheduler{searcher: ss}
	book := models.Book{ID: 1, AuthorID: 99, Title: "Orphan", MediaType: models.MediaTypeEbook}

	s.searchAndGrabFormats(indexer.WithSearchOrigin(context.Background(), indexer.OriginScheduled), book, neededFormats(&book), nil)

	if got := int(ss.calls.Load()); got != 1 {
		t.Fatalf("guard closed with no authors repo: got %d searches, want 1 (fail open)", got)
	}
}

// TestUnmonitoredAuthorGuard_MissingAuthorRowStillSearches covers the other
// fail open branch: the book's author row has gone (an orphan left by a delete
// racing the sweep). Nothing says the author is unmonitored, so nothing should
// stop the search.
func TestUnmonitoredAuthorGuard_MissingAuthorRowStillSearches(t *testing.T) {
	s, ss, book := unmonitoredAuthorFixture(t, false)
	book.AuthorID = 9999 // no such row

	s.searchAndGrabFormats(indexer.WithSearchOrigin(context.Background(), indexer.OriginScheduled), book, neededFormats(&book), nil)

	if got := int(ss.calls.Load()); got != 1 {
		t.Fatalf("guard closed for a book with no author row: got %d searches, want 1 (fail open)", got)
	}
}

// The log line names the author, because "a book was skipped" on its own does
// not tell a user which switch to go and flip. The name comes from the joined
// projection when the caller already loaded one, and from the author row
// otherwise; both paths are exercised here through the one they feed.
func TestUnmonitoredAuthorGuard_UsesTheJoinedAuthorName(t *testing.T) {
	s, ss, book := unmonitoredAuthorFixture(t, false)
	book.Author = &models.Author{ID: book.AuthorID, Name: "P. G. Wodehouse"}

	ctx := indexer.WithSearchOrigin(context.Background(), indexer.OriginScheduled)
	if name, unmonitored := s.authorMonitorState(ctx, book, nil); name != "P. G. Wodehouse" || !unmonitored {
		t.Fatalf("authorMonitorState = (%q, %v), want (\"P. G. Wodehouse\", true)", name, unmonitored)
	}

	s.searchAndGrabFormats(ctx, book, neededFormats(&book), nil)
	if got := int(ss.calls.Load()); got != 0 {
		t.Fatalf("got %d searches, want 0", got)
	}
}

// TestSweepContext_AnswersAuthorMonitoringFromOneLoad is the #2370 invariant
// applied to the new table: the sweep must not add a query per book. With the
// authors repo detached, a snapshot that still answers correctly can only be
// reading what newSweepContext loaded once.
func TestSweepContext_AnswersAuthorMonitoringFromOneLoad(t *testing.T) {
	s, ss, book := unmonitoredAuthorFixture(t, false)
	ctx := indexer.WithSearchOrigin(context.Background(), indexer.OriginScheduled)

	sweep := s.newSweepContext(ctx)
	s.authors = nil

	s.searchAndGrabFormats(ctx, book, neededFormats(&book), sweep)

	if got := int(ss.calls.Load()); got != 0 {
		t.Fatalf("sweep snapshot did not carry author monitoring: got %d searches, want 0", got)
	}
}

// TestAuthorMonitoringChokepointHolds is the drift guard, modelled on
// TestAuthorGrabChokepointHolds' sibling for the auto grab switch (#2256).
// The fix for #2742 is not "the sweep checks the author", it is "the one place
// a grab is dispatched from checks the author", so a future automatic caller
// inherits the rule instead of having to know it exists.
func TestAuthorMonitoringChokepointHolds(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(schedulerSourceDir, "scheduler.go"))
	if err != nil {
		t.Fatalf("reading scheduler.go: %v (this guard needs the full repo checkout)", err)
	}
	body, _, _ := funcBody(t, string(raw), "func (s *Scheduler) searchAndGrabFormats(")

	checkAt := strings.Index(body, "s.skipUnmonitoredAuthor(")
	if checkAt < 0 {
		t.Fatal("searchAndGrabFormats does not call s.skipUnmonitoredAuthor; the author monitoring rule (#2742) is only enforced there, so removing it un-guards every automatic caller at once")
	}
	dispatchAt := strings.Index(body, "s.searchAndGrabFormat(")
	if dispatchAt < 0 {
		t.Fatal("searchAndGrabFormats no longer calls s.searchAndGrabFormat; update this drift guard to follow the new dispatch call")
	}
	if checkAt > dispatchAt {
		t.Fatal("searchAndGrabFormats dispatches a search before checking s.skipUnmonitoredAuthor; the check must gate the dispatch (#2742)")
	}
}
