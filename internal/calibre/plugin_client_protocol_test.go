package calibre

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vavallee/bindery/internal/useragent"
)

// shrinkBackoff replaces the 503 backoff schedule for the duration of a test
// so the suite does not actually sleep for half a minute.
func shrinkBackoff(t *testing.T) {
	t.Helper()
	prev := pluginRetryBackoff
	pluginRetryBackoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { pluginRetryBackoff = prev })
}

func healthJSON(caps ...string) string {
	b, _ := json.Marshal(struct {
		PluginVersion  string   `json:"plugin_version"`
		CalibreVersion string   `json:"calibre_version"`
		Library        string   `json:"library"`
		Capabilities   []string `json:"capabilities"`
	}{"0.6.0", "9.8", "/books", caps})
	return string(b)
}

// TestPluginClient_UserAgentCarriesVersion pins the protocol.md requirement
// that the client identifies its own version, so a plugin log can tell which
// Bindery build produced a request (review item 19).
func TestPluginClient_UserAgentCarriesVersion(t *testing.T) {
	useragent.Set("1.37.0")
	t.Cleanup(func() { useragent.Set("dev") })

	seen := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen[r.URL.Path] = r.Header.Get("User-Agent")
		if r.URL.Path == "/v1/health" {
			_, _ = w.Write([]byte(healthJSON("book_metadata")))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":1}`))
	}))
	defer srv.Close()

	c := NewPluginClient(srv.URL, "k")
	if _, err := c.Add(context.Background(), "/a.epub", Metadata{Title: "Dune"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	want := "bindery/1.37.0 plugin-api/v1"
	for path, got := range seen {
		if got != want {
			t.Errorf("User-Agent on %s = %q, want %q", path, got, want)
		}
	}
	if len(seen) != 2 {
		t.Fatalf("expected both endpoints to be called, saw %v", seen)
	}
}

// TestPluginClient_NoLegacyRetryOnErrnoPathError is review item 6: a 400 whose
// body is an operating system error about the file is a mount problem, not a
// metadata problem. Retrying doubles the request count and the warning points
// the operator at the wrong subsystem.
func TestPluginClient_NoLegacyRetryOnErrnoPathError(t *testing.T) {
	var posts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/health" {
			_, _ = w.Write([]byte(healthJSON("book_metadata")))
			return
		}
		atomic.AddInt32(&posts, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"[Errno 2] No such file or directory: '/books/a.epub'"}`))
	}))
	defer srv.Close()

	c := NewPluginClient(srv.URL, "k")
	_, err := c.Add(context.Background(), "/books/a.epub", Metadata{Title: "Dune"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := atomic.LoadInt32(&posts); got != 1 {
		t.Errorf("POST count = %d, want 1 (no legacy retry for a path error)", got)
	}
}

// TestPluginClient_LegacyRetryTriggers covers the cases that should still
// retry: an explicit invalid_metadata code, a 422, and an old plugin that
// sends no code at all with a message that is not an operating system error.
func TestPluginClient_LegacyRetryTriggers(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"invalid_metadata code", http.StatusBadRequest, `{"error":"metadata must be an object","code":"invalid_metadata"}`},
		{"unprocessable entity", http.StatusUnprocessableEntity, `{"error":"cannot apply metadata"}`},
		{"old plugin, no code", http.StatusBadRequest, `{"error":"unknown field metadata"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var posts int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/health" {
					_, _ = w.Write([]byte(healthJSON("book_metadata")))
					return
				}
				if atomic.AddInt32(&posts, 1) == 1 {
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(tc.body))
					return
				}
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{"id":5}`))
			}))
			defer srv.Close()

			c := NewPluginClient(srv.URL, "k")
			id, err := c.Add(context.Background(), "/a.epub", Metadata{Title: "Dune"})
			if err != nil {
				t.Fatalf("Add: %v", err)
			}
			if id != 5 || atomic.LoadInt32(&posts) != 2 {
				t.Errorf("id = %d, posts = %d, want 5 and 2", id, atomic.LoadInt32(&posts))
			}
		})
	}
}

// TestPluginClient_NoLegacyRetryOnPathNotFoundCode is the same as the errno
// case but for a 0.6.0 plugin that names the problem in a machine readable
// code.
func TestPluginClient_NoLegacyRetryOnPathNotFoundCode(t *testing.T) {
	var posts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/health" {
			_, _ = w.Write([]byte(healthJSON("book_metadata", "error_codes")))
			return
		}
		atomic.AddInt32(&posts, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"cannot open the file","code":"path_not_found"}`))
	}))
	defer srv.Close()

	c := NewPluginClient(srv.URL, "k")
	_, err := c.Add(context.Background(), "/books/a.epub", Metadata{Title: "Dune"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "path_not_found") {
		t.Errorf("error = %v, want it to carry the code", err)
	}
	if got := atomic.LoadInt32(&posts); got != 1 {
		t.Errorf("POST count = %d, want 1", got)
	}
}

