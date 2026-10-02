package importer

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/vavallee/bindery/internal/db"
	"github.com/vavallee/bindery/internal/models"
)

// volumeScanEnv is a scanner over a separate ebook and audiobook root with the
// unmatched unit store wired, so a test can see both what reconciled and what
// the scan suggested for what did not.
type volumeScanEnv struct {
	s        *Scanner
	books    *db.BookRepo
	authors  *db.AuthorRepo
	libDir   string
	abDir    string
	ctx      context.Context
	authorID int64
}

func newVolumeScanEnv(t *testing.T, authorName string) *volumeScanEnv {
	t.Helper()
	database, err := db.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	e := &volumeScanEnv{
		books:   db.NewBookRepo(database),
		authors: db.NewAuthorRepo(database),
		libDir:  t.TempDir(),
		abDir:   t.TempDir(),
		ctx:     context.Background(),
	}
	e.s = NewScanner(db.NewDownloadRepo(database), db.NewDownloadClientRepo(database), e.books, e.authors,
		db.NewHistoryRepo(database), e.libDir, e.abDir, "", "", "")
	e.s.WithUnmatchedUnits(db.NewUnmatchedUnitRepo(database))
	a := &models.Author{ForeignID: "hc:" + authorName, Name: authorName, SortName: authorName}
	if err := e.authors.Create(e.ctx, a); err != nil {
		t.Fatal(err)
	}
	e.authorID = a.ID
	return e
}

func (e *volumeScanEnv) wanted(t *testing.T, title, mediaType string) *models.Book {
	t.Helper()
	b := &models.Book{ForeignID: "hc:" + title, AuthorID: e.authorID, Title: title,
		Status: models.BookStatusWanted, MediaType: mediaType}
	if err := e.books.Create(e.ctx, b); err != nil {
		t.Fatal(err)
	}
	return b
}

