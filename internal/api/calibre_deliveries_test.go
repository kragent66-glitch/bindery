package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vavallee/bindery/internal/auth"
	"github.com/vavallee/bindery/internal/calibre"
	"github.com/vavallee/bindery/internal/db"
	"github.com/vavallee/bindery/internal/models"
)

type fakeDeliveryWorker struct {
	mu     sync.Mutex
	kicks  int
	health calibre.DeliveryHealth
	pull   calibre.PullContact
}

func (f *fakeDeliveryWorker) Kick() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kicks++
}

func (f *fakeDeliveryWorker) Health() calibre.DeliveryHealth { return f.health }

func (f *fakeDeliveryWorker) PullContact() calibre.PullContact { return f.pull }

type deliveryFixture struct {
	ctx    context.Context
	repo   *db.CalibreDeliveryRepo
	books  *db.BookRepo
	worker *fakeDeliveryWorker
	mode   calibre.Mode
	h      *CalibreDeliveryHandler
	owner  int64
	other  int64
	book   *models.Book
	// fileIDs are the book's two ebook files: the first delivered, the
	// second failed with an error that names a path.
	fileIDs [2]int64
}

const failedErrorText = "path_forbidden: /srv/library/Frank Herbert/Dune.epub is outside the library"

func newDeliveryFixture(t *testing.T) *deliveryFixture {
	t.Helper()
	database, err := db.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	f := &deliveryFixture{
		ctx:    context.Background(),
		repo:   db.NewCalibreDeliveryRepo(database),
		books:  db.NewBookRepo(database),
		worker: &fakeDeliveryWorker{},
		mode:   calibre.ModePlugin,
	}
	users := db.NewUserRepo(database)
	u1, err := users.Create(f.ctx, "alice", "h1")
	if err != nil {
		t.Fatal(err)
	}
	u2, err := users.Create(f.ctx, "bob", "h2")
	if err != nil {
		t.Fatal(err)
	}
	f.owner, f.other = u1.ID, u2.ID
	authors := db.NewAuthorRepo(database)
	a := &models.Author{ForeignID: "OLA1", Name: "Frank Herbert", SortName: "Herbert, Frank", Monitored: true}
	if err := authors.Create(f.ctx, a); err != nil {
		t.Fatal(err)
	}
	f.book = &models.Book{ForeignID: "OLB1", AuthorID: a.ID, Title: "Dune", SortTitle: "Dune",
		Status: models.BookStatusWanted, Monitored: true, AnyEditionOK: true}
	if err := f.books.Create(f.ctx, f.book); err != nil {
		t.Fatal(err)
	}
	setOwner(t, database, "books", f.book.ID, f.owner)
	for i, p := range []string{"/srv/library/Dune.azw3", "/srv/library/Dune.epub"} {
		if err := f.books.AddBookFile(f.ctx, f.book.ID, models.MediaTypeEbook, p); err != nil {
			t.Fatal(err)
		}
		files, _ := f.books.ListFiles(f.ctx, f.book.ID)
		for _, bf := range files {
			if bf.Path == p {
				f.fileIDs[i] = bf.ID
			}
		}
		if _, err := f.repo.Enqueue(f.ctx, f.book.ID, f.fileIDs[i], nil, p, "epub"); err != nil {
			t.Fatal(err)
		}
	}
	d1, _ := f.repo.GetByBookFile(f.ctx, f.fileIDs[1])
	if err := f.repo.MarkFailed(f.ctx, d1.ID, "path_forbidden", failedErrorText, time.Now(), true); err != nil {
		t.Fatal(err)
	}
	f.h = NewCalibreDeliveryHandler(f.repo, f.worker, f.books, func() calibre.Mode { return f.mode })
	return f
}

func (f *deliveryFixture) deliverFirst(t *testing.T) {
	t.Helper()
	d, _ := f.repo.GetByBookFile(f.ctx, f.fileIDs[0])
	if err := f.repo.MarkDelivered(f.ctx, d.ID, 77, calibre.DeliveryOutcomeAdded, "/calibre"); err != nil {
		t.Fatal(err)
	}
}

func deliveryAdminReq(method, target, body string) *http.Request {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	return req.WithContext(withAuthCtx(req.Context(), 1, "admin"))
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return out
}

