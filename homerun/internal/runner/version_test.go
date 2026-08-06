package runner

import "testing"

// TestMacARM64VersionPin guards against shipping the actions/runner release
// that deadlocks every job on Apple Silicon (actions/runner#4570).
func TestMacARM64VersionPin(t *testing.T) {
	cases := []struct {
		version  string
		platform string
		want     string
		pinned   bool
	}{
		{"2.336.0", "osx-arm64", MacARM64SafeVersion, true},
		{"2.337.0", "osx-arm64", MacARM64SafeVersion, true},
		{"2.335.1", "osx-arm64", "2.335.1", false},
		{"2.330.0", "osx-arm64", "2.330.0", false},
		// Other platforms are unaffected and must track latest.
		{"2.336.0", "linux-x64", "2.336.0", false},
		{"2.336.0", "osx-x64", "2.336.0", false},
	}
	for _, c := range cases {
		got, pinned, why := SafeVersionFor(c.version, c.platform)
		if got != c.want || pinned != c.pinned {
			t.Errorf("SafeVersionFor(%s, %s) = (%s, %v), want (%s, %v)",
				c.version, c.platform, got, pinned, c.want, c.pinned)
		}
		if pinned && why == "" {
			t.Errorf("a pinned version must explain itself for the daemon log")
		}
	}
}

// TestVersionComparison covers the dotted-version ordering helper.
func TestVersionComparison(t *testing.T) {
	cases := []struct {
		v, min string
		want   bool
	}{
		{"2.336.0", "2.336.0", true},
		{"2.336.1", "2.336.0", true},
		{"2.337.0", "2.336.0", true},
		{"3.0.0", "2.336.0", true},
		{"2.335.1", "2.336.0", false},
		{"2.9.0", "2.336.0", false},   // numeric, not lexicographic
		{"v2.336.0", "2.336.0", true}, // leading v tolerated
	}
	for _, c := range cases {
		if got := isAtLeast(c.v, c.min); got != c.want {
			t.Errorf("isAtLeast(%q, %q) = %v, want %v", c.v, c.min, got, c.want)
		}
	}
}

// TestFallbackVersionIsSafe ensures the offline-first-run default is not the
// known-broken release.
func TestFallbackVersionIsSafe(t *testing.T) {
	if got, pinned, _ := SafeVersionFor(FallbackVersion, "osx-arm64"); pinned {
		t.Errorf("FallbackVersion %s is unsafe on macOS arm64 (would pin to %s)", FallbackVersion, got)
	}
}
