package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vavallee/bindery/internal/db"
)

func pluginStub(t *testing.T, caps string, probe func(path string) (int, string)) *httptest.Server {
	t.Helper()
	return pluginStubVersion(t, "0.6.0", caps, probe)
}

// pluginStubVersion is pluginStub with the bridge version it reports.
func pluginStubVersion(t *testing.T, version, caps string, probe func(path string) (int, string)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/health":
			_, _ = w.Write([]byte(`{"plugin_version":"` + version + `","calibre_version":"9.8","library":"/calibre-library","capabilities":[` + caps + `]}`))
		case "/v1/paths":
			if probe == nil {
				t.Errorf("unexpected path probe for %q", r.URL.Query().Get("path"))
				w.WriteHeader(http.StatusNotFound)
				return
			}
			status, body := probe(r.URL.Query().Get("path"))
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testConnection(t *testing.T, h *CalibreHandler) (int, map[string]string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.Test(rec, httptest.NewRequest(http.MethodPost, "/api/v1/calibre/test", nil))
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

// TestCalibre_Test_ReportsAnUnreachableLibraryRoot is review item 5. Test
// connection only called Health, so it reported "plugin reachable" while
// every push was about to fail with "No such file or directory" because the
// Calibre container mounts the library somewhere else (#1346).
func TestCalibre_Test_ReportsAnUnreachableLibraryRoot(t *testing.T) {
	h, repo, ctx := calibreFixture(t)
	root := t.TempDir()
	srv := pluginStub(t, `"book_metadata","path_probe"`, func(path string) (int, string) {
		if path != root {
			t.Errorf("probed %q, want %q", path, root)
		}
		return http.StatusOK, `{"path":"` + path + `","exists":false,"readable":false,"isDir":false}`
	})
	if err := repo.Set(ctx, SettingCalibreMode, "plugin"); err != nil {
		t.Fatal(err)
	}
	if err := repo.Set(ctx, SettingCalibrePluginURL, srv.URL); err != nil {
		t.Fatal(err)
	}
	h = h.WithLibraryRoot(root)

	code, body := testConnection(t, h)
	if code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %v", code, body)
	}
	if !strings.Contains(body["error"], root) {
		t.Errorf("error = %q, want it to name the path", body["error"])
	}
	if !strings.Contains(body["error"], "push path remap") {
		t.Errorf("error = %q, want it to name the remedy", body["error"])
	}
}

// TestCalibre_Test_PassesWhenTheRootIsVisible keeps the happy path honest:
// the success message now says the path was checked, not just that the plugin
// answered.
func TestCalibre_Test_PassesWhenTheRootIsVisible(t *testing.T) {
	h, repo, ctx := calibreFixture(t)
	root := t.TempDir()
	srv := pluginStub(t, `"book_metadata","path_probe"`, func(path string) (int, string) {
		return http.StatusOK, `{"path":"` + path + `","exists":true,"readable":true,"isDir":true}`
	})
	if err := repo.Set(ctx, SettingCalibreMode, "plugin"); err != nil {
		t.Fatal(err)
	}
	if err := repo.Set(ctx, SettingCalibrePluginURL, srv.URL); err != nil {
		t.Fatal(err)
	}
	h = h.WithLibraryRoot(root)

	code, body := testConnection(t, h)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %v", code, body)
	}
	if !strings.Contains(body["message"], root) {
		t.Errorf("message = %q, want it to name the path it checked", body["message"])
	}
}

