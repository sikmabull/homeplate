package keyring

import (
	"errors"
	"os"
	"runtime"
	"testing"
)

// TestMemoryStore covers the in-process backend used by other packages' tests.
func TestMemoryStore(t *testing.T) {
	m := NewMemory()
	if _, err := m.Get("absent"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(absent) = %v, want ErrNotFound", err)
	}
	if err := m.Set("k", "v"); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.Get("k"); got != "v" {
		t.Errorf("Get = %q, want v", got)
	}
	if err := m.Delete("k"); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(m.Delete("k"), ErrNotFound) {
		t.Error("double delete should report ErrNotFound")
	}
}

// TestPlaintextIsNeverAutomatic is a security guarantee: Homeplate must never
// silently downgrade to plaintext token storage.
func TestPlaintextIsNeverAutomatic(t *testing.T) {
	t.Setenv("HOMEPLATE_KEYRING", "")
	SetOverride(nil)
	t.Cleanup(func() { SetOverride(nil) })

	store, err := Open()
	if err != nil {
		// Acceptable on a machine with no credential store: Open must FAIL
		// rather than silently pick the plaintext backend.
		return
	}
	if store.Name() == (&fileStore{}).Name() {
		t.Fatalf("SECURITY: Open() auto-selected the plaintext backend (%s)", store.Name())
	}
}

// TestRealOSKeychainRoundTrip exercises the actual platform credential store.
func TestRealOSKeychainRoundTrip(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS Keychain test")
	}
	if os.Getenv("CI") != "" {
		t.Skip("CI runners have no unlocked login keychain")
	}
	t.Setenv("HOMEPLATE_KEYRING", "keychain")
	SetOverride(nil)
	t.Cleanup(func() { SetOverride(nil) })

	store, err := Open()
	if err != nil {
		t.Skipf("keychain unavailable: %v", err)
	}
	const key = "homeplate-selftest@github.com"
	const val = "ghp_selftest_value_1234567890"
	t.Cleanup(func() { _ = store.Delete(key) })

	if err := store.Set(key, val); err != nil {
		t.Skipf("keychain write blocked in this environment: %v", err)
	}
	got, err := store.Get(key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != val {
		t.Fatalf("Get = %q, want %q", got, val)
	}

	// Re-setting must update in place (idempotent re-auth), not duplicate.
	if err := store.Set(key, val+"-v2"); err != nil {
		t.Fatalf("re-Set: %v", err)
	}
	if got, _ := store.Get(key); got != val+"-v2" {
		t.Errorf("after overwrite Get = %q, want the new value", got)
	}

	if err := store.Delete(key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Get(key); !errors.Is(err, ErrNotFound) {
		t.Errorf("after Delete, Get = %v, want ErrNotFound", err)
	}
}
