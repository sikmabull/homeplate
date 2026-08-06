//go:build !darwin

package runner

// stripQuarantine is a no-op outside macOS.
func stripQuarantine(dir string) error { return nil }
