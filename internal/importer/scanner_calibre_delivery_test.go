package importer

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/vavallee/bindery/internal/calibre"
	"github.com/vavallee/bindery/internal/db"
	"github.com/vavallee/bindery/internal/jobs"
	"github.com/vavallee/bindery/internal/models"
)

// gatedAdder blocks every Add until the test opens the gate, so a test can
// tell an import that waits on Calibre from one that does not.
type gatedAdder struct {
	gate  chan struct{}
	mu    sync.Mutex
	paths []string
}

func (g *gatedAdder) Add(_ context.Context, path string, _ calibre.Metadata) (int64, error) {
	<-g.gate
	g.mu.Lock()
	defer g.mu.Unlock()
	g.paths = append(g.paths, path)
	return int64(len(g.paths)), nil
}

func (g *gatedAdder) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.paths)
}

// countingQueue records what the scanner asks of the delivery queue.
type countingQueue struct {
	enqueued []string
	kicks    int
}

func (q *countingQueue) Enqueue(_ context.Context, _, _ int64, _ *int64, path string) (bool, error) {
	q.enqueued = append(q.enqueued, path)
	return true, nil
}

func (q *countingQueue) Kick() { q.kicks++ }

type deliveryImportFixture struct {
	ctx        context.Context
	scanner    *Scanner
	books      *db.BookRepo
	deliveries *db.CalibreDeliveryRepo
	book       *models.Book
	dl         *models.Download
}

func newDeliveryImportFixture(t *testing.T) *deliveryImportFixture {
	t.Helper()
	database, err := db.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	f := &deliveryImportFixture{
		ctx:        context.Background(),
		books:      db.NewBookRepo(database),
		deliveries: db.NewCalibreDeliveryRepo(database),
	}
	authors := db.NewAuthorRepo(database)
	a := &models.Author{Name: "Frank Herbert", ForeignID: "OLA1", SortName: "Herbert, Frank", Monitored: true}
	if err := authors.Create(f.ctx, a); err != nil {
		t.Fatal(err)
	}
	f.book = &models.Book{AuthorID: a.ID, Title: "Dune", ForeignID: "OLB1", Status: models.BookStatusWanted, Monitored: true, AnyEditionOK: true}
	if err := f.books.Create(f.ctx, f.book); err != nil {
		t.Fatal(err)
	}
	f.scanner = NewScanner(db.NewDownloadRepo(database), db.NewDownloadClientRepo(database),
		f.books, authors, db.NewHistoryRepo(database), t.TempDir(), "", "", "", "")
	f.dl = &models.Download{GUID: "dune", BookID: &f.book.ID, Title: "Dune", Status: models.StateCompleted}
	return f
}

func (f *deliveryImportFixture) queued(t *testing.T) []models.CalibreDelivery {
	t.Helper()
	rows, err := f.deliveries.List(f.ctx, "", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func writeFiles(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// An ebook import queues exactly one delivery per imported ebook file and
// returns without waiting on Calibre. Before #2832 the import called the
// adder inline, once per file, so a Calibre that did not answer held the
// import up and nothing was queued.
func TestImport_QueuesOneDeliveryPerEbookFileWithoutWaitingOnCalibre(t *testing.T) {
	f := newDeliveryImportFixture(t)
	adder := &gatedAdder{gate: make(chan struct{})}
	group := jobs.NewGroup(context.Background())
	deliverer := calibre.NewDeliverer(f.deliveries, f.books,
		func() calibre.Mode { return calibre.ModeCalibredb },
		func() calibre.Config { return calibre.Config{} },
		func(calibre.Mode) calibre.Adder { return adder }).
		WithJobs(group)
	f.scanner.WithCalibreDeliveries(func() calibre.Mode { return calibre.ModeCalibredb }, deliverer)

	src := writeFiles(t, "Dune.epub", "Dune.MOBI")
	done := make(chan struct{})
	go func() {
		f.scanner.ImportFromPath(f.ctx, f.dl, src, "")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		close(adder.gate)
		t.Fatal("the import waited on Calibre")
	}

	rows := f.queued(t)
	if len(rows) != 2 {
		t.Fatalf("queued rows = %d (%+v), want one per ebook file", len(rows), rows)
	}
	files, err := f.books.ListFiles(f.ctx, f.book.ID)
	if err != nil {
		t.Fatal(err)
	}
	byFile := map[int64]models.BookFile{}
	for _, bf := range files {
		byFile[bf.ID] = bf
	}
	formats := map[string]bool{}
	for _, r := range rows {
		bf, ok := byFile[r.BookFileID]
		if !ok || bf.Path != r.FilePath || r.BookID != f.book.ID {
			t.Errorf("row %+v does not point at an imported file of the book", r)
		}
		formats[r.Format] = true
	}
	if !formats["epub"] || !formats["mobi"] {
		t.Errorf("formats = %v, want epub and mobi in lower case", formats)
	}

	// The kick delivers them in the background once Calibre answers.
	close(adder.gate)
	deadline := time.Now().Add(10 * time.Second)
	for adder.count() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if stuck := group.Shutdown(10 * time.Second); len(stuck) != 0 {
		t.Fatalf("delivery pass still running: %v", stuck)
	}
	if adder.count() != 2 {
		t.Errorf("adds after the gate opened = %d, want 2", adder.count())
	}
	for _, r := range f.queued(t) {
		if r.State != models.CalibreDeliveryDelivered {
			t.Errorf("row %d state = %s, want delivered", r.ID, r.State)
		}
	}
}

// An audiobook import queues nothing: the Calibre hand off takes one ebook
// file.
func TestImport_AudiobookQueuesNothing(t *testing.T) {
	f := newDeliveryImportFixture(t)
	q := &countingQueue{}
	f.scanner.WithCalibreDeliveries(func() calibre.Mode { return calibre.ModePlugin }, q)

	src := writeFiles(t, "Dune.m4b")
	f.scanner.ImportFromPath(f.ctx, f.dl, src, models.MediaTypeAudiobook)

	got, _ := f.books.GetByID(f.ctx, f.book.ID)
	if got == nil || got.AudiobookFilePath == "" {
		t.Fatalf("the audiobook was not imported: %+v", got)
	}
	if len(q.enqueued) != 0 || q.kicks != 0 {
		t.Errorf("enqueued %v with %d kicks, want nothing for an audiobook", q.enqueued, q.kicks)
	}
}

// With the integration off an import queues nothing and kicks nothing.
func TestImport_CalibreOffQueuesNothing(t *testing.T) {
	f := newDeliveryImportFixture(t)
	q := &countingQueue{}
	f.scanner.WithCalibreDeliveries(func() calibre.Mode { return calibre.ModeOff }, q)

	f.scanner.ImportFromPath(f.ctx, f.dl, writeFiles(t, "Dune.epub"), "")

	if len(q.enqueued) != 0 || q.kicks != 0 {
		t.Errorf("enqueued %v with %d kicks, want nothing with Calibre off", q.enqueued, q.kicks)
	}
}

// A multi file download kicks the worker once, after every file is queued.
func TestImport_KicksOnceAfterQueueingEveryFile(t *testing.T) {
	f := newDeliveryImportFixture(t)
	q := &countingQueue{}
	f.scanner.WithCalibreDeliveries(func() calibre.Mode { return calibre.ModePlugin }, q)

	f.scanner.ImportFromPath(f.ctx, f.dl, writeFiles(t, "Dune.epub", "Dune.azw3"), "")

	if len(q.enqueued) != 2 || q.kicks != 1 {
		t.Errorf("enqueued %v with %d kicks, want two files and one kick", q.enqueued, q.kicks)
	}
}
