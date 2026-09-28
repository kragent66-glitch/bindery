package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/vavallee/bindery/internal/calibre"
)

// CalibreSyncHandler exposes POST /calibre/sync (Push all: queue every
// eligible book for the Calibre delivery worker) and GET /calibre/sync/status
// (the run's progress, read from the delivery ledger). Admin only.
type CalibreSyncHandler struct {
	syncer   syncerAPI
	loadCfg  func() calibre.Config
	loadMode func() calibre.Mode
}

// syncerAPI is the subset of *calibre.Syncer the API touches so tests can
// swap in a stub without wiring the full repo stack.
type syncerAPI interface {
	Start(mode calibre.Mode) error
	Progress(ctx context.Context) (calibre.SyncProgress, error)
}

func NewCalibreSyncHandler(s syncerAPI, loadCfg func() calibre.Config, loadMode func() calibre.Mode) *CalibreSyncHandler {
	return &CalibreSyncHandler{syncer: s, loadCfg: loadCfg, loadMode: loadMode}
}

// Start is POST /api/v1/calibre/sync. It checks that plugin mode is selected
// and the plugin URL is set, then starts the queueing in the background and
// answers 202 with the first progress snapshot so the UI can go straight
// into polling. The queueing does not run on the request's context: the
// syncer owns its own shutdown scoped one, so there is nothing for a
// finished response to cancel.
func (h *CalibreSyncHandler) Start(w http.ResponseWriter, r *http.Request) {
	mode := h.loadMode()
	if mode != calibre.ModePlugin {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "calibre mode is not 'plugin' — bulk sync only targets the Bindery Bridge plugin"})
		return
	}
	cfg := h.loadCfg()
	if cfg.PluginURL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "calibre plugin_url is empty"})
		return
	}

	err := h.syncer.Start(mode)
	switch {
	case errors.Is(err, calibre.ErrSyncAlreadyRunning):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	case errors.Is(err, calibre.ErrSyncModeNotPlugin):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	case err != nil:
		writeServerError(w, r, err)
		return
	}
	p, err := h.syncer.Progress(r.Context())
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, p)
}

// Status is GET /api/v1/calibre/sync/status.
func (h *CalibreSyncHandler) Status(w http.ResponseWriter, r *http.Request) {
	p, err := h.syncer.Progress(r.Context())
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}
