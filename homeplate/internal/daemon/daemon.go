// Package daemon is the long-running supervisor: it holds sleep assertions,
// watches power and connectivity, runs ephemeral listeners for every linked
// target, and drives the sync brain.
package daemon

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/homeplate-ci/homeplate/internal/config"
	"github.com/homeplate-ci/homeplate/internal/connectivity"
	"github.com/homeplate-ci/homeplate/internal/ghapi"
	"github.com/homeplate-ci/homeplate/internal/labels"
	"github.com/homeplate-ci/homeplate/internal/offline"
	"github.com/homeplate-ci/homeplate/internal/power"
	"github.com/homeplate-ci/homeplate/internal/runner"
	"github.com/homeplate-ci/homeplate/internal/store"
	"github.com/homeplate-ci/homeplate/internal/syncbrain"
)

// IdleRotate is how long a listener waits for work before releasing its
// concurrency slot to another linked target.
//
// WHY THIS EXISTS: an ephemeral runner registration serves exactly one job, so
// a listener occupies a slot while idle. With max_concurrent_jobs = 1 and three
// linked repos, repo #2 and #3 would never be covered. Rotation gives every
// target a fair share of the available slots. The cost is that a job pushed
// while its repo is not currently being listened to waits up to one rotation
// period - which is why `homeplate status` prints the current rotation set.
const IdleRotate = 60 * time.Second

// Daemon is the running supervisor.
type Daemon struct {
	Config  *config.Config
	DB      *store.DB
	HomeDir string

	Power   *power.Manager
	Conn    *connectivity.Monitor
	Clients syncbrain.Clients

	// Docker/native engines.
	Docker *runner.Docker
	Cache  *runner.Cache

	// Log sinks.
	Out io.Writer

	// Paused gates all job pickup (homeplate pause/resume).
	paused atomic.Bool

	// busy counts slots currently EXECUTING a job (connected or offline).
	// Engine A jobs only reach the database after they finish, so the sleep
	// assertion cannot key off the job table - a machine would idle-sleep
	// mid-job. This counter is what the assertion actually watches.
	busy atomic.Int32

	// Idle implements only_when_idle (sustained user CPU yields pickup).
	Idle *power.BusyTracker

	// clamshellOn records whether the managed lid-close override is held.
	clamshellOn bool

	mu      sync.RWMutex
	targets []runner.Target
	// activeTargets is what the listeners are currently serving.
	activeTargets []string
	releaseSleep  func()
	lastConn      connectivity.Status
	lastPower     power.State
	configMTime   time.Time
}

// New builds a daemon.
func New(cfg *config.Config, db *store.DB, homeDir string, clients syncbrain.Clients, out io.Writer) *Daemon {
	return &Daemon{
		Config:  cfg,
		DB:      db,
		HomeDir: homeDir,
		Power:   power.NewManager(),
		Conn:    connectivity.NewMonitor(),
		Clients: clients,
		Cache:   runner.NewCache(homeDir),
		Out:     out,
	}
}

func (d *Daemon) logf(format string, args ...any) {
	if d.Out == nil {
		return
	}
	fmt.Fprintf(d.Out, "%s "+format+"\n", append([]any{time.Now().Format("15:04:05")}, args...)...)
}

// Pause stops picking up new jobs. Running jobs finish.
func (d *Daemon) Pause() { d.paused.Store(true) }

// Resume re-enables job pickup.
func (d *Daemon) Resume() { d.paused.Store(false) }

// IsPaused reports pickup state.
func (d *Daemon) IsPaused() bool { return d.paused.Load() }

// PauseFile is the on-disk flag so the CLI can pause a running daemon without
// an IPC channel. Simple, inspectable, and survives a daemon restart.
func PauseFile(homeDir string) string { return filepath.Join(homeDir, "paused") }

