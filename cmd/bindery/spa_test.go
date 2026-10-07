package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/go-chi/chi/v5"

	"github.com/vavallee/bindery/internal/api"
)

const testManifest = `{"name":"Bindery","start_url":".","scope":"."}`

// spaTestServer wires spaHandler the way main() does: behind the security
// headers and, when urlBase is set, under the URL base mount.
func spaTestServer(urlBase string) http.Handler {
	dist := fstest.MapFS{
		"index.html":            {Data: []byte(`<!doctype html><html><head></head><body></body></html>`)},
		"manifest.webmanifest":  {Data: []byte(testManifest)},
		"apple-touch-icon.png":  {Data: []byte("\x89PNG\r\n\x1a\n")},
		"icon-192.png":          {Data: []byte("\x89PNG\r\n\x1a\n")},
		"icon-maskable-512.png": {Data: []byte("\x89PNG\r\n\x1a\n")},
	}
	r := chi.NewRouter()
	r.Use(api.SecurityHeaders(""))
	r.Get("/*", spaHandler(dist, []byte(injectBaseHTML(`<html><head></head></html>`, urlBase))))
	return mountUnderURLBase(r, urlBase)
}

func TestSPAHandler_ServesManifest(t *testing.T) {
	for _, base := range []string{"", "/bindery"} {
		t.Run("base="+base, func(t *testing.T) {
			rec := httptest.NewRecorder()
			spaTestServer(base).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, base+"/manifest.webmanifest", nil))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			// Go's mime table has no .webmanifest entry, so without the override
			// this sniffs as text/plain and nosniff pins it there.
			if ct := rec.Header().Get("Content-Type"); ct != "application/manifest+json" {
				t.Fatalf("Content-Type = %q, want application/manifest+json", ct)
			}
			if rec.Body.String() != testManifest {
				t.Fatalf("body = %q, want the manifest, not the SPA fallback", rec.Body.String())
			}
			// manifest-src falls back to default-src 'self', which allows a
			// same-origin manifest. A manifest-src directive added later must
			// not lock it out.
			csp := rec.Header().Get("Content-Security-Policy")
			if !strings.Contains(csp, "default-src 'self'") {
				t.Fatalf("CSP lost default-src 'self': %q", csp)
			}
			if strings.Contains(csp, "manifest-src") && !strings.Contains(csp, "manifest-src 'self'") {
				t.Fatalf("CSP manifest-src does not allow 'self': %q", csp)
			}
		})
	}
}

func TestSPAHandler_ServesIcons(t *testing.T) {
	for _, name := range []string{"apple-touch-icon.png", "icon-192.png", "icon-maskable-512.png"} {
		rec := httptest.NewRecorder()
		spaTestServer("/bindery").ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/bindery/"+name, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", name, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
			t.Fatalf("%s: Content-Type = %q, want image/png", name, ct)
		}
	}
}

func TestSPAHandler_FallsBackToIndex(t *testing.T) {
	rec := httptest.NewRecorder()
	spaTestServer("/bindery").ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/bindery/author/10", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html", ct)
	}
	if !strings.Contains(rec.Body.String(), `<base href="/bindery/">`) {
		t.Fatalf("fallback did not serve the injected index: %q", rec.Body.String())
	}
}