// TestPluginClient_503BacksOffExponentially is review item 14: protocol.md
// specifies exponential backoff to roughly 30 seconds and the client retried
// exactly once after a flat two seconds.
func TestPluginClient_503BacksOffExponentially(t *testing.T) {
	if len(pluginRetryBackoff) < 5 {
		t.Fatalf("backoff schedule has %d steps, want at least 5", len(pluginRetryBackoff))
	}
	var total time.Duration
	for _, d := range pluginRetryBackoff {
		total += d
	}
	if total < 25*time.Second || total > 35*time.Second {
		t.Errorf("total backoff = %s, want roughly 30s", total)
	}
	for i := 1; i < 4; i++ {
		if pluginRetryBackoff[i] < pluginRetryBackoff[i-1]*2 {
			t.Errorf("step %d (%s) does not at least double step %d (%s)", i, pluginRetryBackoff[i], i-1, pluginRetryBackoff[i-1])
		}
	}

	shrinkBackoff(t)
	var posts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/health" {
			_, _ = w.Write([]byte(healthJSON("book_metadata")))
			return
		}
		if atomic.AddInt32(&posts, 1) < 6 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"library not ready","code":"db_unavailable"}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":11}`))
	}))
	defer srv.Close()

	c := NewPluginClient(srv.URL, "k")
	id, err := c.Add(context.Background(), "/a.epub", Metadata{Title: "Dune"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if id != 11 {
		t.Errorf("id = %d, want 11", id)
	}
	if got := atomic.LoadInt32(&posts); got != 6 {
		t.Errorf("POST count = %d, want 6 (one attempt plus five retries)", got)
	}
}

// TestPluginClient_CapabilityCacheExpires is review item 16: upgrading the
// plugin underneath a running Bindery used to leave the client sending legacy
// payloads until the process restarted.
func TestPluginClient_CapabilityCacheExpires(t *testing.T) {
	prev := pluginCapabilityTTL
	pluginCapabilityTTL = 20 * time.Millisecond
	t.Cleanup(func() { pluginCapabilityTTL = prev })

	var upgraded atomic.Bool
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/health" {
			if upgraded.Load() {
				_, _ = w.Write([]byte(healthJSON("book_metadata")))
			} else {
				_, _ = w.Write([]byte(healthJSON()))
			}
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":1}`))
	}))
	defer srv.Close()

	c := NewPluginClient(srv.URL, "k")
	if _, err := c.Add(context.Background(), "/a.epub", Metadata{Title: "Dune"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, ok := bodies[0]["metadata"]; ok {
		t.Fatalf("pre-upgrade push should be legacy: %+v", bodies[0])
	}

	upgraded.Store(true)
	time.Sleep(40 * time.Millisecond)
	if _, err := c.Add(context.Background(), "/b.epub", Metadata{Title: "Dune"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, ok := bodies[1]["metadata"]; !ok {
		t.Fatalf("post-upgrade push should carry metadata once the cache expires: %+v", bodies[1])
	}
}

// TestPluginClient_CapabilityCacheInvalidatedOnTransportError covers the other
// half of item 16: a plugin that went away and came back upgraded must be
// re-probed rather than answered from a stale cache.
func TestPluginClient_CapabilityCacheInvalidatedOnTransportError(t *testing.T) {
	var caps atomic.Value
	caps.Store(healthJSON("book_metadata"))
	var refuse atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/health" {
			_, _ = w.Write([]byte(caps.Load().(string)))
			return
		}
		if refuse.Load() {
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Error("response writer is not a hijacker")
				return
			}
			conn, _, err := hj.Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":1}`))
	}))
	defer srv.Close()

	c := NewPluginClient(srv.URL, "k")
	if _, err := c.Add(context.Background(), "/a.epub", Metadata{Title: "Dune"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if !c.capabilitiesFresh() {
		t.Fatal("capabilities should be cached after a successful add")
	}

	refuse.Store(true)
	if _, err := c.Add(context.Background(), "/b.epub", Metadata{Title: "Dune"}); err == nil {
		t.Fatal("expected a transport error")
	}
	if c.capabilitiesFresh() {
		t.Error("a transport error must invalidate the capability cache")
	}
}

// TestPluginClient_CoverPathIsRemapped is review item 9: a cover path sent
// without the operator's push path remap breaks on exactly the cross container
// mount mismatch the remap exists to fix.
func TestPluginClient_CoverPathIsRemapped(t *testing.T) {
	var got struct {
		Path     string   `json:"path"`
		Metadata Metadata `json:"metadata"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/health" {
			_, _ = w.Write([]byte(healthJSON("book_metadata", "cover")))
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":1}`))
	}))
	defer srv.Close()

	c := NewPluginClient(srv.URL, "k").WithPushPathRemap("/books:/mnt/books")
	if _, err := c.Add(context.Background(), "/books/a.epub", Metadata{Title: "Dune", CoverPath: "/books/.covers/a.jpg"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if got.Metadata.CoverPath != "/mnt/books/.covers/a.jpg" {
		t.Errorf("coverPath = %q, want the remapped path", got.Metadata.CoverPath)
	}
}

// TestPluginClient_CoverPathDroppedWithoutCapability keeps the compatibility
// rule: a plugin that does not advertise `cover` never sees a coverPath.
func TestPluginClient_CoverPathDroppedWithoutCapability(t *testing.T) {
	var got struct {
		Metadata Metadata `json:"metadata"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/health" {
			_, _ = w.Write([]byte(healthJSON("book_metadata")))
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":1}`))
	}))
	defer srv.Close()

	c := NewPluginClient(srv.URL, "k")
	if _, err := c.Add(context.Background(), "/books/a.epub", Metadata{Title: "Dune", CoverPath: "/books/.covers/a.jpg"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if got.Metadata.CoverPath != "" {
		t.Errorf("coverPath = %q, want it dropped", got.Metadata.CoverPath)
	}
}

// TestPluginClient_ProbePath covers the new GET /v1/paths endpoint, including
// that the probe goes through the same remap as a push (otherwise it would
// report on a path no push ever sends).
func TestPluginClient_ProbePath(t *testing.T) {
	var gotQuery, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/health" {
			_, _ = w.Write([]byte(healthJSON("book_metadata", "path_probe")))
			return
		}
		if r.URL.Path != "/v1/paths" {
			t.Errorf("path = %q", r.URL.Path)
		}
		gotQuery = r.URL.Query().Get("path")
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"path":"/mnt/books","exists":true,"readable":true,"isDir":true}`))
	}))
	defer srv.Close()

	c := NewPluginClient(srv.URL, "secret").WithPushPathRemap("/books:/mnt/books")
	if !c.SupportsPathProbe(context.Background()) {
		t.Fatal("SupportsPathProbe = false, want true")
	}
	probe, err := c.ProbePath(context.Background(), "/books")
	if err != nil {
		t.Fatalf("ProbePath: %v", err)
	}
	if gotQuery != "/mnt/books" {
		t.Errorf("probed path = %q, want the remapped path", gotQuery)
	}
	if gotAuth != "Bearer secret" {
		t.Errorf("Authorization = %q, want the bearer token", gotAuth)
	}
	if !probe.Exists || !probe.IsDir || !probe.Readable {
		t.Errorf("probe = %+v, want all true", probe)
	}
}

func TestPluginClient_ProbePathReportsMissingPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/health" {
			_, _ = w.Write([]byte(healthJSON("path_probe")))
			return
		}
		_, _ = w.Write([]byte(`{"path":"/books","exists":false,"readable":false,"isDir":false}`))
	}))
	defer srv.Close()

	c := NewPluginClient(srv.URL, "k")
	probe, err := c.ProbePath(context.Background(), "/books")
	if err != nil {
		t.Fatalf("ProbePath: %v", err)
	}
	if probe.Exists {
		t.Error("exists = true, want false")
	}
}

// TestPluginClient_UpdateMetadata covers review item 13's client half: a 409
// stops being a dead end only if there is a way to write to an existing row.
func TestPluginClient_UpdateMetadata(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody Metadata
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/health" {
			_, _ = w.Write([]byte(healthJSON("book_metadata", "metadata_update")))
			return
		}
		gotMethod, gotPath = r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"id":42,"updated":true,"fields":["series"]}`))
	}))
	defer srv.Close()

	c := NewPluginClient(srv.URL, "k")
	if !c.SupportsMetadataUpdate(context.Background()) {
		t.Fatal("SupportsMetadataUpdate = false, want true")
	}
	fields, err := c.UpdateMetadata(context.Background(), 42, Metadata{Title: "Dune", Series: "Dune Chronicles"})
	if err != nil {
		t.Fatalf("UpdateMetadata: %v", err)
	}
	if len(fields) != 1 || fields[0] != "series" {
		t.Errorf("fields = %v, want the list of what was written", fields)
	}
	if gotMethod != http.MethodPatch || gotPath != "/v1/books/42" {
		t.Errorf("request = %s %s, want PATCH /v1/books/42", gotMethod, gotPath)
	}
	if gotBody.Series != "Dune Chronicles" {
		t.Errorf("body = %+v, want the metadata object", gotBody)
	}
}

