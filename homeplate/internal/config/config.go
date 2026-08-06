// Package config holds Homeplate's on-disk configuration (~/.homeplate/config.toml).
//
// The config is hot-reloadable: the daemon watches the file's mtime and reloads
// limits without restarting. All limits are advisory defaults that individual
// engines translate into platform-specific enforcement (Docker --cpus/--memory,
// nice/taskpolicy on native macOS jobs).
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is the root of ~/.homeplate/config.toml.
type Config struct {
	Limits  Limits            `toml:"limits"`
	Power   Power             `toml:"power"`
	Sync    Sync              `toml:"sync"`
	Engine  Engine            `toml:"engine"`
	Repos   []LinkedRepo      `toml:"repos"`
	Savings SavingsCfg        `toml:"savings"`
	Extra   map[string]string `toml:"extra,omitempty"`

	path string
	mu   sync.RWMutex
}

// Limits are the resource caps applied to every job.
type Limits struct {
	// MaxCPUs is fractional CPU allowance passed to `docker run --cpus`.
	MaxCPUs float64 `toml:"max_cpus"`
	// MaxMemory is a human string ("8g", "512m") passed to `docker run --memory`.
	MaxMemory string `toml:"max_memory"`
	// MaxDiskGB caps the per-job workspace + container writable layer.
	MaxDiskGB int `toml:"max_disk_gb"`
	// MaxConcurrentJobs is how many jobs may execute at once on this machine.
	MaxConcurrentJobs int `toml:"max_concurrent_jobs"`
	// JobTimeout is a hard wall-clock kill for a single job.
	JobTimeout Duration `toml:"job_timeout"`
}

// Power controls when Homeplate is allowed to pick up work and when it holds
// a sleep assertion.
type Power struct {
	// PauseBelowBatteryPct pauses job *pickup* (not running jobs) when on
	// battery and below this level. 0 disables the check.
	PauseBelowBatteryPct int `toml:"pause_below_battery_pct"`
	// RunOnBattery permits picking up jobs while unplugged at all. Default
	// true: battery + no-wifi operation is a supported mode (offline results
	// replay on reconnect); the battery floor below is the safety net.
	RunOnBattery bool `toml:"run_on_battery"`
	// HoldSleepAssertion holds caffeinate/systemd-inhibit while work exists.
	HoldSleepAssertion bool `toml:"hold_sleep_assertion"`
	// AllowClamshellPmset permits the managed lid-close toggle: Homeplate sets
	// `sudo pmset -a disablesleep 1` while jobs run and reverts it afterwards,
	// on daemon exit, and on uninstall. Off by default: it is system-wide
	// (the pmset power-source prefix is ignored for disablesleep) and needs a
	// one-time sudoers helper via `homeplate power setup`.
	AllowClamshellPmset bool `toml:"allow_clamshell_pmset"`
	// ClamshellOnBattery permits the lid-close toggle while on battery,
	// reverting at ClamshellBatteryFloorPct. AC-only by default.
	ClamshellOnBattery bool `toml:"clamshell_on_battery"`
	// ClamshellBatteryFloorPct is where the clamshell toggle reverts on
	// battery. Only used with ClamshellOnBattery.
	ClamshellBatteryFloorPct int `toml:"clamshell_battery_floor_pct"`
	// OnlyWhenIdle pauses job pickup while the user is actively working:
	// sustained user CPU above IdleUserCPUPercent for IdleSustainedFor means
	// "the human is using the machine; yield".
	OnlyWhenIdle bool `toml:"only_when_idle"`
	// IdleUserCPUPercent is the user-CPU threshold (default 40).
	IdleUserCPUPercent int `toml:"idle_user_cpu_percent"`
	// IdleSustainedFor is how long user CPU must exceed the threshold
	// (default 5m).
	IdleSustainedFor Duration `toml:"idle_sustained_for"`
}

