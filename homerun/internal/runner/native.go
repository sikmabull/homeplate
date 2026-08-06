package runner

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/homerun-ci/homerun/internal/labels"
)

// NativeEngine runs macOS jobs directly on the host.
//
// HONEST LIMITATION (documented in README "Known limits"):
// There is no macOS container runtime. Apple's Virtualization.framework can
// boot a full macOS VM, but only on Apple Silicon, only 2 guests per host, and
// it needs a prepared IPSW image - too heavy for an MVP. So macOS jobs get:
//
//	isolation:   a fresh per-job working directory, a scrubbed environment,
//	             and a dedicated ephemeral runner registration. NOT a sandbox.
//	CPU limit:   `nice` + `taskpolicy -b` (background QoS). This is a SCHEDULER
//	             PRIORITY hint, not a hard cap. A macOS job CAN use all cores
//	             if the machine is otherwise idle.
//	memory:      NOT enforceable without a VM. Reported as "not enforced".
//
// Linux jobs on the same machine still get hard Docker caps. Only native
// macOS jobs have the softer guarantee, and `homerun status` says so.
type NativeEngine struct {
	Cache   *Cache
	HomeDir string
	Host    string
	Minter  TokenMinter
	// RunnerVersion pins the agent.
	RunnerVersion string
	// NiceLevel is the scheduling penalty (0-20; higher = lower priority).
	NiceLevel int
	// UseTaskPolicy applies macOS background QoS via taskpolicy(8).
	UseTaskPolicy bool
}

// Enforcement describes what resource limits actually apply, for status output.
type Enforcement struct {
	CPUHard    bool
	MemoryHard bool
	DiskHard   bool
	Mechanism  string
	Caveat     string
}

// NativeEnforcement is the honest description of native macOS limits.
func NativeEnforcement() Enforcement {
	return Enforcement{
		CPUHard:    false,
		MemoryHard: false,
		DiskHard:   false,
		Mechanism:  "nice(1) + taskpolicy(8) background QoS",
		Caveat: "macOS has no cgroups. CPU caps are scheduling hints, not hard limits, " +
			"and memory is not capped at all. Linux jobs in Docker get hard --cpus/--memory caps.",
	}
}

// DockerEnforcement is the description for containerised jobs.
func DockerEnforcement(cpus float64, mem string, diskGB int) Enforcement {
	return Enforcement{
		CPUHard:    cpus > 0,
		MemoryHard: mem != "",
		DiskHard:   diskGB > 0,
		Mechanism:  fmt.Sprintf("docker --cpus %g --memory %s --memory-swap %s", cpus, mem, mem),
		Caveat:     "--storage-opt disk caps require an overlay2+xfs or devicemapper driver; ignored otherwise",
	}
}

