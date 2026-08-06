package service

import (
	"runtime"
	"strings"
	"testing"
)

// TestRenderProducesValidUnit checks the service definition without loading
// anything into the developer's real launchd/systemd.
func TestRenderProducesValidUnit(t *testing.T) {
	i := &Installer{BinPath: "/opt/homebrew/bin/homerun", HomeDir: "/Users/dev/.homerun"}
	out, err := i.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	if !strings.Contains(out, "/opt/homebrew/bin/homerun") {
		t.Error("unit does not reference the homerun binary")
	}
	if !strings.Contains(out, "daemon") || !strings.Contains(out, "run") {
		t.Error("unit does not invoke `daemon run`")
	}
	// Docker/act live in Homebrew paths that launchd does not provide.
	if !strings.Contains(out, "/opt/homebrew/bin") {
		t.Error("unit PATH omits Homebrew; the daemon would not find docker or act")
	}

	switch runtime.GOOS {
	case "darwin":
		for _, must := range []string{
			"<?xml", "com.homerun.daemon", "<key>RunAtLoad</key>", "<key>KeepAlive</key>",
			"<key>ThrottleInterval</key>", "Background", "HOMERUN_HOME",
		} {
			if !strings.Contains(out, must) {
				t.Errorf("plist missing %q", must)
			}
		}
		if strings.Count(out, "<plist") != 1 || !strings.Contains(out, "</plist>") {
			t.Error("plist is malformed")
		}
	case "linux":
		for _, must := range []string{
			"[Unit]", "[Service]", "[Install]", "ExecStart=", "Restart=always",
			"WantedBy=default.target", "HOMERUN_HOME=",
		} {
			if !strings.Contains(out, must) {
				t.Errorf("systemd unit missing %q", must)
			}
		}
	}
}

// TestRenderIsDeterministic keeps re-installs from churning the unit file.
func TestRenderIsDeterministic(t *testing.T) {
	i := &Installer{BinPath: "/usr/local/bin/homerun", HomeDir: "/home/dev/.homerun"}
	first, err := i.Render()
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 5; n++ {
		got, err := i.Render()
		if err != nil {
			t.Fatal(err)
		}
		if got != first {
			t.Fatal("Render() is not deterministic")
		}
	}
}

// TestPathLocation checks where the unit is written per platform.
func TestPathLocation(t *testing.T) {
	i := &Installer{BinPath: "/x/homerun", HomeDir: "/x/.homerun"}
	p, err := i.Path()
	if err != nil {
		t.Skipf("unsupported platform: %v", err)
	}
	switch runtime.GOOS {
	case "darwin":
		if !strings.Contains(p, "Library/LaunchAgents") || !strings.HasSuffix(p, ".plist") {
			t.Errorf("macOS path = %q, want a LaunchAgents plist", p)
		}
		// User-level, never a system LaunchDaemon: the daemon needs the user's
		// keychain and Docker socket, and must not run as root.
		if strings.HasPrefix(p, "/Library/") {
			t.Error("must install as a user LaunchAgent, not a system LaunchDaemon")
		}
	case "linux":
		if !strings.Contains(p, ".config/systemd/user") {
			t.Errorf("linux path = %q, want a systemd user unit", p)
		}
	}
}
