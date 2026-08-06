// Package daemon is the long-running supervisor: it holds sleep assertions,
// watches power and connectivity, runs ephemeral listeners for every linked
// target, and drives the sync brain.
package daemon

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/homerun-ci/homerun/internal/config"
	"github.com/homerun-ci/homerun/internal/connectivity"
	"github.com/homerun-ci/homerun/internal/ghapi"
	"github.com/homerun-ci/homerun/internal/labels"
	"github.com/homerun-ci/homerun/internal/power"
	"github.com/homerun-ci/homerun/internal/runner"
	"github.com/homerun-ci/homerun/internal/store"
	"github.com/homerun-ci/homerun/internal/syncbrain"
)

// IdleRotate is how long a listener waits for work before releasing its
// concurrency slot to another linked target.
//
// WHY THIS EXISTS: an ephemeral runner registration serves exactly one job, so
// a listener occupies a slot while idle. With max_concurrent_jobs = 1 and three
// linked repos, repo #2 and #3 would never be covered. Rotation gives every
// target a fair share of the available slots. The cost is that a job pushed
// while its repo is not currently being listened to waits up to one rotation
// period - which is why `homerun status` prints the current rotation set.
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

	// Paused gates all job pickup (homerun pause/resume).
	paused atomic.Bool

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
	d.logf("homerun daemon starting (home=%s)", d.HomeDir)

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

	<-ctx.Done()
	d.logf("shutting down; releasing sleep assertion")
	d.Power.ReleaseAll()
	wg.Wait()
	return nil
}

// SetTargets replaces the served target list (called after `homerun link`).
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

		st := power.Read(ctx)
		d.mu.Lock()
		d.lastPower = st
		d.mu.Unlock()

		// Hold a sleep assertion whenever there is work AND we are on AC.
		// Holding it on battery would drain the machine to 0% and is exactly
		// the behaviour a laptop user would (rightly) consider hostile.
		stats, err := d.DB.Stats(ctx)
		hasWork := err == nil && (stats.Queued > 0 || stats.Running > 0)

		d.mu.Lock()
		holding := d.releaseSleep != nil
		d.mu.Unlock()

		want := d.Config.Power.HoldSleepAssertion && hasWork && st.OnAC()
		switch {
		case want && !holding:
			rel, err := d.Power.Hold("Homerun is running CI jobs")
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
	}
}

// reloadConfigIfChanged implements hot reload: `homerun limit --cpus 4` takes
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

	if d.Docker == nil {
		dk, err := runner.NewDocker()
		if err != nil {
			d.logf("slot %d: %v", slot, err)
			sleepCtx(ctx, 60*time.Second)
			return
		}
		d.Docker = dk
	}

	eng := &runner.Engine{
		Docker:  d.Docker,
		Cache:   d.Cache,
		Config:  d.Config,
		HomeDir: d.HomeDir,
		Host:    "github.com",
		Minter:  client,
		DB:      d.DB,
	}

	// Rotation: if this listener sits idle it must release the slot so other
	// linked repos get covered. A listener that HAS picked up a job is never
	// rotated - OnJobStart cancels the timer.
	listenCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var started atomic.Bool
	eng.OnJobStart = func(jobName string) {
		started.Store(true)
		d.logf("slot %d: %s picked up job %q", slot, t.Slug, jobName)
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

	out, runErr := eng.RunOne(listenCtx, t, nil)

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

	res, err := d.runOfflineJob(ctx, job)
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

// TargetsFromConfig converts linked repos into runner targets.
func TargetsFromConfig(cfg *config.Config) []runner.Target {
	out := make([]runner.Target, 0, len(cfg.Repos))
	for _, r := range cfg.Repos {
		lbls := r.Labels
		if len(lbls) == 0 {
			lbls = labels.Default()
		}
		out = append(out, runner.Target{
			Slug:        r.Slug,
			Scope:       r.Scope,
			Profile:     r.Profile,
			RunnerGroup: r.RunnerGroup,
			Labels:      lbls,
		})
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
		return nil, fmt.Errorf("no authenticated client for profile %q (run `homerun auth add %s`)", profile, profile)
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
