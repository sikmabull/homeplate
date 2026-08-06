package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/homeplate-ci/homeplate/internal/config"
)

// Docker is a thin, explicit wrapper over the docker CLI. Shelling out (rather
// than using the Docker SDK) keeps the binary small, avoids API-version
// pinning problems across Docker Desktop/Colima/Podman, and means every action
// Homeplate takes is a command a user can copy-paste to reproduce.
type Docker struct {
	Bin string
}

// NewDocker locates a container runtime. Podman is accepted because its CLI is
// docker-compatible for everything Homeplate uses.
func NewDocker() (*Docker, error) {
	for _, bin := range []string{"docker", "podman"} {
		if p, err := exec.LookPath(bin); err == nil {
			return &Docker{Bin: p}, nil
		}
	}
	return nil, fmt.Errorf("no container runtime found: install Docker Desktop, Colima, or Podman")
}

// Available reports whether the daemon is actually reachable, not merely that
// the CLI exists. `docker info` failing is the single most common Homeplate
// support issue, so doctor checks this explicitly.
func (d *Docker) Available(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, d.Bin, "info", "--format", "{{.ServerVersion}}")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("container runtime not responding: %s", msg)
	}
	if strings.TrimSpace(string(out)) == "" {
		return fmt.Errorf("container runtime returned no server version")
	}
	return nil
}

