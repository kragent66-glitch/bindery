package calibre

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/vavallee/bindery/internal/models"
)

// formatAddedAt answers a push that asked for addFormat the way
// calibre-bridge 0.7.0 does when the row lacks the format.
func formatAddedAt(id int64) func(string) (AddResult, error) {
	return func(string) (AddResult, error) { return AddResult{ID: id, FormatAdded: true}, nil }
}

func (f *fakeBridge) sentAddFormat() []bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]bool(nil), f.addFormats...)
}

func (f *fakeBridge) pushed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.adds))
	for i, p := range f.adds {
		out[i] = filepath.Base(p)
	}
	return out
}

// A second format of a book Calibre already has is sent with addFormat and
// recorded against the same Calibre record.
func TestDeliverer_SecondFormatJoinsTheSameRecord(t *testing.T) {
	bridge := &fakeBridge{add: added(7), addWith: formatAddedAt(7), supportsAddFormat: true, health: HealthState{Library: "/lib"}}
	f := newWorkerFixture(t, ModePlugin, bridge)
	epub := f.addFile(t, "Dune.epub")
	pdf := f.addFile(t, "Dune.pdf")

	f.d.RunDeliveries(f.ctx)

	if got := bridge.sentAddFormat(); len(got) != 2 || got[0] || !got[1] {
		t.Fatalf("addFormat per push = %v, want [false true]: only the second format asks to join", got)
	}
	first, second := f.row(t, epub.ID), f.row(t, pdf.ID)
	if first.State != models.CalibreDeliveryDelivered || first.Outcome != DeliveryOutcomeAdded {
		t.Errorf("epub row = %s/%s, want delivered/added", first.State, first.Outcome)
	}
	if second.State != models.CalibreDeliveryDelivered || second.Outcome != DeliveryOutcomeFormatAdded {
		t.Errorf("pdf row = %s/%s, want delivered/%s", second.State, second.Outcome, DeliveryOutcomeFormatAdded)
	}
	if second.CalibreID == nil || *second.CalibreID != 7 || first.CalibreID == nil || *first.CalibreID != 7 {
		t.Errorf("calibre ids = %v and %v, want both 7", first.CalibreID, second.CalibreID)
	}
}

// The same format a second time, or a match on a rung other than the bindery
// identifier, is a 409 even with addFormat. That is not a failure.
func TestDeliverer_SecondFormatConflictIsAlready(t *testing.T) {
	bridge := &fakeBridge{
		add:               added(7),
		addWith:           func(string) (AddResult, error) { return AddResult{ID: 7}, ErrAlreadyInCalibre },
		supportsAddFormat: true,
	}
	f := newWorkerFixture(t, ModePlugin, bridge)
	f.addFile(t, "Dune.epub")
	pdf := f.addFile(t, "Dune.pdf")

	f.d.RunDeliveries(f.ctx)

	got := f.row(t, pdf.ID)
	if got.State != models.CalibreDeliveryDelivered || got.Outcome != DeliveryOutcomeAlready || got.CalibreID == nil || *got.CalibreID != 7 {
		t.Errorf("pdf row = %s/%s id %v, want delivered/already id 7", got.State, got.Outcome, got.CalibreID)
	}
}

// Without add_format the second format is held back with a reason the
// operator can act on, instead of being sent to come back 409, or in
// calibredb mode to make a second record.
func TestDeliverer_SecondFormatWithoutTheCapabilityIsSkipped(t *testing.T) {
	cases := map[string]struct {
		mode  Mode
		adder func() (Adder, func() int)
	}{
		"old bridge": {mode: ModePlugin, adder: func() (Adder, func() int) {
			b := &fakeBridge{add: added(7)}
			return b, b.addCount
		}},
		"calibredb": {mode: ModeCalibredb, adder: func() (Adder, func() int) {
			c := &fakeCalibredb{add: added(7)}
			return c, func() int { return len(c.adds) }
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			adder, count := tc.adder()
			f := newWorkerFixture(t, tc.mode, adder)
			epub := f.addFile(t, "Dune.epub")
			pdf := f.addFile(t, "Dune.pdf")

			f.d.RunDeliveries(f.ctx)

			if n := count(); n != 1 {
				t.Errorf("adds = %d, want only the first format sent", n)
			}
			if got := f.row(t, epub.ID); got.State != models.CalibreDeliveryDelivered {
				t.Errorf("epub row = %s, want delivered", got.State)
			}
			got := f.row(t, pdf.ID)
			if got.State != models.CalibreDeliverySkipped || got.Outcome != DeliverySkipNeedsAddFormat {
				t.Errorf("pdf row = %s/%q, want skipped/%q", got.State, got.Outcome, DeliverySkipNeedsAddFormat)
			}
		})
	}
}

// A book with one file is not a second format of anything, whatever the
// bridge can do.
func TestDeliverer_SingleFileNeverAsksToAddAFormat(t *testing.T) {
	bridge := &fakeBridge{add: added(7), supportsAddFormat: true}
	f := newWorkerFixture(t, ModePlugin, bridge)
	row := f.addFile(t, "Dune.pdf")
	f.d.RunDeliveries(f.ctx)
	if got := bridge.sentAddFormat(); len(got) != 1 || got[0] {
		t.Fatalf("addFormat per push = %v, want one push without it", got)
	}
	if got := f.row(t, row.ID); got.Outcome != DeliveryOutcomeAdded {
		t.Errorf("outcome = %q, want added", got.Outcome)
	}
}

