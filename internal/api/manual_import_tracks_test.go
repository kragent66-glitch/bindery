package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/vavallee/bindery/internal/db"
	"github.com/vavallee/bindery/internal/importer"
	"github.com/vavallee/bindery/internal/models"
)

// Tests for the Discord report behind #2935: a folder of 45 audiobook tracks
// imported through Import, From a folder, one row per track, all assigned the
// same book. Only two tracks reached the library, in two separate folders,
// and the other 43 were reported imported while still sitting in the source.

const trackCount = 45

// trackImportFixture wires the real importer behind the manual import handler,
// so the batch endpoint runs the same placement code a user's import does.
func trackImportFixture(t *testing.T, mode, template string) (h *ManualImportHandler, book *models.Book, books *db.BookRepo, downloads *db.DownloadRepo, library, src string) {
	t.Helper()
	database, err := db.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	ctx := context.Background()
	authors := db.NewAuthorRepo(database)
	books = db.NewBookRepo(database)
	downloads = db.NewDownloadRepo(database)
	settings := db.NewSettingsRepo(database)
	if err := settings.Set(ctx, "import.mode", mode); err != nil {
		t.Fatal(err)
	}
	if template != "" {
		if err := settings.Set(ctx, "naming.audiobook_file_template", template); err != nil {
			t.Fatal(err)
		}
	}
	author := &models.Author{ForeignID: "OL-MIT-A", Name: "Zogarth", SortName: "Zogarth"}
	if err := authors.Create(ctx, author); err != nil {
		t.Fatal(err)
	}
	book = &models.Book{ForeignID: "OL-MIT-W", AuthorID: author.ID, Title: "The Primal Hunter", Status: models.BookStatusWanted, MediaType: models.MediaTypeAudiobook}
	if err := books.Create(ctx, book); err != nil {
		t.Fatal(err)
	}

	library = resolvedTempDir(t)
	src = filepath.Join(resolvedTempDir(t), "The Primal Hunter")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= trackCount; i++ {
		p := filepath.Join(src, fmt.Sprintf("%02d - The Primal Hunter.mp3", i))
		if err := os.WriteFile(p, []byte(fmt.Sprintf("track-%02d", i)), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	scanner := importer.NewScanner(downloads, db.NewDownloadClientRepo(database), books, authors, db.NewHistoryRepo(database),
		library, library, "", "", "").WithSettings(settings)
	h = NewManualImportHandler(scanner, downloads, books)
	return h, book, books, downloads, library, src
}

// submitTracksOneRowEach posts what the Import page sent for the report: one
// batch item per track, every one bound to the same book.
func submitTracksOneRowEach(t *testing.T, h *ManualImportHandler, src string, bookID int64) BatchImportResponse {
	t.Helper()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	items := make([]BatchImportItem, 0, len(entries))
	for _, e := range entries {
		items = append(items, BatchImportItem{Path: filepath.Join(src, e.Name()), BookID: bookID, Format: models.MediaTypeAudiobook})
	}
	body, _ := json.Marshal(items)
	rec := httptest.NewRecorder()
	h.ImportBatch(rec, httptest.NewRequest(http.MethodPost, "/api/v1/queue/manual-import/batch", bytes.NewReader(body)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body = %s", rec.Code, rec.Body.String())
	}
	var resp BatchImportResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	waitImportsSettled(t, h.downloads)
	return resp
}

// waitImportsSettled polls until no download is still waiting on or running
// its import. Batch imports run on background goroutines.
func waitImportsSettled(t *testing.T, downloads *db.DownloadRepo) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		all, err := downloads.List(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		busy := false
		for _, d := range all {
			switch d.Status {
			case models.StateCompleted, models.StateImportPending, models.StateImporting:
				busy = true
			}
		}
		if !busy {
			// Let the last import finish its post-status work (OPF sidecar).
			time.Sleep(50 * time.Millisecond)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("imports did not settle")
}

// audioUnder lists every audio file below dir, as paths relative to it.
func audioUnder(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && importer.IsAudioFile(p) {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, rel)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func bookFolders(t *testing.T, library string) []string {
	t.Helper()
	seen := map[string]bool{}
	for _, rel := range audioUnder(t, library) {
		seen[filepath.Dir(rel)] = true
	}
	var out []string
	for d := range seen {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

func TestManualImportBatch_TracksOfOneAudiobookLandTogether(t *testing.T) {
	for _, tc := range []struct {
		mode, template string
	}{
		{"move", ""},
		{"copy", ""},
		{"hardlink", ""},
		{"move", "{Author} - {Title} - Part {Part:3}.{ext}"},
		{"copy", "{Author} - {Title} - Part {Part:3}.{ext}"},
	} {
		t.Run(tc.mode+"/template="+fmt.Sprint(tc.template != ""), func(t *testing.T) {
			h, book, books, downloads, library, src := trackImportFixture(t, tc.mode, tc.template)
			ctx := context.Background()
			resp := submitTracksOneRowEach(t, h, src, book.ID)

			placed := audioUnder(t, library)
			folders := bookFolders(t, library)
			left := audioUnder(t, src)
			t.Logf("accepted=%d placed=%d folders=%v left in source=%d", resp.Accepted, len(placed), folders, len(left))

			if len(placed) != trackCount {
				t.Errorf("library holds %d tracks, want all %d", len(placed), trackCount)
			}
			if len(folders) != 1 {
				t.Errorf("tracks landed in %d folders %v, want one", len(folders), folders)
			}
			if tc.mode == "move" && len(left) != 0 {
				t.Errorf("%d tracks still in the source after a move import", len(left))
			}
			// Nothing may be lost: every track's content exists somewhere,
			// in the library or still in the source.
			if missing := missingTracks(t, library, src); len(missing) > 0 {
				t.Errorf("tracks %v exist nowhere after the import", missing)
			}
			// With a file template the tracks are numbered in playback order.
			if tc.template != "" && len(folders) == 1 {
				for i := 1; i <= trackCount; i++ {
					name := fmt.Sprintf("Zogarth - The Primal Hunter - Part %03d.mp3", i)
					got, err := os.ReadFile(filepath.Join(library, folders[0], name))
					if err != nil || string(got) != fmt.Sprintf("track-%02d", i) {
						t.Errorf("%s holds %q (%v), want track %02d", name, got, err, i)
					}
				}
			}

			files, err := books.ListFiles(ctx, book.ID)
			if err != nil {
				t.Fatal(err)
			}
			var recorded []string
			for _, f := range files {
				if f.Format == models.MediaTypeAudiobook {
					recorded = append(recorded, f.Path)
				}
			}
			if len(recorded) != 1 || len(folders) != 1 || recorded[0] != filepath.Join(library, folders[0]) {
				t.Errorf("book records audiobook paths %v, want the one folder %v", recorded, folders)
			}

			// No download may claim success for a track it did not place.
			all, err := downloads.List(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, d := range all {
				if d.Status != models.StateImported {
					t.Errorf("download %d status %q (%s), want imported", d.ID, d.Status, d.ErrorMessage)
				}
			}
			// The tracks are one audiobook, so one import record carries them.
			if len(all) != 1 {
				t.Errorf("%d import records for one audiobook, want 1", len(all))
			}
			if resp.Accepted != trackCount {
				t.Errorf("accepted %d rows, want %d", resp.Accepted, trackCount)
			}
			for _, r := range resp.Results {
				if !r.Accepted || len(all) == 0 || r.DownloadID != all[0].ID {
					t.Errorf("row %s: accepted=%v download=%d, want accepted under the one record", r.Path, r.Accepted, r.DownloadID)
				}
			}
		})
	}
}

// missingTracks returns the numbers of the fixture tracks whose content is in
// no file under library or src.
func missingTracks(t *testing.T, library, src string) []int {
	t.Helper()
	have := map[string]bool{}
	for _, dir := range []string{library, src} {
		for _, rel := range audioUnder(t, dir) {
			if b, err := os.ReadFile(filepath.Join(dir, rel)); err == nil {
				have[string(b)] = true
			}
		}
	}
	var missing []int
	for i := 1; i <= trackCount; i++ {
		if !have[fmt.Sprintf("track-%02d", i)] {
			missing = append(missing, i)
		}
	}
	return missing
}

// TestManualImportSingle_TracksOneRequestEachNeverLoseFiles covers the other
// way to import track rows: the per-row Import button, one request per track,
// all in flight at once. Those are separate imports, so the first places the
// audiobook and every later one must refuse with a reason rather than race it
// into a second folder, overwrite its file, or claim an import that moved
// nothing.
func TestManualImportSingle_TracksOneRequestEachNeverLoseFiles(t *testing.T) {
	h, book, books, downloads, library, src := trackImportFixture(t, "move", "{Author} - {Title} - Part {Part:3}.{ext}")
	ctx := context.Background()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		body, _ := json.Marshal(map[string]any{"path": filepath.Join(src, e.Name()), "bookId": book.ID, "format": models.MediaTypeAudiobook})
		rec := httptest.NewRecorder()
		h.Import(rec, httptest.NewRequest(http.MethodPost, "/api/v1/queue/manual-import", bytes.NewReader(body)))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d; body = %s", rec.Code, rec.Body.String())
		}
	}
	waitImportsSettled(t, downloads)

	placed := audioUnder(t, library)
	folders := bookFolders(t, library)
	left := audioUnder(t, src)
	t.Logf("placed=%d folders=%v left in source=%d", len(placed), folders, len(left))
	if len(placed) != 1 || len(folders) != 1 {
		t.Errorf("library holds %d tracks in %v, want the first track in one folder", len(placed), folders)
	}
	if len(left) != trackCount-1 {
		t.Errorf("%d tracks left in the source, want %d", len(left), trackCount-1)
	}
	if missing := missingTracks(t, library, src); len(missing) > 0 {
		t.Errorf("tracks %v exist nowhere after the import", missing)
	}

	all, err := downloads.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	imported, blocked := 0, 0
	for _, d := range all {
		switch d.Status {
		case models.StateImported:
			imported++
		case models.StateImportBlocked:
			blocked++
			if !strings.Contains(d.ErrorMessage, "already has an audiobook") {
				t.Errorf("download %d blocked with %q, want the already-has-an-audiobook reason", d.ID, d.ErrorMessage)
			}
		default:
			t.Errorf("download %d status %q", d.ID, d.Status)
		}
	}
	if imported != 1 || blocked != trackCount-1 {
		t.Errorf("imported=%d blocked=%d, want 1 and %d", imported, blocked, trackCount-1)
	}
	files, err := books.ListFiles(ctx, book.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Errorf("book records %d files, want 1", len(files))
	}
}

func TestIsAudiobookTrack(t *testing.T) {
	dir := resolvedTempDir(t)
	mp3 := filepath.Join(dir, "01.mp3")
	epub := filepath.Join(dir, "book.epub")
	writeTestFile(t, mp3)
	writeTestFile(t, epub)
	for _, tc := range []struct {
		path, format string
		want         bool
	}{
		{mp3, "", true},
		{mp3, models.MediaTypeAudiobook, true},
		{mp3, models.MediaTypeEbook, false},
		{epub, "", false},
		{epub, models.MediaTypeAudiobook, true}, // the person said so
		{dir, models.MediaTypeAudiobook, false}, // a folder is already one unit
	} {
		if got := isAudiobookTrack(tc.path, tc.format); got != tc.want {
			t.Errorf("isAudiobookTrack(%s, %q) = %v, want %v", filepath.Base(tc.path), tc.format, got, tc.want)
		}
	}
}

func TestDeepestCommonDir(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "data", "in")
	for _, tc := range []struct {
		files []string
		want  string
	}{
		{[]string{filepath.Join(root, "a", "1.mp3"), filepath.Join(root, "a", "2.mp3")}, filepath.Join(root, "a")},
		{[]string{filepath.Join(root, "a", "CD1", "1.mp3"), filepath.Join(root, "a", "CD2", "1.mp3")}, filepath.Join(root, "a")},
		{[]string{filepath.Join(root, "ab", "1.mp3"), filepath.Join(root, "a", "1.mp3")}, root},
	} {
		if got := deepestCommonDir(tc.files); got != tc.want {
			t.Errorf("deepestCommonDir(%v) = %q, want %q", tc.files, got, tc.want)
		}
	}
}
