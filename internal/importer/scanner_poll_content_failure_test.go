package importer

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/vavallee/bindery/internal/db"
	"github.com/vavallee/bindery/internal/decision"
	"github.com/vavallee/bindery/internal/downloader/nzbget"
	"github.com/vavallee/bindery/internal/downloader/sabnzbd"
	"github.com/vavallee/bindery/internal/models"
)

// contentFailureFixture is a scanner with a blocklist wired the way main.go
// wires it, plus one in flight usenet download owned by the given client.
type contentFailureFixture struct {
	scanner   *Scanner
	downloads *db.DownloadRepo
	blocklist *db.BlocklistRepo
	clients   *db.DownloadClientRepo
}

func newContentFailureFixture(t *testing.T) contentFailureFixture {
	t.Helper()
	database, err := db.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	dlRepo := db.NewDownloadRepo(database)
	clientRepo := db.NewDownloadClientRepo(database)
	blocklist := db.NewBlocklistRepo(database)
	s := NewScanner(dlRepo, clientRepo, db.NewBookRepo(database), db.NewAuthorRepo(database),
		db.NewHistoryRepo(database), t.TempDir(), "", "", "", "")
	s.WithFormatEnforcement(db.NewQualityProfileRepo(database), blocklist)
	return contentFailureFixture{scanner: s, downloads: dlRepo, blocklist: blocklist, clients: clientRepo}
}

func (f contentFailureFixture) addDownload(t *testing.T, ctx context.Context, client *models.DownloadClient, guid, sourceID string) {
	t.Helper()
	id := sourceID
	dl := &models.Download{
		GUID:             guid,
		Title:            "Broken Book",
		Status:           models.StateDownloading,
		Protocol:         "usenet",
		SABnzbdNzoID:     &id,
		DownloadClientID: &client.ID,
	}
	if err := f.downloads.Create(ctx, dl); err != nil {
		t.Fatal(err)
	}
}

// assertBlocklisted checks the row is failed, whether it is on the blocklist
// with the expected reason, and that the blocklist the wanted sweep builds
// (decision.NewBlocklistedSpec, keyed on GUID) skips the release next time.
func (f contentFailureFixture) assertBlocklisted(t *testing.T, ctx context.Context, guid string, want bool, wantReason string) {
	t.Helper()
	got, err := f.downloads.GetByGUID(ctx, guid)
	if err != nil || got == nil {
		t.Fatalf("get download: %v", err)
	}
	if got.Status != models.StateFailed {
		t.Errorf("status = %q, want %q", got.Status, models.StateFailed)
	}
	entries, err := f.blocklist.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !want {
		if len(entries) != 0 {
			t.Fatalf("blocklist = %+v, want empty: a client side failure must keep the #2710 cooldown", entries)
		}
		return
	}
	if len(entries) != 1 {
		t.Fatalf("blocklist has %d entries, want 1", len(entries))
	}
	if entries[0].GUID != guid || entries[0].Reason != wantReason {
		t.Errorf("blocklist entry = {guid %q, reason %q}, want {%q, %q}",
			entries[0].GUID, entries[0].Reason, guid, wantReason)
	}
	ok, _ := decision.NewBlocklistedSpec(entries).IsSatisfiedBy(decision.Release{GUID: guid}, models.Book{})
	if ok {
		t.Error("the next sweep's blocklist spec would still accept the release")
	}
}

// TestCheckNZBGetDownloads_ContentFailureBlocklists covers #3024 for NZBGet: a
// status about the NZB itself blocklists the release, one about the client
// does not.
func TestCheckNZBGetDownloads_ContentFailureBlocklists(t *testing.T) {
	cases := []struct {
		status string
		want   bool
	}{
		{"FAILURE/PAR", true},
		{"FAILURE/UNPACK", true},
		{"FAILURE/HEALTH", true},
		{"FAILURE/MOVE", false},
		{"FAILURE/INTERNAL_ERROR", false},
	}
	for _, tc := range cases {
		t.Run(tc.status, func(t *testing.T) {
			ctx := context.Background()
			srv := httptest.NewServer(nzbgetHandler(t, []nzbget.HistoryItem{{
				NZBID: 77, NZBName: "Broken Book", Status: tc.status,
			}}, nil))
			defer srv.Close()

			f := newContentFailureFixture(t)
			client := nzbgetClient(t, ctx, f.clients, srv.URL)
			f.addDownload(t, ctx, client, "guid-ng-3024", "77")

			f.scanner.checkNZBGetDownloads(ctx, client)
			f.assertBlocklisted(t, ctx, "guid-ng-3024", tc.want, "downloadFailed: "+tc.status)

			// A second poll of the same history must not add a second row.
			f.scanner.checkNZBGetDownloads(ctx, client)
			f.assertBlocklisted(t, ctx, "guid-ng-3024", tc.want, "downloadFailed: "+tc.status)
		})
	}
}

// TestCheckSABnzbdDownloads_ContentFailureBlocklists is the SABnzbd analogue,
// using fail_message strings copied from SABnzbd's source.
func TestCheckSABnzbdDownloads_ContentFailureBlocklists(t *testing.T) {
	cases := []struct {
		message string
		want    bool
	}{
		{"Aborted, cannot be completed - https://sabnzbd.org/not-complete", true},
		{"Repair failed, not enough repair blocks (412 short)", true},
		{"Unpacking failed, CRC error", true},
		{"Unpacking failed, disk full", false},
		{"Failed to move files", false},
		{"some message SABnzbd has never written", false},
	}
	for _, tc := range cases {
		t.Run(tc.message, func(t *testing.T) {
			ctx := context.Background()
			srv := httptest.NewServer(sabHistoryHandler(t, []sabnzbd.HistorySlot{{
				NzoID: "SABnzbd_nzo_3024", Name: "Broken Book", Status: "Failed", FailMessage: tc.message,
			}}, nil))
			defer srv.Close()

			f := newContentFailureFixture(t)
			client := sabClient(t, ctx, f.clients, srv.URL)
			f.addDownload(t, ctx, client, "guid-sab-3024", "SABnzbd_nzo_3024")

			f.scanner.checkSABnzbdDownloads(ctx, client)
			f.assertBlocklisted(t, ctx, "guid-sab-3024", tc.want, "downloadFailed: "+tc.message)
		})
	}
}

// TestBlocklistRejectedRelease_SkipsAlreadyBlocked pins the dedupe: a manual
// grab can send a blocklisted release again, and its second failure must not
// stack another row on the blocklist.
func TestBlocklistRejectedRelease_SkipsAlreadyBlocked(t *testing.T) {
	ctx := context.Background()
	f := newContentFailureFixture(t)
	dl := &models.Download{GUID: "guid-dupe", Title: "Broken Book"}
	f.scanner.blocklistRejectedRelease(ctx, dl, "downloadFailed: FAILURE/PAR")
	f.scanner.blocklistRejectedRelease(ctx, dl, "downloadFailed: FAILURE/PAR")
	entries, err := f.blocklist.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("blocklist has %d entries, want 1", len(entries))
	}
}