// Run is the main loop. It returns when ctx is cancelled.
func (d *Daemon) Run(ctx context.Context) error {
	d.logf("homeplate daemon starting (home=%s)", d.HomeDir)

	// Crash auto-revert for the lid-close override: if a previous daemon died
	// while holding `pmset disablesleep`, put the system back before anything
	// else runs. The user asked for a managed toggle, not a permanent one.
	if reverted, err := power.RevertStaleClamshell(ctx, d.HomeDir); reverted {
		if err != nil {
			d.logf("could not revert stale clamshell override: %v", err)
		} else {
			d.logf("reverted stale lid-close override left by a crashed daemon")
		}
	}

	// Any job still marked running belongs to a previous process that died or
	// a machine that slept. Never silently drop it.
	if n, err := d.DB.MarkInterrupted(ctx, "daemon restarted or machine slept mid-job"); err == nil && n > 0 {
		d.logf("marked %d job(s) interrupted from a previous session", n)
		// Offline jobs are re-queued automatically; connected jobs defer to
		// GitHub's own re-run path, which is the documented behaviour.
		if m, err := d.DB.RequeueInterrupted(ctx); err == nil && m > 0 {
			d.logf("re-queued %d offline job(s) for another attempt", m)
		}
	}

	// Reap containers orphaned by a hard power loss.
	if d.Docker != nil {
		if n, err := d.Docker.CleanupOrphans(ctx); err == nil && n > 0 {
			d.logf("removed %d orphaned job container(s)", n)
		}
	}

	var wg sync.WaitGroup

	// Supervisor: power, config reload, sleep assertions, pause flag.
	wg.Add(1)
	go func() { defer wg.Done(); d.superviseLoop(ctx) }()

	// Sync brain: replay offline results when connectivity returns.
	wg.Add(1)
	go func() { defer wg.Done(); d.replayLoop(ctx) }()

	// Listeners: the actual job execution slots.
	wg.Add(1)
	go func() { defer wg.Done(); d.listenerLoop(ctx) }()

	// Local-clone watcher: run never-pushed commits while offline.
	wg.Add(1)
	go func() { defer wg.Done(); d.watchLoop(ctx) }()

	// Drift reconcile: renamed/transferred repos and revoked access.
	wg.Add(1)
	go func() { defer wg.Done(); d.driftLoop(ctx) }()

	<-ctx.Done()
	d.logf("shutting down; releasing sleep assertion")
	d.Power.ReleaseAll()
	if d.clamshellOn {
		if err := power.EnsureClamshell(context.Background(), d.HomeDir, false); err != nil {
			d.logf("could not revert lid-close override on shutdown: %v", err)
		} else {
			d.logf("reverted lid-close override (pmset disablesleep 0)")
		}
	}
	wg.Wait()
	return nil
}

// SetTargets replaces the served target list (called after `homeplate link`).
func (d *Daemon) SetTargets(t []runner.Target) {
	d.mu.Lock()
	d.targets = t
	d.mu.Unlock()
}

// Targets returns the current target list.
func (d *Daemon) Targets() []runner.Target {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]runner.Target, len(d.targets))
	copy(out, d.targets)
	return out
}

// superviseLoop owns the sleep assertion and hot config reload.
func (d *Daemon) superviseLoop(ctx context.Context) {
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}

		d.reloadConfigIfChanged()
		d.syncPauseFile()

		if d.Idle != nil && d.Config.Power.OnlyWhenIdle {
			d.Idle.Sample(ctx)
		}

		st := power.Read(ctx)
		d.mu.Lock()
		d.lastPower = st
		d.mu.Unlock()

		// Hold a sleep assertion whenever there is work AND we are on AC.
		// Holding it on battery would drain the machine to 0% and is exactly
		// the behaviour a laptop user would (rightly) consider hostile.
		// "Work" is the DB queue OR a slot actively executing: Engine A jobs
		// only reach the DB after completion, so keying off the table alone
		// would let the machine idle-sleep mid-job.
		stats, err := d.DB.Stats(ctx)
		hasWork := (err == nil && (stats.Queued > 0 || stats.Running > 0)) || d.busy.Load() > 0

		d.mu.Lock()
		holding := d.releaseSleep != nil
		d.mu.Unlock()

		want := d.Config.Power.HoldSleepAssertion && hasWork && st.OnAC()
		switch {
		case want && !holding:
			rel, err := d.Power.Hold("Homeplate is running CI jobs")
			if err != nil {
				d.logf("could not acquire sleep assertion: %v", err)
			} else {
				d.mu.Lock()
				d.releaseSleep = rel
				d.mu.Unlock()
				d.logf("holding sleep assertion (%d running, %d queued)", stats.Running, stats.Queued)
			}
		case !want && holding:
			d.mu.Lock()
			rel := d.releaseSleep
			d.releaseSleep = nil
			d.mu.Unlock()
			if rel != nil {
				rel()
			}
			d.logf("released sleep assertion")
		}

		d.manageClamshell(ctx, st, hasWork)
	}
}

