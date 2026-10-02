package calibre

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/vavallee/bindery/internal/models"
)

// pullFixture is a worker fixture in plugin mode with the pull transport.
func newPullFixture(t *testing.T) (*workerFixture, *fakeBridge) {
	t.Helper()
	bridge := &fakeBridge{add: added(1), supportsAddFormat: true, health: HealthState{Library: "/lib"}}
	f := newWorkerFixture(t, ModePlugin, bridge)
	transport := TransportPull
	f.d.WithTransport(func() Transport { return transport })
	return f, bridge
}

// addBook creates another book of the fixture's author and queues one file
// per name for it, in the order given.
func (f *workerFixture) addBook(t *testing.T, title string, names ...string) (*models.Book, []models.CalibreDelivery) {
	t.Helper()
	b := &models.Book{
		ForeignID: "OL" + title, AuthorID: f.book.AuthorID, Title: title, SortTitle: title,
		Status: models.BookStatusWanted, Monitored: true, AnyEditionOK: true, MetadataProvider: "openlibrary",
	}
	if err := f.books.Create(f.ctx, b); err != nil {
		t.Fatal(err)
	}
	var rows []models.CalibreDelivery
	dir := t.TempDir()
	for _, name := range names {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("content of "+name), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := f.books.AddBookFile(f.ctx, b.ID, models.MediaTypeEbook, path); err != nil {
			t.Fatal(err)
		}
		files, err := f.books.ListFiles(f.ctx, b.ID)
		if err != nil {
			t.Fatal(err)
		}
		var fileID int64
		for _, bf := range files {
			if bf.Path == path {
				fileID = bf.ID
			}
		}
		if _, err := f.d.Enqueue(f.ctx, b.ID, fileID, nil, path); err != nil {
			t.Fatal(err)
		}
		row, err := f.repo.GetByBookFile(f.ctx, fileID)
		if err != nil || row == nil {
			t.Fatalf("queued row: %v, %v", row, err)
		}
		rows = append(rows, *row)
	}
	return b, rows
}

// The push worker must stand down in pull: no probe, no add, rows untouched.
func TestDeliverer_PullTransportIdlesThePushWorker(t *testing.T) {
	f, bridge := newPullFixture(t)
	row := f.addFile(t, "a.epub")

	f.d.RunDeliveries(f.ctx)

	if bridge.probes != 0 || bridge.addCount() != 0 {
		t.Fatalf("pull transport: probes=%d adds=%d, want the pass to do nothing", bridge.probes, bridge.addCount())
	}
	if got := f.row(t, row.ID); got.State != models.CalibreDeliveryPending || got.Attempts != 0 {
		t.Fatalf("row = %+v, want it left pending", got)
	}
	if f.d.Health().LastPassAt != nil {
		t.Fatal("an idle pull pass should not even record a pass")
	}
}

// Pull only means something in plugin mode; calibredb keeps delivering even
// if the transport setting says pull.
func TestDeliverer_PullTransportDoesNotStopCalibredb(t *testing.T) {
	cdb := &fakeCalibredb{add: added(9)}
	f := newWorkerFixture(t, ModeCalibredb, cdb)
	f.d.WithTransport(func() Transport { return TransportPull })
	row := f.addFile(t, "a.epub")

	f.d.RunDeliveries(f.ctx)

	if got := f.row(t, row.ID); got.State != models.CalibreDeliveryDelivered {
		t.Fatalf("calibredb row = %+v, want delivered", got)
	}
}

func pullIDs(items []PullItem) []int64 {
	out := make([]int64, 0, len(items))
	for _, it := range items {
		out = append(out, it.ID)
	}
	return out
}

