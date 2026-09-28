package calibre

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vavallee/bindery/internal/models"
)

// waitUntil polls the predicate every millisecond until it returns true or
// the deadline elapses.
func waitUntil(t *testing.T, timeout time.Duration, pred func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pred() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}

// fakeBookLister serves books and their book_files rows from memory.
type fakeBookLister struct {
	// books is what ListByStatus returns (imported and monitored); all is
	// what List returns (the whole visible catalogue). all defaults to books.
	books []models.Book
	all   []models.Book
	files map[int64][]models.BookFile
	// gate, when set, holds ListByStatus until it is closed.
	gate chan struct{}
}

func (f *fakeBookLister) ListByStatus(_ context.Context, _ string) ([]models.Book, error) {
	if f.gate != nil {
		<-f.gate
	}
	return f.books, nil
}

func (f *fakeBookLister) List(_ context.Context) ([]models.Book, error) {
	if f.all != nil {
		return f.all, nil
	}
	return f.books, nil
}

func (f *fakeBookLister) ListFiles(_ context.Context, bookID int64) ([]models.BookFile, error) {
	return f.files[bookID], nil
}

// fakeLedger is the delivery ledger in memory. fakeQueue writes to it the
// way the real Enqueue does: a new pending row, or nothing when the file
// already has one.
type fakeLedger struct {
	mu     sync.Mutex
	rows   []models.CalibreDelivery
	nextID int64
}

func (l *fakeLedger) ListAll(context.Context) ([]models.CalibreDelivery, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]models.CalibreDelivery(nil), l.rows...), nil
}

func (l *fakeLedger) add(row models.CalibreDelivery) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.nextID++
	row.ID = l.nextID
	l.rows = append(l.rows, row)
}

// set changes the row for a file, as the worker would.
func (l *fakeLedger) set(fileID int64, mutate func(*models.CalibreDelivery)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := range l.rows {
		if l.rows[i].BookFileID == fileID {
			mutate(&l.rows[i])
		}
	}
}

func (l *fakeLedger) remove(fileID int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	kept := l.rows[:0]
	for _, r := range l.rows {
		if r.BookFileID != fileID {
			kept = append(kept, r)
		}
	}
	l.rows = kept
}

type fakeQueue struct {
	ledger   *fakeLedger
	mu       sync.Mutex
	enqueued []int64 // book_files ids, in call order
	kicks    int
}

func (q *fakeQueue) Enqueue(_ context.Context, bookID, fileID int64, _ *int64, path string) (bool, error) {
	q.mu.Lock()
	q.enqueued = append(q.enqueued, fileID)
	q.mu.Unlock()
	rows, _ := q.ledger.ListAll(context.Background())
	for _, r := range rows {
		if r.BookFileID == fileID {
			return false, nil
		}
	}
	q.ledger.add(models.CalibreDelivery{BookID: bookID, BookFileID: fileID, FilePath: path, State: models.CalibreDeliveryPending})
	return true, nil
}

func (q *fakeQueue) Kick() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.kicks++
}

func (q *fakeQueue) calls() ([]int64, int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]int64(nil), q.enqueued...), q.kicks
}

func ebook(id, bookID int64, path string) models.BookFile {
	return models.BookFile{ID: id, BookID: bookID, Format: models.MediaTypeEbook, Path: path}
}

func delivered(bookID, fileID int64, outcome string) models.CalibreDelivery {
	return models.CalibreDelivery{BookID: bookID, BookFileID: fileID, State: models.CalibreDeliveryDelivered, Outcome: outcome}
}

func newTestSyncer(books *fakeBookLister) (*Syncer, *fakeLedger, *fakeQueue) {
	ledger := &fakeLedger{}
	queue := &fakeQueue{ledger: ledger}
	return NewSyncer(books, ledger, queue), ledger, queue
}

// runPushAll starts Push all and waits for the queueing phase to end.
func runPushAll(t *testing.T, s *Syncer) SyncProgress {
	t.Helper()
	if err := s.Start(ModePlugin); err != nil {
		t.Fatalf("start: %v", err)
	}
	var p SyncProgress
	waitUntil(t, 2*time.Second, func() bool {
		s.mu.Lock()
		queueing := s.run.queueing
		s.mu.Unlock()
		if queueing {
			return false
		}
		var err error
		p, err = s.Progress(context.Background())
		if err != nil {
			t.Fatalf("progress: %v", err)
		}
		return true
	})
	return p
}