// manageClamshell drives the managed lid-close toggle (Fact: lid close beats
// caffeinate; only a root `pmset -a disablesleep 1` overrides it). It is set
// while work exists and reverted as soon as work drains. AC-only unless the
// user opted into battery with a floor. All of this is gated behind
// allow_clamshell_pmset = true plus the one-time sudoers helper.
func (d *Daemon) manageClamshell(ctx context.Context, st power.State, hasWork bool) {
	if !d.Config.Power.AllowClamshellPmset {
		return
	}

	allowed := st.OnAC()
	if !allowed && d.Config.Power.ClamshellOnBattery && st.HasBattery && st.Source == power.SourceBattery {
		if st.BatteryPercent >= d.Config.Power.ClamshellBatteryFloorPct {
			allowed = true
		}
	}

	want := hasWork && allowed

	d.mu.Lock()
	on := d.clamshellOn
	d.mu.Unlock()

	switch {
	case want && !on:
		if err := power.EnsureClamshell(ctx, d.HomeDir, true); err != nil {
			d.logf("lid-close toggle: %v", err)
			return
		}
		d.mu.Lock()
		d.clamshellOn = true
		d.mu.Unlock()
		d.logf("lid-close override ON (pmset disablesleep 1; jobs keep running with the lid closed)")
	case !want && on:
		// On battery below the floor, or work drained, or unplugged without
		// opt-in: revert. Never leave a system-wide override lying around.
		if err := power.EnsureClamshell(ctx, d.HomeDir, false); err != nil {
			d.logf("could not revert lid-close override: %v", err)
			return
		}
		d.mu.Lock()
		d.clamshellOn = false
		d.mu.Unlock()
		d.logf("lid-close override OFF (pmset disablesleep 0)")
	}
}

// reloadConfigIfChanged implements hot reload: `homeplate limit --cpus 4` takes
// effect on the next job without restarting the daemon.
func (d *Daemon) reloadConfigIfChanged() {
	st, err := os.Stat(config.Path())
	if err != nil {
		return
	}
	d.mu.Lock()
	changed := st.ModTime().After(d.configMTime)
	if changed {
		d.configMTime = st.ModTime()
	}
	d.mu.Unlock()
	if !changed {
		return
	}
	fresh, err := config.Load()
	if err != nil {
		d.logf("config reload failed, keeping previous settings: %v", err)
		return
	}
	d.mu.Lock()
	d.Config = fresh
	d.mu.Unlock()
	d.logf("config reloaded: cpus=%g memory=%s concurrency=%d",
		fresh.Limits.MaxCPUs, fresh.Limits.MaxMemory, fresh.Limits.MaxConcurrentJobs)
}

func (d *Daemon) syncPauseFile() {
	_, err := os.Stat(PauseFile(d.HomeDir))
	d.paused.Store(err == nil)
}

// replayLoop pushes offline results back to GitHub when it returns.
func (d *Daemon) replayLoop(ctx context.Context) {
	interval := d.Config.Sync.PollInterval.Duration
	if interval <= 0 {
		interval = 30 * time.Second
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()

	replayer := &syncbrain.Replayer{
		DB:      d.DB,
		Clients: d.Clients,
		Config:  d.Config,
		Log:     d.logf,
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}

		status := d.Conn.Current(ctx)
		d.mu.Lock()
		prev := d.lastConn
		d.lastConn = status
		d.mu.Unlock()
		if prev.State != status.State && prev.State != "" {
			d.logf("connectivity: %s -> %s (%s)", prev.State, status.State, status.Reason)
		}

		// Replay only makes sense when GitHub's API is reachable. Note this
		// deliberately uses APIReachable, not State: Actions can be down while
		// the REST API (which serves the Status API) is perfectly healthy, and
		// that is precisely when we want to push queued results.
		if !status.APIReachable {
			continue
		}
		out, err := replayer.ReplayAll(ctx)
		if err != nil {
			d.logf("replay pass failed: %v", err)
			continue
		}
		if out.Posted > 0 || out.Approved > 0 || out.Merged > 0 {
			d.logf("replayed %d status(es), %d approval(s), %d merge(s)", out.Posted, out.Approved, out.Merged)
		}
		for _, e := range out.Errors {
			d.logf("replay error: %v", e)
		}
	}
}

