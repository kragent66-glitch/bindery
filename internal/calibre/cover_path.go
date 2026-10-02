package calibre

import (
	"context"
	"log/slog"
	"strings"

	"github.com/vavallee/bindery/internal/covers"
)

// CoverCapable is implemented by an adder that can say whether a cover path
// will be used. Only the plugin client implements it, because only the plugin
// has a capability list; calibredb always takes `--cover`, so an adder that
// does not implement this is assumed to accept one. Asking before resolving
// matters: materialising a remote cover is a network fetch, and doing it for
// a plugin that would drop the field is pure waste.
type CoverCapable interface {
	SupportsCover(ctx context.Context) bool
}

// CoverSource is where the push paths find a cover file to hand over: the
// store that backs bindery-cover: references (#2564) and a writable cache for
// remote covers that have to be downloaded first. The zero value sends no
// cover at all.
type CoverSource struct {
	Store    *covers.Store
	CacheDir string
}

// PathFor turns a book's cover reference or URL into a path the target can
// open, or "" when there is nothing to send. The delivery worker and the bulk
// push both call it, so a book gets the same cover whichever path sends it.
//
// The plugin client puts the result through the operator's push path remap,
// the same as the book file, because a cover path is just as subject to the
// cross container mount mismatch the remap exists to fix.
func (c CoverSource) PathFor(ctx context.Context, imageURL string, target any) string {
	if strings.TrimSpace(imageURL) == "" {
		return ""
	}
	if capable, ok := target.(CoverCapable); ok && !capable.SupportsCover(ctx) {
		return ""
	}
	if covers.IsRef(imageURL) {
		// A cover Bindery stored itself. A reference is not a filesystem
		// path, so it has to be resolved before it goes anywhere, and
		// MaterializeCover only knows how to fetch URLs (its SSRF policy
		// would refuse the scheme).
		if c.Store == nil {
			return ""
		}
		path, _, ok := c.Store.Resolve(imageURL)
		if !ok {
			return ""
		}
		return path
	}
	path, err := MaterializeCover(ctx, c.CacheDir, imageURL)
	if err != nil {
		slog.Debug("calibre: cover materialization skipped", "error", err)
		return ""
	}
	return path
}