// TestSyncer_PushAllQueuesOnlyUndeliveredEligibleFiles is the core of #2832's
// Push all: queue every ebook file the ledger does not hold, never re-queue a
// delivered file, and never re-arm a failed one (that is Retry failed's job).
// A book already in Calibre still gets its other formats queued: the worker
// adds them to the same record.
func TestSyncer_PushAllQueuesOnlyUndeliveredEligibleFiles(t *testing.T) {
	books := &fakeBookLister{
		books: []models.Book{
			{ID: 1, Title: "New", Status: models.BookStatusImported, EbookFilePath: "/l/new.epub"},
			{ID: 2, Title: "Delivered", Status: models.BookStatusImported, EbookFilePath: "/l/done.epub"},
			// Two ebook files, one delivered. The other is queued so the
			// worker can add it to the same Calibre record.
			{ID: 3, Title: "Two Formats", Status: models.BookStatusImported, EbookFilePath: "/l/two.azw3"},
			{ID: 4, Title: "Failed Before", Status: models.BookStatusImported, EbookFilePath: "/l/bad.epub"},
		},
		files: map[int64][]models.BookFile{
			1: {ebook(11, 1, "/l/new.epub")},
			2: {ebook(21, 2, "/l/done.epub")},
			3: {ebook(31, 3, "/l/two.azw3"), ebook(32, 3, "/l/two.epub")},
			4: {ebook(41, 4, "/l/bad.epub")},
		},
	}
	s, ledger, queue := newTestSyncer(books)
	ledger.add(delivered(2, 21, DeliveryOutcomeAdded))
	ledger.add(delivered(3, 32, "backfilled"))
	ledger.add(models.CalibreDelivery{BookID: 4, BookFileID: 41, State: models.CalibreDeliveryFailed, LastError: "bad_format: nope"})

	p := runPushAll(t, s)

	enqueued, kicks := queue.calls()
	if len(enqueued) != 2 || enqueued[0] != 11 || enqueued[1] != 31 {
		t.Fatalf("enqueued files = %v, want [11 31]: every untracked ebook file, and no delivered or failed one again", enqueued)
	}
	if kicks != 1 {
		t.Errorf("kicks = %d, want 1", kicks)
	}
	rows, _ := ledger.ListAll(context.Background())
	if len(rows) != 5 {
		t.Errorf("ledger rows = %d, want 5 (two new)", len(rows))
	}
	if p.Stats.Total != 5 {
		t.Errorf("Total = %d, want 5 files", p.Stats.Total)
	}
	if p.Stats.AlreadyInCalibre != 2 {
		t.Errorf("AlreadyInCalibre = %d, want 2 (files 21 and 32 were delivered before the run)", p.Stats.AlreadyInCalibre)
	}
	if p.Stats.Pushed != 0 {
		t.Errorf("Pushed = %d, want 0: a book delivered before the run was not pushed by it", p.Stats.Pushed)
	}
	if p.Stats.Failed != 1 || len(p.Errors) != 1 || p.Errors[0].BookID != 4 || p.Errors[0].Reason != "bad_format: nope" {
		t.Errorf("failed = %d, errors = %+v, want book 4 with its last error", p.Stats.Failed, p.Errors)
	}
	if !p.Running || p.Stats.Processed != 3 {
		t.Errorf("running = %v, processed = %d, want running with 3 of 5 processed", p.Running, p.Stats.Processed)
	}
}

// TestSyncer_PushAllDoesNotDeliverInline: Push all only queues. The row is
// still pending when the run has finished queueing, the worker is kicked,
// and the modal says the book is waiting for Calibre.
func TestSyncer_PushAllDoesNotDeliverInline(t *testing.T) {
	books := &fakeBookLister{
		books: []models.Book{{ID: 1, Title: "Dune", Status: models.BookStatusImported, EbookFilePath: "/l/dune.epub"}},
		files: map[int64][]models.BookFile{1: {ebook(11, 1, "/l/dune.epub")}},
	}
	s, ledger, queue := newTestSyncer(books)
	p := runPushAll(t, s)

	rows, _ := ledger.ListAll(context.Background())
	if len(rows) != 1 || rows[0].State != models.CalibreDeliveryPending {
		t.Fatalf("rows = %+v, want one pending row", rows)
	}
	if _, kicks := queue.calls(); kicks != 1 {
		t.Errorf("kicks = %d, want 1", kicks)
	}
	if !p.Running || p.Stats.Pushed != 0 || p.Message != "1 waiting for Calibre" {
		t.Errorf("progress = %+v, want running with the book waiting", p)
	}
	if p.FinishedAt != nil {
		t.Errorf("FinishedAt = %v while a row is pending", p.FinishedAt)
	}
}