func TestPullList_OrdersByFormatPreferenceAndWithholdsSiblings(t *testing.T) {
	f, _ := newPullFixture(t)
	// Enqueued mobi first: the listing must still offer the epub first.
	_, rows := f.addBook(t, "Emma", "emma.mobi", "emma.epub")
	mobi, epub := rows[0], rows[1]

	page, err := f.d.PullList(f.ctx, PullCaps{AddFormat: true}, "", 20)
	if err != nil {
		t.Fatal(err)
	}
	if got := pullIDs(page.Deliveries); !reflect.DeepEqual(got, []int64{epub.ID}) {
		t.Fatalf("listed %v, want only the epub %d (the mobi waits for it)", got, epub.ID)
	}
	it := page.Deliveries[0]
	if it.Action != PullActionAdd || it.Format != "epub" || it.SizeBytes != int64(len("content of emma.epub")) {
		t.Fatalf("item = %+v, want add/epub with the file size", it)
	}
	if it.Metadata.Title != "Emma" || it.Metadata.Identifiers["bindery"] != fmt.Sprint(it.BookID) {
		t.Fatalf("metadata = %+v, want the push payload with a bindery identifier", it.Metadata)
	}
	if it.Metadata.CoverPath != "" {
		t.Fatal("pull must not leak a server cover path into the metadata")
	}
	if page.Pending != 2 || page.NextCursor != "" {
		t.Fatalf("pending=%d cursor=%q, want 2 and no next page", page.Pending, page.NextCursor)
	}

	// Once the epub is acknowledged, the mobi is offered as a new format.
	if err := f.d.PullAck(f.ctx, epub.ID, PullAckRequest{CalibreID: 40, Outcome: DeliveryOutcomeAdded, Library: "C:/Calibre"}); err != nil {
		t.Fatal(err)
	}
	page, err = f.d.PullList(f.ctx, PullCaps{AddFormat: true}, "", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Deliveries) != 1 || page.Deliveries[0].ID != mobi.ID || page.Deliveries[0].Action != PullActionAddFormat {
		t.Fatalf("after ack: %+v, want the mobi as add_format", page.Deliveries)
	}
}

// Without add_format a second format is skipped with the push worker's
// reason, and comes back once the plugin advertises the capability.
func TestPullList_SecondFormatNeedsAddFormat(t *testing.T) {
	f, _ := newPullFixture(t)
	_, rows := f.addBook(t, "Emma", "emma.epub", "emma.mobi")
	if err := f.d.PullAck(f.ctx, rows[0].ID, PullAckRequest{CalibreID: 40, Outcome: DeliveryOutcomeAdded, Library: "lib"}); err != nil {
		t.Fatal(err)
	}

	page, err := f.d.PullList(f.ctx, PullCaps{}, "", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Deliveries) != 0 {
		t.Fatalf("listed %+v to a plugin without add_format", page.Deliveries)
	}
	got := f.row(t, rows[1].ID)
	if got.State != models.CalibreDeliverySkipped || got.Outcome != DeliverySkipNeedsAddFormat {
		t.Fatalf("mobi row = %+v, want skipped with the add_format reason", got)
	}

	page, err = f.d.PullList(f.ctx, PullCaps{AddFormat: true}, "", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Deliveries) != 1 || page.Deliveries[0].ID != rows[1].ID || page.Deliveries[0].Action != PullActionAddFormat {
		t.Fatalf("with add_format: %+v, want the re-armed mobi as add_format", page.Deliveries)
	}
}

