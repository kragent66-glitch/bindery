package calibre

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vavallee/bindery/internal/covers"
	"github.com/vavallee/bindery/internal/models"
)

type coverTarget struct{ ok bool }

func (c coverTarget) SupportsCover(context.Context) bool { return c.ok }

// A remote cover is handed over from the cache to any target that says it
// can apply one. calibredb has no capability list and is assumed capable; a
// plugin that does not advertise `cover` gets nothing, and no fetch.
func TestCoverSource_RemoteCoverFollowsTheTargetCapability(t *testing.T) {
	ctx := context.Background()
	cacheDir := t.TempDir()
	imageURL := "https://93.184.216.34/cover.jpg"
	sum := sha256.Sum256([]byte(imageURL))
	cached := filepath.Join(cacheDir, fmt.Sprintf("%x.jpg", sum))
	if err := os.WriteFile(cached, []byte("cached cover"), 0o640); err != nil {
		t.Fatal(err)
	}
	src := CoverSource{CacheDir: cacheDir}

	if got := src.PathFor(ctx, imageURL, &fakeCalibredb{}); got != cached {
		t.Errorf("calibredb cover = %q, want %q", got, cached)
	}
	if got := src.PathFor(ctx, imageURL, coverTarget{ok: true}); got != cached {
		t.Errorf("capable plugin cover = %q, want %q", got, cached)
	}
	if got := src.PathFor(ctx, imageURL, coverTarget{ok: false}); got != "" {
		t.Errorf("incapable plugin cover = %q, want none", got)
	}
	if got := src.PathFor(ctx, "  ", coverTarget{ok: true}); got != "" {
		t.Errorf("blank source gave %q", got)
	}
}

// A bindery-cover: reference (#2564) resolves to the stored file and never
// goes through MaterializeCover, whose SSRF policy would refuse the scheme.
func TestCoverSource_StoredCoverResolvesFromTheStore(t *testing.T) {
	ctx := context.Background()
	store := covers.NewStore(filepath.Join(t.TempDir(), "covers"))
	src := filepath.Join(t.TempDir(), "cover.jpg")
	if err := os.WriteFile(src, testJPEG(), 0o600); err != nil {
		t.Fatal(err)
	}
	ref, err := store.Put(src)
	if err != nil {
		t.Fatal(err)
	}
	want, _, _ := store.Resolve(ref)

	withStore := CoverSource{Store: store, CacheDir: t.TempDir()}
	if got := withStore.PathFor(ctx, ref, coverTarget{ok: true}); got != want || strings.HasPrefix(got, covers.Scheme) {
		t.Errorf("stored cover = %q, want the file %q", got, want)
	}
	if got := withStore.PathFor(ctx, covers.Scheme+strings.Repeat("0", 64)+".jpg", coverTarget{ok: true}); got != "" {
		t.Errorf("absent stored cover gave %q, want none", got)
	}
	if got := (CoverSource{CacheDir: t.TempDir()}).PathFor(ctx, ref, coverTarget{ok: true}); got != "" {
		t.Errorf("no store gave %q, want none", got)
	}
}

// The bulk push never sent a cover. It now resolves one the same way the
// delivery worker does.
func TestSyncer_SendsTheCover(t *testing.T) {
	store := covers.NewStore(t.TempDir())
	src := filepath.Join(t.TempDir(), "cover.jpg")
	if err := os.WriteFile(src, testJPEG(), 0o600); err != nil {
		t.Fatal(err)
	}
	ref, err := store.Put(src)
	if err != nil {
		t.Fatal(err)
	}
	want, _, _ := store.Resolve(ref)

	books := &fakeBookLister{books: []models.Book{{ID: 1, Title: "Dune", FilePath: "/l/dune.epub", ImageURL: ref}}}
	pusher := &fakePusher{calls: map[string]func() (int64, error){
		"/l/dune.epub": func() (int64, error) { return 1, nil },
	}}
	s := NewSyncer(books).WithCovers(CoverSource{Store: store})
	s.newClient = func(Config) pluginPusher { return pusher }
	if err := s.Start(context.Background(), Config{}, ModePlugin); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 2*time.Second, func() bool { return !s.Running() })
	if got := pusher.meta("/l/dune.epub").CoverPath; got != want || want == "" {
		t.Fatalf("bulk push cover = %q, want %q", got, want)
	}
}
