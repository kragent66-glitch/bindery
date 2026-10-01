package importer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/vavallee/bindery/internal/downloader/qbittorrent"
	"github.com/vavallee/bindery/internal/models"
)

// TestQbittorrentFilesFor_ContentPathFallback covers #2878. Real qBittorrent
// names a multi-file torrent's files relative to save_path, so the names carry
// the root folder and save_path + name is the file on disk. rdt-client (and
// other qBittorrent-compatible clients that copied its shape) names them
// relative to content_path, so the same join lands one directory too high and
// automatic import found nothing. The resolver keeps save_path + name as the
// primary join and falls back to content_path + name only when the primary
// file is missing.
func TestQbittorrentFilesFor_ContentPathFallback(t *testing.T) {
	const release = "Example Book - Example Series, Book 2 - Example Author"

	type layout struct {
		// clientRoot is the directory the download client reports paths
		// under. Empty means "the same as the Bindery side" (no remap).
		clientRoot string
		saveRel    string // save_path relative to the root
		contentRel string // content_path relative to the root ("" = unset)
		trailing   bool   // append "/" to content_path, as rdt-client does
		files      []string
		onDisk     []string // files created under the Bindery-side root
	}

	tests := []struct {
		name   string
		layout layout
		// want is relative to the Bindery-side root.
		want []string
		// wantImportable is how many survive filterImportableFiles.
		wantImportable int
	}{
		{
			name: "real qBittorrent multi-file: names include the root, save_path join",
			layout: layout{
				saveRel:    "bindery",
				contentRel: "bindery/" + release,
				files:      []string{release + "/Example Book.m4b", release + "/Example Book.jpg"},
				onDisk:     []string{"bindery/" + release + "/Example Book.m4b", "bindery/" + release + "/Example Book.jpg"},
			},
			want:           []string{"bindery/" + release + "/Example Book.m4b"},
			wantImportable: 1,
		},
		{
			name: "rdt-client multi-file: names omit the root, content_path fallback",
			layout: layout{
				saveRel:    "bindery",
				contentRel: "bindery/" + release,
				trailing:   true,
				files:      []string{"Example Book.jpg", "Example Book.m4b"},
				onDisk:     []string{"bindery/" + release + "/Example Book.m4b", "bindery/" + release + "/Example Book.jpg"},
			},
			want:           []string{"bindery/" + release + "/Example Book.m4b"},
			wantImportable: 1,
		},
		{
			name: "single-file torrent: content_path is the file, save_path join",
			layout: layout{
				saveRel:    "bindery",
				contentRel: "bindery/Lone Book.epub",
				files:      []string{"Lone Book.epub"},
				onDisk:     []string{"bindery/Lone Book.epub"},
			},
			want:           []string{"bindery/Lone Book.epub"},
			wantImportable: 1,
		},
		{
			name: "file genuinely missing: primary path returned and filtered out",
			layout: layout{
				saveRel:    "bindery",
				contentRel: "bindery/" + release,
				trailing:   true,
				files:      []string{"Example Book.m4b"},
				onDisk:     []string{"bindery/" + release + "/Example Book.jpg"},
			},
			want:           []string{"bindery/Example Book.m4b"},
			wantImportable: 0,
		},
		{
			name: "content folder missing entirely: primary path returned and filtered out",
			layout: layout{
				saveRel:    "bindery",
				contentRel: "bindery/" + release,
				files:      []string{"Example Book.m4b"},
			},
			want:           []string{"bindery/Example Book.m4b"},
			wantImportable: 0,
		},
		{
			name: "path remap /data/downloads to the local mount applies to the fallback",
			layout: layout{
				clientRoot: "/data/downloads",
				saveRel:    "bindery",
				contentRel: "bindery/" + release,
				trailing:   true,
				files:      []string{"Example Book.jpg", "Example Book.m4b"},
				onDisk:     []string{"bindery/" + release + "/Example Book.m4b", "bindery/" + release + "/Example Book.jpg"},
			},
			want:           []string{"bindery/" + release + "/Example Book.m4b"},
			wantImportable: 1,
		},
		{
			name: "path remap with real qBittorrent names keeps the save_path join",
			layout: layout{
				clientRoot: "/data/downloads",
				saveRel:    "bindery",
				contentRel: "bindery/" + release,
				files:      []string{release + "/Example Book.m4b"},
				onDisk:     []string{"bindery/" + release + "/Example Book.m4b"},
			},
			want:           []string{"bindery/" + release + "/Example Book.m4b"},
			wantImportable: 1,
		},
		{
			// The no-double-root guard: a name already led by the content
			// folder is qBittorrent's own shape. Even when that file is gone,
			// joining it onto content_path would double the folder and could
			// pick up an unrelated file that happens to sit there.
			name: "root-led name missing under save_path never doubles the root folder",
			layout: layout{
				saveRel:    "bindery",
				contentRel: "bindery/" + release,
				files:      []string{release + "/Example Book.m4b"},
				onDisk:     []string{"bindery/" + release + "/" + release + "/Example Book.m4b"},
			},
			want:           []string{"bindery/" + release + "/Example Book.m4b"},
			wantImportable: 0,
		},
		{
			// The fallback is a fallback: when save_path + name exists it wins
			// even if content_path + name exists too.
			name: "save_path join preferred when both candidates exist",
			layout: layout{
				saveRel:    "bindery",
				contentRel: "bindery/" + release,
				files:      []string{"Example Book.m4b"},
				onDisk:     []string{"bindery/Example Book.m4b", "bindery/" + release + "/Example Book.m4b"},
			},
			want:           []string{"bindery/Example Book.m4b"},
			wantImportable: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			localRoot := t.TempDir()
			for _, rel := range tc.layout.onDisk {
				p := filepath.Join(localRoot, filepath.FromSlash(rel))
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte("data"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.MkdirAll(filepath.Join(localRoot, filepath.FromSlash(tc.layout.saveRel)), 0o755); err != nil {
				t.Fatal(err)
			}

			clientRoot := filepath.ToSlash(localRoot)
			client := &models.DownloadClient{Name: "qbit-compat", Type: "qbittorrent"}
			if tc.layout.clientRoot != "" {
				clientRoot = tc.layout.clientRoot
				client.PathRemap = tc.layout.clientRoot + ":" + localRoot
			}
			savePath := clientRoot + "/" + tc.layout.saveRel
			contentPath := ""
			if tc.layout.contentRel != "" {
				contentPath = clientRoot + "/" + tc.layout.contentRel
				if tc.layout.trailing {
					contentPath += "/"
				}
			}

			const hash = "28782878287828782878287828782878aaaabbbb"
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v2/auth/login":
					_, _ = w.Write([]byte("Ok."))
				case "/api/v2/torrents/files":
					out := make([]map[string]any, 0, len(tc.layout.files))
					for i, n := range tc.layout.files {
						out = append(out, map[string]any{"index": i, "name": n, "size": 4, "progress": 1})
					}
					_ = json.NewEncoder(w).Encode(out)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()

			host, port := scannerTestHostPort(t, srv.URL)
			qb := qbittorrent.New(host, port, "", "", "", false)
			s, _, _, _ := scannerFixture(t, t.TempDir())

			torrent := qbittorrent.Torrent{
				Hash:        hash,
				Name:        release,
				SavePath:    savePath,
				ContentPath: contentPath,
			}
			got := s.qbittorrentFilesFor(context.Background(), qb, client, torrent)

			want := make([]string, 0, len(tc.want))
			for _, rel := range tc.want {
				want = append(want, filepath.Join(localRoot, filepath.FromSlash(rel)))
			}
			if len(got) != len(want) {
				t.Fatalf("files: want %v, got %v", want, got)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Errorf("file[%d]: want %q, got %q", i, want[i], got[i])
				}
			}
			if n := len(filterImportableFiles(got)); n != tc.wantImportable {
				t.Errorf("importable files: want %d, got %d (%v)", tc.wantImportable, n, got)
			}
		})
	}
}
