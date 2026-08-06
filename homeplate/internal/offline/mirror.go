// Package offline implements Engine B: running workflows locally with
// nektos/act when GitHub is unreachable or degraded.
//
// nektos/act (https://github.com/nektos/act) is MIT licensed and is credited
// in Homeplate's README. Homeplate shells out to the `act` binary rather than
// vendoring it, so users get act's own release cadence and its Docker image
// handling, and so act's license obligations stay clean and visible.
package offline

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Mirror is a local bare clone of a watched repository.
//
// Engine B must run workflows against REAL commits while offline, so Homeplate
// keeps a bare mirror per linked repo and refreshes it on every connected
// sync. Bare (rather than a full checkout) keeps disk use low; per-job working
// trees are created from the mirror and destroyed after the run.
type Mirror struct {
	Slug string
	Path string
}

// MirrorRoot is where all mirrors live.
func MirrorRoot(homeDir string) string { return filepath.Join(homeDir, "mirrors") }

// MirrorPath is the bare clone directory for a repo.
func MirrorPath(homeDir, slug string) string {
	return filepath.Join(MirrorRoot(homeDir), strings.ReplaceAll(slug, "/", "__")+".git")
}

// EnsureMirror creates or updates the bare clone.
//
// remoteURL should be an authenticated HTTPS URL or an SSH URL. Homeplate passes
// x-access-token style HTTPS so that no SSH key is required, and deliberately
// never writes the token into the repo config. The token is extracted from the
// URL and handed to git via a GIT_ASKPASS helper + environment variable, so it
// never appears in the process argument list either (argv is visible in `ps`).
func EnsureMirror(ctx context.Context, homeDir, slug, remoteURL string, log func(string)) (*Mirror, error) {
	path := MirrorPath(homeDir, slug)
	m := &Mirror{Slug: slug, Path: path}

	env, cleanURL, err := gitCredentialEnv(homeDir, remoteURL)
	if err != nil {
		return nil, err
	}

	if _, err := os.Stat(filepath.Join(path, "HEAD")); err == nil {
		if log != nil {
			log(fmt.Sprintf("refreshing mirror %s", slug))
		}
		cmd := exec.CommandContext(ctx, "git", "--git-dir", path, "fetch", "--prune", "--tags", cleanURL,
			"+refs/heads/*:refs/heads/*", "+refs/pull/*/head:refs/pull/*/head")
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			// git may echo the remote URL in its error text; scrub anyway in
			// case a credential ever ends up there.
			return m, fmt.Errorf("git fetch %s: %w: %s", slug, err, scrub(string(out), remoteURL))
		}
		return m, nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if log != nil {
		log(fmt.Sprintf("creating mirror %s (first time)", slug))
	}
	cmd := exec.CommandContext(ctx, "git", "clone", "--mirror", cleanURL, path)
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("git clone --mirror %s: %w: %s", slug, err, scrub(string(out), remoteURL))
	}
	return m, nil
}

// gitCredentialEnv splits an x-access-token URL into a credential-free URL
// plus an environment that answers git's credential prompt. If the URL has
// no token, the plain environment is returned.
func gitCredentialEnv(homeDir, remoteURL string) ([]string, string, error) {
	token := ""
	clean := remoteURL
	if i := strings.Index(remoteURL, "://x-access-token:"); i >= 0 {
		if j := strings.Index(remoteURL[i+len("://x-access-token:"):], "@"); j >= 0 {
			token = remoteURL[i+len("://x-access-token:") : i+len("://x-access-token:")+j]
			clean = remoteURL[:i+3] + remoteURL[i+len("://x-access-token:")+j+1:]
		}
	}
	if token == "" {
		return os.Environ(), clean, nil
	}
	helper, err := askpassHelper(homeDir)
	if err != nil {
		return nil, "", err
	}
	env := append(os.Environ(),
		"GIT_ASKPASS="+helper,
		"GIT_TERMINAL_PROMPT=0",
		"HOMEPLATE_GIT_TOKEN="+token,
	)
	return env, clean, nil
}

// askpassHelper writes (once) the tiny script git invokes for credentials.
func askpassHelper(homeDir string) (string, error) {
	dir := filepath.Join(homeDir, "bin")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	p := filepath.Join(dir, "git-askpass.sh")
	const script = `#!/bin/sh
# Homeplate GIT_ASKPASS helper: answers git's credential prompt from the
# environment so tokens never appear in argv or .git/config.
case "$1" in
  *sername*) echo "x-access-token" ;;
  *) echo "${HOMEPLATE_GIT_TOKEN:-}" ;;
esac
`
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}
	if err := os.WriteFile(p, []byte(script), 0o700); err != nil {
		return "", err
	}
	return p, nil
}

// HasCommit reports whether the mirror already contains a SHA, which decides
// whether an offline run is even possible.
func (m *Mirror) HasCommit(ctx context.Context, sha string) bool {
	if sha == "" {
		return false
	}
	cmd := exec.CommandContext(ctx, "git", "--git-dir", m.Path, "cat-file", "-e", sha+"^{commit}")
	return cmd.Run() == nil
}

// ResolveRef turns a ref into a SHA using only local data.
func (m *Mirror) ResolveRef(ctx context.Context, ref string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "--git-dir", m.Path, "rev-parse", ref+"^{commit}").Output()
	if err != nil {
		return "", fmt.Errorf("mirror %s has no ref %q locally", m.Slug, ref)
	}
	return strings.TrimSpace(string(out)), nil
}

