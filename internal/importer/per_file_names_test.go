package importer

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/vavallee/bindery/internal/models"
)

// Tests for #2935: per-file audiobook placement names tracks from the
// audiobook file template, the way the folder branch does.

func TestPerFileAudiobookNames(t *testing.T) {
	s, book, _, _, settingsRepo, _, ctx := sharedFormatFixture(t, t.TempDir())
	author := &models.Author{Name: "Jordan B. Peterson"}
	dir := filepath.Join(t.TempDir(), "rip")
	files := []string{
		filepath.Join(dir, "CD 2", "01 - b.mp3"),
		filepath.Join(dir, "CD 1", "02 - a.MP3"),
		filepath.Join(dir, "CD 1", "01 - a.mp3"),
		filepath.Join(dir, "booklet.pdf"),
	}

	t.Run("no template keeps every name", func(t *testing.T) {
		got, err := s.perFileAudiobookNames(ctx, dir, files, author, book, "", "")
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"01 - b.mp3", "02 - a.MP3", "01 - a.mp3", "booklet.pdf"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("names = %v, want %v", got, want)
		}
	})

	t.Run("template numbers tracks in playback order", func(t *testing.T) {
		if err := settingsRepo.Set(ctx, "naming.audiobook_file_template", "{Title} - Part {Part:3}.{ext}"); err != nil {
			t.Fatal(err)
		}
		got, err := s.perFileAudiobookNames(ctx, dir, files, author, book, "", "")
		if err != nil {
			t.Fatal(err)
		}
		want := []string{
			"We Who Wrestle with God - Part 003.mp3",
			"We Who Wrestle with God - Part 002.mp3",
			"We Who Wrestle with God - Part 001.mp3",
			"booklet.pdf",
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("names = %v, want %v", got, want)
		}
	})

	t.Run("template without part refuses rather than overwrite", func(t *testing.T) {
		if err := settingsRepo.Set(ctx, "naming.audiobook_file_template", "{Title}.{ext}"); err != nil {
			t.Fatal(err)
		}
		_, err := s.perFileAudiobookNames(ctx, dir, files, author, book, "", "")
		if err == nil || !strings.Contains(err.Error(), "Include {Part}") {
			t.Errorf("err = %v, want a duplicate name refusal", err)
		}
	})

	t.Run("a lone track takes the single file name", func(t *testing.T) {
		if err := settingsRepo.Set(ctx, "naming.audiobook_file_template", conditionalPartTemplate); err != nil {
			t.Fatal(err)
		}
		got, err := s.perFileAudiobookNames(context.Background(), dir, files[:1], author, book, "", "")
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"Jordan B. Peterson - We Who Wrestle with God.mp3"}; !reflect.DeepEqual(got, want) {
			t.Errorf("names = %v, want %v", got, want)
		}
	})
}
