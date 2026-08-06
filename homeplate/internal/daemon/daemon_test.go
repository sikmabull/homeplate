package daemon

import (
	"runtime"
	"testing"

	"github.com/homeplate-ci/homeplate/internal/config"
	"github.com/homeplate-ci/homeplate/internal/labels"
)

func TestTargetsArePerClassAndNeverMixLabels(t *testing.T) {
	cfg := config.Defaults()
	cfg.Engine.NativeMacOS = true
	cfg.Repos = []config.LinkedRepo{
		{Slug: "acme/api", Scope: "repo", Profile: "personal",
			// legacy config: full generated label set from the homerun era
			Labels: []string{"homeplate", "homeplate-macos", "homeplate-macos-arm64", "gpu"}},
	}
	targets := TargetsFromConfig(cfg, func(p string) string { return "git.example.com" })
	if len(targets) == 0 {
		t.Fatal("no targets")
	}
	for _, tg := range targets {
		if tg.Host != "git.example.com" {
			t.Errorf("host not propagated: %q", tg.Host)
		}
		for _, l := range tg.Labels {
			if tg.Class == labels.ClassLinux && (l == "homeplate-macos" || l == "homeplate-macos-arm64") {
				t.Errorf("linux target carries macOS label %q - it would claim macOS jobs", l)
			}
			if tg.Class == labels.ClassMacOS && (l == "homeplate-linux" || l == "homeplate-linux-arm64" || l == "homeplate-linux-x64") {
				t.Errorf("macOS target carries linux label %q", l)
			}
		}
		// user extras survive
		found := false
		for _, l := range tg.Labels {
			if l == "gpu" {
				found = true
			}
		}
		if !found {
			t.Errorf("user extra label 'gpu' lost for %s/%s", tg.Slug, tg.Class)
		}
	}
	// On a Mac with native enabled there must be BOTH a linux and a macos target.
	if runtime.GOOS == "darwin" {
		var haveLinux, haveMac bool
		for _, tg := range targets {
			if tg.Class == labels.ClassLinux {
				haveLinux = true
			}
			if tg.Class == labels.ClassMacOS {
				haveMac = true
			}
		}
		if !haveLinux || !haveMac {
			t.Errorf("darwin should serve both classes, got %+v", targets)
		}
	}
}
