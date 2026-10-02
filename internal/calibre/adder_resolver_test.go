package calibre

import (
	"testing"
)

// TestAdderResolver_CachesUntilTheSettingsChange is the other half of review
// item 1: resolving per push must not mean building an HTTP client, and a new
// connection pool with it, for every book.
func TestAdderResolver_CachesUntilTheSettingsChange(t *testing.T) {
	cfg := Config{Enabled: true, PluginURL: "http://a:8099", PluginAPIKey: "k"}
	loads := 0
	r := NewAdderResolver(func() Config {
		loads++
		return cfg
	})

	first := r.For(ModePlugin)
	second := r.For(ModePlugin)
	if first != second {
		t.Error("two pushes with unchanged settings should share one client")
	}
	if loads != 2 {
		t.Errorf("settings loads = %d, want one per push", loads)
	}

	cfg.PluginURL = "http://b:8099"
	third := r.For(ModePlugin)
	if third == second {
		t.Error("changing plugin_url must rebuild the client")
	}

	cfg.PushPathRemap = "/books:/mnt/books"
	if r.For(ModePlugin) == third {
		t.Error("changing push_path_remap must rebuild the client")
	}
}

// TestAdderResolver_FollowsTheMode covers the report behind #1355: booting in
// one mode and selecting another in the UI used to keep the boot-time client.
func TestAdderResolver_FollowsTheMode(t *testing.T) {
	r := NewAdderResolver(func() Config {
		return Config{Enabled: true, PluginURL: "http://a:8099", LibraryPath: "/lib"}
	})

	plugin, ok := r.For(ModePlugin).(*PluginClient)
	if !ok || plugin == nil {
		t.Fatalf("plugin mode resolved to %T", r.For(ModePlugin))
	}
	cli, ok := r.For(ModeCalibredb).(*Client)
	if !ok || cli == nil {
		t.Fatalf("calibredb mode resolved to %T", r.For(ModeCalibredb))
	}
	if r.For(ModeOff) != nil {
		t.Error("mode off must resolve to no adder at all")
	}
}

func TestAdderResolver_NilLoaderIsSafe(t *testing.T) {
	var r *AdderResolver
	if r.For(ModePlugin) != nil {
		t.Error("a nil resolver must not hand back an adder")
	}
}
