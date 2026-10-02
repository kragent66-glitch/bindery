package calibre

import (
	"context"
	"strings"
	"sync"
)

// Adder is the hand off contract both Calibre clients implement: put this
// file into the library and tell me the id it got.
type Adder interface {
	Add(ctx context.Context, filePath string, meta Metadata) (int64, error)
}

// AdderResolver hands out the Calibre client the current settings call for.
//
// The importer used to hold one client built at boot from the boot-time mode,
// while being given a live mode resolver. The two drifted apart the moment
// anyone touched Settings: choosing plugin mode after booting with the
// integration off left the scanner calling calibredb's Add, which returned
// ErrDisabled and was swallowed without a log line, and correcting
// plugin_url, plugin_api_key or push_path_remap did nothing until restart.
// That is the mechanism behind the "Test connection passes but nothing
// reaches Calibre" reports (#1355, #1346).
//
// Resolving per push is not the same as building per push. The client is
// cached against the settings that define it and rebuilt only when one of
// them changes, so an import run reuses one connection pool rather than
// opening a fresh one per book. The syncer has always done this; this is the
// same pattern for the import path.
type AdderResolver struct {
	load func() Config

	mu    sync.Mutex
	key   string
	adder Adder
}

// NewAdderResolver builds a resolver over a settings loader. The loader is
// called once per push and must be cheap; in production it is a settings
// table read.
func NewAdderResolver(load func() Config) *AdderResolver {
	return &AdderResolver{load: load}
}

// For returns the adder for mode, or nil when the mode needs none. The
// returned value is shared between pushes, so callers must not mutate it.
func (r *AdderResolver) For(mode Mode) Adder {
	if r == nil || r.load == nil {
		return nil
	}
	if mode != ModePlugin && mode != ModeCalibredb {
		return nil
	}
	cfg := r.load()
	key := adderKey(mode, cfg)

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.adder != nil && r.key == key {
		return r.adder
	}
	if mode == ModePlugin {
		r.adder = NewPluginClient(cfg.PluginURL, cfg.PluginAPIKey).WithPushPathRemap(cfg.PushPathRemap)
	} else {
		// Test connection force-enables for the duration of a probe; a real
		// push must not, because the mode we were asked for already says the
		// integration is on and Config.Enabled carries the legacy flag.
		r.adder = New(cfg)
	}
	r.key = key
	return r.adder
}

// adderKey is the identity of a client: change any part of it and the cached
// client is no longer the right one to use.
func adderKey(mode Mode, cfg Config) string {
	parts := []string{
		string(mode),
		cfg.PluginURL,
		cfg.PluginAPIKey,
		cfg.PushPathRemap,
		cfg.LibraryPath,
		cfg.BinaryPath,
	}
	if cfg.Enabled {
		parts = append(parts, "enabled")
	}
	// NUL cannot appear in any of these values, so it cannot be smuggled
	// across a field boundary to make two different configurations collide.
	return strings.Join(parts, "\x00")
}
