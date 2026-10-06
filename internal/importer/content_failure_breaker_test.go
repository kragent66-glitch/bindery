package importer

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vavallee/bindery/internal/downloader"
	"github.com/vavallee/bindery/internal/downloader/nzbget"
)

func TestContentFailureBreaker(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	b := &contentFailureBreaker{now: func() time.Time { return now }}

	// A single bad release, and the same release failing again, stay allowed.
	for i := 0; i < 3; i++ {
		if allow, _, _ := b.recordFailure(1, "guid-a", "FAILURE/PAR"); !allow {
			t.Fatalf("repeat failure %d of one release was refused", i)
		}
	}
	// Different kinds do not add up.
	if allow, _, _ := b.recordFailure(1, "guid-b", "FAILURE/HEALTH"); !allow {
		t.Fatal("a second release with a different status was refused")
	}
	// Other clients are independent.
	if allow, _, _ := b.recordFailure(2, "guid-c", "FAILURE/PAR"); !allow {
		t.Fatal("another client's failure was refused")
	}
	// The second distinct release with FAILURE/PAR is still allowed, the third trips.
	if allow, _, _ := b.recordFailure(1, "guid-d", "FAILURE/PAR"); !allow {
		t.Fatal("second distinct release refused")
	}
	allow, tripped, distinct := b.recordFailure(1, "guid-e", "FAILURE/PAR")
	if allow || !tripped || distinct != 3 {
		t.Fatalf("third distinct release: allow=%v tripped=%v distinct=%d, want false true 3", allow, tripped, distinct)
	}
	// Open: refused without tripping again, for any kind, even past the window.
	now = now.Add(5 * time.Hour)
	if allow, tripped, _ := b.recordFailure(1, "guid-f", "FAILURE/HEALTH"); allow || tripped {
		t.Fatalf("open breaker: allow=%v tripped=%v, want false false", allow, tripped)
	}
	// A completed download closes it.
	if !b.recordSuccess(1) {
		t.Fatal("recordSuccess did not report the open breaker")
	}
	if allow, _, _ := b.recordFailure(1, "guid-g", "FAILURE/PAR"); !allow {
		t.Fatal("failure after a success was refused")
	}

	// Failures further apart than the window do not add up.
	b2 := &contentFailureBreaker{now: func() time.Time { return now }}
	for i := 0; i < 5; i++ {
		if allow, _, _ := b2.recordFailure(1, fmt.Sprintf("guid-%d", i), "FAILURE/UNPACK"); !allow {
			t.Fatalf("failure %d, %s after the previous one, was refused", i, contentBreakerWindow)
		}
		now = now.Add(contentBreakerWindow + time.Minute)
	}
}

// TestCheckNZBGetDownloads_ContentFailureBreakerRecovers drives the breaker
// through the poll path: a storm trips it and reports it on the client's
// health, a completed download closes it and clears the report, and the next
// single bad release is blocklisted again.
func TestCheckNZBGetDownloads_ContentFailureBreakerRecovers(t *testing.T) {
	ctx := context.Background()
	var items []nzbget.HistoryItem
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nzbgetHandler(t, items, nil)(w, r)
	}))
	defer srv.Close()

	f := newContentFailureFixture(t)
	health := downloader.NewHealthStore()
	f.scanner.WithClientHealth(health)
	client := nzbgetClient(t, ctx, f.clients, srv.URL)

	for i := 1; i <= 3; i++ {
		items = append(items, nzbget.HistoryItem{NZBID: 200 + i, Status: "FAILURE/UNPACK"})
		f.addDownload(t, ctx, client, fmt.Sprintf("guid-r-%d", i), fmt.Sprint(200+i), nil)
	}
	f.scanner.checkNZBGetDownloads(ctx, client)
	if got := f.entries(t, ctx); len(got) != 2 {
		t.Fatalf("blocklist has %d entries after the storm, want 2", len(got))
	}
	h := health.Get(client.ID)
	if h == nil || h.Status != downloader.HealthError || !strings.Contains(h.Message, "Automatic blocklisting is paused") {
		t.Fatalf("client health after the storm = %+v, want the paused blocklisting advisory", h)
	}

	// The client completes a download: the breaker closes and the advisory goes.
	items = []nzbget.HistoryItem{{NZBID: 210, Status: "SUCCESS/ALL", DestDir: t.TempDir()}}
	f.addDownload(t, ctx, client, "guid-r-ok", "210", nil)
	f.scanner.checkNZBGetDownloads(ctx, client)
	if h := health.Get(client.ID); h != nil {
		t.Fatalf("client health after a completed download = %+v, want the advisory cleared", h)
	}

	// One bad release now blocklists again.
	items = []nzbget.HistoryItem{{NZBID: 211, Status: "FAILURE/UNPACK"}}
	f.addDownload(t, ctx, client, "guid-r-bad", "211", nil)
	f.scanner.checkNZBGetDownloads(ctx, client)
	if got := f.entries(t, ctx); len(got) != 3 {
		t.Fatalf("blocklist has %d entries, want 3: a single bad release after recovery must blocklist", len(got))
	}
}
