// Package repofind discovers Git working copies on the local machine and
// identifies the GitHub (or GHES) repo each one tracks.
//
// This powers two features:
//
//	homeplate scan   - show/link the repos this computer already has
//	link LocalPath   - so the daemon can run never-pushed commits offline
//
// Scanning is deliberately conservative: bounded depth, skips vendor and
// dependency directories, never crosses into network mounts' typical homes
// without being asked, and stops descending into a directory once a .git is
// found (no submodule spelunking).
package repofind

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Found is one discovered working copy.
type Found struct {
	Path   string // absolute path of the working copy root
	Slug   string // owner/repo, when the origin remote is a GitHub URL
	Host   string // github.com or GHES hostname
	Branch string // current branch
	Dirty  bool   // uncommitted changes present
}

// DefaultRoots returns the directories developers actually keep clones in.
// The home directory itself is scanned shallowly as a catch-all.
func DefaultRoots() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	var roots []string
	for _, sub := range []string{
		"Documents", "code", "Code", "dev", "Dev", "devel", "src", "Source",
		"Projects", "projects", "work", "Work", "workspace", "repos", "git",
		"github", "GitHub", "go/src",
	} {
		p := filepath.Join(home, sub)
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			roots = append(roots, p)
		}
	}
	return roots
}

// skipDirs are never descended into: dependency/vendor trees are huge and
// never the user's own clone.
var skipDirs = map[string]bool{
	"node_modules": true, "vendor": true, ".git": true,
	"Library": true, ".cache": true, ".npm": true, ".cargo": true,
	".rustup": true, ".pyenv": true, ".venv": true, "venv": true,
	"dist": true, "build": true, ".next": true, "target": true,
}

// Scan walks roots to maxDepth looking for working copies with a GitHub
// origin remote. ctx cancellation stops the walk promptly.
func Scan(ctx context.Context, roots []string, maxDepth int) ([]Found, error) {
	if maxDepth <= 0 {
		maxDepth = 5
	}
	seen := map[string]bool{}
	var out []Found

	for _, root := range roots {
		root, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		baseDepth := len(strings.Split(root, string(filepath.Separator)))
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			if !d.IsDir() {
				return nil
			}
			name := d.Name()
			if path != root {
				if skipDirs[name] || strings.HasPrefix(name, ".") {
					return filepath.SkipDir
				}
				if len(strings.Split(path, string(filepath.Separator)))-baseDepth >= maxDepth {
					return filepath.SkipDir
				}
			}
			if st, err := os.Stat(filepath.Join(path, ".git")); err == nil && st.IsDir() {
				if !seen[path] {
					seen[path] = true
					if f, err := inspect(ctx, path); err == nil && f.Slug != "" {
						out = append(out, f)
					}
				}
				return filepath.SkipDir // do not descend into a clone
			}
			return nil
		})
	}
	return out, nil
}

// inspect resolves one working copy: origin slug, host, branch, dirtiness.
func inspect(ctx context.Context, path string) (Found, error) {
	f := Found{Path: path}
	url, err := gitOut(ctx, path, "remote", "get-url", "origin")
	if err != nil {
		return f, err
	}
	host, slug, ok := ParseRemoteURL(url)
	if !ok {
		return f, fmt.Errorf("origin is not a GitHub remote: %s", url)
	}
	f.Host, f.Slug = host, slug
	if b, err := gitOut(ctx, path, "rev-parse", "--abbrev-ref", "HEAD"); err == nil && b != "HEAD" {
		f.Branch = b
	}
	if st, err := gitOut(ctx, path, "status", "--porcelain"); err == nil {
		f.Dirty = strings.TrimSpace(st) != ""
	}
	return f, nil
}

// ParseRemoteURL extracts (host, owner/repo) from HTTPS or SSH GitHub-style
// remote URLs:
//
//	https://github.com/owner/repo(.git)
//	git@github.com:owner/repo(.git)
//	ssh://git@host/owner/repo(.git)
func ParseRemoteURL(u string) (host, slug string, ok bool) {
	u = strings.TrimSpace(strings.TrimSuffix(u, ".git"))
	if u == "" {
		return "", "", false
	}
	var rest string
	switch {
	case strings.HasPrefix(u, "https://"), strings.HasPrefix(u, "http://"):
		i := strings.Index(u, "://")
		host = u[i+3:]
		if j := strings.Index(host, "/"); j >= 0 {
			rest = host[j+1:]
			host = host[:j]
		}
	case strings.HasPrefix(u, "ssh://"):
		rest = strings.TrimPrefix(u, "ssh://")
		rest = strings.TrimPrefix(rest, "git@")
		if i := strings.Index(rest, "/"); i >= 0 {
			host, rest = rest[:i], rest[i+1:]
		}
	case strings.Contains(u, "@") && strings.Contains(u, ":"):
		// scp-like: git@host:owner/repo
		at := strings.Index(u, "@")
		colon := strings.Index(u[at:], ":")
		if colon < 0 {
			return "", "", false
		}
		host = u[at+1 : at+colon]
		rest = u[at+colon+1:]
	default:
		return "", "", false
	}
	// Strip any userinfo remnants and port.
	if i := strings.Index(host, "@"); i >= 0 {
		host = host[i+1:]
	}
	if i := strings.Index(host, ":"); i >= 0 {
		host = host[:i]
	}
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if host == "" || len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return host, parts[0] + "/" + parts[1], true
}

// CloneOf returns the GitHub slug for the repo containing dir, if dir is
// inside a working copy with a GitHub origin remote. Used by `homeplate
// link` to record LocalPath and by `homeplate run` for slug inference.
func CloneOf(ctx context.Context, dir string) (Found, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Found{}, err
	}
	top, err := gitOut(ctx, abs, "rev-parse", "--show-toplevel")
	if err != nil {
		return Found{}, fmt.Errorf("%s is not inside a git working copy", dir)
	}
	return inspect(ctx, top)
}

// FindForSlug looks for a local working copy of slug: the current directory
// first, then the standard roots. A short timeout keeps `link` snappy on
// machines with big home directories.
func FindForSlug(ctx context.Context, slug string) (string, bool) {
	if f, err := CloneOf(ctx, "."); err == nil && strings.EqualFold(f.Slug, slug) {
		return f.Path, true
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	found, _ := Scan(ctx, DefaultRoots(), 5)
	for _, f := range found {
		if strings.EqualFold(f.Slug, slug) {
			return f.Path, true
		}
	}
	return "", false
}

func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	full := append([]string{"-C", dir}, args...)
	out, err := exec.CommandContext(ctx, "git", full...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
