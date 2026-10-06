package downloader

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/vavallee/bindery/internal/models"
)

// TestHealthStoreAdvisorySurvivesProbe pins that the 15 minute path probe,
// which rewrites the client's health with Set, does not erase the advisory
// the importer records when it pauses automatic blocklisting (#3024).
func TestHealthStoreAdvisorySurvivesProbe(t *testing.T) {
	store := NewHealthStore()
	store.SetAdvisory(7, models.DownloadClientHealth{Status: HealthError, Message: "paused"})
	store.Set(7, models.DownloadClientHealth{Status: HealthOK, Message: "paths fine"})
	if h := store.Get(7); h == nil || h.Message != "paused" {
		t.Fatalf("after a passing probe = %+v, want the advisory", h)
	}
	store.Set(7, models.DownloadClientHealth{Status: HealthError, Message: "path missing."})
	if h := store.Get(7); h == nil || h.Message != "path missing. paused" {
		t.Fatalf("after a failing probe = %+v, want both messages", h)
	}
	store.ClearAdvisory(7)
	if h := store.Get(7); h == nil || h.Message != "path missing." {
		t.Fatalf("after ClearAdvisory = %+v, want the probe result", h)
	}
}

// TestWithNZBGetUnpackers covers the sysinfo part of the 15 minute probe: a
// missing UnRAR turns a passing path check into an error naming it and is
// carried for the importer, and an NZBGet without sysinfo changes nothing.
func TestWithNZBGetUnpackers(t *testing.T) {
	serve := func(withSysinfo bool, unrarPath string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			var req struct {
				Method string `json:"method"`
			}
			_ = json.Unmarshal(body, &req)
			if req.Method != "sysinfo" || !withSysinfo {
				_ = json.NewEncoder(w).Encode(map[string]any{"result": nil, "error": map[string]any{"code": 1, "message": "Invalid procedure"}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"Tools": []map[string]any{
				{"Name": "7-Zip", "Path": "/usr/bin/7z"},
				{"Name": "UnRAR", "Path": unrarPath},
			}}})
		}))
	}
	client := func(t *testing.T, srv *httptest.Server) *models.DownloadClient {
		u, _ := url.Parse(srv.URL)
		host, portStr, _ := net.SplitHostPort(u.Host)
		port, _ := strconv.Atoi(portStr)
		return &models.DownloadClient{ID: 99, Type: "nzbget", Host: host, Port: port, Enabled: true}
	}
	ok := models.DownloadClientHealth{Status: HealthOK, Message: "paths fine"}

	missing := serve(true, "")
	defer missing.Close()
	got := withNZBGetUnpackers(context.Background(), client(t, missing), ok)
	if got.Status != HealthError || !strings.Contains(got.Message, "cannot find UnRAR") || len(got.MissingUnpackers) != 1 {
		t.Fatalf("missing UnRAR: %+v", got)
	}

	present := serve(true, "/usr/bin/unrar")
	defer present.Close()
	if got := withNZBGetUnpackers(context.Background(), client(t, present), ok); got.Status != HealthOK || got.MissingUnpackers != nil {
		t.Fatalf("unpackers present: %+v", got)
	}

	old := serve(false, "")
	defer old.Close()
	if got := withNZBGetUnpackers(context.Background(), client(t, old), ok); got.Status != HealthOK || got.Message != "paths fine" {
		t.Fatalf("NZBGet without sysinfo: %+v, want the path check unchanged", got)
	}
}

// TestHealthStoreCheckingKeepsMissingUnpackers pins that the "checking"
// placeholder written before every probe does not drop what the importer
// reads in the meantime.
func TestHealthStoreCheckingKeepsMissingUnpackers(t *testing.T) {
	store := NewHealthStore()
	store.Set(7, models.DownloadClientHealth{Status: HealthError, Message: "x", MissingUnpackers: []string{"UnRAR"}})
	store.Set(7, CheckingHealth())
	if h := store.Get(7); h == nil || len(h.MissingUnpackers) != 1 {
		t.Fatalf("after the checking placeholder = %+v, want MissingUnpackers kept", h)
	}
}

// TestHealthStoreAdvisoryNotifiesOnce pins that the advisory publishes the
// health event on entry only, like Set's entry into HealthError.
func TestHealthStoreAdvisoryNotifiesOnce(t *testing.T) {
	spy := &healthSpy{}
	store := NewHealthStore().WithNotifier(spy)
	store.SetAdvisory(7, models.DownloadClientHealth{Status: HealthError, Message: "paused"})
	store.SetAdvisory(7, models.DownloadClientHealth{Status: HealthError, Message: "paused"})
	if len(spy.calls) != 1 || spy.calls[0].eventType != notifierEventHealth {
		t.Fatalf("health events = %+v, want exactly one", spy.calls)
	}
}