// Updating the plugin re-arms the formats held back for it, with nothing
// else in the queue to wake the worker, and delivers them to the record.
func TestDeliverer_RearmsHeldFormatsOnceTheBridgeCanAddThem(t *testing.T) {
	bridge := &fakeBridge{add: added(7), addWith: formatAddedAt(7)}
	f := newWorkerFixture(t, ModePlugin, bridge)
	f.addFile(t, "Dune.epub")
	pdf := f.addFile(t, "Dune.pdf")
	f.d.RunDeliveries(f.ctx)
	if got := f.row(t, pdf.ID); got.State != models.CalibreDeliverySkipped {
		t.Fatalf("pdf row = %s, want skipped before the upgrade", got.State)
	}

	// Another skip, for a reason an upgrade does not fix, must stay put.
	other := f.addFile(t, "Dune.mobi")
	if err := f.repo.MarkSkipped(f.ctx, other.ID, deliverySkipNoFile); err != nil {
		t.Fatal(err)
	}

	bridge.mu.Lock()
	bridge.supportsAddFormat = true
	bridge.mu.Unlock()
	f.offset = time.Minute
	f.d.RunDeliveries(f.ctx)

	got := f.row(t, pdf.ID)
	if got.State != models.CalibreDeliveryDelivered || got.Outcome != DeliveryOutcomeFormatAdded || got.CalibreID == nil || *got.CalibreID != 7 {
		t.Errorf("pdf row after the upgrade = %s/%s id %v, want delivered/format_added id 7", got.State, got.Outcome, got.CalibreID)
	}
	if got := f.row(t, other.ID); got.State != models.CalibreDeliverySkipped || got.Outcome != deliverySkipNoFile {
		t.Errorf("unrelated skip = %s/%q, want it left alone", got.State, got.Outcome)
	}
}

// An idle queue asks the bridge about add_format only now and then, and not
// at all when nothing is waiting on it.
func TestDeliverer_IdleRearmProbeIsThrottled(t *testing.T) {
	bridge := &fakeBridge{add: added(7)}
	f := newWorkerFixture(t, ModePlugin, bridge)
	f.addFile(t, "Dune.epub")
	f.addFile(t, "Dune.pdf")
	f.d.RunDeliveries(f.ctx) // delivers the epub, skips the pdf
	base := bridge.probes

	f.offset = time.Minute
	f.d.RunDeliveries(f.ctx)
	if bridge.probes != base+1 {
		t.Fatalf("probes = %d, want one idle probe for the held format", bridge.probes-base)
	}
	f.offset = 2 * time.Minute
	f.d.RunDeliveries(f.ctx)
	if bridge.probes != base+1 {
		t.Errorf("probes = %d, want no second idle probe inside the interval", bridge.probes-base)
	}
	f.offset = deliveryIdleRearmInterval + 2*time.Minute
	f.d.RunDeliveries(f.ctx)
	if bridge.probes != base+2 {
		t.Errorf("probes = %d, want another idle probe once the interval passed", bridge.probes-base)
	}
}

// The preferred format goes first whatever order the files were queued in,
// so the EPUB makes the Calibre record and the PDF joins it.
func TestDeliverer_DeliversThePreferredFormatFirst(t *testing.T) {
	bridge := &fakeBridge{add: added(7), addWith: formatAddedAt(7), supportsAddFormat: true}
	f := newWorkerFixture(t, ModePlugin, bridge)
	f.addFile(t, "Dune.pdf")
	f.addFile(t, "Dune.mobi")
	epub := f.addFile(t, "Dune.epub")

	f.d.RunDeliveries(f.ctx)

	want := []string{"Dune.epub", "Dune.mobi", "Dune.pdf"}
	got := bridge.pushed()
	if len(got) != len(want) {
		t.Fatalf("pushed %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("pushed %v, want %v", got, want)
		}
	}
	if r := f.row(t, epub.ID); r.Outcome != DeliveryOutcomeAdded {
		t.Errorf("epub outcome = %q, want added: it should have made the record", r.Outcome)
	}
}

func TestOrderDeliveries_GroupsByBookInPreferenceOrder(t *testing.T) {
	rows := []models.CalibreDelivery{
		{ID: 1, BookID: 10, FilePath: "/a/x.pdf"},
		{ID: 2, BookID: 20, FilePath: "/b/y.mobi"},
		{ID: 3, BookID: 10, FilePath: "/a/x.cbz"},
		{ID: 4, BookID: 10, FilePath: "/a/x.azw3"},
		{ID: 5, BookID: 20, FilePath: "/b/y.kepub.epub"},
		{ID: 6, BookID: 10, FilePath: "/a/x.epub"},
		{ID: 7, BookID: 10, FilePath: "/a/x.azw"},
	}
	orderDeliveries(rows)
	want := []int64{6, 4, 1, 7, 3, 5, 2}
	for i, r := range rows {
		if r.ID != want[i] {
			got := make([]int64, len(rows))
			for j := range rows {
				got[j] = rows[j].ID
			}
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}