// TestCalibre_Test_AppliesTheRemapBeforeProbing: a probe that skipped the
// remap would report on a path no push ever sends.
func TestCalibre_Test_AppliesTheRemapBeforeProbing(t *testing.T) {
	h, repo, ctx := calibreFixture(t)
	root := t.TempDir()
	var probed string
	srv := pluginStub(t, `"path_probe"`, func(path string) (int, string) {
		probed = path
		return http.StatusOK, `{"path":"` + path + `","exists":true,"readable":true,"isDir":true}`
	})
	for k, v := range map[string]string{
		SettingCalibreMode:          "plugin",
		SettingCalibrePluginURL:     srv.URL,
		SettingCalibrePushPathRemap: root + ":/mnt/books",
	} {
		if err := repo.Set(ctx, k, v); err != nil {
			t.Fatal(err)
		}
	}
	h = h.WithLibraryRoot(root)

	if code, body := testConnection(t, h); code != http.StatusOK {
		t.Fatalf("status = %d: %v", code, body)
	}
	if probed != "/mnt/books" {
		t.Errorf("probed %q, want the remapped path", probed)
	}
}

// TestCalibre_Test_FallsBackWithoutTheCapability holds the compatibility rule:
// an older plugin gets exactly today's answer, with no probe attempted.
func TestCalibre_Test_FallsBackWithoutTheCapability(t *testing.T) {
	h, repo, ctx := calibreFixture(t)
	srv := pluginStub(t, `"book_metadata"`, nil)
	if err := repo.Set(ctx, SettingCalibreMode, "plugin"); err != nil {
		t.Fatal(err)
	}
	if err := repo.Set(ctx, SettingCalibrePluginURL, srv.URL); err != nil {
		t.Fatal(err)
	}
	h = h.WithLibraryRoot(t.TempDir())

	code, body := testConnection(t, h)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %v", code, body)
	}
	if body["message"] != "plugin reachable" {
		t.Errorf("message = %q, want the pre-0.6.0 message", body["message"])
	}
}

// probeReply renders a /v1/paths body. json.Marshal, not string concatenation,
// because a share path's backslashes must be escaped to stay valid JSON.
func probeReply(path string, exists, readable, isDir bool) string {
	b, _ := json.Marshal(map[string]any{"path": path, "exists": exists, "readable": readable, "isDir": isDir})
	return string(b)
}

// libraryWithBook makes a library root holding Author/Book.epub, the two
// level layout the #2831 report used, and returns the root.
func libraryWithBook(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "Author"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Author", "Book.epub"), []byte("epub"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// pluginModeWithRemap points the handler's settings at srv with the given
// push path remap.
func pluginModeWithRemap(t *testing.T, repo *db.SettingsRepo, srvURL, remap string) {
	t.Helper()
	ctx := context.Background()
	for k, v := range map[string]string{
		SettingCalibreMode:          "plugin",
		SettingCalibrePluginURL:     srvURL,
		SettingCalibrePushPathRemap: remap,
	} {
		if err := repo.Set(ctx, k, v); err != nil {
			t.Fatal(err)
		}
	}
}

// onlyRootVisible answers the root's wire path as a readable directory and
// every other path as missing: the remap that is right for the root and wrong
// for everything under it.
func onlyRootVisible(rootWire string) func(string) (int, string) {
	return func(path string) (int, string) {
		if path == rootWire {
			return http.StatusOK, probeReply(path, true, true, true)
		}
		return http.StatusOK, probeReply(path, false, false, false)
	}
}

// TestCalibre_Test_ProbesARealBookThroughTheRemap is #2831. Probing only the
// library root is an exact prefix match, so it skips the remap's join and
// passed while every real push was about to fail. Test must also probe one
// book, name the joined wire path, and quote it without doubling backslashes.
func TestCalibre_Test_ProbesARealBookThroughTheRemap(t *testing.T) {
	h, repo, _ := calibreFixture(t)
	root := libraryWithBook(t)
	shareRoot := `\\nas\share\BOOKS`
	var probed []string
	probe := onlyRootVisible(shareRoot)
	srv := pluginStubVersion(t, "0.6.2", `"path_probe"`, func(path string) (int, string) {
		probed = append(probed, path)
		return probe(path)
	})
	pluginModeWithRemap(t, repo, srv.URL, root+":"+shareRoot)
	h = h.WithLibraryRoot(root)

	code, body := testConnection(t, h)
	if code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 because the book is not visible: %v (probed %q)", code, body, probed)
	}
	wantWire := `\\nas\share\BOOKS\Author\Book.epub`
	if !strings.Contains(body["error"], `"`+wantWire+`"`) {
		t.Errorf("error = %s\nwant it to name the joined wire path %s in plain quotes", body["error"], wantWire)
	}
	if !strings.Contains(body["error"], "covers the root but not the book") {
		t.Errorf("error = %s\nwant it to say the remap covers the root but not the book", body["error"])
	}
	if !strings.Contains(body["error"], "other root folders") {
		t.Errorf("error = %s\nwant it to suggest checking other root folders", body["error"])
	}
	if body["sample"] != wantWire {
		t.Errorf("sample = %q, want %q", body["sample"], wantWire)
	}
	if strings.Contains(body["error"], "mapped drive") {
		t.Errorf("error = %s\na share path must not get the mapped drive hint", body["error"])
	}
}

