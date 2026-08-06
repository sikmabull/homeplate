package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/homeplate-ci/homeplate/internal/auth"
	"github.com/homeplate-ci/homeplate/internal/config"
	"github.com/homeplate-ci/homeplate/internal/connectivity"
	"github.com/homeplate-ci/homeplate/internal/daemon"
	"github.com/homeplate-ci/homeplate/internal/power"
	"github.com/homeplate-ci/homeplate/internal/runner"
	"github.com/homeplate-ci/homeplate/internal/savings"
	"github.com/homeplate-ci/homeplate/internal/service"
	"github.com/homeplate-ci/homeplate/internal/store"
)

func newStatusCmd() *cobra.Command {
	var explainSavings bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show linked repos, queue, resource caps, sleep state, and dollars saved",
		RunE: func(c *cobra.Command, args []string) error {
			ctx, cancel := signalContext()
			defer cancel()
			return runStatus(ctx, explainSavings)
		},
	}
	cmd.Flags().BoolVar(&explainSavings, "explain-savings", false, "show the full savings calculation and its assumptions")
	return cmd
}

func runStatus(ctx context.Context, explain bool) error {
	cfg, err := mustConfig()
	if err != nil {
		return err
	}
	db, err := openStore()
	if err != nil {
		return err
	}
	defer db.Close()

	stats, err := db.Stats(ctx)
	if err != nil {
		return err
	}

	calc := savings.New(cfg.Savings)
	// Wire repo visibility into the savings counter: public repos are exempt
	// from GitHub's self-hosted control-plane fee; anything not explicitly
	// marked public is treated as private (fee applies) - conservative.
	savings.IsPublicRepo = func(slug string) bool {
		for _, r := range cfg.Repos {
			if r.Slug == slug {
				return r.Public
			}
		}
		return false
	}
	from, to := savings.MonthToDate(time.Now())
	jobs, err := db.JobsSince(ctx, from)
	if err != nil {
		return err
	}
	sum := calc.Compute(jobs, from, to)

	if flagJSON {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{
			"queued": stats.Queued, "running": stats.Running,
			"succeeded_today": stats.SucceededToday, "failed_today": stats.FailedToday,
			"pending_replay": stats.PendingReplay,
			"savings_usd":    sum.USD, "savings_minutes": sum.Minutes, "savings_jobs": sum.Jobs,
			"limits": cfg.Limits,
		})
	}

	// ---- headline ----
	fmt.Println()
	fmt.Println("  " + sum.Format())
	fmt.Println()

	// ---- identities and links ----
	authStore, err := auth.OpenStore()
	if err == nil {
		profiles := authStore.List()
		if len(profiles) == 0 {
			fmt.Println("IDENTITIES   none (run `homeplate auth add personal`)")
		} else {
			fmt.Println("IDENTITIES")
			for _, p := range profiles {
				linked := cfg.ReposForProfile(p.Name)
				fmt.Printf("  %-12s @%-18s %d linked\n", p.Name, p.Login, len(linked))
				for _, r := range linked {
					vis := ""
					if r.Public {
						vis = "  [PUBLIC]"
					}
					fmt.Printf("      %-42s %s%s\n", r.Slug, r.Scope, vis)
				}
			}
		}
	}

	// ---- queue ----
	fmt.Println()
	fmt.Println("QUEUE")
	fmt.Printf("  running %d   queued %d   today: %d passed / %d failed\n",
		stats.Running, stats.Queued, stats.SucceededToday, stats.FailedToday)
	if stats.PendingReplay > 0 {
		fmt.Printf("  %d offline result(s) waiting to be pushed to GitHub on reconnect\n", stats.PendingReplay)
	}

	// ---- resource caps ----
	fmt.Println()
	fmt.Println("RESOURCE CAPS")
	fmt.Printf("  cpus %-6g memory %-8s disk %dGB   concurrency %d   timeout %s\n",
		cfg.Limits.MaxCPUs, cfg.Limits.MaxMemory, cfg.Limits.MaxDiskGB,
		cfg.Limits.MaxConcurrentJobs, cfg.Limits.JobTimeout)
	dockerEnf := runner.DockerEnforcement(cfg.Limits.MaxCPUs, cfg.Limits.MaxMemory, cfg.Limits.MaxDiskGB)
	fmt.Printf("  linux jobs : HARD  (%s)\n", dockerEnf.Mechanism)
	nativeEnf := runner.NativeEnforcement()
	fmt.Printf("  macos jobs : SOFT  (%s)\n", nativeEnf.Mechanism)
	fmt.Printf("               %s\n", nativeEnf.Caveat)

	// ---- power / sleep ----
	fmt.Println()
	fmt.Println("POWER")
	rep := power.Describe(ctx, cfg.Power.HoldSleepAssertion)
	fmt.Printf("  %s\n", power.FormatState(rep.State))
	pol := power.Policy{PauseBelowPct: cfg.Power.PauseBelowBatteryPct, RunOnBattery: cfg.Power.RunOnBattery}
	dec := pol.Evaluate(rep.State)
	verb := "accepting jobs"
	if !dec.Allow {
		verb = "PAUSED"
	}
	fmt.Printf("  %s: %s\n", verb, dec.Reason)
	fmt.Printf("  sleep suppression: %s\n", rep.Assertion)
	fmt.Printf("  will keep running with lid closed: %s\n", rep.Clamshell.String())

	// ---- connectivity / engine ----
	fmt.Println()
	fmt.Println("CONNECTIVITY")
	mon := connectivity.NewMonitor()
	cs := mon.Check(ctx)
	engine := "A (connected: GitHub dispatches to this machine)"
	if cs.UseOffline() {
		engine = "B (offline: nektos/act, results replayed on reconnect)"
	}
	fmt.Printf("  %s - %s\n", strings.ToUpper(string(cs.State)), cs.Reason)
	fmt.Printf("  active engine: %s\n", engine)

	// ---- daemon ----
	fmt.Println()
	fmt.Println("DAEMON")
	if inst, err := service.New(config.Dir()); err == nil {
		installed, running, detail := inst.Status()
		switch {
		case !installed:
			fmt.Printf("  not installed - %s\n", detail)
		case running:
			fmt.Printf("  running - %s\n", detail)
		default:
			fmt.Printf("  installed but NOT running - %s\n", detail)
		}
	}
	if _, err := os.Stat(daemon.PauseFile(config.Dir())); err == nil {
		fmt.Println("  PAUSED by `homeplate pause` - run `homeplate resume` to accept jobs again")
	}

	// ---- savings detail ----
	if explain {
		fmt.Println()
		fmt.Println(sum.Explain())
	} else {
		fmt.Println()
		fmt.Printf("  (run `homeplate status --explain-savings` to audit the $%.2f figure)\n", sum.USD)
	}
	fmt.Println()
	return nil
}

func newPauseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pause",
		Short: "Stop picking up new jobs (running jobs finish)",
		RunE: func(c *cobra.Command, args []string) error {
			p := daemon.PauseFile(config.Dir())
			if err := os.MkdirAll(config.Dir(), 0o700); err != nil {
				return err
			}
			if err := os.WriteFile(p, []byte(time.Now().Format(time.RFC3339)), 0o600); err != nil {
				return err
			}
			fmt.Println("Paused. Jobs already running will finish; no new jobs will be picked up.")
			fmt.Println("Resume with: homeplate resume")
			return nil
		},
	}
}

func newResumeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "resume",
		Short: "Resume picking up jobs",
		RunE: func(c *cobra.Command, args []string) error {
			if err := os.Remove(daemon.PauseFile(config.Dir())); err != nil && !os.IsNotExist(err) {
				return err
			}
			fmt.Println("Resumed. The daemon will pick up jobs again within a few seconds.")
			return nil
		},
	}
}

func newLogsCmd() *cobra.Command {
	var follow bool
	var limit int

	// followDaemonLog implements `logs --follow`: tail -f on the service log,
	// falling back to the most recent log file when the daemon has not
	// written one yet. Kept as a closure so the command stays self-contained.
	followDaemonLog := func(ctx context.Context) error {
		logDir := config.Dir() + string(os.PathSeparator) + "logs"
		path := logDir + string(os.PathSeparator) + "daemon.out.log"
		if _, err := os.Stat(path); err != nil {
			// Fall back to the most recently modified *.log in the log dir.
			entries, derr := os.ReadDir(logDir)
			if derr != nil {
				return fmt.Errorf("no log directory at %s (is the daemon installed?)", logDir)
			}
			best := ""
			var bestMod time.Time
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".log") {
					continue
				}
				info, ierr := e.Info()
				if ierr != nil {
					continue
				}
				if best == "" || info.ModTime().After(bestMod) {
					best, bestMod = e.Name(), info.ModTime()
				}
			}
			if best == "" {
				return fmt.Errorf("no log files in %s yet", logDir)
			}
			path = logDir + string(os.PathSeparator) + best
		}

		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()

		// Print the last few lines for context, like `tail -f` does.
		const tailBytes = 8 << 10
		if st, err := f.Stat(); err == nil && st.Size() > tailBytes {
			if _, err := f.Seek(-tailBytes, 2); err == nil {
				chunk := make([]byte, tailBytes)
				n, _ := f.Read(chunk)
				// Drop the first, likely partial, line.
				if idx := strings.Index(string(chunk[:n]), "\n"); idx >= 0 {
					os.Stdout.Write(chunk[idx+1 : n])
				}
			}
		} else {
			if _, err := f.Seek(0, 0); err == nil {
				chunk := make([]byte, tailBytes)
				n, _ := f.Read(chunk)
				os.Stdout.Write(chunk[:n])
			}
		}
		fmt.Fprintf(os.Stderr, "==> following %s (Ctrl-C to stop)\n", path)

		buf := make([]byte, 32<<10)
		for {
			n, _ := f.Read(buf)
			if n > 0 {
				os.Stdout.Write(buf[:n])
				continue
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(500 * time.Millisecond):
			}
		}
	}

	cmd := &cobra.Command{
		Use:   "logs [job-id]",
		Short: "Show recent jobs, or one job's full log",
		RunE: func(c *cobra.Command, args []string) error {
			ctx, cancel := signalContext()
			defer cancel()
			if follow {
				return followDaemonLog(ctx)
			}
			db, err := openStore()
			if err != nil {
				return err
			}
			defer db.Close()

			if len(args) == 0 {
				jobs, err := db.RecentJobs(ctx, limit)
				if err != nil {
					return err
				}
				if len(jobs) == 0 {
					fmt.Println("No jobs yet.")
					return nil
				}
				fmt.Printf("%-5s %-9s %-28s %-14s %-10s %8s\n", "ID", "ENGINE", "REPO", "JOB", "STATE", "TIME")
				for _, j := range jobs {
					dur := time.Duration(j.BillableSeconds) * time.Second
					fmt.Printf("%-5d %-9s %-28s %-14s %-10s %8s\n",
						j.ID, j.Engine, truncStr(j.RepoSlug, 28), truncStr(j.JobName, 14), j.State, dur.Round(time.Second))
				}
				fmt.Println("\nFull log:  homeplate logs <id>")
				return nil
			}

			var id int64
			if _, err := fmt.Sscanf(args[0], "%d", &id); err != nil {
				return fmt.Errorf("invalid job id %q", args[0])
			}
			job, err := db.Job(ctx, id)
			if err != nil {
				return fmt.Errorf("no job %d", id)
			}
			fmt.Printf("Job %d  %s  %s  %s  (engine %s)\n", job.ID, job.RepoSlug, job.JobName, job.State, job.Engine)
			if job.Reason != "" {
				fmt.Printf("Reason: %s\n", job.Reason)
			}
			if job.LogPath == "" {
				fmt.Println("(no log file recorded)")
				return nil
			}
			b, err := os.ReadFile(job.LogPath)
			if err != nil {
				return fmt.Errorf("log file %s: %w", job.LogPath, err)
			}
			fmt.Println(strings.Repeat("-", 70))
			os.Stdout.Write(b)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "follow the daemon log")
	cmd.Flags().IntVarP(&limit, "limit", "n", 20, "how many recent jobs to list")
	return cmd
}

func truncStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}

// jobStateSymbol renders a compact state marker.
func jobStateSymbol(s store.JobState) string {
	switch s {
	case store.StateSucceeded:
		return "pass"
	case store.StateFailed:
		return "FAIL"
	case store.StateInterrupted:
		return "intr"
	default:
		return string(s)
	}
}
