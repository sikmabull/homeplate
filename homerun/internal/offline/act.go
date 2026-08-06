package offline

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/homerun-ci/homerun/internal/config"
	"github.com/homerun-ci/homerun/internal/labels"
	"github.com/homerun-ci/homerun/internal/store"
)

// Act wraps the nektos/act binary.
//
// STATUS: BETA. act is an excellent reimplementation of the Actions runtime but
// it is NOT byte-identical to GitHub's runner. Known divergences that Homerun
// surfaces to the user rather than hiding:
//
//   - macOS and Windows runners are not supported by act at all (Linux only).
//   - `services:` containers, some `container:` options, and a few contexts
//     behave differently.
//   - Artifacts require act's artifact server flags and are not uploaded to
//     GitHub by Homerun (see README "Known limits").
//   - Secrets are NOT available offline unless the user supplies them locally.
//
// Because of this, every offline result posted back to GitHub is explicitly
// labelled as having run locally via act.
type Act struct {
	Bin     string
	Version string
}

// MinimumVersion is the oldest act Homerun is tested against.
const MinimumVersion = "0.2.60"

// Find locates the act binary.
func Find() (*Act, error) {
	p, err := exec.LookPath("act")
	if err != nil {
		return nil, fmt.Errorf("nektos/act not found. Install it with `brew install act` " +
			"(macOS) or see https://github.com/nektos/act#installation. " +
			"Offline mode (Engine B) needs it; connected mode (Engine A) does not")
	}
	a := &Act{Bin: p}
	if out, err := exec.Command(p, "--version").Output(); err == nil {
		a.Version = strings.TrimSpace(strings.TrimPrefix(string(out), "act version "))
	}
	return a, nil
}

// Event is the synthetic webhook payload act replays.
//
// Homerun constructs this from local git state so that `github.sha`,
// `github.ref`, and `github.event_name` inside the workflow match what GitHub
// itself would have provided.
type Event struct {
	Name       string
	Ref        string
	SHA        string
	Repository string
	Actor      string
	Branch     string
}

// Payload builds the JSON event body act consumes with --eventpath.
func (e Event) Payload() map[string]any {
	owner, name := "", e.Repository
	if i := strings.Index(e.Repository, "/"); i > 0 {
		owner, name = e.Repository[:i], e.Repository[i+1:]
	}
	repo := map[string]any{
		"name":           name,
		"full_name":      e.Repository,
		"default_branch": e.Branch,
		"owner":          map[string]any{"login": owner, "name": owner},
	}
	switch e.Name {
	case "push":
		return map[string]any{
			"ref":        e.Ref,
			"after":      e.SHA,
			"repository": repo,
			"pusher":     map[string]any{"name": e.Actor},
			"head_commit": map[string]any{
				"id":      e.SHA,
				"message": "homerun offline run",
			},
		}
	case "pull_request":
		return map[string]any{
			"action": "synchronize",
			"number": 0,
			"pull_request": map[string]any{
				"head": map[string]any{"sha": e.SHA, "ref": e.Branch},
				"base": map[string]any{"ref": e.Branch},
			},
			"repository": repo,
		}
	default:
		return map[string]any{"repository": repo}
	}
}

// RunOpts configures one act invocation.
type RunOpts struct {
	WorkDir     string
	Event       Event
	Workflow    string // path relative to WorkDir, empty = all
	Job         string // job id, empty = all jobs for the event
	Image       string // maps ubuntu-latest -> image
	Limits      config.Limits
	SecretsFile string
	EnvFile     string
	// ArtifactDir enables act's local artifact server.
	ArtifactDir string
}

// Args renders the act command line. Exposed for testing so resource-cap
// plumbing can be asserted without running Docker.
func (a *Act) Args(o RunOpts, eventPath string) []string {
	args := []string{
		o.Event.Name,
		"--eventpath", eventPath,
		// Never let act reuse a container between runs; clean state per job.
		"--rm",
		// Do not prompt: the daemon has no tty.
		"--no-cache-server",
	}
	if o.Workflow != "" {
		args = append(args, "-W", o.Workflow)
	}
	if o.Job != "" {
		args = append(args, "-j", o.Job)
	}

	img := o.Image
	if img == "" {
		img = "catthehacker/ubuntu:act-latest"
	}
	// Map every hosted Linux label AND Homerun's own labels onto the image, so
	// a repo that has already run `homerun adopt` still works offline.
	for _, l := range []string{
		"ubuntu-latest", "ubuntu-24.04", "ubuntu-22.04", "ubuntu-20.04",
		labels.Homerun, "homerun-linux", "self-hosted",
	} {
		args = append(args, "-P", l+"="+img)
	}

	// Resource caps: act forwards container options verbatim to Docker, which
	// is how Homerun's max_cpus/max_memory apply to Engine B as well.
	var copts []string
	if o.Limits.MaxCPUs > 0 {
		copts = append(copts, "--cpus="+strconv.FormatFloat(o.Limits.MaxCPUs, 'f', -1, 64))
	}
	if o.Limits.MaxMemory != "" {
		copts = append(copts, "--memory="+o.Limits.MaxMemory, "--memory-swap="+o.Limits.MaxMemory)
	}
	copts = append(copts, "--pids-limit=4096")
	if len(copts) > 0 {
		args = append(args, "--container-options", strings.Join(copts, " "))
	}

	if o.SecretsFile != "" {
		args = append(args, "--secret-file", o.SecretsFile)
	}
	if o.EnvFile != "" {
		args = append(args, "--env-file", o.EnvFile)
	}
	if o.ArtifactDir != "" {
		args = append(args, "--artifact-server-path", o.ArtifactDir)
	}
	return args
}