// TestCalibre_Test_ExplainsAMappedDriveLetter: a drive letter mapped in one
// Windows logon session is invisible to a Calibre running in another, which
// is the most common way a Windows remap that looks right still fails.
func TestCalibre_Test_ExplainsAMappedDriveLetter(t *testing.T) {
	h, repo, _ := calibreFixture(t)
	root := libraryWithBook(t)
	srv := pluginStubVersion(t, "0.6.2", `"path_probe"`, onlyRootVisible(`S:\BOOKS`))
	pluginModeWithRemap(t, repo, srv.URL, root+`:S:\BOOKS`)
	h = h.WithLibraryRoot(root)

	code, body := testConnection(t, h)
	if code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %v", code, body)
	}
	if !strings.Contains(body["error"], `"S:\BOOKS\Author\Book.epub"`) {
		t.Errorf("error = %s\nwant the drive letter wire path in plain quotes", body["error"])
	}
	if !strings.Contains(body["error"], "A mapped drive belongs to one Windows logon session") {
		t.Errorf("error = %s\nwant the mapped drive explanation", body["error"])
	}
	if !strings.Contains(body["error"], `\\nas\share\books`) {
		t.Errorf("error = %s\nwant it to suggest a share address", body["error"])
	}
}

// TestCalibre_Test_QuotesAShareRootWithoutDoublingBackslashes: %q escaped every
// backslash, so an invisible share root read as \\\\nas\\share\\BOOKS, which
// is not a path anyone can paste into Explorer.
func TestCalibre_Test_QuotesAShareRootWithoutDoublingBackslashes(t *testing.T) {
	h, repo, _ := calibreFixture(t)
	root := t.TempDir()
	srv := pluginStubVersion(t, "0.6.2", `"path_probe"`, func(path string) (int, string) {
		return http.StatusOK, probeReply(path, false, false, false)
	})
	pluginModeWithRemap(t, repo, srv.URL, root+`:\\nas\share\BOOKS`)
	h = h.WithLibraryRoot(root)

	code, body := testConnection(t, h)
	if code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %v", code, body)
	}
	if !strings.Contains(body["error"], `cannot see "\\nas\share\BOOKS"`) {
		t.Errorf("error = %s\nwant the share root quoted as typed", body["error"])
	}
}

// TestCalibre_Test_PassesWhenTheBookIsVisible: the happy path names the book it
// checked, and reports it as sample so the UI can show it.
func TestCalibre_Test_PassesWhenTheBookIsVisible(t *testing.T) {
	h, repo, _ := calibreFixture(t)
	root := libraryWithBook(t)
	srv := pluginStubVersion(t, "0.6.2", `"path_probe"`, func(path string) (int, string) {
		return http.StatusOK, probeReply(path, true, true, !strings.HasSuffix(path, ".epub"))
	})
	pluginModeWithRemap(t, repo, srv.URL, root+":/mnt/books")
	h = h.WithLibraryRoot(root)

	code, body := testConnection(t, h)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %v", code, body)
	}
	if body["sample"] != "/mnt/books/Author/Book.epub" {
		t.Errorf("sample = %q, want the remapped book path", body["sample"])
	}
	if !strings.Contains(body["message"], `"/mnt/books/Author/Book.epub"`) {
		t.Errorf("message = %q, want it to name the book it checked", body["message"])
	}
}