// listenerLoop runs the concurrency slots.
func (d *Daemon) listenerLoop(ctx context.Context) {
	slots := d.Config.Limits.MaxConcurrentJobs
	if slots < 1 {
		slots = 1
	}

	var wg sync.WaitGroup
	for i := 0; i < slots; i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			d.slotLoop(ctx, slot)
		}(i)
	}
	wg.Wait()
}

// slotLoop is one execution slot: pick a target, listen, run one job, repeat.
func (d *Daemon) slotLoop(ctx context.Context, slot int) {
	rotation := slot
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if d.IsPaused() {
			sleepCtx(ctx, 5*time.Second)
			continue
		}

		// Battery gate. Checked before every pickup, never mid-job.
		pol := power.Policy{
			PauseBelowPct: d.Config.Power.PauseBelowBatteryPct,
			RunOnBattery:  d.Config.Power.RunOnBattery,
		}
		if dec := pol.Evaluate(power.Read(ctx)); !dec.Allow {
			d.logf("slot %d paused: %s", slot, dec.Reason)
			sleepCtx(ctx, 30*time.Second)
			continue
		}

		// only_when_idle: yield while the human is actively using the machine.
		if d.Config.Power.OnlyWhenIdle && d.Idle != nil &&
			d.Idle.ShouldYield(d.Config.Power.IdleSustainedFor.Duration) {
			sleepCtx(ctx, 30*time.Second)
			continue
		}

		targets := d.Targets()
		if len(targets) == 0 {
			sleepCtx(ctx, 15*time.Second)
			continue
		}

		conn := d.Conn.Current(ctx)
		if conn.UseOffline() {
			// Engine B: drain the local queue instead of listening to GitHub.
			d.runOfflineSlot(ctx, slot)
			continue
		}

		target := targets[rotation%len(targets)]
		rotation++
		d.runConnectedSlot(ctx, slot, target, len(targets) > d.Config.Limits.MaxConcurrentJobs)
	}
}

// runConnectedSlot registers one ephemeral runner and serves at most one job.
func (d *Daemon) runConnectedSlot(ctx context.Context, slot int, t runner.Target, rotate bool) {
	client, err := d.Clients.For(t.Profile)
	if err != nil {
		d.logf("slot %d: no GitHub client for profile %q: %v", slot, t.Profile, err)
		sleepCtx(ctx, 30*time.Second)
		return
	}

	// Rotation: if this listener sits idle it must release the slot so other
	// linked repos get covered. A listener that HAS picked up a job is never
	// rotated - OnJobStart cancels the timer.
	listenCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var started atomic.Bool
	onStart := func(jobName string) {
		started.Store(true)
		d.logf("slot %d: %s picked up job %q", slot, t.Slug, jobName)
	}

	// Pick the engine by the target's execution class. A macOS-class target
	// runs on the host via NativeEngine (no macOS container runtime exists);
	// everything else runs in a capped Docker container. The target's labels
	// were set per-class in TargetsFromConfig, so a Linux container can never
	// claim a macOS job.
	var runOne func(context.Context, runner.Target, io.Writer) (*runner.JobOutcome, error)
	if t.Class == labels.ClassMacOS {
		nat := &runner.NativeEngine{
			Cache:         d.Cache,
			HomeDir:       d.HomeDir,
			Host:          client.Host,
			Minter:        client,
			UseTaskPolicy: true,
			NiceLevel:     10,
		}
		runOne = nat.RunOne
	} else {
		if d.Docker == nil {
			dk, err := runner.NewDocker()
			if err != nil {
				d.logf("slot %d: %v", slot, err)
				sleepCtx(ctx, 60*time.Second)
				cancel()
				return
			}
			d.Docker = dk
		}
		eng := &runner.Engine{
			Docker:     d.Docker,
			Cache:      d.Cache,
			Config:     d.Config,
			HomeDir:    d.HomeDir,
			Host:       client.Host, // GHES-aware: never hardcode github.com
			Minter:     client,
			DB:         d.DB,
			OnJobStart: onStart,
		}
		runOne = eng.RunOne
	}

	if rotate {
		go func() {
			timer := time.NewTimer(IdleRotate)
			defer timer.Stop()
			select {
			case <-listenCtx.Done():
			case <-timer.C:
				if !started.Load() {
					cancel() // rotate to the next target
				}
			}
		}()
	}

	// Mark the slot busy BEFORE the run so the sleep assertion is held for
	// the entire job, including the registration wait.
	d.busy.Add(1)
	out, runErr := runOne(listenCtx, t, nil)
	d.busy.Add(-1)

	if !out.PickedUpJob {
		if runErr != nil && listenCtx.Err() == nil {
			d.logf("slot %d: listener for %s failed: %v", slot, t.Slug, runErr)
			sleepCtx(ctx, 20*time.Second)
		}
		return // idle rotation or clean shutdown; nothing to record
	}

	d.recordOutcome(ctx, t, out, runErr)
}

