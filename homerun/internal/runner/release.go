// Package runner implements Engine A: Homerun's wrapper around GitHub's
// official actions/runner agent.
//
// Homerun does not reimplement the runner protocol. It downloads the official
// agent, registers it as an EPHEMERAL runner (one job per registration, so
// state cannot leak between jobs), and executes it inside a throwaway Docker
// container with hard resource caps.
package runner

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Release describes an actions/runner release asset for one platform.
type Release struct {
	Version  string // "2.330.0" (no leading v)
	Platform string // "osx-arm64", "linux-x64", ...
	URL      string
	SHA256   string // optional; verified when known
}

// FallbackVersion is used when GitHub's release API is unreachable (offline
// first run). It is deliberately a real, known-good version rather than a
// guess. Homerun always passes --disableupdate so the agent cannot silently
// self-update into a broken release mid-job.
const FallbackVersion = "2.335.1"

// MacARM64SafeVersion pins actions/runner on Apple Silicon.
//
// WHY: actions/runner v2.336.0 has an open macOS/arm64 regression where
// Runner.Worker deadlocks inside Process.Start - every job wedges at 100% CPU
// and never completes (actions/runner issues #4570 and #4575, fix PR #4572).
// v2.335.1 is the last known-good release on this platform. Homerun pins it
// for NATIVE macOS arm64 jobs only; Linux container jobs are unaffected and
// track latest.
//
// Remove this pin once a release after 2.336.0 ships with the fix.
const MacARM64SafeVersion = "2.335.1"

// SafeVersionFor applies platform-specific pins to a resolved version.
func SafeVersionFor(version, platform string) (safe string, pinned bool, why string) {
	if platform == "osx-arm64" && version != MacARM64SafeVersion {
		if isAtLeast(version, "2.336.0") {
			return MacARM64SafeVersion, true,
				"actions/runner " + version + " deadlocks on macOS arm64 (actions/runner#4570); " +
					"pinned to " + MacARM64SafeVersion
		}
	}
	return version, false, ""
}

// isAtLeast compares dotted versions numerically (2.336.0 >= 2.336.0).
func isAtLeast(v, min string) bool {
	pv, pm := parseVersion(v), parseVersion(min)
	for i := 0; i < 3; i++ {
		switch {
		case pv[i] > pm[i]:
			return true
		case pv[i] < pm[i]:
			return false
		}
	}
	return true
}

func parseVersion(v string) [3]int {
	var out [3]int
	for i, part := range strings.SplitN(strings.TrimPrefix(v, "v"), ".", 3) {
		if i > 2 {
			break
		}
		n := 0
		for _, r := range part {
			if r < '0' || r > '9' {
				break
			}
			n = n*10 + int(r-'0')
		}
		out[i] = n
	}
	return out
}

// Platform returns the actions/runner platform moniker for this host.
func Platform() (string, error) { return platformFor(runtime.GOOS, runtime.GOARCH) }

func platformFor(goos, goarch string) (string, error) {
	var osPart string
	switch goos {
	case "darwin":
		osPart = "osx"
	case "linux":
		osPart = "linux"
	case "windows":
		osPart = "win"
	default:
		return "", fmt.Errorf("actions/runner does not ship for %s", goos)
	}
	var archPart string
	switch goarch {
	case "amd64":
		archPart = "x64"
	case "arm64":
		archPart = "arm64"
	case "arm":
		archPart = "arm"
	default:
		return "", fmt.Errorf("actions/runner does not ship for %s/%s", goos, goarch)
	}
	if osPart == "osx" && archPart == "arm" {
		return "", fmt.Errorf("actions/runner does not ship for macOS 32-bit arm")
	}
	return osPart + "-" + archPart, nil
}

// LatestVersion asks GitHub for the newest actions/runner release. It works
// unauthenticated but accepts a token to avoid rate limiting.
func LatestVersion(ctx context.Context, token string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.github.com/repos/actions/runner/releases/latest", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "homerun")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("actions/runner releases: %s", resp.Status)
	}
	var out struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return strings.TrimPrefix(out.TagName, "v"), nil
}

