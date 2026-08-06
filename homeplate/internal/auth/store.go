// Package auth implements multi-identity GitHub authentication.
//
// Two credential sources are supported:
//
//	Device Flow OAuth  - no browser redirect server, no client secret.
//	Fine-grained PAT   - pasted in, used verbatim.
//
// Tokens live in the OS keychain (see internal/keyring). Only non-secret
// metadata (login, scopes, host, token kind) is written to
// ~/.homeplate/profiles.json, so that `homeplate auth list` and the daemon's
// multiplexer can enumerate identities without unlocking the keychain.
package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/homeplate-ci/homeplate/internal/config"
	"github.com/homeplate-ci/homeplate/internal/keyring"
)

// Kind distinguishes how a token was obtained.
type Kind string

const (
	KindDeviceFlow Kind = "device_flow"
	KindPAT        Kind = "pat"
)

// Profile is a named GitHub identity on this machine.
type Profile struct {
	Name      string    `json:"name"`
	Login     string    `json:"login"`
	Host      string    `json:"host"`
	Kind      Kind      `json:"kind"`
	Scopes    []string  `json:"scopes"`
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt changes on re-auth, so `auth list` shows token freshness.
	UpdatedAt time.Time `json:"updated_at"`
	// ClientID records which OAuth app minted a device-flow token.
	ClientID string `json:"client_id,omitempty"`
}

// Store manages the set of profiles and their secrets.
type Store struct {
	mu       sync.RWMutex
	path     string
	profiles map[string]*Profile
	ring     keyring.Store
}

// ErrNoProfile indicates the named identity is not configured.
var ErrNoProfile = errors.New("auth: no such profile")

// OpenStore loads ~/.homeplate/profiles.json and the OS keyring.
func OpenStore() (*Store, error) {
	ring, err := keyring.Open()
	if err != nil {
		return nil, err
	}
	return OpenStoreWith(ring)
}

// OpenStoreWith allows injecting a keyring backend (tests).
func OpenStoreWith(ring keyring.Store) (*Store, error) {
	s := &Store{
		path:     filepath.Join(config.Dir(), "profiles.json"),
		profiles: map[string]*Profile{},
		ring:     ring,
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) load() error {
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var list []*Profile
	if err := json.Unmarshal(b, &list); err != nil {
		return fmt.Errorf("parse %s: %w", s.path, err)
	}
	for _, p := range list {
		s.profiles[p.Name] = p
	}
	return nil
}

func (s *Store) persist() error {
	list := make([]*Profile, 0, len(s.profiles))
	for _, p := range s.profiles {
		list = append(list, p)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// keyFor namespaces keychain entries per profile and host so that a work
// profile on GHES and a personal profile on github.com never collide.
func keyFor(p *Profile) string {
	host := p.Host
	if host == "" {
		host = "github.com"
	}
	return fmt.Sprintf("%s@%s", p.Name, host)
}

// Save writes a profile and its token. Re-saving an existing profile updates
// it in place, which makes `homeplate auth add work` safe to re-run.
func (s *Store) Save(p *Profile, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.Name == "" {
		return errors.New("auth: profile name required")
	}
	if strings.ContainsAny(p.Name, "/\\ \t") {
		return fmt.Errorf("auth: profile name %q must not contain spaces or slashes", p.Name)
	}
	if p.Host == "" {
		p.Host = "github.com"
	}
	now := time.Now().UTC()
	if existing, ok := s.profiles[p.Name]; ok {
		p.CreatedAt = existing.CreatedAt
	} else {
		p.CreatedAt = now
	}
	p.UpdatedAt = now
	if token != "" {
		if err := s.ring.Set(keyFor(p), token); err != nil {
			return err
		}
	}
	s.profiles[p.Name] = p
	return s.persist()
}

// Token returns the secret for a profile.
func (s *Store) Token(name string) (string, error) {
	s.mu.RLock()
	p, ok := s.profiles[name]
	s.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrNoProfile, name)
	}
	tok, err := s.ring.Get(keyFor(p))
	if err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			return "", fmt.Errorf("auth: profile %q has no token in %s; re-run `homeplate auth add %s`",
				name, s.ring.Name(), name)
		}
		return "", err
	}
	return tok, nil
}

// Get returns profile metadata.
func (s *Store) Get(name string) (*Profile, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.profiles[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNoProfile, name)
	}
	cp := *p
	return &cp, nil
}

// List returns all profiles sorted by name.
func (s *Store) List() []*Profile {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Profile, 0, len(s.profiles))
	for _, p := range s.profiles {
		cp := *p
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Remove deletes a profile and its secret.
func (s *Store) Remove(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.profiles[name]
	if !ok {
		return fmt.Errorf("%w: %q", ErrNoProfile, name)
	}
	if err := s.ring.Delete(keyFor(p)); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return err
	}
	delete(s.profiles, name)
	return s.persist()
}

// KeyringName exposes the backend name for `homeplate doctor`.
func (s *Store) KeyringName() string { return s.ring.Name() }

// Default returns the profile to use when none is specified: the only profile
// if there is exactly one, otherwise the one named "default"/"personal".
func (s *Store) Default() (*Profile, error) {
	list := s.List()
	switch len(list) {
	case 0:
		return nil, errors.New("auth: no identities configured. Run `homeplate auth add <name>`")
	case 1:
		return list[0], nil
	}
	for _, want := range []string{"default", "personal"} {
		for _, p := range list {
			if p.Name == want {
				return p, nil
			}
		}
	}
	names := make([]string, 0, len(list))
	for _, p := range list {
		names = append(names, p.Name)
	}
	return nil, fmt.Errorf("auth: multiple identities (%s); pass --profile", strings.Join(names, ", "))
}