// Result is the outcome of an act run.
type Result struct {
	ExitCode  int
	Started   time.Time
	Finished  time.Time
	Steps     []store.Step
	JobNames  []string
	LogPath   string
	Succeeded bool
	Err       error
}

var (
	// act prefixes each line with [Workflow/Job] and marks steps with emoji
	// or ASCII depending on TTY. Homerun parses the stable ASCII markers.
	reActStep    = regexp.MustCompile(`^\[([^\]]+)\]\s+(?:\x{2b50}|\*)?\s*Run\s+(.+)$`)
	reActSuccess = regexp.MustCompile(`^\[([^\]]+)\]\s+(?:\x{2705}|\x{1f3c1})?\s*(?:Success|Job succeeded)\s*-?\s*(.*)$`)
	reActFailure = regexp.MustCompile(`^\[([^\]]+)\]\s+(?:\x{274c})?\s*(?:Failure|Job failed)\s*-?\s*(.*)$`)
	reActJob     = regexp.MustCompile(`^\[([^/\]]+)/([^\]]+)\]`)
)

// Run executes act and captures a structured result.
func (a *Act) Run(ctx context.Context, o RunOpts, logPath string, live io.Writer) (*Result, error) {
	res := &Result{LogPath: logPath}

	payload := o.Event.Payload()
	eventFile, err := os.CreateTemp("", "homerun-event-*.json")
	if err != nil {
		return res, err
	}
	defer os.Remove(eventFile.Name())
	if err := json.NewEncoder(eventFile).Encode(payload); err != nil {
		eventFile.Close()
		return res, err
	}
	eventFile.Close()

	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return res, err
	}
	logFile, err := os.Create(logPath)
	if err != nil {
		return res, err
	}
	defer logFile.Close()

	runCtx := ctx
	if o.Limits.JobTimeout.Duration > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, o.Limits.JobTimeout.Duration)
		defer cancel()
	}

	args := a.Args(o, eventFile.Name())
	cmd := exec.CommandContext(runCtx, a.Bin, args...)
	cmd.Dir = o.WorkDir
	cmd.WaitDelay = 20 * time.Second

	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		res.Steps, res.JobNames = parseAct(pr)
	}()

	sinks := []io.Writer{logFile, pw}
	if live != nil {
		sinks = append(sinks, live)
	}
	mw := io.MultiWriter(sinks...)
	cmd.Stdout = mw
	cmd.Stderr = mw

	res.Started = time.Now().UTC()
	runErr := cmd.Run()
	res.Finished = time.Now().UTC()
	pw.Close()
	<-done

	if ee, ok := runErr.(*exec.ExitError); ok {
		res.ExitCode = ee.ExitCode()
	} else if runErr != nil {
		res.Err = runErr
		res.ExitCode = -1
		return res, runErr
	}
	if runCtx.Err() == context.DeadlineExceeded {
		res.Err = fmt.Errorf("act run exceeded job_timeout %s", o.Limits.JobTimeout)
		return res, res.Err
	}
	res.Succeeded = res.ExitCode == 0
	return res, nil
}

// ansiRe matches SGR/ANSI escape sequences. act colourises its output when it
// detects a pipe as well as a TTY, so every line arrives prefixed with escape
// codes. Parsing must strip them first or every `^\[Workflow/Job\]` anchor
// fails silently - which previously cost us both job names and step records.
var ansiRe = regexp.MustCompile(`\[[0-9;]*[a-zA-Z]`)

func stripANSI(s string) string { return ansiRe.ReplaceAllString(s, "") }

// parseAct extracts per-step records from act's output.
func parseAct(r io.Reader) ([]store.Step, []string) {
	var steps []store.Step
	seenJobs := map[string]bool{}
	var jobs []string

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	num := 0
	var current *store.Step
	var buf strings.Builder

	flush := func(exitCode int) {
		if current == nil {
			return
		}
		current.ExitCode = exitCode
		current.Finished = time.Now().UTC()
		current.Output = truncate(buf.String(), 60000)
		steps = append(steps, *current)
		current = nil
		buf.Reset()
	}

	for sc.Scan() {
		line := strings.TrimSpace(stripANSI(sc.Text()))
		if m := reActJob.FindStringSubmatch(line); m != nil {
			job := strings.TrimSpace(m[2])
			if job != "" && !seenJobs[job] {
				seenJobs[job] = true
				jobs = append(jobs, job)
			}
		}
		switch {
		case reActStep.MatchString(line):
			flush(0)
			m := reActStep.FindStringSubmatch(line)
			num++
			current = &store.Step{Number: num, Name: strings.TrimSpace(m[2]), Started: time.Now().UTC()}
		case reActFailure.MatchString(line):
			buf.WriteString(line + "\n")
			flush(1)
		case reActSuccess.MatchString(line):
			buf.WriteString(line + "\n")
			flush(0)
		default:
			if current != nil {
				buf.WriteString(line + "\n")
			}
		}
	}
	flush(0)
	return steps, jobs
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n... (truncated by homerun; full log on disk)"
}
