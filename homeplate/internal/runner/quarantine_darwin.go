//go:build darwin

package runner

import "os/exec"

// stripQuarantine removes com.apple.quarantine from a freshly downloaded
// runner so Gatekeeper does not block execution.
func stripQuarantine(dir string) error {
	return exec.Command("xattr", "-dr", "com.apple.quarantine", dir).Run()
}