// ResolveRelease builds the download descriptor for a version+platform.
// The URL layout is stable across releases:
//
//	https://github.com/actions/runner/releases/download/v<V>/actions-runner-<platform>-<V>.tar.gz
func ResolveRelease(version, platform string) Release {
	version = strings.TrimPrefix(version, "v")
	ext := "tar.gz"
	if strings.HasPrefix(platform, "win") {
		ext = "zip"
	}
	return Release{
		Version:  version,
		Platform: platform,
		URL: fmt.Sprintf("https://github.com/actions/runner/releases/download/v%s/actions-runner-%s-%s.%s",
			version, platform, version, ext),
	}
}

// Cache manages downloaded runner tarballs under ~/.homerun/runner.
type Cache struct{ Dir string }

// NewCache roots the cache at homeDir/runner.
func NewCache(homeDir string) *Cache { return &Cache{Dir: filepath.Join(homeDir, "runner")} }

// TarballPath is where a release archive is stored.
func (c *Cache) TarballPath(r Release) string {
	return filepath.Join(c.Dir, "dist", fmt.Sprintf("actions-runner-%s-%s.tar.gz", r.Platform, r.Version))
}

// ExtractedDir is where a release is unpacked for native (macOS) execution.
func (c *Cache) ExtractedDir(r Release) string {
	return filepath.Join(c.Dir, "v"+r.Version+"-"+r.Platform)
}

// Ensure downloads and extracts the runner if not already cached, and returns
// the extracted directory. Downloads are atomic: a partial download can never
// be mistaken for a complete one.
func (c *Cache) Ensure(ctx context.Context, r Release, progress func(string)) (string, error) {
	dir := c.ExtractedDir(r)
	// run.sh is the last thing extracted from a valid archive; its presence is
	// the completion marker.
	if _, err := os.Stat(filepath.Join(dir, "run.sh")); err == nil {
		return dir, nil
	}

	tarball := c.TarballPath(r)
	if _, err := os.Stat(tarball); err != nil {
		if progress != nil {
			progress(fmt.Sprintf("downloading actions/runner %s (%s)", r.Version, r.Platform))
		}
		if err := download(ctx, r.URL, tarball); err != nil {
			return "", fmt.Errorf("download actions/runner: %w", err)
		}
	}
	if r.SHA256 != "" {
		sum, err := fileSHA256(tarball)
		if err != nil {
			return "", err
		}
		if !strings.EqualFold(sum, r.SHA256) {
			os.Remove(tarball)
			return "", fmt.Errorf("actions/runner checksum mismatch: got %s want %s", sum, r.SHA256)
		}
	}

	if progress != nil {
		progress("extracting actions/runner " + r.Version)
	}
	tmp := dir + ".tmp"
	os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return "", err
	}
	if err := extractTarGz(tarball, tmp); err != nil {
		os.RemoveAll(tmp)
		return "", fmt.Errorf("extract actions/runner: %w", err)
	}
	os.RemoveAll(dir)
	if err := os.Rename(tmp, dir); err != nil {
		return "", err
	}

	if runtime.GOOS == "darwin" {
		// Downloads carry com.apple.quarantine, which makes Gatekeeper refuse
		// to exec the binaries with an opaque "cannot be opened" error.
		// Stripping the attribute on a file we just downloaded ourselves is
		// the standard, documented fix.
		_ = stripQuarantine(dir)
	}
	return dir, nil
}

func download(ctx context.Context, url, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "homerun")
	resp, err := (&http.Client{Timeout: 30 * time.Minute}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// extractTarGz unpacks an archive, rejecting path traversal entries.
func extractTarGz(archive, dest string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		// Reject "../" escapes (zip-slip).
		target := filepath.Join(dest, filepath.Clean("/"+hdr.Name))
		if !strings.HasPrefix(target, filepath.Clean(dest)+string(os.PathSeparator)) && target != filepath.Clean(dest) {
			return fmt.Errorf("refusing tar entry outside destination: %q", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)|0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			out.Close()
		case tar.TypeSymlink:
			os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		}
	}
}
