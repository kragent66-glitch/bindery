package importer

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/vavallee/bindery/internal/models"
)

// #2934: a series numbered with a bare number and then a subtitle. Real
// catalogue data for The Primal Hunter mixes "The Primal Hunter 3", "The
// Primal Hunter 9: A LitRPG Adventure" and "The Primal Hunter 7 - A LitRPG
// Adventure" in one author's works. The volume veto only read a number at the
// END of a title, so against "The Primal Hunter 17: A LitRPG Adventure" volume
// 3's folder had nothing to compare, the two shared "primal" and "hunter", and
// the matcher bound volume 3's audiobook to volume 17. Volume 3's own import
// then recorded nothing, because book_files.path is UNIQUE and the insert is
// OR IGNORE, so book 3 stayed wanted while its history said imported.

func TestLibraryVolumeConflict_NumberBeforeSubtitle(t *testing.T) {
	for _, tc := range []struct {
		file, folder, wanted string
		want                 bool
	}{
		// The reported shape: numbered folder, subtitled wanted title.
		{"The Primal Hunter 3", "The Primal Hunter 3", "The Primal Hunter 17: A LitRPG Adventure", true},
		{"The Primal Hunter 3 A LitRPG Adventure", "The Primal Hunter 3", "The Primal Hunter 17: A LitRPG Adventure", true},
		// Subtitled folder and file, plain wanted title.
		{"The Primal Hunter 3 A LitRPG Adventure", "The Primal Hunter 3 A LitRPG Adventure", "The Primal Hunter 17", true},
		{"The Primal Hunter 3 A LitRPG Adventure", "", "The Primal Hunter 13", true},
		// Other separators and spellings seen in the same catalogue.
		{"The Primal Hunter 3", "", "The Primal Hunter 7 - A LitRPG Adventure", true},
		{"Primal Hunter, 3", "", "The Primal Hunter 9: A LitRPG Adventure", true},
		// The same volume with and without its subtitle still matches.
		{"The Primal Hunter 3", "The Primal Hunter 3", "The Primal Hunter 3: A LitRPG Adventure", false},
		{"The Primal Hunter 3 A LitRPG Adventure", "The Primal Hunter 3 A LitRPG Adventure", "The Primal Hunter 3", false},
		{"The Primal Hunter 03", "", "The Primal Hunter 3: A LitRPG Adventure", false},
		// A subtitled numbered folder outranks its track numbers.
		{"Defiance of the Fall 01", "Defiance of the Fall 7 A LitRPG Adventure", "Defiance of the Fall 7: A LitRPG Adventure", false},
		// A number with no shared words before it says nothing.
		{"2001 A Space Odyssey", "", "2010 Odyssey Two", false},
		{"The 7 Habits of Highly Effective People", "", "The 8th Habit", false},
		{"Catch 22", "Catch 22", "Catch-22: A Novel", false},
	} {
		if got := libraryVolumeConflict(tc.file, tc.folder, tc.wanted); got != tc.want {
			t.Errorf("libraryVolumeConflict(%q, %q, %q) = %v, want %v", tc.file, tc.folder, tc.wanted, got, tc.want)
		}
	}
}

func TestTitleMatch_NumberBeforeSubtitle(t *testing.T) {
	for _, tc := range []struct {
		book, parsed string
		want         bool
	}{
		{"The Primal Hunter 17: A LitRPG Adventure", "The Primal Hunter 3", false},
		{"The Primal Hunter 13", "The Primal Hunter 3 A LitRPG Adventure", false},
		{"The Primal Hunter 3: A LitRPG Adventure", "The Primal Hunter 3", true},
		{"The Primal Hunter 3", "The Primal Hunter 3 A LitRPG Adventure", true},
	} {
		if got := titleMatch(tc.book, tc.parsed); got != tc.want {
			t.Errorf("titleMatch(%q, %q) = %v, want %v", tc.book, tc.parsed, got, tc.want)
		}
	}
}

// FindExisting is what binds a file when an author refresh creates a new
// book. It must not hand volume 3's audiobook to a new volume 17.
func TestFindExisting_NumberBeforeSubtitle(t *testing.T) {
	root := t.TempDir()
	vol3 := filepath.Join(root, "Zogarth", "The Primal Hunter 3 (2022)", "The Primal Hunter 3.m4b")
	writeFile(t, vol3)

	ls := NewLibrarySnapshot("", root)
	for _, title := range []string{
		"The Primal Hunter 17: A LitRPG Adventure",
		"The Primal Hunter 13: A LitRPG Adventure",
		"The Primal Hunter 12 - A LitRPG Adventure",
	} {
		if got := ls.FindExisting(context.Background(), title, "Zogarth", models.MediaTypeAudiobook); got != "" {
			t.Errorf("FindExisting(%q) = %q, volume 3's file is not this book", title, got)
		}
	}
	if got := ls.FindExisting(context.Background(), "The Primal Hunter 3: A LitRPG Adventure", "Zogarth", models.MediaTypeAudiobook); got != vol3 {
		t.Errorf("FindExisting(volume 3 with subtitle) = %q, want %q", got, vol3)
	}
}

// The library scan's title tier must not reconcile an untracked volume 3
// folder onto a wanted, subtitled volume 17, and must give it to volume 3.
func TestScanLibrary_NumberBeforeSubtitle(t *testing.T) {
	e := newVolumeScanEnv(t, "Zogarth")
	folder := filepath.Join(e.abDir, "Zogarth", "The Primal Hunter 3 (2022)")
	writeFile(t, filepath.Join(folder, "The Primal Hunter 3.m4b"))
	vol17 := e.wanted(t, "The Primal Hunter 17: A LitRPG Adventure", models.MediaTypeAudiobook)
	vol3 := e.wanted(t, "The Primal Hunter 3", models.MediaTypeAudiobook)

	e.s.ScanLibrary(e.ctx)

	if got := e.get(t, vol17.ID); got.Status != models.BookStatusWanted || got.AudiobookFilePath != "" {
		t.Errorf("volume 17 reconciled to %q (status %s); that folder is volume 3", got.AudiobookFilePath, got.Status)
	}
	if got := e.get(t, vol3.ID); got.AudiobookFilePath != folder {
		t.Errorf("volume 3: AudiobookFilePath = %q, want %q", got.AudiobookFilePath, folder)
	}
}
