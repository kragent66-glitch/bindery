package calibre

import (
	"math"
	"strings"

	"github.com/vavallee/bindery/internal/models"
)

// MetadataSource is everything the Calibre push paths know about one book.
// It exists so the live import hand off and the bulk "Push all to Calibre"
// job build the same payload. They used to hand-roll two different literals,
// and the bulk one was six fields to the import's fifteen, so a library
// pushed in bulk arrived in Calibre with no series, no series index, no
// description, no publisher, no published date and no rating.
//
// Author and Authors are alternatives: Author wins when set, otherwise the
// precomputed Authors / AuthorSort pair is used (the bulk job resolves the
// author through a repo). Identifiers likewise overrides the map derived from
// the book and edition, which is how the bulk job drops a source library's
// calibre id before pushing into a different library.
//
// Covers are deliberately not here. Resolving one is I/O, it differs per
// caller (a bindery-cover reference from the store, or a remote URL that has
// to be materialised first) and it is gated on what the target supports, so
// callers set Metadata.CoverPath themselves after calling BuildMetadata.
type MetadataSource struct {
	Book        *models.Book
	Author      *models.Author
	Authors     []string
	AuthorSort  string
	Edition     *models.Edition
	SeriesTitle string
	SeriesIndex string
	Identifiers map[string]string
}

// BuildMetadata renders the Bindery metadata contract for one book. A nil
// Book yields the zero Metadata, which Metadata.empty reports as empty, so
// every caller's "nothing to send" branch keeps working.
func BuildMetadata(src MetadataSource) Metadata {
	if src.Book == nil {
		return Metadata{}
	}
	book := src.Book
	meta := Metadata{
		Title:         book.Title,
		Description:   book.Description,
		Genres:        book.Genres,
		Language:      NormalizeLanguageForCalibre(book.Language),
		Series:        src.SeriesTitle,
		SeriesIndex:   src.SeriesIndex,
		PublishedDate: FormatPublishedDate(book.ReleaseDate),
		Rating:        halfStar(book.AverageRating),
	}
	switch {
	case src.Author != nil && strings.TrimSpace(src.Author.Name) != "":
		meta.Authors = []string{strings.TrimSpace(src.Author.Name)}
		meta.AuthorSort = strings.TrimSpace(src.Author.SortName)
	case len(src.Authors) > 0:
		meta.Authors = src.Authors
		meta.AuthorSort = src.AuthorSort
	}
	if src.Identifiers != nil {
		meta.Identifiers = src.Identifiers
	} else {
		meta.Identifiers = IdentifiersForBook(book, src.Edition)
	}
	if ed := src.Edition; ed != nil {
		if strings.TrimSpace(ed.Publisher) != "" {
			meta.Publisher = ed.Publisher
		}
		if ed.PublishDate != nil {
			meta.PublishedDate = FormatPublishedDate(ed.PublishDate)
		}
		if strings.TrimSpace(ed.Language) != "" {
			meta.Language = NormalizeLanguageForCalibre(ed.Language)
		}
	}
	return meta
}

// CoverSourceFor returns the image the push should try to attach: the
// edition's own artwork when it has some, otherwise the book's. The value can
// be a remote URL or a bindery-cover reference; resolving it is the caller's
// job because the two push paths resolve it differently.
func CoverSourceFor(book *models.Book, edition *models.Edition) string {
	source := ""
	if book != nil {
		source = book.ImageURL
	}
	if edition != nil && strings.TrimSpace(edition.ImageURL) != "" {
		source = edition.ImageURL
	}
	return source
}

// halfStar snaps a rating onto a half star boundary.
//
// Calibre stores ratings as an integer 0 to 10, two units per star, and both
// hand off paths speak stars on the wire: `calibredb set_metadata --field
// rating:<v>` goes through field_from_string, which multiplies a rating field
// by two, and the plugin's own _calibre_rating multiplies by two as well. The
// scales agree. What does not agree is the rounding on the far side: Calibre's
// adapter truncates the doubled value while the plugin rounds it, so a raw
// 4.4 lands as four stars through calibredb and four and a half through the
// plugin. Sending a value that is already on a half star boundary makes both
// arithmetics produce the same integer.
func halfStar(rating float64) float64 {
	if rating <= 0 || math.IsNaN(rating) || math.IsInf(rating, 0) {
		return 0
	}
	if rating > 5 {
		rating = 5
	}
	return math.Round(rating*2) / 2
}