// recordOutcome persists a completed Engine A job.
func (d *Daemon) recordOutcome(ctx context.Context, t runner.Target, out *runner.JobOutcome, runErr error) {
	machineID, _ := d.DB.GetMeta(ctx, "machine_id")
	extID := fmt.Sprintf("%s|%s|%s|%s", store.EngineA, t.Slug, out.RunnerName, out.JobName)

	j := &store.Job{
		ExternalID:  extID,
		Profile:     t.Profile,
		RepoSlug:    t.Slug,
		Engine:      store.EngineA,
		JobName:     out.JobName,
		State:       store.StateQueued,
		RunnerClass: string(out.Class),
		MachineID:   machineID,
		LogPath:     out.LogPath,
		QueuedAt:    out.Started,
	}
	if _, err := d.DB.EnqueueJob(ctx, j); err != nil {
		d.logf("could not record job: %v", err)
		return
	}
	// Stamp the real timings so the savings counter is accurate.
	if _, err := d.DB.SQL().ExecContext(ctx,
		`UPDATE jobs SET started_at=?, finished_at=?, billable_seconds=?, state=?, exit_code=?, reason=?, replayed=1
		 WHERE id=?`,
		out.Started, out.Finished, out.Duration().Seconds(), string(out.State()), out.ExitCode,
		reasonOf(runErr), j.ID); err != nil {
		d.logf("could not finalise job %d: %v", j.ID, err)
	}

	d.logf("job %q on %s: %s in %s (Engine A, container destroyed)",
		out.JobName, t.Slug, out.State(), out.Duration().Round(time.Second))
}