// Checkout materialises a working tree at a SHA. The caller must call the
// returned cleanup function.
func (m *Mirror) Checkout(ctx context.Context, dest, sha string) (cleanup func(), err error) {
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return func() {}, err
	}
	noop := func() { os.RemoveAll(dest) }

	// A local clone from the mirror is cheap (hardlinked objects) and gives a
	// normal working tree that act and the workflow's own git commands expect.
	cmd := exec.CommandContext(ctx, "git", "clone", "--no-checkout", "--local", "--shared", m.Path, dest)
	if out, err := cmd.CombinedOutput(); err != nil {
		return noop, fmt.Errorf("local clone: %w: %s", err, strings.TrimSpace(string(out)))
	}
	co := exec.CommandContext(ctx, "git", "-C", dest, "checkout", "--detach", sha)
	if out, err := co.CombinedOutput(); err != nil {
		return noop, fmt.Errorf("checkout %s: %w: %s", sha, err, strings.TrimSpace(string(out)))
	}
	return noop, nil
}

// LocalRepoMirror registers the developer's OWN working clone as a source, so
// that a commit made while offline (and therefore never pushed) can still be
// tested. This is what makes the "unplug ethernet, commit, watch it run"
// demo work: the commit exists nowhere but the local disk.
func LocalRepoMirror(ctx context.Context, homeDir, slug, localRepoPath string) (*Mirror, error) {
	gitDir := filepath.Join(localRepoPath, ".git")
	if _, err := os.Stat(gitDir); err != nil {
		return nil, fmt.Errorf("%s is not a git working copy", localRepoPath)
	}
	path := MirrorPath(homeDir, slug)
	if _, err := os.Stat(filepath.Join(path, "HEAD")); err != nil {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		cmd := exec.CommandContext(ctx, "git", "clone", "--mirror", localRepoPath, path)
		if out, err := cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("mirror local repo: %w: %s", err, strings.TrimSpace(string(out)))
		}
		return &Mirror{Slug: slug, Path: path}, nil
	}
	cmd := exec.CommandContext(ctx, "git", "--git-dir", path, "fetch", "--prune", localRepoPath,
		"+refs/heads/*:refs/heads/*")
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("fetch from local repo: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return &Mirror{Slug: slug, Path: path}, nil
}

// AuthenticatedURL builds an HTTPS clone URL carrying a token.
//
// The token is embedded in the URL passed to git as an ARGUMENT, never written
// to .git/config, and never logged (callers redact it).
func AuthenticatedURL(host, slug, token string) string {
	if host == "" {
		host = "github.com"
	}
	if token == "" {
		return fmt.Sprintf("https://%s/%s.git", host, slug)
	}
	return fmt.Sprintf("https://x-access-token:%s@%s/%s.git", token, host, slug)
}

// scrub removes any credential material from captured subprocess output.
//
// git is passed an authenticated URL as an argv element. Its error messages
// sometimes echo that URL back, which would write a live token into Homeplate's
// logs. Both the full URL and the bare token are replaced.
func scrub(out, remoteURL string) string {
	out = strings.TrimSpace(out)
	if remoteURL == "" {
		return out
	}
	out = strings.ReplaceAll(out, remoteURL, RedactURL(remoteURL))
	if tok := tokenFromURL(remoteURL); tok != "" {
		out = strings.ReplaceAll(out, tok, "***")
	}
	return out
}

// tokenFromURL extracts the password component of https://user:token@host/...
func tokenFromURL(u string) string {
	at := strings.LastIndex(u, "@")
	scheme := strings.Index(u, "://")
	if at < 0 || scheme < 0 || at < scheme {
		return ""
	}
	userinfo := u[scheme+3 : at]
	if i := strings.Index(userinfo, ":"); i >= 0 {
		return userinfo[i+1:]
	}
	return ""
}

// RedactURL removes credentials for safe logging.
func RedactURL(u string) string {
	if i := strings.Index(u, "@"); i > 0 {
		if j := strings.Index(u, "://"); j >= 0 && j < i {
			return u[:j+3] + "***@" + u[i+1:]
		}
	}
	return u
}

// MirrorInfo reports mirror freshness for `homeplate status`.
type MirrorInfo struct {
	Slug    string
	Path    string
	Exists  bool
	SizeMB  float64
	Updated time.Time
	HeadSHA string
}

// Inspect gathers mirror metadata.
func Inspect(ctx context.Context, homeDir, slug string) MirrorInfo {
	path := MirrorPath(homeDir, slug)
	info := MirrorInfo{Slug: slug, Path: path}
	st, err := os.Stat(filepath.Join(path, "HEAD"))
	if err != nil {
		return info
	}
	info.Exists = true
	info.Updated = st.ModTime()
	if out, err := exec.CommandContext(ctx, "git", "--git-dir", path, "rev-parse", "HEAD").Output(); err == nil {
		info.HeadSHA = strings.TrimSpace(string(out))
	}
	var total int64
	_ = filepath.Walk(path, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			total += fi.Size()
		}
		return nil
	})
	info.SizeMB = float64(total) / (1 << 20)
	return info
}