// RunOne registers an ephemeral runner and runs one macOS job natively.
func (n *NativeEngine) RunOne(ctx context.Context, t Target, log io.Writer) (*JobOutcome, error) {
	out := &JobOutcome{Class: labels.ClassMacOS}
	if runtime.GOOS != "darwin" {
		return out, fmt.Errorf("native execution is macOS-only; this host is %s", runtime.GOOS)
	}

	plat, err := Platform()
	if err != nil {
		return out, err
	}
	version := n.RunnerVersion
	if version == "" {
		version = FallbackVersion
	}
	// Apply the macOS arm64 safety pin before downloading, so a user never
	// lands on the release that deadlocks every job.
	if safe, pinned, why := SafeVersionFor(version, plat); pinned {
		fmt.Fprintf(log, "homerun: %s\n", why)
		version = safe
	}
	rel := ResolveRelease(version, plat)

	// The pristine extracted runner is the template; each job gets a COPY so
	// that config.sh writing .runner/.credentials never contaminates the next.
	template, err := n.Cache.Ensure(ctx, rel, func(m string) { fmt.Fprintln(log, "homerun:", m) })
	if err != nil {
		return out, err
	}

	runnerName := RunnerName(t.Slug)
	out.RunnerName = runnerName

	jobDir, err := WorkspaceDir(n.HomeDir, runnerName)
	if err != nil {
		return out, err
	}
	defer DestroyWorkspace(jobDir)

	instance := filepath.Join(jobDir, "actions-runner")
	if err := copyTree(template, instance); err != nil {
		return out, fmt.Errorf("clone runner template: %w", err)
	}

	tok, err := MintFor(ctx, n.Minter, t)
	if err != nil {
		return out, fmt.Errorf("mint registration token for %s: %w", t.Slug, err)
	}

	labelList := t.Labels
	if len(labelList) == 0 {
		labelList = labels.Default()
	}

	logPath := filepath.Join(n.HomeDir, "logs", runnerName+".log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return out, err
	}
	logFile, err := os.Create(logPath)
	if err != nil {
		return out, err
	}
	defer logFile.Close()
	out.LogPath = logPath

	// --- configure (ephemeral) ---
	cfgArgs := []string{
		"--unattended", "--ephemeral", "--disableupdate", "--replace",
		"--url", t.URL(n.Host),
		"--token", tok.Token,
		"--name", runnerName,
		"--labels", strings.Join(labelList, ","),
		"--work", filepath.Join(jobDir, "_work"),
	}
	if t.RunnerGroup != "" {
		cfgArgs = append(cfgArgs, "--runnergroup", t.RunnerGroup)
	}

	cfg := exec.CommandContext(ctx, filepath.Join(instance, "config.sh"), cfgArgs...)
	cfg.Dir = instance
	cfg.Env = scrubbedEnv()
	// Mask the registration token out of anything written to disk.
	masking := newMaskWriter(io.MultiWriter(logFile, log), tok.Token)
	cfg.Stdout = masking
	cfg.Stderr = masking
	if err := cfg.Run(); err != nil {
		return out, fmt.Errorf("config.sh failed: %w (see %s)", err, logPath)
	}

	// --- run exactly one job under a background scheduling policy ---
	runPath := filepath.Join(instance, "run.sh")
	var cmd *exec.Cmd
	switch {
	case n.UseTaskPolicy && hasTaskPolicy():
		// -b puts the process (and children) in the background QoS tier, so
		// interactive work on the laptop stays responsive during a build.
		cmd = exec.CommandContext(ctx, "taskpolicy", "-b", runPath)
	case n.NiceLevel > 0:
		cmd = exec.CommandContext(ctx, "nice", "-n", strconv.Itoa(n.NiceLevel), runPath)
	default:
		cmd = exec.CommandContext(ctx, runPath)
	}
	cmd.Dir = instance
	cmd.Env = scrubbedEnv()
	cmd.WaitDelay = 20 * time.Second

	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() { defer close(done); parseNative(pr, out) }()

	mw := io.MultiWriter(logFile, pw, log)
	cmd.Stdout = mw
	cmd.Stderr = mw

	out.Started = time.Now().UTC()
	runErr := cmd.Run()
	out.Finished = time.Now().UTC()
	pw.Close()
	<-done

	if ee, ok := runErr.(*exec.ExitError); ok {
		out.ExitCode = ee.ExitCode()
	} else if runErr != nil {
		out.Err = runErr
	}

	// Deregister defensively: an ephemeral runner normally removes itself, but
	// if the process was killed the registration would linger in GitHub's UI.
	rmCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	n.deregister(rmCtx, instance, t)

	return out, out.Err
}

func (n *NativeEngine) deregister(ctx context.Context, instance string, t Target) {
	if _, err := os.Stat(filepath.Join(instance, ".runner")); err != nil {
		return // never configured; nothing to remove
	}
	// `config.sh remove --local` tears down the local registration without
	// needing a remove token. GitHub reaps the ephemeral registration on its
	// side once the runner disconnects.
	cmd := exec.CommandContext(ctx, filepath.Join(instance, "config.sh"), "remove", "--local")
	cmd.Dir = instance
	_ = cmd.Run()
}

func parseNative(r io.Reader, out *JobOutcome) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if m := reRunningJob.FindStringSubmatch(line); m != nil {
			out.PickedUpJob = true
			out.JobName = strings.TrimSpace(m[1])
		}
		if m := reJobDone.FindStringSubmatch(line); m != nil {
			out.PickedUpJob = true
			if out.JobName == "" {
				out.JobName = strings.TrimSpace(m[1])
			}
			out.Result = strings.TrimSpace(m[2])
		}
	}
}

func hasTaskPolicy() bool {
	_, err := exec.LookPath("taskpolicy")
	return err == nil
}

// scrubbedEnv builds a minimal environment for a native job. The developer's
// own shell environment routinely contains AWS_*, NPM_TOKEN, OPENAI_API_KEY and
// similar; inheriting it would hand every workflow the user's whole secret
// store. Only an explicit allowlist survives.
func scrubbedEnv() []string {
	allow := map[string]bool{
		"HOME": true, "USER": true, "LOGNAME": true, "SHELL": true,
		"LANG": true, "LC_ALL": true, "TZ": true, "TMPDIR": true,
		"PATH": true, "SSL_CERT_FILE": true, "TERM": true,
	}
	var out []string
	for _, kv := range os.Environ() {
		k, _, ok := strings.Cut(kv, "=")
		if ok && allow[k] {
			out = append(out, kv)
		}
	}
	out = append(out, "HOMERUN=1", "CI=true", "RUNNER_MANUALLY_TRAP_SIG=1")
	return out
}

// copyTree copies a directory preserving permissions and symlinks.
func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case info.IsDir():
			return os.MkdirAll(target, info.Mode()|0o700)
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			os.Remove(target)
			return os.Symlink(link, target)
		default:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			return copyFileMode(path, target, info.Mode())
		}
	})
}

func copyFileMode(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