// TestSyncer_StatusIsComputedFromTheLedger follows the run's rows as the
// worker settles them.
func TestSyncer_StatusIsComputedFromTheLedger(t *testing.T) {
	books := &fakeBookLister{files: map[int64][]models.BookFile{}}
	for i := int64(1); i <= 5; i++ {
		path := "/l/" + string(rune('a'+i)) + ".epub"
		books.books = append(books.books, models.Book{ID: i, Title: "B", Status: models.BookStatusImported, EbookFilePath: path})
		books.files[i] = []models.BookFile{ebook(i*10, i, path)}
	}
	s, ledger, _ := newTestSyncer(books)
	p := runPushAll(t, s)
	if p.Stats.Total != 5 || !p.Running || p.Stats.Processed != 0 {
		t.Fatalf("after queueing: %+v", p.Stats)
	}

	ledger.set(10, func(r *models.CalibreDelivery) {
		r.State, r.Outcome = models.CalibreDeliveryDelivered, DeliveryOutcomeAdded
	})
	ledger.set(20, func(r *models.CalibreDelivery) {
		r.State, r.Outcome = models.CalibreDeliveryDelivered, DeliveryOutcomeAlready
	})
	ledger.set(30, func(r *models.CalibreDelivery) {
		r.State, r.LastError, r.LastErrorCode = models.CalibreDeliveryFailed, "", "path_forbidden"
	})
	ledger.set(40, func(r *models.CalibreDelivery) {
		r.State, r.Outcome = models.CalibreDeliverySkipped, "file missing on disk"
	})
	ledger.remove(50)

	p, err := s.Progress(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := SyncStats{Total: 4, Processed: 4, Pushed: 1, AlreadyInCalibre: 1, Failed: 2, Skipped: 1}
	if p.Stats != want {
		t.Errorf("stats = %+v, want %+v", p.Stats, want)
	}
	reasons := map[int64]string{}
	for _, e := range p.Errors {
		reasons[e.BookID] = e.Reason
	}
	if reasons[3] != "path_forbidden" || reasons[5] != syncRemovedReason {
		t.Errorf("errors = %+v, want the code for book 3 and the removal for book 5", p.Errors)
	}
	if len(p.Skips) != 1 || p.Skips[0].BookID != 4 || p.Skips[0].Reason != "file missing on disk" {
		t.Errorf("skips = %+v", p.Skips)
	}
	if p.Running || p.FinishedAt == nil || p.Message != "done" {
		t.Errorf("running = %v, finishedAt = %v, message = %q, want a finished run", p.Running, p.FinishedAt, p.Message)
	}

	// Retry failed puts book 3 back in the queue: the run is running again.
	ledger.set(30, func(r *models.CalibreDelivery) { r.State = models.CalibreDeliveryPending })
	if p, _ = s.Progress(context.Background()); !p.Running || p.FinishedAt != nil || p.Queueing {
		t.Errorf("after a retry: running = %v, finishedAt = %v, queueing = %v", p.Running, p.FinishedAt, p.Queueing)
	}
}

// TestSyncer_QueuesEveryFormatInPreferenceOrder: every untracked ebook file
// is queued, EPUB first so it makes the Calibre record, then KEPUB, AZW3,
// MOBI, PDF and the rest by extension. A file the ledger holds in any state
// is left alone.
func TestSyncer_QueuesEveryFormatInPreferenceOrder(t *testing.T) {
	books := &fakeBookLister{
		books: []models.Book{
			{ID: 1, Title: "Many", Status: models.BookStatusImported, EbookFilePath: "/l/m.pdf"},
			{ID: 2, Title: "Recorded", Status: models.BookStatusImported, EbookFilePath: "/l/r.epub"},
		},
		files: map[int64][]models.BookFile{
			1: {
				ebook(11, 1, "/l/m.pdf"), ebook(12, 1, "/l/m.mobi"), ebook(13, 1, "/l/m.epub"),
				ebook(14, 1, "/l/m.cbz"), ebook(15, 1, "/l/m.azw3"), ebook(16, 1, "/l/m.kepub.epub"),
				ebook(17, 1, "/l/m.azw"),
			},
			2: {ebook(21, 2, "/l/r.epub"), ebook(22, 2, "/l/r.pdf"), ebook(23, 2, "/l/r.mobi"), ebook(24, 2, "/l/r.azw3")},
		},
	}
	s, ledger, queue := newTestSyncer(books)
	ledger.add(models.CalibreDelivery{BookID: 2, BookFileID: 21, State: models.CalibreDeliveryPending})
	ledger.add(models.CalibreDelivery{BookID: 2, BookFileID: 22, State: models.CalibreDeliverySkipped, Outcome: DeliverySkipNeedsAddFormat})
	ledger.add(models.CalibreDelivery{BookID: 2, BookFileID: 23, State: models.CalibreDeliveryFailed})

	runPushAll(t, s)

	enqueued, _ := queue.calls()
	want := []int64{13, 16, 15, 12, 11, 17, 14, 24}
	if len(enqueued) != len(want) {
		t.Fatalf("enqueued = %v, want %v", enqueued, want)
	}
	for i := range want {
		if enqueued[i] != want[i] {
			t.Fatalf("enqueued = %v, want %v", enqueued, want)
		}
	}
}

// TestSyncer_SkippedBooksAreCountedAndExplained is discussion #1592. A book
// that the bulk push silently dropped appeared in no counter, no error row and
// no log line.
func TestSyncer_SkippedBooksAreCountedAndExplained(t *testing.T) {
	books := &fakeBookLister{
		all: []models.Book{
			{ID: 1, Title: "Pushable", Status: models.BookStatusImported, EbookFilePath: "/l/a.epub"},
			{ID: 2, Title: "Wanted", Status: models.BookStatusWanted},
			{ID: 3, Title: "Audio Only", Status: models.BookStatusImported, AudiobookFilePath: "/l/b/"},
			{ID: 4, Title: "Ghost", Status: models.BookStatusImported},
			{ID: 5, Title: "Unmonitored", Status: models.BookStatusImported, EbookFilePath: "/l/e.epub"},
			{ID: 6, Title: "Legacy", Status: models.BookStatusImported, FilePath: "/l/legacy.epub"},
		},
		books: []models.Book{
			{ID: 1, Title: "Pushable", Status: models.BookStatusImported, EbookFilePath: "/l/a.epub"},
			{ID: 3, Title: "Audio Only", Status: models.BookStatusImported, AudiobookFilePath: "/l/b/"},
			{ID: 4, Title: "Ghost", Status: models.BookStatusImported},
			{ID: 6, Title: "Legacy", Status: models.BookStatusImported, FilePath: "/l/legacy.epub"},
		},
		files: map[int64][]models.BookFile{
			1: {ebook(11, 1, "/l/a.epub")},
			3: {{ID: 31, BookID: 3, Format: models.MediaTypeAudiobook, Path: "/l/b/"}},
		},
	}
	s, _, _ := newTestSyncer(books)
	p := runPushAll(t, s)

	if p.Stats.Total != 1 {
		t.Errorf("Total = %d, want 1", p.Stats.Total)
	}
	if p.Stats.Skipped != 4 {
		t.Errorf("Skipped = %d, want 4", p.Stats.Skipped)
	}
	got := map[int64]string{}
	for _, s := range p.Skips {
		got[s.BookID] = s.Reason
	}
	if reason, ok := got[2]; ok {
		t.Errorf("wanted book listed as skipped with %q; a book with no file was never a candidate", reason)
	}
	want := map[int64]string{
		3: SkipReasonAudiobookOnly,
		4: SkipReasonNoFile,
		5: SkipReasonNotMonitored,
		6: SkipReasonUntracked,
	}
	for id, reason := range want {
		if got[id] != reason {
			t.Errorf("book %d skip reason = %q, want %q", id, got[id], reason)
		}
	}
}

// TestSyncer_ZeroCaseNamesTheReason replaces the generic "no imported books
// with files to push" with something the reporter in #1592 could act on.
func TestSyncer_ZeroCaseNamesTheReason(t *testing.T) {
	books := &fakeBookLister{
		all: []models.Book{
			{ID: 1, Title: "Wanted", Status: models.BookStatusWanted},
			{ID: 3, Title: "Audio Only", Status: models.BookStatusImported, AudiobookFilePath: "/l/b/"},
		},
		books: []models.Book{
			{ID: 3, Title: "Audio Only", Status: models.BookStatusImported, AudiobookFilePath: "/l/b/"},
		},
	}
	s, _, queue := newTestSyncer(books)
	p := runPushAll(t, s)

	if p.Stats.Total != 0 || p.Running {
		t.Fatalf("Total = %d, running = %v, want an empty finished run", p.Stats.Total, p.Running)
	}
	if !strings.Contains(p.Message, "1 "+SkipReasonAudiobookOnly) {
		t.Errorf("message = %q, want it to name the skip reason with its count", p.Message)
	}
	if _, kicks := queue.calls(); kicks != 0 {
		t.Errorf("kicks = %d, want none for an empty run", kicks)
	}
}

// TestSyncer_WantedBooksDoNotCrowdOutActionableSkips: the skip list is a 50
// row sample, and a large wanted list used to fill it before any actionable
// reason was reached.
func TestSyncer_WantedBooksDoNotCrowdOutActionableSkips(t *testing.T) {
	books := &fakeBookLister{}
	for i := int64(1); i <= 3*maxSyncErrors; i++ {
		books.all = append(books.all, models.Book{ID: i, Title: "Wanted", Status: models.BookStatusWanted})
	}
	books.all = append(books.all, models.Book{ID: 999, Title: "Unmonitored", Status: models.BookStatusImported, EbookFilePath: "/l/u.epub"})
	s, _, _ := newTestSyncer(books)
	p := runPushAll(t, s)

	if p.Stats.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1 (only the unmonitored book)", p.Stats.Skipped)
	}
	if len(p.Skips) != 1 || p.Skips[0].BookID != 999 || p.Skips[0].Reason != SkipReasonNotMonitored {
		t.Errorf("skips = %+v, want only book 999 as %q", p.Skips, SkipReasonNotMonitored)
	}
}

