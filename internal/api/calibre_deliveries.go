package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/vavallee/bindery/internal/auth"
	"github.com/vavallee/bindery/internal/calibre"
	"github.com/vavallee/bindery/internal/models"
)

// calibreDeliveryStore is the subset of *db.CalibreDeliveryRepo the queue
// endpoints use.
type calibreDeliveryStore interface {
	Summary(ctx context.Context) (models.CalibreDeliverySummary, error)
	ListWithBooks(ctx context.Context, state models.CalibreDeliveryState, limit, offset int) ([]models.CalibreDeliveryListItem, error)
	ListByBook(ctx context.Context, bookID int64) ([]models.CalibreDelivery, error)
	Retry(ctx context.Context, state models.CalibreDeliveryState) (int64, error)
	ClearPending(ctx context.Context) (int64, error)
	ResetAll(ctx context.Context) (int64, error)
}

// calibreDeliveryWorker is the subset of *calibre.Deliverer the endpoints use.
type calibreDeliveryWorker interface {
	Kick()
	Health() calibre.DeliveryHealth
	PullContact() calibre.PullContact
}

// calibreBookGetter loads a book for the per book state, so ownership can be
// checked before anything about it is returned.
type calibreBookGetter interface {
	GetByID(ctx context.Context, id int64) (*models.Book, error)
}

// CalibreDeliveryHandler serves the Calibre delivery queue (#2832): the admin
// queue view and actions under /calibre/deliveries, and the per book state
// under /book/{id}/calibre.
type CalibreDeliveryHandler struct {
	store  calibreDeliveryStore
	worker calibreDeliveryWorker
	books  calibreBookGetter
	mode   func() calibre.Mode
	// transport is nil in tests that predate pull; it then reads as push.
	transport func() calibre.Transport
}

func NewCalibreDeliveryHandler(store calibreDeliveryStore, worker calibreDeliveryWorker, books calibreBookGetter, mode func() calibre.Mode) *CalibreDeliveryHandler {
	return &CalibreDeliveryHandler{store: store, worker: worker, books: books, mode: mode}
}

// WithTransport lets the summary report the plugin transport (#2833).
func (h *CalibreDeliveryHandler) WithTransport(t func() calibre.Transport) *CalibreDeliveryHandler {
	h.transport = t
	return h
}

// calibreDeliverySummaryResponse is GET /calibre/deliveries/summary: the
// ledger counts plus what the worker last learned about Calibre.
type calibreDeliverySummaryResponse struct {
	models.CalibreDeliverySummary
	Mode   calibre.Mode           `json:"mode"`
	Target calibre.DeliveryHealth `json:"target"`
	// Transport is the plugin transport. In pull, Target stays empty: the
	// push worker does not probe, and Pull says when the plugin last
	// checked in instead.
	Transport calibre.Transport   `json:"transport"`
	Pull      calibre.PullContact `json:"pull"`
}

// Summary is GET /api/v1/calibre/deliveries/summary. Admin only.
func (h *CalibreDeliveryHandler) Summary(w http.ResponseWriter, r *http.Request) {
	s, err := h.store.Summary(r.Context())
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	transport := calibre.TransportPush
	if h.transport != nil {
		transport = h.transport()
	}
	writeJSON(w, http.StatusOK, calibreDeliverySummaryResponse{
		CalibreDeliverySummary: s,
		Mode:                   h.mode(),
		Target:                 h.worker.Health(),
		Transport:              transport,
		Pull:                   h.worker.PullContact(),
	})
}

// parseDeliveryState accepts a ledger state or "" for all of them.
func parseDeliveryState(raw string) (models.CalibreDeliveryState, bool) {
	switch st := models.CalibreDeliveryState(strings.TrimSpace(raw)); st {
	case "", models.CalibreDeliveryPending, models.CalibreDeliveryDelivered,
		models.CalibreDeliveryFailed, models.CalibreDeliverySkipped:
		return st, true
	default:
		return "", false
	}
}

// List is GET /api/v1/calibre/deliveries?state=&limit=&offset=. Admin only:
// rows carry file paths and Calibre's error text.
func (h *CalibreDeliveryHandler) List(w http.ResponseWriter, r *http.Request) {
	state, ok := parseDeliveryState(r.URL.Query().Get("state"))
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "state must be pending, delivered, failed, skipped or empty"})
		return
	}
	limit, offset := parseLimitOffset(r, 50, 500)
	items, err := h.store.ListWithBooks(r.Context(), state, limit, offset)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	s, err := h.store.Summary(r.Context())
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	total := s.Pending + s.Delivered + s.Failed + s.Skipped
	switch state {
	case models.CalibreDeliveryPending:
		total = s.Pending
	case models.CalibreDeliveryDelivered:
		total = s.Delivered
	case models.CalibreDeliveryFailed:
		total = s.Failed
	case models.CalibreDeliverySkipped:
		total = s.Skipped
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": total})
}