func TestPullList_CursorPagesWholeBooks(t *testing.T) {
	f, _ := newPullFixture(t)
	var want []int64
	for i := 0; i < 5; i++ {
		_, rows := f.addBook(t, fmt.Sprintf("Book%d", i), "b.epub")
		want = append(want, rows[0].ID)
	}

	var got []int64
	cursor := ""
	pages := 0
	for {
		page, err := f.d.PullList(f.ctx, PullCaps{}, cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		if len(page.Deliveries) > 2 {
			t.Fatalf("page of %d items, limit 2", len(page.Deliveries))
		}
		got = append(got, pullIDs(page.Deliveries)...)
		if page.Pending != 5 {
			t.Fatalf("pending = %d, want 5", page.Pending)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
		if pages > 10 {
			t.Fatal("cursor never ran out")
		}
	}
	if pages != 3 || !reflect.DeepEqual(got, want) {
		t.Fatalf("pages=%d ids=%v, want 3 pages listing %v", pages, got, want)
	}

	// Acknowledging between pages must not skip anything: the cursor is a
	// position in book ids, not an offset.
	first, err := f.d.PullList(f.ctx, PullCaps{}, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range first.Deliveries {
		if err := f.d.PullAck(f.ctx, it.ID, PullAckRequest{CalibreID: it.ID + 100, Outcome: DeliveryOutcomeAdded}); err != nil {
			t.Fatal(err)
		}
	}
	second, err := f.d.PullList(f.ctx, PullCaps{}, first.NextCursor, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := pullIDs(second.Deliveries); !reflect.DeepEqual(got, want[2:4]) {
		t.Fatalf("second page after acks = %v, want %v", got, want[2:4])
	}

	if _, err := f.d.PullList(f.ctx, PullCaps{}, "nope", 2); !errors.Is(err, ErrPullInvalid) {
		t.Fatalf("bad cursor: %v, want ErrPullInvalid", err)
	}
}

func TestPullList_OnlyDueRows(t *testing.T) {
	f, _ := newPullFixture(t)
	_, rows := f.addBook(t, "Emma", "emma.epub")
	if err := f.d.PullNack(f.ctx, rows[0].ID, PullNackRequest{Code: "calibre_busy", Retryable: true}); err != nil {
		t.Fatal(err)
	}
	page, err := f.d.PullList(f.ctx, PullCaps{}, "", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Deliveries) != 0 || page.Pending != 0 {
		t.Fatalf("backing off row listed: %+v", page)
	}
}

func TestPullAck_IdempotentAndCalibreIDRule(t *testing.T) {
	f, _ := newPullFixture(t)
	row := f.addFile(t, "a.epub")

	ack := PullAckRequest{CalibreID: 101, Outcome: DeliveryOutcomeAdded, Library: "C:/Users/me/Calibre Library"}
	if err := f.d.PullAck(f.ctx, row.ID, ack); err != nil {
		t.Fatal(err)
	}
	got := f.row(t, row.ID)
	if got.State != models.CalibreDeliveryDelivered || got.CalibreID == nil || *got.CalibreID != 101 ||
		got.TargetLibrary != ack.Library || got.Outcome != DeliveryOutcomeAdded {
		t.Fatalf("row = %+v, want delivered to 101 in the reported library", got)
	}
	// No calibre.library_path set, so the push worker's rule records the id.
	if id := f.calibreID(t); id == nil || *id != 101 {
		t.Fatalf("books.calibre_id = %v, want 101", id)
	}
	if c := f.d.PullContact(); c.Library != ack.Library {
		t.Fatalf("contact library = %q, want %q", c.Library, ack.Library)
	}

	// The same ack again is fine; a different id is a conflict.
	if err := f.d.PullAck(f.ctx, row.ID, ack); err != nil {
		t.Fatalf("repeated ack: %v", err)
	}
	if err := f.d.PullAck(f.ctx, row.ID, PullAckRequest{CalibreID: 102, Outcome: DeliveryOutcomeAdded}); !errors.Is(err, ErrPullNotPending) {
		t.Fatalf("ack with another id: %v, want ErrPullNotPending", err)
	}
	if err := f.d.PullAck(f.ctx, row.ID+999, ack); !errors.Is(err, ErrPullNotFound) {
		t.Fatalf("ack of a missing row: %v, want ErrPullNotFound", err)
	}
	for _, bad := range []PullAckRequest{{CalibreID: 1, Outcome: "linked"}, {CalibreID: 0, Outcome: DeliveryOutcomeAdded}} {
		if err := f.d.PullAck(f.ctx, row.ID, bad); !errors.Is(err, ErrPullInvalid) {
			t.Fatalf("ack %+v: %v, want ErrPullInvalid", bad, err)
		}
	}
}

// A library path pointing somewhere else keeps books.calibre_id untouched,
// the same as a push into a different library.
func TestPullAck_DifferentLibraryLeavesCalibreIDAlone(t *testing.T) {
	f, _ := newPullFixture(t)
	f.cfg.LibraryPath = "/source"
	row := f.addFile(t, "a.epub")
	if err := f.d.PullAck(f.ctx, row.ID, PullAckRequest{CalibreID: 7, Outcome: DeliveryOutcomeAlready, Library: "/elsewhere"}); err != nil {
		t.Fatal(err)
	}
	if got := f.row(t, row.ID); got.Outcome != DeliveryOutcomeAlready {
		t.Fatalf("row = %+v, want outcome already", got)
	}
	if id := f.calibreID(t); id != nil {
		t.Fatalf("books.calibre_id = %d, want NULL for another library", *id)
	}
}

func TestPullNack_TerminalAndBackoff(t *testing.T) {
	cases := []struct {
		name     string
		req      PullNackRequest
		terminal bool
	}{
		{"retryable backs off", PullNackRequest{Code: "calibre_busy", Error: "db locked", Retryable: true}, false},
		{"not retryable gives up", PullNackRequest{Code: "calibre_busy", Error: "nope", Retryable: false}, true},
		{"bad_format gives up even if retryable", PullNackRequest{Code: "bad_format", Retryable: true}, true},
		{"path_forbidden gives up even if retryable", PullNackRequest{Code: "path_forbidden", Retryable: true}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := newPullFixture(t)
			fixed := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
			f.d.now = func() time.Time { return fixed }
			row := f.addFile(t, "a.epub")
			if err := f.d.PullNack(f.ctx, row.ID, tc.req); err != nil {
				t.Fatal(err)
			}
			got := f.row(t, row.ID)
			if got.Attempts != 1 || got.LastErrorCode != tc.req.Code {
				t.Fatalf("row = %+v, want one attempt with code %q", got, tc.req.Code)
			}
			if tc.terminal {
				if got.State != models.CalibreDeliveryFailed {
					t.Fatalf("state = %s, want failed", got.State)
				}
				return
			}
			if got.State != models.CalibreDeliveryPending || !got.NextAttemptAt.Equal(fixed.Add(deliveryBackoff[0])) {
				t.Fatalf("row = %+v, want pending until %s", got, fixed.Add(deliveryBackoff[0]))
			}
		})
	}

	f, _ := newPullFixture(t)
	row := f.addFile(t, "a.epub")
	if err := f.d.PullNack(f.ctx, row.ID, PullNackRequest{Code: "no spaces allowed"}); !errors.Is(err, ErrPullInvalid) {
		t.Fatalf("bad code: %v, want ErrPullInvalid", err)
	}
	if err := f.d.PullNack(f.ctx, row.ID, PullNackRequest{Code: "x", Retryable: false}); err != nil {
		t.Fatal(err)
	}
	if err := f.d.PullNack(f.ctx, row.ID, PullNackRequest{Code: "x"}); !errors.Is(err, ErrPullNotPending) {
		t.Fatalf("nack of a failed row: %v, want ErrPullNotPending", err)
	}
}

func TestParseBridgeCapabilities(t *testing.T) {
	got := ParseBridgeCapabilities(" book_metadata, Cover,add_format,,cover, <script>, add format ")
	want := []string{"book_metadata", "cover", "add_format"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if c := PullCapsFrom(got); !c.AddFormat || !c.Cover {
		t.Fatalf("caps = %+v", c)
	}
	if v := CleanBridgeVersion(" 0.8.0\n\x00beta "); v != "0.8.0beta" {
		t.Fatalf("version = %q", v)
	}
}