// TestSyncer_FailuresCappedButAllCounted: the error list is a sample, the
// count is not.
func TestSyncer_FailuresCappedButAllCounted(t *testing.T) {
	books := &fakeBookLister{files: map[int64][]models.BookFile{}}
	n := int64(maxSyncErrors + 20)
	for i := int64(1); i <= n; i++ {
		books.books = append(books.books, models.Book{ID: i, Title: "B", Status: models.BookStatusImported, EbookFilePath: "/l/x.epub"})
		books.files[i] = []models.BookFile{ebook(i, i, "/l/x.epub")}
	}
	s, ledger, _ := newTestSyncer(books)
	runPushAll(t, s)
	for i := int64(1); i <= n; i++ {
		ledger.set(i, func(r *models.CalibreDelivery) { r.State, r.LastError = models.CalibreDeliveryFailed, "boom" })
	}
	p, err := s.Progress(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p.Stats.Failed != int(n) || len(p.Errors) != maxSyncErrors {
		t.Errorf("failed = %d, errors = %d, want %d and %d", p.Stats.Failed, len(p.Errors), n, maxSyncErrors)
	}
}

func TestSyncer_Start_RejectsNonPluginMode(t *testing.T) {
	s, _, _ := newTestSyncer(&fakeBookLister{})
	for _, mode := range []Mode{ModeOff, ModeCalibredb} {
		if err := s.Start(mode); !errors.Is(err, ErrSyncModeNotPlugin) {
			t.Errorf("mode %q: err = %v, want ErrSyncModeNotPlugin", mode, err)
		}
	}
}

// TestSyncer_Start_RejectsConcurrentQueueing: only the queueing phase is
// exclusive. Once it is done a new run may start while rows still wait.
func TestSyncer_Start_RejectsConcurrentQueueing(t *testing.T) {
	gate := make(chan struct{})
	books := &fakeBookLister{
		gate:  gate,
		books: []models.Book{{ID: 1, Title: "Dune", Status: models.BookStatusImported, EbookFilePath: "/l/d.epub"}},
		files: map[int64][]models.BookFile{1: {ebook(11, 1, "/l/d.epub")}},
	}
	s, _, _ := newTestSyncer(books)
	if err := s.Start(ModePlugin); err != nil {
		t.Fatal(err)
	}
	p, err := s.Progress(context.Background())
	if err != nil || !p.Running || !p.Queueing {
		t.Fatalf("progress while queueing = %+v, %v; want running and queueing", p, err)
	}
	if err := s.Start(ModePlugin); !errors.Is(err, ErrSyncAlreadyRunning) {
		t.Fatalf("second start while queueing: err = %v, want ErrSyncAlreadyRunning", err)
	}
	close(gate)
	books.gate = nil
	waitUntil(t, 2*time.Second, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return !s.run.queueing
	})
	if err := s.Start(ModePlugin); err != nil {
		t.Fatalf("start after queueing finished: %v", err)
	}
}