// decodeOptionalJSON reads a small JSON body into v. An empty body is fine.
func decodeOptionalJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(v)
	if err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return false
	}
	return true
}

// Retry is POST /api/v1/calibre/deliveries/retry with {state: "failed" |
// "skipped" | ""}. It puts those rows back in the queue and wakes the worker.
func (h *CalibreDeliveryHandler) Retry(w http.ResponseWriter, r *http.Request) {
	var req struct {
		State string `json:"state"`
	}
	if !decodeOptionalJSON(w, r, &req) {
		return
	}
	state := models.CalibreDeliveryState(strings.TrimSpace(req.State))
	if state != "" && state != models.CalibreDeliveryFailed && state != models.CalibreDeliverySkipped {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "state must be failed, skipped or empty"})
		return
	}
	n, err := h.store.Retry(r.Context(), state)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	if n > 0 {
		h.worker.Kick()
	}
	writeJSON(w, http.StatusOK, map[string]int64{"requeued": n})
}

// Clear is DELETE /api/v1/calibre/deliveries?state=pending. It empties the
// queue. Only pending rows can be cleared: the record of what was delivered
// is what keeps a book from being sent twice.
func (h *CalibreDeliveryHandler) Clear(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("state") != string(models.CalibreDeliveryPending) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "only state=pending can be cleared"})
		return
	}
	n, err := h.store.ClearPending(r.Context())
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"cleared": n})
}

// Reset is POST /api/v1/calibre/deliveries/reset with {confirm: true}. It
// forgets the whole ledger, for pointing Bindery at a different Calibre
// library. Nothing is sent to the new library until Push all or an import
// queues it.
func (h *CalibreDeliveryHandler) Reset(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Confirm bool `json:"confirm"`
	}
	if !decodeOptionalJSON(w, r, &req) {
		return
	}
	if !req.Confirm {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "reset forgets every delivery; send {\"confirm\": true}"})
		return
	}
	n, err := h.store.ResetAll(r.Context())
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"removed": n})
}

// bookCalibreState is GET /book/{id}/calibre. State is for everyone who can
// see the book; the rest is admin only, because the error text can carry
// paths and host names.
type bookCalibreState struct {
	// State is "off" when the integration is off, "none" when the book has no
	// delivery row, or the row's ledger state.
	State         string     `json:"state"`
	Outcome       string     `json:"outcome,omitempty"`
	LastError     string     `json:"lastError,omitempty"`
	LastErrorCode string     `json:"lastErrorCode,omitempty"`
	Attempts      int        `json:"attempts,omitempty"`
	CalibreID     *int64     `json:"calibreId,omitempty"`
	DeliveredAt   *time.Time `json:"deliveredAt,omitempty"`
}

// bookDeliveryRank orders a book's rows: the one that speaks for the book is
// its delivered row, else a pending one, else failed, else skipped.
var bookDeliveryRank = map[models.CalibreDeliveryState]int{
	models.CalibreDeliveryDelivered: 0,
	models.CalibreDeliveryPending:   1,
	models.CalibreDeliveryFailed:    2,
	models.CalibreDeliverySkipped:   3,
}

// BookState is GET /api/v1/book/{id}/calibre. A book the caller cannot see
// is a 404, exactly like GET /book/{id}.
func (h *CalibreDeliveryHandler) BookState(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	book, err := h.books.GetByID(r.Context(), id)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	if book == nil || !auth.CheckOwnership(r.Context(), book.OwnerUserID) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "book not found"})
		return
	}
	mode := h.mode()
	if mode != calibre.ModePlugin && mode != calibre.ModeCalibredb {
		writeJSON(w, http.StatusOK, bookCalibreState{State: "off"})
		return
	}
	rows, err := h.store.ListByBook(r.Context(), book.ID)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	if len(rows) == 0 {
		writeJSON(w, http.StatusOK, bookCalibreState{State: "none"})
		return
	}
	best := rows[0]
	for _, row := range rows[1:] {
		if bookDeliveryRank[row.State] < bookDeliveryRank[best.State] {
			best = row
		}
	}
	out := bookCalibreState{State: string(best.State)}
	if auth.UserRoleFromContext(r.Context()) == "admin" {
		out.Outcome = best.Outcome
		out.LastError = best.LastError
		out.LastErrorCode = best.LastErrorCode
		out.Attempts = best.Attempts
		out.CalibreID = best.CalibreID
		out.DeliveredAt = best.DeliveredAt
	}
	writeJSON(w, http.StatusOK, out)
}