func reasonOf(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// runOfflineSlot drains the Engine B queue.
func (d *Daemon) runOfflineSlot(ctx context.Context, slot int) {
	job, err := d.DB.NextQueued(ctx, store.EngineB)
	if err != nil {
		d.logf("slot %d: queue read failed: %v", slot, err)
		sleepCtx(ctx, 15*time.Second)
		return
	}
	if job == nil {
		sleepCtx(ctx, 10*time.Second)
		return
	}
	d.logf("slot %d: running %s %s offline via act", slot, job.RepoSlug, short(job.CommitSHA))

	d.busy.Add(1)
	res, err := d.runOfflineJob(ctx, job)
	d.busy.Add(-1)
	state := store.StateFailed
	exit := 1
	reason := ""
	if err != nil {
		reason = err.Error()
	} else if res != nil {
		exit = res.ExitCode
		if res.Succeeded {
			state = store.StateSucceeded
		}
		for i := range res.Steps {
			res.Steps[i].JobID = job.ID
			_ = d.DB.AddStep(ctx, &res.Steps[i])
		}
	}
	if err := d.DB.FinishJob(ctx, job.ID, state, exit, reason); err != nil {
		d.logf("could not finish job %d: %v", job.ID, err)
	}
	d.logf("offline job %d finished: %s (will be replayed to GitHub on reconnect)", job.ID, state)
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// TargetsFromConfig converts linked repos into per-class runner targets.
//
// Labels are generated HERE, per execution class, not taken from the linked
// repo's stored label list. The stored list is treated as extra user labels
// only. This is the fix for the routing bug: a Docker listener (always
// linux-class, even on a Mac) must never carry homeplate-macos, or GitHub's
// superset matching would hand it macOS jobs it cannot run.
//
// hostFor resolves the GitHub hostname for an auth profile (github.com or a
// GHES host); pass nil to default everything to github.com.
func TargetsFromConfig(cfg *config.Config, hostFor func(profile string) string) []runner.Target {
	var out []runner.Target
	for _, r := range cfg.Repos {
		host := ""
		if hostFor != nil {
			host = hostFor(r.Profile)
		}
		extra := extraLabels(r.Labels)
		base := runner.Target{
			Slug:        r.Slug,
			Scope:       r.Scope,
			Profile:     r.Profile,
			RunnerGroup: r.RunnerGroup,
			Host:        host,
		}
		// Every target serves Linux jobs in a container.
		linux := base
		linux.Class = labels.ClassLinux
		linux.Labels = labels.LabelsForClass(labels.ClassLinux, extra...)
		out = append(out, linux)

		// macOS jobs run natively (no macOS container runtime exists), only
		// on a Mac and only when enabled.
		if runtime.GOOS == "darwin" && cfg.Engine.NativeMacOS {
			mac := base
			mac.Class = labels.ClassMacOS
			mac.Labels = labels.LabelsForClass(labels.ClassMacOS, extra...)
			out = append(out, mac)
		}
	}
	return out
}

// extraLabels strips Homeplate's own generated labels from a stored list so
// legacy configs (which stored the full generated set) don't reintroduce the
// wrong class labels. Only genuine user extras survive.
func extraLabels(stored []string) []string {
	var out []string
	for _, l := range stored {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(strings.ToLower(l), labels.Homeplate) {
			continue
		}
		out = append(out, l)
	}
	return out
}

// ClientMap is the default Clients implementation: one *ghapi.Client per profile.
type ClientMap struct {
	mu sync.RWMutex
	m  map[string]*ghapi.Client
}

// NewClientMap builds an empty registry.
func NewClientMap() *ClientMap { return &ClientMap{m: map[string]*ghapi.Client{}} }

// Add registers a client for a profile.
func (c *ClientMap) Add(profile string, client *ghapi.Client) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[profile] = client
}

// For resolves a client, which is how one daemon multiplexes several identities.
func (c *ClientMap) For(profile string) (*ghapi.Client, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	cl, ok := c.m[profile]
	if !ok {
		return nil, fmt.Errorf("no authenticated client for profile %q (run `homeplate auth add %s`)", profile, profile)
	}
	return cl, nil
}

// Profiles lists registered identities.
func (c *ClientMap) Profiles() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]string, 0, len(c.m))
	for k := range c.m {
		out = append(out, k)
	}
	return out
}

// ---------------- local-clone watcher (offline auto-run) ----------------

// watchInterval is how often local working clones are polled for new commits.
const watchInterval = 45 * time.Second

// watchLoop is what makes "commit on a plane, results post when you land"
// work WITHOUT any manual command: while the machine is offline (or Actions
// is degraded), new commits in a linked repo's local clone are mirrored and
// queued for Engine B automatically. While online it only refreshes mirrors
// and tracks HEAD - GitHub dispatches those jobs to Engine A itself.
func (d *Daemon) watchLoop(ctx context.Context) {
	if !d.Config.Sync.WatchLocalClones {
		return
	}
	tick := time.NewTicker(watchInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		d.watchOnce(ctx)
	}
}

