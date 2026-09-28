package calibre

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// addFormatServer is a bridge that records every POST body and answers a
// push with formatAdded as calibre-bridge 0.7.0 does.
type addFormatServer struct {
	mu     sync.Mutex
	bodies []map[string]any
}

func (s *addFormatServer) start(t *testing.T, caps []string, answer string, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/health" {
			_, _ = w.Write([]byte(healthJSON(caps...)))
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("request body %q is not JSON: %v", raw, err)
		}
		s.mu.Lock()
		s.bodies = append(s.bodies, body)
		s.mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(answer))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (s *addFormatServer) last(t *testing.T) map[string]any {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.bodies) == 0 {
		t.Fatal("no push reached the bridge")
	}
	return s.bodies[len(s.bodies)-1]
}

func TestPluginClient_SupportsAddFormat(t *testing.T) {
	for name, tc := range map[string]struct {
		caps []string
		want bool
	}{
		"advertised":     {caps: []string{"book_metadata", "add_format"}, want: true},
		"not advertised": {caps: []string{"book_metadata", "metadata_update"}, want: false},
	} {
		t.Run(name, func(t *testing.T) {
			var s addFormatServer
			srv := s.start(t, tc.caps, `{"id":1}`, http.StatusCreated)
			if got := NewPluginClient(srv.URL, "k").SupportsAddFormat(context.Background()); got != tc.want {
				t.Errorf("SupportsAddFormat = %v, want %v", got, tc.want)
			}
		})
	}
}

// addFormat must reach only a bridge that said it understands it. An older
// bridge ignores unknown fields today, but the protocol makes no promise, and
// a push that does not ask for it has to be byte for byte what it was before.
func TestPluginClient_AddFormatOnlySentWhenAdvertised(t *testing.T) {
	meta := Metadata{Title: "Dune", Identifiers: map[string]string{"bindery": "1"}}
	cases := []struct {
		name string
		caps []string
		opts AddOptions
		want bool
	}{
		{name: "asked and advertised", caps: []string{"book_metadata", "add_format"}, opts: AddOptions{AddFormat: true}, want: true},
		{name: "asked but not advertised", caps: []string{"book_metadata"}, opts: AddOptions{AddFormat: true}, want: false},
		{name: "advertised but not asked", caps: []string{"book_metadata", "add_format"}, opts: AddOptions{}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var s addFormatServer
			srv := s.start(t, tc.caps, `{"id":7,"duplicate":false}`, http.StatusCreated)
			if _, err := NewPluginClient(srv.URL, "k").AddWithOptions(context.Background(), "/a.pdf", meta, tc.opts); err != nil {
				t.Fatalf("AddWithOptions: %v", err)
			}
			v, present := s.last(t)["addFormat"]
			if present != tc.want || (present && v != true) {
				t.Errorf("addFormat on the wire = %v (present %v), want present %v", v, present, tc.want)
			}
		})
	}
}

// The legacy retry drops the metadata object, not the question of which row
// the file belongs on.
func TestPluginClient_AddFormatSurvivesTheLegacyRetry(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/health" {
			_, _ = w.Write([]byte(healthJSON("book_metadata", "add_format")))
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		bodies = append(bodies, body)
		n := len(bodies)
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"error":"bad metadata"}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":7,"duplicate":false,"format_added":true}`))
	}))
	defer srv.Close()

	res, err := NewPluginClient(srv.URL, "k").AddWithOptions(context.Background(), "/a.pdf",
		Metadata{Title: "Dune"}, AddOptions{AddFormat: true})
	if err != nil {
		t.Fatalf("AddWithOptions: %v", err)
	}
	if len(bodies) != 2 {
		t.Fatalf("posts = %d, want the rejected one and the legacy retry", len(bodies))
	}
	if _, ok := bodies[1]["metadata"]; ok {
		t.Errorf("legacy retry still carried metadata: %v", bodies[1])
	}
	if bodies[1]["addFormat"] != true {
		t.Errorf("legacy retry dropped addFormat: %v", bodies[1])
	}
	if !res.FormatAdded {
		t.Error("FormatAdded = false after a retry that answered format_added")
	}
}

func TestPluginClient_FormatAddedIsParsed(t *testing.T) {
	cases := []struct {
		name      string
		answer    string
		status    int
		wantID    int64
		wantAdded bool
		wantErr   error
	}{
		{name: "format added", answer: `{"id":7,"duplicate":false,"format_added":true}`, status: http.StatusCreated, wantID: 7, wantAdded: true},
		{name: "fresh row", answer: `{"id":8,"duplicate":false}`, status: http.StatusCreated, wantID: 8},
		{name: "same format again", answer: `{"id":7,"duplicate":true}`, status: http.StatusConflict, wantID: 7, wantErr: ErrAlreadyInCalibre},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var s addFormatServer
			srv := s.start(t, []string{"book_metadata", "add_format"}, tc.answer, tc.status)
			c := NewPluginClient(srv.URL, "k")
			res, err := c.AddWithOptions(context.Background(), "/a.pdf", Metadata{Title: "Dune"}, AddOptions{AddFormat: true})
			if !errors.Is(err, tc.wantErr) || (tc.wantErr == nil && err != nil) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if res.ID != tc.wantID || res.FormatAdded != tc.wantAdded {
				t.Errorf("result = %+v, want id %d formatAdded %v", res, tc.wantID, tc.wantAdded)
			}
			// Add is the same call with no options, and keeps its old shape.
			id, err := c.Add(context.Background(), "/b.pdf", Metadata{Title: "Dune"})
			if id != tc.wantID || !errors.Is(err, tc.wantErr) {
				t.Errorf("Add = %d, %v; want %d, %v", id, err, tc.wantID, tc.wantErr)
			}
		})
	}
}