func (e *volumeScanEnv) get(t *testing.T, id int64) *models.Book {
	t.Helper()
	b, err := e.books.GetByID(e.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestScanLibrary_DoesNotReconcileAnotherVolume is #2860 as reported: an
// untracked Libation-style volume 1 folder, whose number is only in the
// folder name, and a wanted volume 17. Jaro-Winkler puts the two at 0.983, so
// the title tier reconciled volume 1's folder onto volume 17, which flipped
// to imported and was never searched. Volume 17 must stay wanted, the folder
// must be reported unmatched, and volume 17 must not be offered as the
// folder's adoption suggestion either.
func TestScanLibrary_DoesNotReconcileAnotherVolume(t *testing.T) {
	e := newVolumeScanEnv(t, "TheFirstDefier")
	writeFile(t, filepath.Join(e.abDir, "TheFirstDefier", "Defiance of the Fall 01",
		"Defiance of the Fall_B094JZMCJX_LC_128_44100_Stereo.m4b"))
	vol17 := e.wanted(t, "Defiance of the Fall 17", models.MediaTypeAudiobook)

	e.s.ScanLibrary(e.ctx)

	got := e.get(t, vol17.ID)
	if got.Status != models.BookStatusWanted || got.AudiobookFilePath != "" {
		t.Errorf("volume 17 reconciled to %q (status %s); volume 1's folder is not volume 17",
			got.AudiobookFilePath, got.Status)
	}
	units := readUnmatchedFiles(t, e.ctx, e.s)
	if len(units) != 1 {
		t.Fatalf("unmatched units = %+v, want volume 1's folder", units)
	}
	for _, c := range units[0].Candidates {
		if c.BookID == vol17.ID {
			t.Errorf("volume 17 suggested for volume 1's folder (score %.3f)", c.Score)
		}
	}
}

// TestScanLibrary_VolumeGoesToItsOwnBook: with both volumes wanted, the
// title tier walks candidates in title order, so volume 1 is offered volume
// 17's folder first. The file must skip volume 1 and land on volume 17, the
// book it is. Before #2860 the first candidate over 0.85 took it.
func TestScanLibrary_VolumeGoesToItsOwnBook(t *testing.T) {
	e := newVolumeScanEnv(t, "TheFirstDefier")
	folder := filepath.Join(e.abDir, "TheFirstDefier", "Defiance of the Fall 17")
	writeFile(t, filepath.Join(folder, "Defiance of the Fall_B0DKTZ4RV2_LC_128_44100_Stereo.m4b"))
	vol1 := e.wanted(t, "Defiance of the Fall 1", models.MediaTypeAudiobook)
	vol17 := e.wanted(t, "Defiance of the Fall 17", models.MediaTypeAudiobook)

	e.s.ScanLibrary(e.ctx)

	if got := e.get(t, vol1.ID); got.AudiobookFilePath != "" {
		t.Errorf("volume 1 took %q", got.AudiobookFilePath)
	}
	if got := e.get(t, vol17.ID); got.AudiobookFilePath != folder {
		t.Errorf("volume 17: AudiobookFilePath = %q, want %q", got.AudiobookFilePath, folder)
	}
}

// TestScanLibrary_VolumeNumberInTitleVetoes covers the layouts where the
// number is in the file's own title rather than only in a book folder: a flat
// audiobook with no book folder, and a numbered ebook in a book folder whose
// number agrees with it.
func TestScanLibrary_VolumeNumberInTitleVetoes(t *testing.T) {
	t.Run("flat audiobook file", func(t *testing.T) {
		e := newVolumeScanEnv(t, "TheFirstDefier")
		writeFile(t, filepath.Join(e.abDir, "TheFirstDefier", "Defiance of the Fall 01.m4b"))
		vol17 := e.wanted(t, "Defiance of the Fall 17", models.MediaTypeAudiobook)

		e.s.ScanLibrary(e.ctx)

		if got := e.get(t, vol17.ID); got.Status != models.BookStatusWanted || got.AudiobookFilePath != "" {
			t.Errorf("volume 17 reconciled to %q (status %s)", got.AudiobookFilePath, got.Status)
		}
	})
	t.Run("ebook with an explicit marker", func(t *testing.T) {
		e := newVolumeScanEnv(t, "Kugane Maruyama")
		writeFile(t, filepath.Join(e.libDir, "Kugane Maruyama", "Overlord, Vol. 1", "Overlord, Vol. 1.epub"))
		vol9 := e.wanted(t, "Overlord, Vol. 9", models.MediaTypeEbook)

		e.s.ScanLibrary(e.ctx)

		if got := e.get(t, vol9.ID); got.Status != models.BookStatusWanted || got.EbookFilePath != "" {
			t.Errorf("Vol. 9 reconciled to %q (status %s)", got.EbookFilePath, got.Status)
		}
	})
	t.Run("ebook numbered only in its folder", func(t *testing.T) {
		e := newVolumeScanEnv(t, "Jane Doe")
		writeFile(t, filepath.Join(e.libDir, "Jane Doe", "Long Series Name 01", "Long Series Name - Jane Doe.epub"))
		vol3 := e.wanted(t, "Long Series Name 3", models.MediaTypeEbook)

		e.s.ScanLibrary(e.ctx)

		if got := e.get(t, vol3.ID); got.Status != models.BookStatusWanted || got.EbookFilePath != "" {
			t.Errorf("volume 3 reconciled to %q (status %s)", got.EbookFilePath, got.Status)
		}
	})
}

// TestScanLibrary_VolumeVetoKeepsRealMatches pins what the #2860 veto must not
// break: numbers that are part of a title, track numbers inside a book's own
// numbered folder, a split audiobook's Part files against a "Book N" title,
// and "Book 1" against "Book One", which the rule cannot read as a volume and
// so leaves to title similarity.
func TestScanLibrary_VolumeVetoKeepsRealMatches(t *testing.T) {
	for _, tc := range []struct {
		name, author, title, mediaType string
		files                          []string // relative to the root for mediaType
		want                           string   // relative path the book must record
	}{
		{"Fahrenheit 451", "Ray Bradbury", "Fahrenheit 451", models.MediaTypeEbook,
			[]string{"Ray Bradbury/Fahrenheit 451/Fahrenheit 451.epub"}, "Ray Bradbury/Fahrenheit 451/Fahrenheit 451.epub"},
		{"1984", "George Orwell", "1984", models.MediaTypeEbook,
			[]string{"George Orwell/1984/1984.epub"}, "George Orwell/1984/1984.epub"},
		{"11/22/63", "Stephen King", "11/22/63", models.MediaTypeEbook,
			[]string{"Stephen King/11-22-63/11-22-63.epub"}, "Stephen King/11-22-63/11-22-63.epub"},
		{"Catch-22", "Joseph Heller", "Catch-22", models.MediaTypeEbook,
			[]string{"Joseph Heller/Catch 22/Catch 22.epub"}, "Joseph Heller/Catch 22/Catch 22.epub"},
		{"track numbers in a numbered folder", "TheFirstDefier", "Defiance of the Fall 7", models.MediaTypeAudiobook,
			[]string{"TheFirstDefier/Defiance of the Fall 7/Defiance of the Fall 01.mp3",
				"TheFirstDefier/Defiance of the Fall 7/Defiance of the Fall 02.mp3"},
			"TheFirstDefier/Defiance of the Fall 7"},
		{"part files against Book N", "Brandon Sanderson", "Rhythm of War (The Stormlight Archive, Book 4)", models.MediaTypeAudiobook,
			[]string{"Brandon Sanderson/Rhythm of War/Rhythm of War Part 1.mp3",
				"Brandon Sanderson/Rhythm of War/Rhythm of War Part 2.mp3"},
			"Brandon Sanderson/Rhythm of War"},
		{"Book 1 against Book One", "Brandon Sanderson", "Mistborn Book One", models.MediaTypeEbook,
			[]string{"Brandon Sanderson/Mistborn Book 1/Mistborn Book 1.epub"}, "Brandon Sanderson/Mistborn Book 1/Mistborn Book 1.epub"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newVolumeScanEnv(t, tc.author)
			root := e.libDir
			if tc.mediaType == models.MediaTypeAudiobook {
				root = e.abDir
			}
			for _, f := range tc.files {
				writeFile(t, filepath.Join(root, filepath.FromSlash(f)))
			}
			b := e.wanted(t, tc.title, tc.mediaType)

			e.s.ScanLibrary(e.ctx)

			got := e.get(t, b.ID)
			path := got.EbookFilePath
			if tc.mediaType == models.MediaTypeAudiobook {
				path = got.AudiobookFilePath
			}
			if want := filepath.Join(root, filepath.FromSlash(tc.want)); path != want {
				t.Errorf("%q: recorded %q (status %s), want %q", tc.title, path, got.Status, want)
			}
		})
	}
}

// TestLibraryVolumeConflict is the shared rule on its own: the book folder's
// number settles the volume when it has one, the file's title otherwise.
func TestLibraryVolumeConflict(t *testing.T) {
	for _, tc := range []struct {
		file, folder, wanted string
		want                 bool
	}{
		// #2860: the number only in the folder.
		{"Defiance of the Fall", "Defiance of the Fall 01", "Defiance of the Fall 17", true},
		{"Defiance of the Fall", "Defiance of the Fall 01", "Defiance of the Fall 1", false},
		{"Defiance of the Fall", "Defiance of the Fall 01", "Defiance of the Fall", false},
		// The number in the title, no book folder.
		{"Defiance of the Fall 01", "", "Defiance of the Fall 17", true},
		{"Overlord, Vol. 1", "", "Overlord, Vol. 9", true},
		// A numbered folder outranks the track number in the filename.
		{"Defiance of the Fall 01", "Defiance of the Fall 7", "Defiance of the Fall 7", false},
		{"Defiance of the Fall 01", "Defiance of the Fall 7", "Defiance of the Fall 17", true},
		// Numbers that are titles.
		{"Fahrenheit 451", "Fahrenheit 451", "Fahrenheit 451", false},
		{"Catch 22", "Catch 22", "Catch-22", false},
		{"11-22-63", "11-22-63", "11/22/63", false},
		{"1984", "1984", "1984", false},
		// A one-sided Part marker is not a series position; two are.
		{"Rhythm of War Part 1", "Rhythm of War", "Rhythm of War (The Stormlight Archive, Book 4)", false},
		{"Rhythm of War", "Part 1", "Rhythm of War (The Stormlight Archive, Book 4)", false},
		{"The Way of Kings, Part 1", "", "The Way of Kings, Part 2", true},
		// Words are not digits: "Book One" carries no number to compare.
		{"Mistborn Book 1", "Mistborn Book 1", "Mistborn Book One", false},
	} {
		if got := libraryVolumeConflict(tc.file, tc.folder, tc.wanted); got != tc.want {
			t.Errorf("libraryVolumeConflict(%q, %q, %q) = %v, want %v", tc.file, tc.folder, tc.wanted, got, tc.want)
		}
	}
}