// TestCalibre_Test_SaysWhenNoBookWasAvailable: an empty library can only have
// its root checked, and a green result must say that is all it proved.
func TestCalibre_Test_SaysWhenNoBookWasAvailable(t *testing.T) {
	h, repo, _ := calibreFixture(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "Empty Author"), 0o755); err != nil {
		t.Fatal(err)
	}
	srv := pluginStubVersion(t, "0.6.2", `"path_probe"`, func(path string) (int, string) {
		return http.StatusOK, probeReply(path, true, true, true)
	})
	pluginModeWithRemap(t, repo, srv.URL, "")
	h = h.WithLibraryRoot(root)

	code, body := testConnection(t, h)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %v", code, body)
	}
	if !strings.Contains(body["message"], "No imported book was found to test, so only the library root was checked") {
		t.Errorf("message = %q, want it to say only the root was checked", body["message"])
	}
	if body["sample"] != "" {
		t.Errorf("sample = %q, want none", body["sample"])
	}
}

// fakeEbookPaths is a recentEbookPathLister over a fixed list.
type fakeEbookPaths []string

func (f fakeEbookPaths) RecentEbookPaths(_ context.Context, limit int) ([]string, error) {
	if limit < len(f) {
		return f[:limit], nil
	}
	return f, nil
}

// TestCalibre_Test_PrefersTheNewestImportedBookThatExists: the sample comes
// from book_files, newest first, skipping rows whose file is gone, before the
// directory walk is tried.
func TestCalibre_Test_PrefersTheNewestImportedBookThatExists(t *testing.T) {
	h, repo, _ := calibreFixture(t)
	root := libraryWithBook(t) // the walk would find Author/Book.epub
	imported := filepath.Join(root, "Other", "Imported.epub")
	if err := os.MkdirAll(filepath.Dir(imported), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(imported, []byte("epub"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := pluginStubVersion(t, "0.6.2", `"path_probe"`, func(path string) (int, string) {
		return http.StatusOK, probeReply(path, true, true, !strings.HasSuffix(path, ".epub"))
	})
	pluginModeWithRemap(t, repo, srv.URL, "")
	h = h.WithLibraryRoot(root).WithBookFiles(fakeEbookPaths{
		filepath.Join(root, "Gone", "Deleted.epub"),
		imported,
	})

	code, body := testConnection(t, h)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %v", code, body)
	}
	if body["sample"] != imported {
		t.Errorf("sample = %q, want the newest imported book that exists, %q", body["sample"], imported)
	}
}

// TestCalibre_Test_WarnsAboutAnOldBridge: 0.6.1 fixes long share paths and
// 0.6.2 fixes the empty book a failed add leaves behind. The comparison is
// numeric, so 0.10.0 is newer, not older.
func TestCalibre_Test_WarnsAboutAnOldBridge(t *testing.T) {
	for _, tc := range []struct {
		version string
		warn    bool
	}{
		{"0.6.1", true},
		{"0.5.9", true},
		{"0.6.2", false},
		{"0.10.0", false},
		{"1.0.0", false},
	} {
		t.Run(tc.version, func(t *testing.T) {
			h, repo, _ := calibreFixture(t)
			srv := pluginStubVersion(t, tc.version, `"book_metadata"`, nil)
			pluginModeWithRemap(t, repo, srv.URL, "")

			code, body := testConnection(t, h)
			if code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %v", code, body)
			}
			got := body["warning"]
			namesFixes := strings.Contains(got, "0.6.1") && strings.Contains(got, "0.6.2") && strings.Contains(got, tc.version)
			if tc.warn && !namesFixes {
				t.Errorf("warning = %q, want it to name %s and what 0.6.1 and 0.6.2 fix", got, tc.version)
			}
			if !tc.warn && got != "" {
				t.Errorf("warning = %q, want none for %s", got, tc.version)
			}
		})
	}
}