// Sync configures the reconciliation brain.
type Sync struct {
	// PollInterval is how often the brain reconciles queues with GitHub.
	PollInterval Duration `toml:"poll_interval"`
	// StatusPollInterval is how often githubstatus.com is polled.
	StatusPollInterval Duration `toml:"status_poll_interval"`
	// AutoApprove opts in to approving/merging PRs whose local checks passed.
	// Default false. See README security section.
	AutoApprove bool `toml:"auto_approve"`
	// AutoMerge additionally merges. Requires AutoApprove.
	AutoMerge bool `toml:"auto_merge"`
	// MergeMethod is one of merge|squash|rebase.
	MergeMethod string `toml:"merge_method"`
	// OfflineFallback enables Engine B when GitHub is down/unreachable.
	OfflineFallback bool `toml:"offline_fallback"`
	// AutoRelink lets the hourly drift reconcile repair renamed/transferred
	// repos automatically instead of only flagging them.
	AutoRelink bool `toml:"auto_relink"`
	// WatchLocalClones polls linked repos' local working copies (LinkedRepo
	// .LocalPath) for new commits and runs them via Engine B when GitHub is
	// offline or degraded. This is the "commit on a plane, results post when
	// you land" path.
	WatchLocalClones bool `toml:"watch_local_clones"`
}

// Engine picks execution backends.
type Engine struct {
	// RunnerVersion pins actions/runner. Empty means "resolve latest".
	RunnerVersion string `toml:"runner_version"`
	// DefaultImage is the container image for Linux jobs in Engine A.
	DefaultImage string `toml:"default_image"`
	// ActImage maps ubuntu-latest for Engine B (act -P).
	ActImage string `toml:"act_image"`
	// ContainerUser forces non-root execution where the image allows it.
	ContainerUser string `toml:"container_user"`
	// HostNetwork is false by default (no host network for job containers).
	HostNetwork bool `toml:"host_network"`
	// NativeMacOS allows macOS jobs to run directly on the host (sandboxed
	// per-job directory, no container). There is no macOS container runtime,
	// so this is documented as a softer isolation guarantee.
	NativeMacOS bool `toml:"native_macos"`
}

// SavingsCfg tunes the "$ saved" counter.
type SavingsCfg struct {
	// RateOverrideUSDPerMin lets a user pin rates if GitHub changes pricing.
	// Keys: "linux", "windows", "macos".
	RateOverrideUSDPerMin map[string]float64 `toml:"rate_override_usd_per_min"`
	// CountFreeMinutes counts jobs that would have been covered by the
	// account's included free minutes. Default false = conservative.
	CountFreeMinutes bool `toml:"count_free_minutes"`
}

// LinkedRepo records a repo/org this machine serves and which identity owns it.
type LinkedRepo struct {
	// Slug is "owner/repo" for repo-level, or "owner" for org-level.
	Slug string `toml:"slug"`
	// Scope is "repo" or "org".
	Scope string `toml:"scope"`
	// Profile is the auth profile name that owns this link.
	Profile string `toml:"profile"`
	// Labels are the runner labels registered for this link.
	Labels []string `toml:"labels"`
	// Public records that this is a public repo (requires explicit opt-in).
	Public bool `toml:"public"`
	// RunnerGroup is the org runner group, if any.
	RunnerGroup string `toml:"runner_group,omitempty"`
	// Mirror is the local bare clone path used by Engine B while offline.
	Mirror string `toml:"mirror,omitempty"`
	// LocalPath is the developer's working clone on this machine, when known
	// (set by `homeplate scan` or `homeplate link` from inside a clone). Used
	// to run never-pushed commits while fully offline.
	LocalPath string `toml:"local_path,omitempty"`
	// RepoID is GitHub's numeric repository id. It survives renames and
	// transfers, which is what the drift reconcile keys on.
	RepoID int64 `toml:"repo_id,omitempty"`
	// LinkedAt is when the link was created.
	LinkedAt time.Time `toml:"linked_at"`
}