func TestCalibreDeliveries_Summary(t *testing.T) {
	f := newDeliveryFixture(t)
	f.deliverFirst(t)
	reachable := false
	f.worker.health = calibre.DeliveryHealth{Reachable: &reachable, LastError: "dial tcp: connection refused"}

	rec := httptest.NewRecorder()
	f.h.Summary(rec, deliveryAdminReq(http.MethodGet, "/calibre/deliveries/summary", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	if out["delivered"] != float64(1) || out["failed"] != float64(1) || out["pending"] != float64(0) {
		t.Errorf("counts = %v", out)
	}
	if out["lastDeliveredAt"] == nil || out["mode"] != "plugin" {
		t.Errorf("summary = %v, want lastDeliveredAt and mode", out)
	}
	target, _ := out["target"].(map[string]any)
	if target["reachable"] != false || target["lastError"] != "dial tcp: connection refused" {
		t.Errorf("target = %v", target)
	}
}

func TestCalibreDeliveries_ListJoinsTitleAndAuthor(t *testing.T) {
	f := newDeliveryFixture(t)
	rec := httptest.NewRecorder()
	f.h.List(rec, deliveryAdminReq(http.MethodGet, "/calibre/deliveries?state=failed&limit=10", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Items []models.CalibreDeliveryListItem `json:"items"`
		Total int                              `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Total != 1 || len(out.Items) != 1 {
		t.Fatalf("list = %+v, want the one failed row", out)
	}
	it := out.Items[0]
	if it.BookTitle != "Dune" || it.AuthorName != "Frank Herbert" || it.LastErrorCode != "path_forbidden" || it.Attempts != 1 {
		t.Errorf("item = %+v", it)
	}

	rec = httptest.NewRecorder()
	f.h.List(rec, deliveryAdminReq(http.MethodGet, "/calibre/deliveries?state=bogus", ""))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bogus state: status = %d, want 400", rec.Code)
	}
}

func TestCalibreDeliveries_RetryFailedRequeuesAndKicks(t *testing.T) {
	f := newDeliveryFixture(t)
	rec := httptest.NewRecorder()
	f.h.Retry(rec, deliveryAdminReq(http.MethodPost, "/calibre/deliveries/retry", `{"state":"failed"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if out := decodeBody(t, rec); out["requeued"] != float64(1) {
		t.Errorf("response = %v, want requeued 1", out)
	}
	d, _ := f.repo.GetByBookFile(f.ctx, f.fileIDs[1])
	if d.State != models.CalibreDeliveryPending || d.LastError != "" {
		t.Errorf("row after retry = %+v", d)
	}
	if f.worker.kicks != 1 {
		t.Errorf("kicks = %d, want 1", f.worker.kicks)
	}

	rec = httptest.NewRecorder()
	f.h.Retry(rec, deliveryAdminReq(http.MethodPost, "/calibre/deliveries/retry", `{"state":"delivered"}`))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("retry delivered: status = %d, want 400", rec.Code)
	}
}

func TestCalibreDeliveries_ClearOnlyPending(t *testing.T) {
	f := newDeliveryFixture(t)
	rec := httptest.NewRecorder()
	f.h.Clear(rec, deliveryAdminReq(http.MethodDelete, "/calibre/deliveries?state=failed", ""))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("clear failed: status = %d, want 400", rec.Code)
	}
	rec = httptest.NewRecorder()
	f.h.Clear(rec, deliveryAdminReq(http.MethodDelete, "/calibre/deliveries?state=pending", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if out := decodeBody(t, rec); out["cleared"] != float64(1) {
		t.Errorf("response = %v, want the one pending row cleared", out)
	}
	if d, _ := f.repo.GetByBookFile(f.ctx, f.fileIDs[1]); d == nil || d.State != models.CalibreDeliveryFailed {
		t.Errorf("failed row = %+v, want it kept", d)
	}
}

func TestCalibreDeliveries_ResetNeedsConfirm(t *testing.T) {
	f := newDeliveryFixture(t)
	for _, body := range []string{"", `{}`, `{"confirm":false}`} {
		rec := httptest.NewRecorder()
		f.h.Reset(rec, deliveryAdminReq(http.MethodPost, "/calibre/deliveries/reset", body))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %q: status = %d, want 400", body, rec.Code)
		}
	}
	if s, _ := f.repo.Summary(f.ctx); s.Pending+s.Failed != 2 {
		t.Fatalf("rows gone without a confirm: %+v", s)
	}
	rec := httptest.NewRecorder()
	f.h.Reset(rec, deliveryAdminReq(http.MethodPost, "/calibre/deliveries/reset", `{"confirm":true}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if out := decodeBody(t, rec); out["removed"] != float64(2) {
		t.Errorf("response = %v, want 2 removed", out)
	}
}

// TestCalibreDeliveries_BookStateStripsDetailForNonAdmins: the owner of a
// book sees where it stands, and nothing else. The error text names a path
// on the server, which is admin only.
func TestCalibreDeliveries_BookStateStripsDetailForNonAdmins(t *testing.T) {
	auth.SetEnforceTenancyForTests(t, true)
	f := newDeliveryFixture(t)
	// Both files failed, so the row that speaks for the book carries the
	// error text, attempts and code an admin would see.
	d, _ := f.repo.GetByBookFile(f.ctx, f.fileIDs[0])
	if err := f.repo.MarkFailed(f.ctx, d.ID, "path_forbidden", failedErrorText, time.Now(), true); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	f.h.BookState(rec, newRequestForID(http.MethodGet, "/book/x/calibre", f.book.ID, withAuthCtx(f.ctx, f.owner, "user")))
	if rec.Code != http.StatusOK {
		t.Fatalf("owner: status = %d: %s", rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	if len(out) != 1 || out["state"] != "failed" {
		t.Errorf("owner body = %v, want only the state", out)
	}
	if strings.Contains(rec.Body.String(), "/srv/library") {
		t.Errorf("owner body leaks a path: %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	f.h.BookState(rec, newRequestForID(http.MethodGet, "/book/x/calibre", f.book.ID, withAuthCtx(f.ctx, f.other, "user")))
	if rec.Code != http.StatusNotFound {
		t.Errorf("other user: status = %d, want 404", rec.Code)
	}
}

func TestCalibreDeliveries_BookStateForAdmins(t *testing.T) {
	f := newDeliveryFixture(t)
	f.deliverFirst(t)
	rec := httptest.NewRecorder()
	f.h.BookState(rec, newRequestForID(http.MethodGet, "/book/x/calibre", f.book.ID, withAuthCtx(f.ctx, 1, "admin")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	// The delivered row speaks for the book over the failed one.
	if out["state"] != "delivered" || out["calibreId"] != float64(77) || out["outcome"] != "added" || out["deliveredAt"] == nil {
		t.Errorf("admin body = %v", out)
	}

	// With only the failed row left, an admin sees why.
	d, _ := f.repo.GetByBookFile(f.ctx, f.fileIDs[0])
	if err := f.repo.MarkFailed(f.ctx, d.ID, "path_forbidden", failedErrorText, time.Now(), true); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	f.h.BookState(rec, newRequestForID(http.MethodGet, "/book/x/calibre", f.book.ID, withAuthCtx(f.ctx, 1, "admin")))
	out = decodeBody(t, rec)
	if out["state"] != "failed" || out["lastError"] != failedErrorText || out["lastErrorCode"] != "path_forbidden" {
		t.Errorf("admin failed body = %v", out)
	}
}

func TestCalibreDeliveries_BookStateOffAndNone(t *testing.T) {
	f := newDeliveryFixture(t)
	f.mode = calibre.ModeOff
	rec := httptest.NewRecorder()
	f.h.BookState(rec, newRequestForID(http.MethodGet, "/book/x/calibre", f.book.ID, withAuthCtx(f.ctx, 1, "admin")))
	if out := decodeBody(t, rec); out["state"] != "off" || len(out) != 1 {
		t.Errorf("mode off body = %v, want state off only", out)
	}

	f.mode = calibre.ModeCalibredb
	if _, err := f.repo.ResetAll(f.ctx); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	f.h.BookState(rec, newRequestForID(http.MethodGet, "/book/x/calibre", f.book.ID, withAuthCtx(f.ctx, 1, "admin")))
	if out := decodeBody(t, rec); out["state"] != "none" {
		t.Errorf("no rows body = %v, want state none", out)
	}

	rec = httptest.NewRecorder()
	f.h.BookState(rec, newRequestForID(http.MethodGet, "/book/x/calibre", 99999, withAuthCtx(f.ctx, 1, "admin")))
	if rec.Code != http.StatusNotFound {
		t.Errorf("missing book: status = %d, want 404", rec.Code)
	}
}

// TestCalibreDeliveries_ServerErrorsDoNotLeak: a failing store answers with
// the generic message, never the error text.
func TestCalibreDeliveries_ServerErrorsDoNotLeak(t *testing.T) {
	database, err := db.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	repo := db.NewCalibreDeliveryRepo(database)
	_ = database.Close()
	h := NewCalibreDeliveryHandler(repo, &fakeDeliveryWorker{}, db.NewBookRepo(database), func() calibre.Mode { return calibre.ModePlugin })
	rec := httptest.NewRecorder()
	h.Summary(rec, deliveryAdminReq(http.MethodGet, "/calibre/deliveries/summary", ""))
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "calibre deliveries") {
		t.Errorf("status = %d, body = %s; want a generic 500", rec.Code, rec.Body.String())
	}
}
