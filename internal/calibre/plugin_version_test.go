package calibre

import "testing"

// TestBridgeUpgradeWarning pins the numeric comparison behind the Test
// connection warning (#2831). A string comparison would call 0.10.0 older
// than 0.6.2, and a version that does not parse must not nag.
func TestBridgeUpgradeWarning(t *testing.T) {
	for _, tc := range []struct {
		version string
		warn    bool
	}{
		{"0.6.1", true},
		{"0.6.0", true},
		{"0.5.99", true},
		{"v0.6.1", true},
		{"0.6.2-rc1", false},
		{"0.6.2", false},
		{"0.6.10", false},
		{"0.10.0", false},
		{"0.7", false},
		{"1", false},
		{"0.6", true},
		{"", false},
		{"dev", false},
	} {
		got := BridgeUpgradeWarning(tc.version)
		if (got != "") != tc.warn {
			t.Errorf("BridgeUpgradeWarning(%q) = %q, want warning=%v", tc.version, got, tc.warn)
		}
	}
}