// Duration wraps time.Duration with TOML string marshalling ("30s", "6h").
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalText(text []byte) error {
	v, err := time.ParseDuration(string(text))
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// Dir returns the Homeplate home directory, honouring $HOMEPLATE_HOME for tests.
//
// Upgrade path: Homeplate was briefly named "homerun". If ~/.homeplate does
// not exist but ~/.homerun does, the old directory is renamed in place so
// auth profiles, mirrors, and the job database carry over.
func Dir() string {
	if v := os.Getenv("HOMEPLATE_HOME"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".homeplate"
	}
	dir := filepath.Join(home, ".homeplate")
	old := filepath.Join(home, ".homerun")
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		if _, err := os.Stat(old); err == nil {
			if err := os.Rename(old, dir); err == nil {
				fmt.Printf("homeplate: migrated %s -> %s\n", old, dir)
			}
		}
	}
	return dir
}

// Path is the config file location.
func Path() string { return filepath.Join(Dir(), "config.toml") }

// Defaults returns a config sized to the host: half the cores, half the RAM.
func Defaults() *Config {
	cores := runtime.NumCPU()
	halfCPU := float64(cores) / 2
	if halfCPU < 1 {
		halfCPU = 1
	}
	halfMem := HostMemoryBytes() / 2
	if halfMem < 1<<30 {
		halfMem = 1 << 30
	}
	return &Config{
		Limits: Limits{
			MaxCPUs:           round2(halfCPU),
			MaxMemory:         FormatBytes(halfMem),
			MaxDiskGB:         50,
			MaxConcurrentJobs: 1,
			JobTimeout:        Duration{6 * time.Hour},
		},
		Power: Power{
			PauseBelowBatteryPct:     20,
			RunOnBattery:             true,
			HoldSleepAssertion:       true,
			AllowClamshellPmset:      false,
			ClamshellOnBattery:       false,
			ClamshellBatteryFloorPct: 30,
			OnlyWhenIdle:             false,
			IdleUserCPUPercent:       40,
			IdleSustainedFor:         Duration{5 * time.Minute},
		},
		Sync: Sync{
			PollInterval:       Duration{30 * time.Second},
			StatusPollInterval: Duration{2 * time.Minute},
			AutoApprove:        false,
			AutoMerge:          false,
			MergeMethod:        "squash",
			OfflineFallback:    true,
			AutoRelink:         false,
			WatchLocalClones:   true,
		},
		Engine: Engine{
			RunnerVersion: "",
			DefaultImage:  "catthehacker/ubuntu:act-latest",
			ActImage:      "catthehacker/ubuntu:act-latest",
			ContainerUser: "",
			HostNetwork:   false,
			NativeMacOS:   runtime.GOOS == "darwin",
		},
		Savings: SavingsCfg{
			RateOverrideUSDPerMin: map[string]float64{},
			CountFreeMinutes:      false,
		},
		path: Path(),
	}
}

// Load reads config.toml, filling in defaults for missing keys. A missing file
// is not an error: defaults are returned so `homeplate status` works pre-init.
func Load() (*Config, error) {
	c := Defaults()
	c.path = Path()
	b, err := os.ReadFile(c.path)
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return nil, err
	}
	if err := toml.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", c.path, err)
	}
	c.normalize()
	return c, nil
}

func (c *Config) normalize() {
	d := Defaults()
	if c.Limits.MaxCPUs <= 0 {
		c.Limits.MaxCPUs = d.Limits.MaxCPUs
	}
	if strings.TrimSpace(c.Limits.MaxMemory) == "" {
		c.Limits.MaxMemory = d.Limits.MaxMemory
	}
	if c.Limits.MaxConcurrentJobs <= 0 {
		c.Limits.MaxConcurrentJobs = 1
	}
	if c.Limits.MaxDiskGB <= 0 {
		c.Limits.MaxDiskGB = d.Limits.MaxDiskGB
	}
	if c.Limits.JobTimeout.Duration <= 0 {
		c.Limits.JobTimeout = d.Limits.JobTimeout
	}
	if c.Sync.PollInterval.Duration <= 0 {
		c.Sync.PollInterval = d.Sync.PollInterval
	}
	if c.Sync.StatusPollInterval.Duration <= 0 {
		c.Sync.StatusPollInterval = d.Sync.StatusPollInterval
	}
	if c.Sync.MergeMethod == "" {
		c.Sync.MergeMethod = "squash"
	}
	if c.Engine.DefaultImage == "" {
		c.Engine.DefaultImage = d.Engine.DefaultImage
	}
	if c.Engine.ActImage == "" {
		c.Engine.ActImage = d.Engine.ActImage
	}
	if c.Savings.RateOverrideUSDPerMin == nil {
		c.Savings.RateOverrideUSDPerMin = map[string]float64{}
	}
	// AutoMerge without AutoApprove is meaningless and dangerous; clamp it.
	if c.Sync.AutoMerge && !c.Sync.AutoApprove {
		c.Sync.AutoMerge = false
	}
}

