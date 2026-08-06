package runner

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/homerun-ci/homerun/internal/config"
	"github.com/homerun-ci/homerun/internal/ghapi"
	"github.com/homerun-ci/homerun/internal/labels"
	"github.com/homerun-ci/homerun/internal/store"
)

// Target is one linked repo or org that this machine serves.
type Target struct {
	Slug        string // owner/repo or owner
	Scope       string // "repo" | "org"
	Profile     string
	RunnerGroup string
	Labels      []string
}

// URL is the registration URL the runner agent expects.
func (t Target) URL(host string) string {
	base := "https://github.com"
	if host != "" && host != "github.com" {
		base = "https://" + host
	}
	return base + "/" + t.Slug
}

// TokenMinter mints registration tokens; satisfied by *ghapi.Client and by
// fakes in tests.
type TokenMinter interface {
	RepoRegistrationToken(ctx context.Context, slug string) (*ghapi.RegistrationToken, error)
	OrgRegistrationToken(ctx context.Context, org string) (*ghapi.RegistrationToken, error)
}

// MintFor returns a registration token appropriate to the target's scope.
func MintFor(ctx context.Context, m TokenMinter, t Target) (*ghapi.RegistrationToken, error) {
	if t.Scope == "org" {
		return m.OrgRegistrationToken(ctx, t.Slug)
	}
	return m.RepoRegistrationToken(ctx, t.Slug)
}

// Engine A executes exactly one ephemeral job per invocation.
type Engine struct {
	Docker  *Docker
	Cache   *Cache
	Config  *config.Config
	HomeDir string
	Host    string
	Minter  TokenMinter
	DB      *store.DB
	// RunnerVersion is resolved once and reused.
	RunnerVersion string
	// Image is the built clean-room image tag.
	Image string

	// OnJobStart, if set, fires the moment the runner reports it accepted a
	// job. The supervisor uses this to stop its idle-rotation timer, so a
	// listener that is actually working is never rotated away mid-job.
	OnJobStart func(jobName string)

	mu sync.Mutex
}

// JobOutcome is what one ephemeral runner invocation produced.
type JobOutcome struct {
	// PickedUpJob is false when the runner exited without ever receiving work
	// (e.g. cancelled, or the queue was empty and the context expired).
	PickedUpJob bool
	JobName     string
	Result      string // Succeeded | Failed | Canceled
	ExitCode    int
	LogPath     string
	Started     time.Time
	Finished    time.Time
	RunnerName  string
	Class       labels.Class
	Err         error
}

// Duration is the billable wall-clock time.
func (o JobOutcome) Duration() time.Duration {
	if o.Started.IsZero() || o.Finished.IsZero() {
		return 0
	}
	return o.Finished.Sub(o.Started)
}

// State maps the runner's result string to a store state.
func (o JobOutcome) State() store.JobState {
	switch strings.ToLower(o.Result) {
	case "succeeded":
		return store.StateSucceeded
	case "canceled", "cancelled":
		return store.StateCancelled
	case "failed":
		return store.StateFailed
	}
	if o.ExitCode == 0 && o.PickedUpJob {
		return store.StateSucceeded
	}
	if !o.PickedUpJob {
		return store.StateInterrupted
	}
	return store.StateFailed
}

var (
	// The official runner emits these exact markers on stdout.
	reRunningJob = regexp.MustCompile(`Running job:\s*(.+)$`)
	reJobDone    = regexp.MustCompile(`Job (.+) completed with result:\s*(\w+)`)
	reConnected  = regexp.MustCompile(`Connected to GitHub`)
	reListening  = regexp.MustCompile(`Listening for Jobs`)
)

// Prepare resolves the runner version and builds the clean-room image once.
func (e *Engine) Prepare(ctx context.Context, log io.Writer) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.RunnerVersion == "" {
		v := e.Config.Engine.RunnerVersion
		if v == "" {
			resolved, err := LatestVersion(ctx, "")
			if err != nil {
				fmt.Fprintf(log, "homerun: could not reach GitHub for the latest runner version (%v); using %s\n",
					err, FallbackVersion)
				resolved = FallbackVersion
			}
			v = resolved
		}
		e.RunnerVersion = v
	}
	if e.Image == "" {
		b := &ImageBuilder{Docker: e.Docker, Cache: e.Cache, BaseImage: "ubuntu:22.04"}
		tag, err := b.Ensure(ctx, e.RunnerVersion, log)
		if err != nil {
			return err
		}
		e.Image = tag
	}
	return nil
}

