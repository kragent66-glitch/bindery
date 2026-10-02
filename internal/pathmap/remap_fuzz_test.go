package pathmap

import (
	"strings"
	"testing"
)

// FuzzShareRemapRoundTrip checks that a rule with a network share on one side
// round-trips through Apply and ApplyInverse, and that a path joined onto a
// share target is still a share (path.Join collapses a leading `//`). The
// remainder is restricted to clean segments: `..`, `.` and empty segments are
// normalised away by design, and a backslash in a POSIX remainder is data that
// Windows cannot represent, so neither can round-trip.
func FuzzShareRemapRoundTrip(f *testing.F) {
	seeds := []struct {
		host, share, rel   string
		fwd, shareIsTarget bool
	}{
		{"192.168.1.4", "MEDIA", "Author/file.epub", false, true},
		{"nas", "books", "A/B/c.epub", true, true},
		{"NAS", "Downloads", "bindery/Book", false, false},
		{"nas", "dl", "x", true, false},
	}
	for _, s := range seeds {
		f.Add(s.host, s.share, s.rel, s.fwd, s.shareIsTarget)
	}

	f.Fuzz(func(t *testing.T, host, share, rel string, fwd, shareIsTarget bool) {
		for _, seg := range []string{host, share} {
			if !cleanSegment(seg) || seg == "?" || seg == "." {
				t.Skip()
			}
		}
		segs := strings.Split(rel, "/")
		for _, seg := range segs {
			if !cleanSegment(seg) {
				t.Skip()
			}
		}
		sep := `\`
		if fwd {
			sep = "/"
		}
		unc := sep + sep + host + sep + share
		if !IsUNCPath(unc) {
			t.Fatalf("IsUNCPath(%q) = false", unc)
		}
		remote := unc + sep + strings.Join(segs, sep)
		local := "/books/" + rel

		if shareIsTarget {
			r := Parse("/books:" + unc)
			got := r.Apply(local)
			if got != remote {
				t.Fatalf("Apply(%q) = %q, want %q", local, got, remote)
			}
			if back := r.ApplyInverse(got); back != local {
				t.Fatalf("ApplyInverse(%q) = %q, want %q", got, back, local)
			}
			return
		}
		r := Parse(unc + ":/books")
		if got := r.Apply(remote); got != local {
			t.Fatalf("Apply(%q) = %q, want %q", remote, got, local)
		}
		if back := r.ApplyInverse(local); back != remote {
			t.Fatalf("ApplyInverse(%q) = %q, want %q", local, back, remote)
		}
	})
}

// cleanSegment reports a path segment that survives path.Join unchanged and
// cannot be mistaken for rule syntax.
func cleanSegment(s string) bool {
	return s != "" && s != "." && s != ".." && strings.TrimSpace(s) == s &&
		!strings.ContainsAny(s, "/\\:,")
}