// Save atomically writes the config back to disk.
func (c *Config) Save() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.path == "" {
		c.path = Path()
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	enc := toml.NewEncoder(f)
	if err := enc.Encode(c); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, c.path)
}

// AddRepo links a repo idempotently: re-linking updates the existing entry
// rather than creating a duplicate.
func (c *Config) AddRepo(r LinkedRepo) (added bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.Repos {
		if strings.EqualFold(c.Repos[i].Slug, r.Slug) && c.Repos[i].Scope == r.Scope {
			linkedAt := c.Repos[i].LinkedAt
			c.Repos[i] = r
			c.Repos[i].LinkedAt = linkedAt
			return false
		}
	}
	c.Repos = append(c.Repos, r)
	return true
}

// RemoveRepo unlinks by slug. Returns true if something was removed.
func (c *Config) RemoveRepo(slug string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.Repos[:0]
	found := false
	for _, r := range c.Repos {
		if strings.EqualFold(r.Slug, slug) {
			found = true
			continue
		}
		out = append(out, r)
	}
	c.Repos = out
	return found
}

// FindRepo returns the linked repo for a slug.
func (c *Config) FindRepo(slug string) (LinkedRepo, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, r := range c.Repos {
		if strings.EqualFold(r.Slug, slug) {
			return r, true
		}
	}
	return LinkedRepo{}, false
}

// ReposForProfile filters links by identity.
func (c *Config) ReposForProfile(profile string) []LinkedRepo {
	var out []LinkedRepo
	for _, r := range c.Repos {
		if r.Profile == profile {
			out = append(out, r)
		}
	}
	return out
}

// ParseMemory converts "8g"/"512m"/"1024" into bytes.
func ParseMemory(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, fmt.Errorf("empty memory value")
	}
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "gb"), strings.HasSuffix(s, "g"):
		mult = 1 << 30
		s = strings.TrimSuffix(strings.TrimSuffix(s, "gb"), "g")
	case strings.HasSuffix(s, "mb"), strings.HasSuffix(s, "m"):
		mult = 1 << 20
		s = strings.TrimSuffix(strings.TrimSuffix(s, "mb"), "m")
	case strings.HasSuffix(s, "kb"), strings.HasSuffix(s, "k"):
		mult = 1 << 10
		s = strings.TrimSuffix(strings.TrimSuffix(s, "kb"), "k")
	case strings.HasSuffix(s, "b"):
		s = strings.TrimSuffix(s, "b")
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, fmt.Errorf("invalid memory value %q", s)
	}
	if f <= 0 {
		return 0, fmt.Errorf("memory must be > 0")
	}
	return int64(f * float64(mult)), nil
}

// FormatBytes renders bytes as a Docker-compatible memory string.
func FormatBytes(b int64) string {
	switch {
	case b >= 1<<30 && b%(1<<30) == 0:
		return fmt.Sprintf("%dg", b/(1<<30))
	case b >= 1<<30:
		return fmt.Sprintf("%.1fg", float64(b)/float64(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%dm", b/(1<<20))
	default:
		return fmt.Sprintf("%d", b)
	}
}

func round2(f float64) float64 {
	return float64(int(f*100+0.5)) / 100
}
