package calibre

import (
	"path/filepath"
	"strings"
)

// ebookFormatPreference is the order a book's ebook files go to Calibre in
// (#2832). The first file makes the Calibre record, so it should be the one
// Calibre reads metadata and a cover from best; every later file joins that
// record as another format. Anything not listed comes after, by extension.
var ebookFormatPreference = map[string]int{
	"epub":  0,
	"kepub": 1,
	"azw3":  2,
	"mobi":  3,
	"pdf":   4,
}

// ebookFormatOf is the format name the preference order uses for path: the
// extension, lower case, with a Kobo ".kepub.epub" read as kepub.
func ebookFormatOf(path string) string {
	base := strings.ToLower(filepath.Base(path))
	if strings.HasSuffix(base, ".kepub.epub") {
		return "kepub"
	}
	return strings.TrimPrefix(filepath.Ext(base), ".")
}

// formatRank places path in ebookFormatPreference; unlisted formats share
// the rank after the last listed one.
func formatRank(path string) int {
	if r, ok := ebookFormatPreference[ebookFormatOf(path)]; ok {
		return r
	}
	return len(ebookFormatPreference)
}

// lessByFormatPreference orders two paths by format preference, then by
// extension among the unlisted formats. Paths of the same format compare
// equal, so a stable sort keeps their existing order.
func lessByFormatPreference(a, b string) bool {
	ra, rb := formatRank(a), formatRank(b)
	if ra != rb {
		return ra < rb
	}
	return ebookFormatOf(a) < ebookFormatOf(b)
}