func (d *Daemon) watchOnce(ctx context.Context) {
	for _, r := range d.Config.Repos {
		if r.Scope != "repo" || r.LocalPath == "" {
			continue
		}
		mirror, err := offline.LocalRepoMirror(ctx, d.HomeDir, r.Slug, r.LocalPath)
		if err != nil {
			continue // clone moved or deleted; drift reconcile handles flags
		}
		branch := "refs/heads/main"
		// Use the clone's current branch - that is where the developer commits.
		if b, err := headBranch(ctx, r.LocalPath); err == nil && b != "" {
			branch = "refs/heads/" + b
		}
		sha, err := mirror.ResolveRef(ctx, branch)
		if err != nil {
			continue
		}

		metaKey := "watch:" + r.Slug
		last, _ := d.DB.GetMeta(ctx, metaKey)
		if last == sha {
			continue
		}
		if err := d.DB.SetMeta(ctx, metaKey, sha); err != nil {
			d.logf("watch: could not record HEAD for %s: %v", r.Slug, err)
		}
		if last == "" {
			continue // first sighting: record, do not run the world
		}

		// New commit. Online: GitHub will dispatch it to Engine A; nothing to
		// do. Offline/degraded: queue it for act so work continues with no
		// network at all.
		if !d.Conn.Current(ctx).UseOffline() {
			continue
		}
		machineID, _ := d.DB.GetMeta(ctx, "machine_id")
		j := &store.Job{
			ExternalID:  fmt.Sprintf("%s|%s|%s||", store.EngineB, r.Slug, sha),
			Profile:     r.Profile,
			RepoSlug:    r.Slug,
			Engine:      store.EngineB,
			CommitSHA:   sha,
			Ref:         branch,
			EventName:   "push",
			State:       store.StateQueued,
			RunnerClass: string(labels.ClassLinux),
			MachineID:   machineID,
			QueuedAt:    time.Now().UTC(),
		}
		created, err := d.DB.EnqueueJob(ctx, j)
		if err != nil {
			d.logf("watch: could not queue %s %s: %v", r.Slug, short(sha), err)
			continue
		}
		if created {
			d.logf("watch: new local commit %s on %s queued for offline run", short(sha), r.Slug)
		}
	}
}

func headBranch(ctx context.Context, repoPath string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", repoPath, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return "", err
	}
	b := strings.TrimSpace(string(out))
	if b == "HEAD" { // detached
		return "", fmt.Errorf("detached HEAD")
	}
	return b, nil
}

// ---------------- drift reconcile ----------------

// driftInterval is the hourly cadence the spec asks for.
const driftInterval = 1 * time.Hour

// driftLoop checks linked repos for renames, transfers, and revoked access.
// GitHub redirects repo API calls across renames, so GetRepo on the old slug
// returns the NEW full_name - comparing them detects the drift. With
// auto_relink the config is repaired in place; otherwise it is flagged in
// the daemon log and surfaced by `homeplate doctor`.
func (d *Daemon) driftLoop(ctx context.Context) {
	tick := time.NewTicker(driftInterval)
	defer tick.Stop()
	// First pass shortly after start, then hourly.
	timer := time.NewTimer(2 * time.Minute)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}
	for {
		d.driftOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (d *Daemon) driftOnce(ctx context.Context) {
	changed := false
	for i, r := range d.Config.Repos {
		if r.Scope != "repo" {
			continue
		}
		client, err := d.Clients.For(r.Profile)
		if err != nil {
			continue
		}
		repo, err := client.GetRepo(ctx, r.Slug)
		switch {
		case ghapi.IsNotFound(err):
			d.logf("drift: %s is gone or access was revoked (profile %s) - flagged; run `homeplate doctor`",
				r.Slug, r.Profile)
			_ = d.DB.SetMeta(ctx, "drift:"+r.Slug, "inaccessible")
		case err != nil:
			// Transient network/auth failure: not drift.
		case !strings.EqualFold(repo.FullName, r.Slug):
			d.logf("drift: %s was renamed/transferred to %s", r.Slug, repo.FullName)
			if d.Config.Sync.AutoRelink {
				d.Config.Repos[i].Slug = repo.FullName
				changed = true
				d.logf("drift: auto-relinked to %s", repo.FullName)
			} else {
				_ = d.DB.SetMeta(ctx, "drift:"+r.Slug, "renamed to "+repo.FullName)
			}
		default:
			_ = d.DB.SetMeta(ctx, "drift:"+r.Slug, "")
			// Keep RepoID fresh; it is what survives future renames.
			if r.RepoID == 0 && repo.ID != 0 {
				d.Config.Repos[i].RepoID = repo.ID
				changed = true
			}
		}
	}
	if changed {
		if err := d.Config.Save(); err != nil {
			d.logf("drift: could not save repaired config: %v", err)
		}
	}
}