func TestPluginClient_UpdateMetadataMissingBook(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/health" {
			_, _ = w.Write([]byte(healthJSON("metadata_update")))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"no such book","code":"not_found"}`))
	}))
	defer srv.Close()

	c := NewPluginClient(srv.URL, "k")
	_, err := c.UpdateMetadata(context.Background(), 42, Metadata{Title: "Dune"})
	if !errors.Is(err, ErrCalibreBookMissing) {
		t.Fatalf("error = %v, want ErrCalibreBookMissing", err)
	}
}

// TestPluginClient_UpdateMetadataRemapsCoverPath keeps the cover remap rule on
// the update path too.
func TestPluginClient_UpdateMetadataRemapsCoverPath(t *testing.T) {
	var got Metadata
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/health" {
			_, _ = w.Write([]byte(healthJSON("metadata_update", "cover")))
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"id":42,"updated":true,"fields":[]}`))
	}))
	defer srv.Close()

	c := NewPluginClient(srv.URL, "k").WithPushPathRemap("/books:/mnt/books")
	if _, err := c.UpdateMetadata(context.Background(), 42, Metadata{Title: "Dune", CoverPath: "/books/c.jpg"}); err != nil {
		t.Fatalf("UpdateMetadata: %v", err)
	}
	if got.CoverPath != "/mnt/books/c.jpg" {
		t.Errorf("coverPath = %q, want the remapped path", got.CoverPath)
	}
}

// TestPluginClient_CapabilitiesAbsentDegrade is the compatibility rule stated
// once: against a plugin advertising nothing, every new call path either
// reports the capability as missing or refuses rather than guessing.
func TestPluginClient_CapabilitiesAbsentDegrade(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/health" {
			_, _ = w.Write([]byte(`{"plugin_version":"0.4.0","calibre_version":"9.7","library":"/books"}`))
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := NewPluginClient(srv.URL, "k")
	ctx := context.Background()
	if c.SupportsPathProbe(ctx) {
		t.Error("SupportsPathProbe = true against a 0.4.0 plugin")
	}
	if c.SupportsMetadataUpdate(ctx) {
		t.Error("SupportsMetadataUpdate = true against a 0.4.0 plugin")
	}
	if c.SupportsCover(ctx) {
		t.Error("SupportsCover = true against a 0.4.0 plugin")
	}
}
