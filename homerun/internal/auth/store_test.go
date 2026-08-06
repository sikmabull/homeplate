package auth

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/homerun-ci/homerun/internal/keyring"
)

// setupHome isolates HOMERUN_HOME so tests never touch a developer's real
// ~/.homerun or their real keychain.
func setupHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOMERUN_HOME", dir)
	return dir
}

// TestProfileMultiplexing is the core multi-identity requirement: several
// GitHub accounts coexist on one machine, each with its own token, and looking
// up one identity never returns another's credentials.
func TestProfileMultiplexing(t *testing.T) {
	setupHome(t)
	ring := keyring.NewMemory()
	st, err := OpenStoreWith(ring)
	if err != nil {
		t.Fatalf("OpenStoreWith: %v", err)
	}

	identities := []struct {
		profile string
		login   string
		token   string
		kind    Kind
	}{
		{"personal", "alice", "ghp_personal_token_aaaa", KindDeviceFlow},
		{"work", "alice-corp", "ghp_work_token_bbbb", KindPAT},
		{"client", "alice-consulting", "ghp_client_token_cccc", KindPAT},
	}

	for _, id := range identities {
		if err := st.Save(&Profile{Name: id.profile, Login: id.login, Kind: id.kind}, id.token); err != nil {
			t.Fatalf("Save(%s): %v", id.profile, err)
		}
	}

	// Each profile must return exactly its own token.
	for _, id := range identities {
		got, err := st.Token(id.profile)
		if err != nil {
			t.Fatalf("Token(%s): %v", id.profile, err)
		}
		if got != id.token {
			t.Errorf("Token(%s) = %q, want %q", id.profile, got, id.token)
		}
		p, err := st.Get(id.profile)
		if err != nil {
			t.Fatalf("Get(%s): %v", id.profile, err)
		}
		if p.Login != id.login {
			t.Errorf("Get(%s).Login = %q, want %q", id.profile, p.Login, id.login)
		}
	}

	if got := len(st.List()); got != 3 {
		t.Errorf("List() returned %d profiles, want 3", got)
	}

	// Metadata must persist across process restarts, tokens stay in the ring.
	st2, err := OpenStoreWith(ring)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := len(st2.List()); got != 3 {
		t.Fatalf("after reopen List() = %d, want 3", got)
	}
	tok, err := st2.Token("work")
	if err != nil || tok != "ghp_work_token_bbbb" {
		t.Errorf("after reopen Token(work) = %q, %v", tok, err)
	}

	// Removing one identity must not disturb the others.
	if err := st2.Remove("work"); err != nil {
		t.Fatalf("Remove(work): %v", err)
	}
	if _, err := st2.Token("work"); err == nil {
		t.Error("Token(work) succeeded after Remove")
	}
	if tok, err := st2.Token("personal"); err != nil || tok != "ghp_personal_token_aaaa" {
		t.Errorf("Remove(work) damaged personal: %q %v", tok, err)
	}
}

// TestTokensNeverWrittenToDisk guards the "never plaintext" promise: the
// profiles.json sidecar must contain metadata only.
func TestTokensNeverWrittenToDisk(t *testing.T) {
	home := setupHome(t)
	st, err := OpenStoreWith(keyring.NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	const secret = "ghp_supersecrettokenvalue1234567890"
	if err := st.Save(&Profile{Name: "personal", Login: "alice"}, secret); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(filepath.Join(home, "profiles.json"))
	if err != nil {
		t.Fatalf("profiles.json: %v", err)
	}
	if string(b) == "" {
		t.Fatal("profiles.json is empty")
	}
	if contains(string(b), secret) {
		t.Fatalf("SECURITY: token leaked into profiles.json:\n%s", b)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		(func() bool {
			for i := 0; i+len(needle) <= len(haystack); i++ {
				if haystack[i:i+len(needle)] == needle {
					return true
				}
			}
			return false
		})()
}

// TestSaveIsIdempotent proves re-authenticating a profile updates in place
// rather than creating duplicates.
func TestSaveIsIdempotent(t *testing.T) {
	setupHome(t)
	st, err := OpenStoreWith(keyring.NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := st.Save(&Profile{Name: "work", Login: "alice"}, "tok-v"+string(rune('a'+i))); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(st.List()); got != 1 {
		t.Fatalf("re-saving created %d profiles, want 1", got)
	}
	tok, _ := st.Token("work")
	if tok != "tok-vc" {
		t.Errorf("token = %q, want the latest tok-vc", tok)
	}
	// CreatedAt must survive re-auth; UpdatedAt must move.
	p, _ := st.Get("work")
	if p.CreatedAt.After(p.UpdatedAt) {
		t.Error("CreatedAt should not be after UpdatedAt")
	}
}

// TestDefaultProfileResolution covers the "which identity?" UX rules.
func TestDefaultProfileResolution(t *testing.T) {
	setupHome(t)
	st, err := OpenStoreWith(keyring.NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Default(); err == nil {
		t.Error("Default() with zero profiles should error")
	}

	st.Save(&Profile{Name: "solo", Login: "alice"}, "t1")
	p, err := st.Default()
	if err != nil || p.Name != "solo" {
		t.Errorf("single profile should be the default, got %v %v", p, err)
	}

	st.Save(&Profile{Name: "work", Login: "bob"}, "t2")
	if _, err := st.Default(); err == nil {
		t.Error("ambiguous default should error and ask for --profile")
	}

	st.Save(&Profile{Name: "personal", Login: "carol"}, "t3")
	p, err = st.Default()
	if err != nil || p.Name != "personal" {
		t.Errorf(`"personal" should win as default, got %v %v`, p, err)
	}
}

// TestProfileNameValidation rejects names that would break keychain keys.
func TestProfileNameValidation(t *testing.T) {
	setupHome(t)
	st, _ := OpenStoreWith(keyring.NewMemory())
	for _, bad := range []string{"", "has space", "has/slash"} {
		if err := st.Save(&Profile{Name: bad, Login: "x"}, "tok"); err == nil {
			t.Errorf("Save(%q) should have been rejected", bad)
		}
	}
}

// TestKeysAreHostNamespaced ensures a github.com profile and a GHES profile
// with the same name cannot collide in the credential store.
func TestKeysAreHostNamespaced(t *testing.T) {
	a := keyFor(&Profile{Name: "work", Host: "github.com"})
	b := keyFor(&Profile{Name: "work", Host: "ghe.corp.example"})
	if a == b {
		t.Fatalf("keys collide across hosts: %q", a)
	}
	if keyFor(&Profile{Name: "work"}) != "work@github.com" {
		t.Errorf("empty host should default to github.com, got %q", keyFor(&Profile{Name: "work"}))
	}
}
