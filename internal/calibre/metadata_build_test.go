package calibre

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vavallee/bindery/internal/models"
)

func dateOf(s string) *time.Time {
	t, _ := time.Parse("2006-01-02", s)
	return &t
}

// TestBuildMetadata_CarriesEveryFieldBothPathsUsed is review item 3: the bulk
// push hand-rolled six fields while a live import built fifteen, so a library
// pushed in bulk arrived with no series, description, publisher, published
// date or rating.
func TestBuildMetadata_CarriesEveryFieldBothPathsUsed(t *testing.T) {
	book := &models.Book{
		ID:            7,
		Title:         "Dune",
		Description:   "Desert planet.",
		Genres:        []string{"Science Fiction"},
		Language:      "eng",
		ReleaseDate:   dateOf("1965-08-01"),
		AverageRating: 4.5,
	}
	author := &models.Author{Name: "Frank Herbert", SortName: "Herbert, Frank"}
	edition := &models.Edition{
		Publisher:   "Ace",
		PublishDate: dateOf("1990-06-01"),
		Language:    "eng",
		ISBN13:      strPtr("9780441172719"),
	}

	meta := BuildMetadata(MetadataSource{
		Book:        book,
		Author:      author,
		Edition:     edition,
		SeriesTitle: "Dune Chronicles",
		SeriesIndex: "1",
	})

	if meta.Title != "Dune" {
		t.Errorf("title = %q", meta.Title)
	}
	if len(meta.Authors) != 1 || meta.Authors[0] != "Frank Herbert" {
		t.Errorf("authors = %v", meta.Authors)
	}
	if meta.AuthorSort != "Herbert, Frank" {
		t.Errorf("authorSort = %q", meta.AuthorSort)
	}
	if meta.Description != "Desert planet." {
		t.Errorf("description = %q", meta.Description)
	}
	if meta.Publisher != "Ace" {
		t.Errorf("publisher = %q", meta.Publisher)
	}
	if meta.PublishedDate != "1990-06-01" {
		t.Errorf("publishedDate = %q, want the edition's date", meta.PublishedDate)
	}
	if meta.Series != "Dune Chronicles" || meta.SeriesIndex != "1" {
		t.Errorf("series = %q/%q", meta.Series, meta.SeriesIndex)
	}
	if meta.Rating != 4.5 {
		t.Errorf("rating = %v", meta.Rating)
	}
	if meta.Language != "en" {
		t.Errorf("language = %q, want the normalised code", meta.Language)
	}
	if meta.Identifiers["isbn"] != "9780441172719" {
		t.Errorf("identifiers = %v", meta.Identifiers)
	}
	if meta.Identifiers["bindery"] != "7" {
		t.Errorf("identifiers = %v, want the bindery id", meta.Identifiers)
	}
}

// TestBuildMetadata_AcceptsPrecomputedAuthorsAndIdentifiers covers the bulk
// push's shape, where the author comes from a repo lookup and the identifier
// map has already had the source library's calibre id stripped.
func TestBuildMetadata_AcceptsPrecomputedAuthorsAndIdentifiers(t *testing.T) {
	meta := BuildMetadata(MetadataSource{
		Book:        &models.Book{ID: 3, Title: "Dune"},
		Authors:     []string{"Frank Herbert"},
		AuthorSort:  "Herbert, Frank",
		Identifiers: map[string]string{"bindery": "3"},
	})
	if len(meta.Authors) != 1 || meta.AuthorSort != "Herbert, Frank" {
		t.Errorf("authors = %v / %q", meta.Authors, meta.AuthorSort)
	}
	if _, ok := meta.Identifiers["calibre"]; ok {
		t.Errorf("identifiers = %v, want the caller's map used verbatim", meta.Identifiers)
	}
}

func TestBuildMetadata_NilBookIsEmpty(t *testing.T) {
	if !BuildMetadata(MetadataSource{}).empty() {
		t.Error("BuildMetadata with no book should be empty")
	}
}

// calibredbDBRating mirrors what Calibre does with `--field rating:<v>`:
// src/calibre/ebooks/metadata/book/base.py field_from_string multiplies a
// rating datatype by two, and src/calibre/db/write.py get_adapter clamps it
// with adapt_number(int, x), which truncates.
func calibredbDBRating(field string) int {
	v, err := strconv.ParseFloat(field, 64)
	if err != nil {
		return -1
	}
	n := int(v * 2)
	if n < 0 {
		return 0
	}
	if n > 10 {
		return 10
	}
	return n
}

// pluginDBRating mirrors adder.py _calibre_rating in calibre-bridge:
// max(0, min(10, int((rating * 2) + 0.5))).
func pluginDBRating(rating float64) int {
	if rating <= 0 {
		return 0
	}
	n := int(rating*2 + 0.5)
	if n > 10 {
		return 10
	}
	return n
}

// TestRatingLandsOnTheSameCalibreValueInBothModes is review item 8, corrected.
// Both sides use the same 0 to 5 star scale, so the review's "one of them is
// wrong" premise does not hold. What does differ is rounding: calibredb
// truncates the doubled value, the plugin rounds it, so the same book can land
// half a star apart. Bindery settles it by sending a value that is already on
// a half star boundary.
func TestRatingLandsOnTheSameCalibreValueInBothModes(t *testing.T) {
	for _, rating := range []float64{0.2, 1.1, 2.49, 3.3, 4.4, 4.6, 4.75, 5} {
		meta := BuildMetadata(MetadataSource{Book: &models.Book{ID: 1, Title: "x", AverageRating: rating}})

		var field string
		for _, f := range meta.setFields() {
			if strings.HasPrefix(f, "rating:") {
				field = strings.TrimPrefix(f, "rating:")
			}
		}
		if field == "" {
			// A rating that rounds down to zero stars is dropped on both
			// sides, which is agreement, not a gap.
			if got := pluginDBRating(meta.Rating); got != 0 {
				t.Errorf("rating %v sends no calibredb field but %d to the plugin", rating, got)
			}
			continue
		}
		viaCalibredb := calibredbDBRating(field)
		viaPlugin := pluginDBRating(meta.Rating)
		if viaCalibredb != viaPlugin {
			t.Errorf("rating %v: calibredb stores %d, the plugin stores %d", rating, viaCalibredb, viaPlugin)
		}
	}
}
