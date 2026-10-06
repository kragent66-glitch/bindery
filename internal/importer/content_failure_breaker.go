package importer

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/vavallee/bindery/internal/downloader"
	"github.com/vavallee/bindery/internal/models"
)

// Breaker thresholds for blocklisting on a download client's content verdict
// (#3024).
//
// The statuses that mean "this release is broken" are also what a client
// reports when its own host is broken: NZBGet says FAILURE/UNPACK for a
// missing or misconfigured unrar as readily as for a corrupt archive (see
// nzbget.IsContentFailure). A broken release fails alone; a broken client
// fails every job the same way and completes nothing. The blocklist is global
// and never expires, so without a stop a broken unrar path would blocklist
// the best release of every wanted book, one sweep after another.
//
// Three distinct releases is the smallest count that is not a coincidence of
// two bad uploads, and it caps what a broken client can do to two releases
// per trip. Two hours covers the burst a single sweep sends, since its jobs
// finish close together, while staying well under the six hour re-grab
// cooldown, so failures from separate sweeps are not added up. Any completed
// download from the client in between proves the client can unpack, and
// resets the count.
const (
	contentBreakerWindow   = 2 * time.Hour
	contentBreakerDistinct = 3
)

type contentFailure struct {
	guid string
	kind string
	at   time.Time
}

type clientContentState struct {
	recent []contentFailure
	// trippedKind is the failure kind that tripped the breaker; "" means it
	// is closed. It stays open until the client completes a download, not
	// just until the window passes, because a broken unpacker does not heal
	// by waiting.
	trippedKind string
}

// contentFailureBreaker decides, per download client, whether a content
// failure may blocklist its release. In memory on purpose: a restart re-arms
// it, which costs at most contentBreakerDistinct-1 more blocklist rows before
// it trips again, and never blocks a legitimate verdict for good.
type contentFailureBreaker struct {
	mu       sync.Mutex
	byClient map[int64]*clientContentState
	now      func() time.Time // test seam; nil means time.Now
}

func (b *contentFailureBreaker) clock() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

func (b *contentFailureBreaker) state(clientID int64) *clientContentState {
	if b.byClient == nil {
		b.byClient = make(map[int64]*clientContentState)
	}
	st := b.byClient[clientID]
	if st == nil {
		st = &clientContentState{}
		b.byClient[clientID] = st
	}
	return st
}

// recordFailure notes a content failure and reports whether its release may
// be blocklisted. tripped is true only on the call that opens the breaker, and
// distinct is the number of releases that failed with this kind in the window.
func (b *contentFailureBreaker) recordFailure(clientID int64, guid, kind string) (allow, tripped bool, distinct int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := b.state(clientID)
	now := b.clock()

	cutoff := now.Add(-contentBreakerWindow)
	kept := st.recent[:0]
	for _, f := range st.recent {
		if f.at.After(cutoff) {
			kept = append(kept, f)
		}
	}
	st.recent = append(kept, contentFailure{guid: guid, kind: kind, at: now})

	seen := make(map[string]struct{})
	for _, f := range st.recent {
		if f.kind == kind {
			seen[f.guid] = struct{}{}
		}
	}
	distinct = len(seen)

	if st.trippedKind != "" {
		return false, false, distinct
	}
	if distinct >= contentBreakerDistinct {
		st.trippedKind = kind
		return false, true, distinct
	}
	return true, false, distinct
}

// recordSuccess notes a completed download from the client, which closes the
// breaker and clears its count. It reports whether the breaker was open.
func (b *contentFailureBreaker) recordSuccess(clientID int64) (wasOpen bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := b.byClient[clientID]
	if st == nil {
		return false
	}
	wasOpen = st.trippedKind != ""
	delete(b.byClient, clientID)
	return wasOpen
}

// WithClientHealth lets the importer report a paused blocklist on the download
// client's health, next to the path check (#3024).
func (s *Scanner) WithClientHealth(store *downloader.HealthStore) *Scanner {
	s.clientHealth = store
	return s
}

// blocklistContentFailure blocklists a release the download client reported
// as broken, unless the client is failing so many different releases the same
// way that the fault is more likely the client's (contentFailureBreaker). With
// the breaker open the row is left failed and the #2710 cooldown retries it.
func (s *Scanner) blocklistContentFailure(ctx context.Context, client *models.DownloadClient, dl *models.Download, kind, reason string) {
	if s.blocklist == nil || client == nil || dl == nil {
		return
	}
	allow, tripped, distinct := s.contentBreaker.recordFailure(client.ID, dl.GUID, kind)
	if allow {
		s.blocklistRejectedRelease(ctx, dl, reason)
		return
	}
	if !tripped {
		slog.Info("download failed: not blocklisting the release, automatic blocklisting is paused for this client",
			"client", client.Name, "title", dl.Title, "guid", dl.GUID, "reason", reason)
		return
	}
	msg := fmt.Sprintf("Automatic blocklisting is paused: %d different releases failed with %q within two hours and none completed in between, "+
		"which points at the download client (its unrar or 7z, disk space or permissions) rather than the releases. "+
		"Failed downloads are retried as usual; blocklisting resumes after this client completes a download",
		distinct, kind)
	slog.Warn("download client: "+msg, "client", client.Name, "title", dl.Title, "guid", dl.GUID)
	s.clientHealth.SetAdvisory(client.ID, models.DownloadClientHealth{Status: downloader.HealthError, Message: msg})
}

// noteClientSuccess closes the content failure breaker for a client that has
// just completed a download.
func (s *Scanner) noteClientSuccess(client *models.DownloadClient) {
	if client == nil {
		return
	}
	if s.contentBreaker.recordSuccess(client.ID) {
		slog.Info("download client completed a download, automatic blocklisting resumes", "client", client.Name)
		s.clientHealth.ClearAdvisory(client.ID)
	}
}