// RunOne registers ONE ephemeral runner and waits for it to consume exactly one
// job inside a fresh container that is destroyed afterwards.
//
// The full lifecycle:
//
//	mint short-lived registration token  ->  write to a per-job tmpfs-ish dir
//	docker run (capped, non-root, no host net, throwaway workspace)
//	entrypoint: config.sh --ephemeral  ->  run.sh  ->  one job  ->  exit
//	container removed (--rm), workspace destroyed, token file shredded
func (e *Engine) RunOne(ctx context.Context, t Target, log io.Writer) (*JobOutcome, error) {
	out := &JobOutcome{Class: labels.ClassLinux}

	if err := e.Prepare(ctx, log); err != nil {
		return out, err
	}

	tok, err := MintFor(ctx, e.Minter, t)
	if err != nil {
		return out, fmt.Errorf("mint registration token for %s: %w", t.Slug, err)
	}
	if tok.Expired() {
		return out, fmt.Errorf("registration token for %s expired on arrival (check system clock)", t.Slug)
	}

	runnerName := RunnerName(t.Slug)
	out.RunnerName = runnerName

	// Per-job scratch: holds the token file and the mounted workspace.
	jobDir, err := WorkspaceDir(e.HomeDir, runnerName)
	if err != nil {
		return out, err
	}
	// Destroying the workspace is unconditional. A job must not be able to
	// leave anything behind for the next job to find.
	defer func() {
		_ = shred(filepath.Join(jobDir, "token"))
		_ = DestroyWorkspace(jobDir)
	}()

	tokenPath := filepath.Join(jobDir, "token")
	if err := os.WriteFile(tokenPath, []byte(tok.Token), 0o600); err != nil {
		return out, err
	}

	logPath := filepath.Join(e.HomeDir, "logs", runnerName+".log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return out, err
	}
	logFile, err := os.Create(logPath)
	if err != nil {
		return out, err
	}
	defer logFile.Close()
	out.LogPath = logPath

	labelList := t.Labels
	if len(labelList) == 0 {
		labelList = labels.Default()
	}

	spec := SpecFromLimits(e.Config.Limits, e.Config.Engine)
	spec.Image = e.Image
	spec.Name = runnerName
	spec.User = "" // the image already runs as uid 1001 "runner"
	spec.Mounts = []Mount{{Host: tokenPath, Container: "/run/homerun/token", ReadOnly: true}}
	spec.Env = map[string]string{
		"HOMERUN_URL":    t.URL(e.Host),
		"HOMERUN_NAME":   runnerName,
		"HOMERUN_LABELS": strings.Join(labelList, ","),
		"HOMERUN_GROUP":  t.RunnerGroup,
	}
	spec.Labels["homerun.target"] = t.Slug
	spec.Labels["homerun.profile"] = t.Profile

	// A job must not outlive the configured timeout.
	runCtx := ctx
	if e.Config.Limits.JobTimeout.Duration > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, e.Config.Limits.JobTimeout.Duration)
		defer cancel()
	}

	// Tee output: durable log file + live parsing for job markers.
	pr, pw := io.Pipe()
	parseDone := make(chan struct{})
	go func() {
		defer close(parseDone)
		e.parse(pr, out)
	}()

	var sinks []io.Writer = []io.Writer{logFile, pw}
	if log != nil {
		sinks = append(sinks, log)
	}
	w := io.MultiWriter(sinks...)

	out.Started = time.Now().UTC()
	code, runErr := e.Docker.Run(runCtx, spec, w)
	out.Finished = time.Now().UTC()
	pw.Close()
	<-parseDone

	out.ExitCode = code

	// Best-effort container removal in case --rm did not fire (SIGKILL path).
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = e.Docker.Kill(cleanupCtx, runnerName)

	if runErr != nil {
		out.Err = runErr
		return out, runErr
	}
	if runCtx.Err() == context.DeadlineExceeded {
		out.Result = "Failed"
		out.Err = fmt.Errorf("job exceeded job_timeout of %s and was killed", e.Config.Limits.JobTimeout)
		return out, out.Err
	}
	return out, nil
}

// parse consumes runner stdout looking for the job lifecycle markers.
func (e *Engine) parse(r io.Reader, out *JobOutcome) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case reRunningJob.MatchString(line):
			m := reRunningJob.FindStringSubmatch(line)
			out.PickedUpJob = true
			out.JobName = strings.TrimSpace(m[1])
			if e.OnJobStart != nil {
				e.OnJobStart(out.JobName)
			}
		case reJobDone.MatchString(line):
			m := reJobDone.FindStringSubmatch(line)
			out.PickedUpJob = true
			if out.JobName == "" {
				out.JobName = strings.TrimSpace(m[1])
			}
			out.Result = strings.TrimSpace(m[2])
		}
	}
	_ = sc.Err()
}

// RunnerName builds a unique, GitHub-legal runner name.
//
// Uniqueness is load-bearing: ephemeral runners are registered and torn down
// constantly, and two registrations sharing a name would collide on GitHub
// (--replace would silently evict the other machine's live runner). A
// timestamp alone is NOT sufficient - concurrent slots can register inside the
// same millisecond - so a random suffix is appended.
//
// GitHub caps runner names at 64 characters, so the repo slug is truncated
// from the left (keeping the repo name, which is the informative part) and the
// unique suffix is always preserved.
func RunnerName(slug string) string {
	host, _ := os.Hostname()
	host = sanitize(strings.Split(host, ".")[0])
	if host == "" {
		host = "homerun"
	}
	if len(host) > 16 {
		host = host[:16]
	}
	short := sanitize(slug)
	if len(short) > 20 {
		short = short[len(short)-20:]
	}

	var rnd [4]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		// crypto/rand failing is essentially impossible; fall back to the
		// nanosecond clock rather than returning a non-unique name.
		binary.LittleEndian.PutUint32(rnd[:], uint32(time.Now().UnixNano()))
	}

	name := fmt.Sprintf("homerun-%s-%s-%d-%s",
		host, short, time.Now().Unix(), hex.EncodeToString(rnd[:]))
	if len(name) > 64 {
		// Truncate the middle, never the unique tail.
		tail := name[len(name)-18:]
		name = name[:46] + tail
	}
	return name
}

// shred overwrites a small secret file before unlinking it.
func shred(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	zeros := make([]byte, info.Size())
	_, _ = f.WriteAt(zeros, 0)
	_ = f.Sync()
	f.Close()
	return os.Remove(path)
}