// ServerVersion returns the daemon version for `homeplate doctor`.
func (d *Docker) ServerVersion(ctx context.Context) string {
	out, err := exec.CommandContext(ctx, d.Bin, "info", "--format", "{{.ServerVersion}}").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

// ImageExists reports whether an image is present locally.
func (d *Docker) ImageExists(ctx context.Context, image string) bool {
	err := exec.CommandContext(ctx, d.Bin, "image", "inspect", image).Run()
	return err == nil
}

// Pull fetches an image.
func (d *Docker) Pull(ctx context.Context, image string, out io.Writer) error {
	cmd := exec.CommandContext(ctx, d.Bin, "pull", image)
	cmd.Stdout = out
	cmd.Stderr = out
	return cmd.Run()
}

// RunSpec is a fully specified, resource-capped, throwaway container run.
type RunSpec struct {
	Image      string
	Name       string
	Env        map[string]string
	Cmd        []string
	Entrypoint []string
	WorkDir    string

	// Mounts are host:container bind mounts.
	Mounts []Mount

	// Limits map directly onto docker flags.
	CPUs      float64
	Memory    string
	PidsLimit int
	// StorageOptSize applies --storage-opt size=..G. Only supported on some
	// storage drivers (overlay2+xfs project quotas, devicemapper); when the
	// driver rejects it Homeplate degrades rather than failing the job.
	StorageOptSize int

	// Security posture.
	HostNetwork     bool
	User            string
	NoNewPrivileges bool
	DropAllCaps     bool
	ReadOnlyRootfs  bool
	MountDockerSock bool

	// Labels tag the container so orphan cleanup can find it.
	Labels map[string]string
}

// Mount is a bind mount.
type Mount struct {
	Host      string
	Container string
	ReadOnly  bool
}

// Args renders the docker run argument vector. Exposed so `homeplate doctor -v`
// can print the exact command, and so tests can assert on resource caps
// without needing a Docker daemon.
func (d *Docker) Args(s RunSpec) []string {
	args := []string{"run", "--rm"}
	if s.Name != "" {
		args = append(args, "--name", s.Name)
	}

	// --- resource caps (the "critical requirement") ---
	if s.CPUs > 0 {
		args = append(args, "--cpus", strconv.FormatFloat(s.CPUs, 'f', -1, 64))
	}
	if s.Memory != "" {
		args = append(args, "--memory", s.Memory)
		// Without a swap cap equal to the memory cap, the container can exceed
		// max_memory by swapping, which would make the limit a lie.
		args = append(args, "--memory-swap", s.Memory)
	}
	if s.PidsLimit > 0 {
		args = append(args, "--pids-limit", strconv.Itoa(s.PidsLimit))
	}
	if s.StorageOptSize > 0 {
		args = append(args, "--storage-opt", fmt.Sprintf("size=%dG", s.StorageOptSize))
	}

	// --- security posture ---
	if !s.HostNetwork {
		// Default bridge network: the job gets outbound access (it must, to
		// clone and fetch actions) but cannot reach host-bound services.
		args = append(args, "--network", "bridge")
	} else {
		args = append(args, "--network", "host")
	}
	if s.User != "" {
		args = append(args, "--user", s.User)
	}
	if s.NoNewPrivileges {
		args = append(args, "--security-opt", "no-new-privileges")
	}
	if s.DropAllCaps {
		args = append(args, "--cap-drop", "ALL")
	}
	if s.ReadOnlyRootfs {
		args = append(args, "--read-only")
	}
	if s.MountDockerSock {
		// Root-equivalent on the host. Off by default; see README security.
		args = append(args, "-v", "/var/run/docker.sock:/var/run/docker.sock")
	}

	for _, m := range s.Mounts {
		spec := m.Host + ":" + m.Container
		if m.ReadOnly {
			spec += ":ro"
		}
		args = append(args, "-v", spec)
	}
	for _, k := range sortedKeys(s.Env) {
		args = append(args, "-e", k+"="+s.Env[k])
	}
	for _, k := range sortedKeys(s.Labels) {
		args = append(args, "--label", k+"="+s.Labels[k])
	}
	if s.WorkDir != "" {
		args = append(args, "-w", s.WorkDir)
	}
	if len(s.Entrypoint) > 0 {
		args = append(args, "--entrypoint", s.Entrypoint[0])
	}
	args = append(args, s.Image)
	if len(s.Entrypoint) > 1 {
		args = append(args, s.Entrypoint[1:]...)
	}
	args = append(args, s.Cmd...)
	return args
}

// sortedKeys yields map keys in deterministic order so Args() is testable.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Run executes the container, streaming combined output to w.
func (d *Docker) Run(ctx context.Context, s RunSpec, w io.Writer) (exitCode int, err error) {
	args := d.Args(s)
	cmd := exec.CommandContext(ctx, d.Bin, args...)
	cmd.Stdout = w
	cmd.Stderr = w
	// Give the container a chance to shut down cleanly on cancellation before
	// the process is killed, so the runner can deregister itself.
	cmd.WaitDelay = 20 * time.Second

	if err := cmd.Start(); err != nil {
		return -1, err
	}
	err = cmd.Wait()
	if err == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	return -1, err
}

// Kill force-removes a container by name (used on timeout/shutdown).
func (d *Docker) Kill(ctx context.Context, name string) error {
	return exec.CommandContext(ctx, d.Bin, "rm", "-f", name).Run()
}

// CleanupOrphans removes containers Homeplate labelled but never reaped, which
// happens if the machine lost power mid-job.
func (d *Docker) CleanupOrphans(ctx context.Context) (int, error) {
	out, err := exec.CommandContext(ctx, d.Bin, "ps", "-aq", "--filter", "label=homeplate.managed=true").Output()
	if err != nil {
		return 0, err
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		return 0, nil
	}
	args := append([]string{"rm", "-f"}, ids...)
	if err := exec.CommandContext(ctx, d.Bin, args...).Run(); err != nil {
		return 0, err
	}
	return len(ids), nil
}

// SpecFromLimits translates Homeplate's config into container flags. This is the
// single place where "max_cpus = 4" becomes "--cpus 4", so the enforcement
// path is testable end to end.
func SpecFromLimits(lim config.Limits, eng config.Engine) RunSpec {
	mem := lim.MaxMemory
	if _, err := config.ParseMemory(mem); err != nil {
		mem = ""
	}
	return RunSpec{
		Image:  eng.DefaultImage,
		CPUs:   lim.MaxCPUs,
		Memory: mem,
		// A fork bomb in a job should not take down the developer's laptop.
		PidsLimit:       4096,
		StorageOptSize:  lim.MaxDiskGB,
		HostNetwork:     eng.HostNetwork,
		User:            eng.ContainerUser,
		NoNewPrivileges: true,
		DropAllCaps:     false, // the runner needs chown/setuid for tool installs
		Labels:          map[string]string{"homeplate.managed": "true"},
	}
}

// WorkspaceDir creates a per-job workspace under ~/.homeplate/work/<job>.
func WorkspaceDir(homeDir, jobKey string) (string, error) {
	dir := filepath.Join(homeDir, "work", sanitize(jobKey))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// DestroyWorkspace removes a job workspace. Called unconditionally after every
// job so nothing survives into the next one.
func DestroyWorkspace(dir string) error {
	if dir == "" || dir == "/" {
		return fmt.Errorf("refusing to remove %q", dir)
	}
	return os.RemoveAll(dir)
}

func sanitize(s string) string {
	r := strings.NewReplacer("/", "-", ":", "-", " ", "-", "..", "-", string(filepath.Separator), "-")
	out := r.Replace(s)
	if len(out) > 100 {
		out = out[:100]
	}
	return out
}
