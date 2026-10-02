package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vavallee/bindery/internal/calibre"
)

type stubSyncer struct {
	startErr error
	started  int
	progress calibre.SyncProgress
}

func (s *stubSyncer) Start(calibre.Mode) error {
	s.started++
	return s.startErr
}

func (s *stubSyncer) Progress(context.Context) (calibre.SyncProgress, error) {
	return s.progress, nil
}

func newSyncHandler(s *stubSyncer, mode calibre.Mode, url string) *CalibreSyncHandler {
	return NewCalibreSyncHandler(s,
		func() calibre.Config { return calibre.Config{PluginURL: url} },
		func() calibre.Mode { return mode })
}

func TestCalibreSync_StartQueuesAndReturnsTheProgress(t *testing.T) {
	s := &stubSyncer{progress: calibre.SyncProgress{Running: true, Message: "queueing books for Calibre…"}}
	rec := httptest.NewRecorder()
	newSyncHandler(s, calibre.ModePlugin, "http://calibre:8099").Start(rec, httptest.NewRequest(http.MethodPost, "/calibre/sync", nil))
	if rec.Code != http.StatusAccepted || s.started != 1 {
		t.Fatalf("status = %d, started = %d", rec.Code, s.started)
	}
	var p calibre.SyncProgress
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil || !p.Running {
		t.Errorf("body = %s, %v", rec.Body.String(), err)
	}
}

func TestCalibreSync_StartRefusesWithoutThePlugin(t *testing.T) {
	for _, tc := range []struct {
		mode calibre.Mode
		url  string
	}{{calibre.ModeCalibredb, "http://x"}, {calibre.ModeOff, "http://x"}, {calibre.ModePlugin, ""}} {
		s := &stubSyncer{}
		rec := httptest.NewRecorder()
		newSyncHandler(s, tc.mode, tc.url).Start(rec, httptest.NewRequest(http.MethodPost, "/calibre/sync", nil))
		if rec.Code != http.StatusBadRequest || s.started != 0 {
			t.Errorf("mode %q url %q: status = %d, started = %d", tc.mode, tc.url, rec.Code, s.started)
		}
	}
}

// In pull (#2833) the plugin connects to Bindery, so Push all does not need
// a plugin URL.
func TestCalibreSync_PullNeedsNoPluginURL(t *testing.T) {
	s := &stubSyncer{}
	rec := httptest.NewRecorder()
	newSyncHandler(s, calibre.ModePlugin, "").
		WithTransport(func() calibre.Transport { return calibre.TransportPull }).
		Start(rec, httptest.NewRequest(http.MethodPost, "/calibre/sync", nil))
	if rec.Code != http.StatusAccepted || s.started != 1 {
		t.Fatalf("status = %d, started = %d, want 202 and a start", rec.Code, s.started)
	}
}

func TestCalibreSync_StartWhileQueueingIsAConflict(t *testing.T) {
	s := &stubSyncer{startErr: calibre.ErrSyncAlreadyRunning}
	rec := httptest.NewRecorder()
	newSyncHandler(s, calibre.ModePlugin, "http://x").Start(rec, httptest.NewRequest(http.MethodPost, "/calibre/sync", nil))
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", rec.Code)
	}
	s.startErr = errors.New("boom")
	rec = httptest.NewRecorder()
	newSyncHandler(s, calibre.ModePlugin, "http://x").Start(rec, httptest.NewRequest(http.MethodPost, "/calibre/sync", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

func TestCalibreSync_Status(t *testing.T) {
	s := &stubSyncer{progress: calibre.SyncProgress{Stats: calibre.SyncStats{Total: 3, Pushed: 2}}}
	rec := httptest.NewRecorder()
	newSyncHandler(s, calibre.ModePlugin, "http://x").Status(rec, httptest.NewRequest(http.MethodGet, "/calibre/sync/status", nil))
	var p calibre.SyncProgress
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil || p.Stats.Pushed != 2 {
		t.Errorf("status body = %s, %v", rec.Body.String(), err)
	}
}
