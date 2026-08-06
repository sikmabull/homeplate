// Package keyring stores GitHub tokens in the OS credential store.
//
// macOS: the login Keychain via /usr/bin/security.
// Linux: libsecret via secret-tool (gnome-keyring / KWallet backends).
//
// A plaintext file backend exists ONLY for automated tests and headless CI and
// must be opted into with HOMEPLATE_KEYRING=file. It is never selected
// automatically, because silently degrading to plaintext is exactly the
// failure mode this package exists to prevent.
package keyring

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// ErrNotFound is returned when no secret exists for the key.
var ErrNotFound = errors.New("keyring: secret not found")

// Service is the keychain service name under which Homeplate stores tokens.
const Service = "homeplate-ci"

// Store is the credential backend interface.
type Store interface {
	Set(key, value string) error
	Get(key string) (string, error)
	Delete(key string) error
	// Name identifies the backend for `homeplate doctor`.
	Name() string
}

var (
	mu       sync.Mutex
	override Store
)

// SetOverride injects a Store (used by tests).
func SetOverride(s Store) {
	mu.Lock()
	defer mu.Unlock()
	override = s
}

// Open selects the best available backend for this host.
func Open() (Store, error) {
	mu.Lock()
	o := override
	mu.Unlock()
	if o != nil {
		return o, nil
	}
	switch strings.ToLower(os.Getenv("HOMEPLATE_KEYRING")) {
	case "file":
		return newFileStore()
	case "keychain":
		return &macStore{}, nil
	case "libsecret":
		return &secretToolStore{}, nil
	}
	switch runtime.GOOS {
	case "darwin":
		if _, err := exec.LookPath("security"); err != nil {
			return nil, fmt.Errorf("keyring: /usr/bin/security not found; cannot access macOS Keychain")
		}
		return &macStore{}, nil
	case "linux":
		if _, err := exec.LookPath("secret-tool"); err != nil {
			return nil, fmt.Errorf("keyring: secret-tool not found. Install libsecret-tools " +
				"(apt install libsecret-tools / dnf install libsecret) or set HOMEPLATE_KEYRING=file " +
				"to accept plaintext token storage")
		}
		return &secretToolStore{}, nil
	default:
		return nil, fmt.Errorf("keyring: no OS credential store for %s; set HOMEPLATE_KEYRING=file to accept plaintext", runtime.GOOS)
	}
}

// ---------- macOS Keychain ----------

type macStore struct{}

func (m *macStore) Name() string { return "macOS Keychain (login)" }

func (m *macStore) Set(key, value string) error {
	// -U updates in place if the item already exists, making Set idempotent.
	// The secret is passed with -w; we accept the argv exposure tradeoff being
	// visible only to the same UID (macOS `ps` does not show other users' args
	// to non-root), and prefer it to writing a temp file.
	cmd := exec.Command("security", "add-generic-password",
		"-a", key, "-s", Service, "-U", "-w", value,
		"-D", "Homeplate GitHub token",
		"-j", "Managed by Homeplate. Delete with: homeplate auth remove <profile>")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("keychain write failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (m *macStore) Get(key string) (string, error) {
	cmd := exec.Command("security", "find-generic-password", "-a", key, "-s", Service, "-w")
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			// security(1) exits 44 for "item not found".
			return "", ErrNotFound
		}
		return "", err
	}
	return strings.TrimRight(string(out), "\n"), nil
}

func (m *macStore) Delete(key string) error {
	cmd := exec.Command("security", "delete-generic-password", "-a", key, "-s", Service)
	if out, err := cmd.CombinedOutput(); err != nil {
		if strings.Contains(string(out), "could not be found") {
			return ErrNotFound
		}
		return fmt.Errorf("keychain delete failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ---------- Linux libsecret ----------

type secretToolStore struct{}

func (s *secretToolStore) Name() string { return "libsecret (secret-tool)" }

func (s *secretToolStore) Set(key, value string) error {
	cmd := exec.Command("secret-tool", "store", "--label=Homeplate GitHub token",
		"service", Service, "account", key)
	cmd.Stdin = strings.NewReader(value)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("libsecret write failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (s *secretToolStore) Get(key string) (string, error) {
	out, err := exec.Command("secret-tool", "lookup", "service", Service, "account", key).Output()
	if err != nil {
		return "", ErrNotFound
	}
	v := strings.TrimRight(string(out), "\n")
	if v == "" {
		return "", ErrNotFound
	}
	return v, nil
}

func (s *secretToolStore) Delete(key string) error {
	if out, err := exec.Command("secret-tool", "clear", "service", Service, "account", key).CombinedOutput(); err != nil {
		return fmt.Errorf("libsecret delete failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ---------- test-only file backend ----------

type fileStore struct {
	dir string
	mu  sync.Mutex
}

func newFileStore() (Store, error) {
	dir := os.Getenv("HOMEPLATE_KEYRING_DIR")
	if dir == "" {
		home := os.Getenv("HOMEPLATE_HOME")
		if home == "" {
			h, err := os.UserHomeDir()
			if err != nil {
				return nil, err
			}
			home = filepath.Join(h, ".homeplate")
		}
		dir = filepath.Join(home, "insecure-credentials")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &fileStore{dir: dir}, nil
}

func (f *fileStore) Name() string { return "PLAINTEXT FILE (insecure, test-only)" }

func (f *fileStore) path(key string) string {
	safe := strings.NewReplacer("/", "_", "..", "_", string(filepath.Separator), "_").Replace(key)
	return filepath.Join(f.dir, safe+".token")
}

func (f *fileStore) Set(key, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return os.WriteFile(f.path(key), []byte(value), 0o600)
}

func (f *fileStore) Get(key string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, err := os.ReadFile(f.path(key))
	if err != nil {
		if os.IsNotExist(err) {
			return "", ErrNotFound
		}
		return "", err
	}
	return strings.TrimRight(string(b), "\n"), nil
}

func (f *fileStore) Delete(key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	err := os.Remove(f.path(key))
	if os.IsNotExist(err) {
		return ErrNotFound
	}
	return err
}

// Memory is an in-process Store for unit tests.
type Memory struct {
	mu sync.Mutex
	m  map[string]string
}

func NewMemory() *Memory { return &Memory{m: map[string]string{}} }

func (m *Memory) Name() string { return "memory (test)" }

func (m *Memory) Set(key, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.m == nil {
		m.m = map[string]string{}
	}
	m.m[key] = value
	return nil
}

func (m *Memory) Get(key string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.m[key]
	if !ok {
		return "", ErrNotFound
	}
	return v, nil
}

func (m *Memory) Delete(key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.m[key]; !ok {
		return ErrNotFound
	}
	delete(m.m, key)
	return nil
}
