package runner

import (
	"strings"
	"testing"

	"github.com/homeplate-ci/homeplate/internal/config"
)

// argValue extracts the value following a flag in a docker argv.
func argValue(args []string, flag string) (string, bool) {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

func hasArg(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

// TestResourceCapsReachDocker is the resource-enforcement contract: whatever
// the user puts in config.toml must appear as real docker flags. If this test
// passes but the flag name is wrong, the caps are decorative.
func TestResourceCapsReachDocker(t *testing.T) {
	lim := config.Limits{
		MaxCPUs:           2.5,
		MaxMemory:         "6g",
		MaxDiskGB:         40,
		MaxConcurrentJobs: 1,
	}
	eng := config.Engine{DefaultImage: "ubuntu:22.04"}

	d := &Docker{Bin: "docker"}
	args := d.Args(SpecFromLimits(lim, eng))

	if v, ok := argValue(args, "--cpus"); !ok || v != "2.5" {
		t.Errorf("--cpus = %q (present=%v), want 2.5; CPU cap is not enforced", v, ok)
	}
	if v, ok := argValue(args, "--memory"); !ok || v != "6g" {
		t.Errorf("--memory = %q (present=%v), want 6g; memory cap is not enforced", v, ok)
	}
	// Without a swap cap, a container can exceed max_memory by swapping, which
	// would make the advertised limit untrue.
	if v, ok := argValue(args, "--memory-swap"); !ok || v != "6g" {
		t.Errorf("--memory-swap = %q (present=%v), want 6g; memory cap can be bypassed via swap", v, ok)
	}
	if v, ok := argValue(args, "--storage-opt"); !ok || v != "size=40G" {
		t.Errorf("--storage-opt = %q (present=%v), want size=40G", v, ok)
	}
	if v, ok := argValue(args, "--pids-limit"); !ok || v == "" {
		t.Errorf("--pids-limit missing; a fork bomb in a job could take down the host")
	}
}

// TestSecurityDefaults locks in the non-negotiable container posture.
func TestSecurityDefaults(t *testing.T) {
	d := &Docker{Bin: "docker"}
	spec := SpecFromLimits(config.Limits{MaxCPUs: 1, MaxMemory: "1g"}, config.Engine{DefaultImage: "x"})
	args := d.Args(spec)

	if v, _ := argValue(args, "--network"); v != "bridge" {
		t.Errorf("--network = %q, want bridge; jobs must not get host networking by default", v)
	}
	if !hasArg(args, "--rm") {
		t.Error("--rm missing; job containers must be destroyed after the run")
	}
	if v, _ := argValue(args, "--security-opt"); v != "no-new-privileges" {
		t.Errorf("--security-opt = %q, want no-new-privileges", v)
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "/var/run/docker.sock") {
		t.Error("SECURITY: the docker socket must NOT be mounted by default (it is root-equivalent on the host)")
	}
	if v, _ := argValue(args, "--label"); v != "homeplate.managed=true" {
		t.Errorf("--label = %q, want homeplate.managed=true so orphans can be reaped", v)
	}
}

// TestHostNetworkOptIn verifies host networking only happens on explicit request.
func TestHostNetworkOptIn(t *testing.T) {
	d := &Docker{Bin: "docker"}
	spec := SpecFromLimits(config.Limits{MaxCPUs: 1, MaxMemory: "1g"},
		config.Engine{DefaultImage: "x", HostNetwork: true})
	if v, _ := argValue(d.Args(spec), "--network"); v != "host" {
		t.Errorf("--network = %q, want host when explicitly enabled", v)
	}
}

// TestDockerSockOptIn verifies the socket mount is reachable but never default.
func TestDockerSockOptIn(t *testing.T) {
	d := &Docker{Bin: "docker"}
	spec := SpecFromLimits(config.Limits{MaxCPUs: 1, MaxMemory: "1g"}, config.Engine{DefaultImage: "x"})
	spec.MountDockerSock = true
	if !strings.Contains(strings.Join(d.Args(spec), " "), "/var/run/docker.sock:/var/run/docker.sock") {
		t.Error("explicit MountDockerSock did not produce the bind mount")
	}
}

// TestInvalidMemoryIsDroppedNotPassed ensures a malformed config value cannot
// produce a broken docker command line.
func TestInvalidMemoryIsDroppedNotPassed(t *testing.T) {
	d := &Docker{Bin: "docker"}
	spec := SpecFromLimits(config.Limits{MaxCPUs: 1, MaxMemory: "not-a-size"}, config.Engine{DefaultImage: "x"})
	if _, ok := argValue(d.Args(spec), "--memory"); ok {
		t.Error("an unparseable max_memory must be dropped, not passed through to docker")
	}
}

// TestArgsAreDeterministic guards against map-iteration nondeterminism, which
// would make the command line (and this test suite) flaky.
func TestArgsAreDeterministic(t *testing.T) {
	d := &Docker{Bin: "docker"}
	spec := SpecFromLimits(config.Limits{MaxCPUs: 1, MaxMemory: "1g"}, config.Engine{DefaultImage: "x"})
	spec.Env = map[string]string{"B": "2", "A": "1", "C": "3"}
	spec.Labels["extra"] = "y"

	first := strings.Join(d.Args(spec), " ")
	for i := 0; i < 20; i++ {
		if got := strings.Join(d.Args(spec), " "); got != first {
			t.Fatalf("docker args are nondeterministic:\n%s\n%s", first, got)
		}
	}
}

// TestMemoryParsing covers the human-friendly size strings users will type.
func TestMemoryParsing(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		bad  bool
	}{
		{"8g", 8 << 30, false},
		{"8G", 8 << 30, false},
		{"512m", 512 << 20, false},
		{"1.5g", int64(1.5 * float64(1<<30)), false},
		{"2048", 2048, false},
		{"", 0, true},
		{"abc", 0, true},
		{"-4g", 0, true},
	}
	for _, c := range cases {
		got, err := config.ParseMemory(c.in)
		if c.bad {
			if err == nil {
				t.Errorf("ParseMemory(%q) should have failed", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseMemory(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseMemory(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestDefaultsAreHalfTheHost checks the documented default sizing.
func TestDefaultsAreHalfTheHost(t *testing.T) {
	d := config.Defaults()
	if d.Limits.MaxConcurrentJobs != 1 {
		t.Errorf("max_concurrent_jobs default = %d, want 1", d.Limits.MaxConcurrentJobs)
	}
	if d.Power.PauseBelowBatteryPct != 20 {
		t.Errorf("pause_below_battery_pct default = %d, want 20", d.Power.PauseBelowBatteryPct)
	}
	mem, err := config.ParseMemory(d.Limits.MaxMemory)
	if err != nil {
		t.Fatalf("default memory %q is unparseable: %v", d.Limits.MaxMemory, err)
	}
	host := config.HostMemoryBytes()
	// Allow rounding slack from the human-readable formatting.
	if mem > host*6/10 {
		t.Errorf("default memory %s is more than 60%% of host RAM %s", d.Limits.MaxMemory, config.FormatBytes(host))
	}
}

// TestWorkspaceRefusesDangerousPaths guards the destroy path.
func TestWorkspaceRefusesDangerousPaths(t *testing.T) {
	if err := DestroyWorkspace("/"); err == nil {
		t.Error("DestroyWorkspace(/) must be refused")
	}
	if err := DestroyWorkspace(""); err == nil {
		t.Error("DestroyWorkspace(\"\") must be refused")
	}
}

// TestRunnerNameIsUniqueAndLegal covers GitHub's runner name constraints.
func TestRunnerNameIsUniqueAndLegal(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		n := RunnerName("acme/some-very-long-repository-name-here")
		if len(n) > 64 {
			t.Fatalf("runner name %q is %d chars; GitHub caps at 64", n, len(n))
		}
		if strings.ContainsAny(n, "/ \t") {
			t.Fatalf("runner name %q contains illegal characters", n)
		}
		seen[n] = true
	}
	if len(seen) < 2 {
		t.Error("runner names are not unique enough; ephemeral registrations would collide")
	}
}

// TestPlatformMapping covers the actions/runner asset naming.
func TestPlatformMapping(t *testing.T) {
	cases := map[[2]string]string{
		{"darwin", "arm64"}: "osx-arm64",
		{"darwin", "amd64"}: "osx-x64",
		{"linux", "amd64"}:  "linux-x64",
		{"linux", "arm64"}:  "linux-arm64",
	}
	for in, want := range cases {
		got, err := platformFor(in[0], in[1])
		if err != nil {
			t.Errorf("platformFor(%v): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("platformFor(%v) = %q, want %q", in, got, want)
		}
	}
	if _, err := platformFor("plan9", "amd64"); err == nil {
		t.Error("unsupported OS should error rather than produce a bogus URL")
	}
}

// TestReleaseURL pins the download URL layout.
func TestReleaseURL(t *testing.T) {
	r := ResolveRelease("2.330.0", "osx-arm64")
	want := "https://github.com/actions/runner/releases/download/v2.330.0/actions-runner-osx-arm64-2.330.0.tar.gz"
	if r.URL != want {
		t.Errorf("URL = %q, want %q", r.URL, want)
	}
	// A leading v must not double up.
	if ResolveRelease("v2.330.0", "linux-x64").URL !=
		"https://github.com/actions/runner/releases/download/v2.330.0/actions-runner-linux-x64-2.330.0.tar.gz" {
		t.Error("leading v in version was not normalised")
	}
}
