package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// stubBridgeHandler records which layers a request went through: auth, the
// pull gate, and the route itself.
type stubBridgeHandler struct {
	trail []string
	// allow is what the stub auth decides.
	allow bool
}

func (h *stubBridgeHandler) Authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.trail = append(h.trail, "auth")
		if !h.allow {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *stubBridgeHandler) RequirePull(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.trail = append(h.trail, "pull")
		next.ServeHTTP(w, r)
	})
}

func (h *stubBridgeHandler) hit(name string, w http.ResponseWriter) {
	h.trail = append(h.trail, name)
	w.WriteHeader(http.StatusNoContent)
}

func (h *stubBridgeHandler) Hello(w http.ResponseWriter, _ *http.Request) { h.hit("hello", w) }
func (h *stubBridgeHandler) List(w http.ResponseWriter, _ *http.Request)  { h.hit("list", w) }
func (h *stubBridgeHandler) File(w http.ResponseWriter, _ *http.Request)  { h.hit("file", w) }
func (h *stubBridgeHandler) Cover(w http.ResponseWriter, _ *http.Request) { h.hit("cover", w) }
func (h *stubBridgeHandler) Ack(w http.ResponseWriter, _ *http.Request)   { h.hit("ack", w) }
func (h *stubBridgeHandler) Nack(w http.ResponseWriter, _ *http.Request)  { h.hit("nack", w) }

// Every bridge route sits behind the plugin key check, and every delivery
// route behind the pull gate as well; hello is deliberately outside the gate.
func TestRegisterCalibreBridgeRoutes(t *testing.T) {
	cases := []struct {
		method, path, want string
	}{
		{http.MethodGet, "/bridge/v1/hello", "auth,hello"},
		{http.MethodGet, "/bridge/v1/deliveries", "auth,pull,list"},
		{http.MethodGet, "/bridge/v1/deliveries/7/file", "auth,pull,file"},
		{http.MethodGet, "/bridge/v1/deliveries/7/cover", "auth,pull,cover"},
		{http.MethodPost, "/bridge/v1/deliveries/7/ack", "auth,pull,ack"},
		{http.MethodPost, "/bridge/v1/deliveries/7/nack", "auth,pull,nack"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			h := &stubBridgeHandler{allow: true}
			r := chi.NewRouter()
			registerCalibreBridgeRoutes(r, h)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
			if rec.Code != http.StatusNoContent || strings.Join(h.trail, ",") != tc.want {
				t.Fatalf("status %d trail %v, want 204 through %s", rec.Code, h.trail, tc.want)
			}

			denied := &stubBridgeHandler{}
			r = chi.NewRouter()
			registerCalibreBridgeRoutes(r, denied)
			rec = httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
			if rec.Code != http.StatusUnauthorized || strings.Join(denied.trail, ",") != "auth" {
				t.Fatalf("refused by auth: status %d trail %v", rec.Code, denied.trail)
			}
		})
	}
}
