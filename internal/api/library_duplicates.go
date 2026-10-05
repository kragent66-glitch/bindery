package api

import (
	"context"
	"net/http"
	"sort"
	"strings"

	"github.com/vavallee/bindery/internal/auth"
	"github.com/vavallee/bindery/internal/db"
	"github.com/vavallee/bindery/internal/duplicates"
	"github.com/vavallee/bindery/internal/models"
)

// DuplicateReviewHandler serves the library-wide duplicate review (#2999).
type DuplicateReviewHandler struct {
	books  *db.BookRepo
	series *db.SeriesRepo
}

// NewDuplicateReviewHandler builds the library-wide duplicate review handler.
// series may be nil, which disables the series-position guard exactly as it
// does for the per-author window.
func NewDuplicateReviewHandler(books *db.BookRepo, series *db.SeriesRepo) *DuplicateReviewHandler {
	return &DuplicateReviewHandler{books: books, series: series}
}

// libraryDuplicatesResponse is the payload for GET /library/duplicate-candidates.
type libraryDuplicatesResponse struct {
	Groups []duplicates.Group `json:"groups"`
	// Total is the number of groups across every page; Count is this page's.
	Total  int `json:"total"`
	Count  int `json:"count"`
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
}

const (
	libraryDuplicatesDefaultLimit = 25
	libraryDuplicatesMaxLimit     = 100
)

// List implements GET /library/duplicate-candidates (#2999): every author's
// duplicate groups in one paginated list, found by the same detection the
// per-author window runs (duplicates.DetectByAuthor runs duplicates.Detect
// once per author), annotated with the same evidence.
//
// Cost on a large library: a fixed number of queries whatever its size. One
// thin query loads the id, author, title and excluded flag of every visible
// book, one loads every series membership, and the scan runs per author in
// memory. Only after the groups are sorted and the page is cut are the full
// rows and evidence loaded, and only for that page's members, in batched
// queries. Memory is therefore one thin row per book plus one page of full
// rows, never the whole library's descriptions and file lists.
//
// Scoping follows the per-author window: that window lets a caller see an
// author when auth.CheckOwnership passes on the author's owner, so this lists
// the authors auth.ListScopeUserID allows (every author with tenancy off or
// for an admin; otherwise the caller's own and unowned ones). The route is not
// admin only, because the per-author window is not. It writes nothing; the UI
// excludes rows through the existing exclude routes, which check ownership
// per book.
func (h *DuplicateReviewHandler) List(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	limit, offset := parseLimitOffset(r, libraryDuplicatesDefaultLimit, libraryDuplicatesMaxLimit)
	scope := auth.ListScopeUserID(ctx)

	books, names, err := h.books.ListForDuplicateScan(ctx, scope)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	var memberships map[int64][]db.BookSeriesMembership
	if h.series != nil {
		memberships, err = h.series.ListBookSeriesMembershipsForOwner(ctx, scope)
		if err != nil {
			writeServerError(w, r, err)
			return
		}
	}

	groups := duplicates.DetectByAuthor(books, seriesSlotsFrom(memberships))
	for i := range groups {
		groups[i].AuthorName = names[groups[i].AuthorID]
	}
	sort.SliceStable(groups, func(i, j int) bool {
		ni, nj := strings.ToLower(groups[i].AuthorName), strings.ToLower(groups[j].AuthorName)
		if ni != nj {
			return ni < nj
		}
		if groups[i].AuthorID != groups[j].AuthorID {
			return groups[i].AuthorID < groups[j].AuthorID
		}
		return groups[i].Key < groups[j].Key
	})

	total := len(groups)
	start := min(offset, total)
	end := min(start+limit, total)
	page := groups[start:end]

	if err := h.hydrateMembers(ctx, page); err != nil {
		writeServerError(w, r, err)
		return
	}
	if err := annotateDuplicateGroups(ctx, h.books, page, memberships); err != nil {
		writeServerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, libraryDuplicatesResponse{
		Groups: page,
		Total:  total,
		Count:  len(page),
		Limit:  limit,
		Offset: offset,
	})
}

// hydrateMembers swaps the thin scan rows of one page's members for full book
// rows, so the page carries status, language, release date and file columns.
// Descriptions are blanked: the review never shows them and they are most of
// a row's weight. A member whose row vanished between the two queries keeps
// its thin row rather than failing the page.
func (h *DuplicateReviewHandler) hydrateMembers(ctx context.Context, groups []duplicates.Group) error {
	var ids []int64
	for _, g := range groups {
		for _, m := range g.Members {
			ids = append(ids, m.ID)
		}
	}
	full := make(map[int64]*models.Book, len(ids))
	const chunk = 500
	for s := 0; s < len(ids); s += chunk {
		got, err := h.books.GetByIDs(ctx, ids[s:min(s+chunk, len(ids))])
		if err != nil {
			return err
		}
		for id, b := range got {
			full[id] = b
		}
	}
	for gi := range groups {
		for mi := range groups[gi].Members {
			m := &groups[gi].Members[mi]
			if b, ok := full[m.ID]; ok {
				m.Book = *b
				m.Description = ""
			}
		}
	}
	return nil
}
