//go:build darwin

package power

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Managed clamshell (lid-close) support.
//
// Fact: `sudo pmset -a disablesleep 1` is the ONLY mechanism a third-party
// tool can use to keep a MacBook awake with the lid closed and no external
// display (idle-sleep assertions do not survive the lid switch; the kernel
// requires desktopMode=external display+AC, or the Apple-private entitlement
// com.apple.private.iokit.assertonlidclose). Caveats: it is SYSTEM-WIDE (the
// -a/-b/-c prefix is ignored for disablesleep), undocumented in man pmset,
// and requires root.
//
// Homeplate manages it as a toggle, not a permanent setting:
//
//   - set only while work exists (jobs running or queued), on AC by default
//   - reverted when work drains, on daemon shutdown, and on uninstall
//   - crash safety: a marker file records that WE set it; the next daemon
//     start (or `homeplate power setup --revert`) clears a stale override
//   - root comes from a one-time sudoers helper (`homeplate power setup`),
//     so the daemon never prompts
//
// SudoersLine is the single NOPASSWD rule `homeplate power setup` installs.
const SudoersLine = "%s ALL=(root) NOPASSWD: /usr/bin/pmset -a disablesleep 1, /usr/bin/pmset -a disablesleep 0"

// SudoersPath is where the helper rule lives.
const SudoersPath = "/etc/sudoers.d/homeplate-pmset"

// clamshellMarker records that Homeplate (not the user) set disablesleep, so
// a stale override can be attributed and reverted after a crash.
func clamshellMarker(homeDir string) string { return filepath.Join(homeDir, "clamshell-on") }

// SetDisableSleep flips the system-wide override via sudo. Requires the
// sudoers helper (or a root shell) - a plain sudo prompts, which a daemon
// cannot answer, so the helper is mandatory for unattended operation.
func SetDisableSleep(ctx context.Context, on bool) error {
	arg := "0"
	if on {
		arg = "1"
	}
	cmd := exec.CommandContext(ctx, "sudo", "-n", "pmset", "-a", "disablesleep", arg)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if strings.Contains(msg, "password") || strings.Contains(msg, "sudo") {
			return fmt.Errorf("needs the one-time sudoers helper: run `sudo homeplate power setup` (%s)", msg)
		}
		return fmt.Errorf("pmset disablesleep %s: %v: %s", arg, err, msg)
	}
	return nil
}

// EnsureClamshell brings the system to the desired state, maintaining the
// attribution marker so crash recovery can tell "we set it" from "the user
// set it".
func EnsureClamshell(ctx context.Context, homeDir string, on bool) error {
	marker := clamshellMarker(homeDir)
	if on {
		if disableSleepActive(ctx) {
			// Already on (possibly by the user). Only claim it if we set it.
			if _, err := os.Stat(marker); err != nil {
				return nil // user-managed; leave it and the marker alone
			}
			return nil
		}
		if err := SetDisableSleep(ctx, true); err != nil {
			return err
		}
		return os.WriteFile(marker, []byte("managed by homeplate; safe to revert\n"), 0o600)
	}
	// Revert only if WE set it.
	if _, err := os.Stat(marker); err != nil {
		return nil
	}
	if disableSleepActive(ctx) {
		if err := SetDisableSleep(ctx, false); err != nil {
			return err
		}
	}
	return os.Remove(marker)
}

// RevertStaleClamshell is called at daemon start: if a previous Homeplate
// process died while holding the override, put the system back the way the
// user had it. This is the crash auto-revert guarantee.
func RevertStaleClamshell(ctx context.Context, homeDir string) (reverted bool, err error) {
	marker := clamshellMarker(homeDir)
	if _, err := os.Stat(marker); err != nil {
		return false, nil
	}
	if disableSleepActive(ctx) {
		if err := SetDisableSleep(ctx, false); err != nil {
			return true, err
		}
	}
	return true, os.Remove(marker)
}

// InstallSudoersHelper writes the NOPASSWD pmset rule. Must run as root
// (sudo homeplate power setup). Idempotent.
func InstallSudoersHelper(username string) (string, error) {
	if username == "" {
		username = os.Getenv("SUDO_USER")
	}
	if username == "" {
		return "", fmt.Errorf("cannot determine the non-root user; run via `sudo homeplate power setup`")
	}
	line := fmt.Sprintf(SudoersLine, username) + "\n"
	if err := os.WriteFile(SudoersPath, []byte(line), 0o440); err != nil {
		return "", err
	}
	// Validate syntax; a broken sudoers file can lock out sudo entirely.
	if out, err := exec.Command("visudo", "-c", "-f", SudoersPath).CombinedOutput(); err != nil {
		os.Remove(SudoersPath)
		return "", fmt.Errorf("sudoers rule failed validation: %s", strings.TrimSpace(string(out)))
	}
	return SudoersPath, nil
}

// RemoveSudoersHelper deletes the helper rule (uninstall). Best-effort.
func RemoveSudoersHelper() error {
	err := os.Remove(SudoersPath)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