// TestSyncer_EndToEndThroughTheWorker runs Push all against the real ledger
// with the real worker as its queue: the worker, not the syncer, adds the
// book, and the status reads the result back.
func TestSyncer_EndToEndThroughTheWorker(t *testing.T) {
	bridge := &fakeBridge{add: added(501)}
	f := newWorkerFixture(t, ModePlugin, bridge)
	path := filepath.Join(t.TempDir(), "dune.epub")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.books.AddBookFile(f.ctx, f.book.ID, models.MediaTypeEbook, path); err != nil {
		t.Fatal(err)
	}
	s := NewSyncer(f.books, f.repo, f.d)

	if err := s.Start(ModePlugin); err != nil {
		t.Fatal(err)
	}
	var p SyncProgress
	waitUntil(t, 5*time.Second, func() bool {
		var err error
		p, err = s.Progress(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		return !p.Running && p.FinishedAt != nil
	})
	if p.Stats.Pushed != 1 || p.Stats.Total != 1 {
		t.Fatalf("stats = %+v, want the book pushed", p.Stats)
	}
	if bridge.addCount() != 1 {
		t.Errorf("adds = %d, want 1", bridge.addCount())
	}

	// A second run finds the book delivered and queues nothing.
	if err := s.Start(ModePlugin); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 5*time.Second, func() bool {
		var err error
		p, err = s.Progress(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		return !p.Running && p.FinishedAt != nil
	})
	if p.Stats.AlreadyInCalibre != 1 || p.Stats.Pushed != 0 {
		t.Errorf("second run stats = %+v, want the book already in Calibre", p.Stats)
	}
	rows, err := f.repo.ListAll(f.ctx)
	if err != nil || len(rows) != 1 {
		t.Errorf("ledger rows = %d, %v; want 1", len(rows), err)
	}
	if bridge.addCount() != 1 {
		t.Errorf("adds after the second run = %d, want still 1", bridge.addCount())
	}
}

func int64Ptr(v int64) *int64 { return &v }

// A format the run added to an existing record was pushed by the run, not
// already in Calibre.
func TestSyncer_FormatAddedCountsAsPushed(t *testing.T) {
	books := &fakeBookLister{
		books: []models.Book{{ID: 1, Title: "Dune", Status: models.BookStatusImported, EbookFilePath: "/l/d.epub"}},
		files: map[int64][]models.BookFile{1: {ebook(11, 1, "/l/d.epub"), ebook(12, 1, "/l/d.pdf")}},
	}
	s, ledger, _ := newTestSyncer(books)
	runPushAll(t, s)
	ledger.set(11, func(r *models.CalibreDelivery) {
		r.State, r.Outcome = models.CalibreDeliveryDelivered, DeliveryOutcomeAdded
	})
	ledger.set(12, func(r *models.CalibreDelivery) {
		r.State, r.Outcome = models.CalibreDeliveryDelivered, DeliveryOutcomeFormatAdded
	})
	p, err := s.Progress(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p.Stats.Pushed != 2 || p.Stats.AlreadyInCalibre != 0 {
		t.Errorf("stats = %+v, want both files pushed", p.Stats)
	}
}
