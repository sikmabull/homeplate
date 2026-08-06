package cli

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/homeplate-ci/homeplate/internal/auth"
	"github.com/homeplate-ci/homeplate/internal/config"
	"github.com/homeplate-ci/homeplate/internal/connectivity"
	"github.com/homeplate-ci/homeplate/internal/ghapi"
	"github.com/homeplate-ci/homeplate/internal/keyring"
	"github.com/homeplate-ci/homeplate/internal/labels"
	"github.com/homeplate-ci/homeplate/internal/offline"
	"github.com/homeplate-ci/homeplate/internal/power"
	"github.com/homeplate-ci/homeplate/internal/runner"
	"github.com/homeplate-ci/homeplate/internal/service"
)

func newLimitCmd() *cobra.Command {
	var (
		cpus          float64
		memory        string
		diskGB        int
		concurrency   int
		timeout       string
		battPct       int
		onBattery     string
		holdSleep     string
		onlyIdle      string
		idleCPUPct    int
		clamshell     string
		clamshellBatt string
		autoRelink    string
		watchClones   string
	)
	cmd := &cobra.Command{
		Use:   "limit",
		Short: "Show or change resource caps (hot-reloaded, no restart needed)",
		Long: `Resource caps apply to every job.

  homeplate limit                            show current caps
  homeplate limit --cpus 4 --memory 8g       change them (takes effect immediately)

Linux jobs get HARD caps via docker --cpus/--memory.
Native macOS jobs get scheduling-priority hints only; see README "Known limits".`,
		RunE: func(c *cobra.Command, args []string) error {
			cfg, err := mustConfig()
			if err != nil {
				return err
			}
			changed := false

			if c.Flags().Changed("cpus") {
				if cpus <= 0 {
					return fmt.Errorf("--cpus must be > 0")
				}
				host := float64(runtime.NumCPU())
				if cpus > host {
					fmt.Printf("note: --cpus %g exceeds this machine's %d cores; Docker will not "+
						"grant more than the host has\n", cpus, runtime.NumCPU())
				}
				cfg.Limits.MaxCPUs = cpus
				changed = true
			}
			if c.Flags().Changed("memory") {
				b, err := config.ParseMemory(memory)
				if err != nil {
					return err
				}
				if b > config.HostMemoryBytes() {
					return fmt.Errorf("--memory %s exceeds this machine's total RAM (%s)",
						memory, config.FormatBytes(config.HostMemoryBytes()))
				}
				cfg.Limits.MaxMemory = memory
				changed = true
			}
			if c.Flags().Changed("disk") {
				cfg.Limits.MaxDiskGB = diskGB
				changed = true
			}
			if c.Flags().Changed("concurrency") {
				if concurrency < 1 {
					return fmt.Errorf("--concurrency must be >= 1")
				}
				cfg.Limits.MaxConcurrentJobs = concurrency
				changed = true
			}
			if c.Flags().Changed("timeout") {
				d, err := time.ParseDuration(timeout)
				if err != nil {
					return fmt.Errorf("--timeout: %w", err)
				}
				cfg.Limits.JobTimeout = config.Duration{Duration: d}
				changed = true
			}
			if c.Flags().Changed("battery-pause") {
				cfg.Power.PauseBelowBatteryPct = battPct
				changed = true
			}
			if c.Flags().Changed("on-battery") {
				v, err := parseBool(onBattery)
				if err != nil {
					return err
				}
				cfg.Power.RunOnBattery = v
				changed = true
			}
			if c.Flags().Changed("hold-sleep") {
				v, err := parseBool(holdSleep)
				if err != nil {
					return err
				}
				cfg.Power.HoldSleepAssertion = v
				changed = true
			}
			if c.Flags().Changed("only-when-idle") {
				v, err := parseBool(onlyIdle)
				if err != nil {
					return err
				}
				cfg.Power.OnlyWhenIdle = v
				changed = true
			}
			if c.Flags().Changed("idle-cpu-pct") {
				if idleCPUPct < 1 || idleCPUPct > 100 {
					return fmt.Errorf("--idle-cpu-pct must be between 1 and 100")
				}
				cfg.Power.IdleUserCPUPercent = idleCPUPct
				changed = true
			}
			if c.Flags().Changed("clamshell") {
				v, err := parseBool(clamshell)
				if err != nil {
					return err
				}
				cfg.Power.AllowClamshellPmset = v
				changed = true
				if v {
					fmt.Println("note: the lid-close toggle needs a one-time root helper.")
					fmt.Println("      Run `homeplate power setup` (it re-execs via sudo) before")
					fmt.Println("      relying on it, or the daemon will log a permission error.")
				}
			}
			if c.Flags().Changed("clamshell-on-battery") {
				v, err := parseBool(clamshellBatt)
				if err != nil {
					return err
				}
				cfg.Power.ClamshellOnBattery = v
				changed = true
			}
			if c.Flags().Changed("auto-relink") {
				v, err := parseBool(autoRelink)
				if err != nil {
					return err
				}
				cfg.Sync.AutoRelink = v
				changed = true
			}
			if c.Flags().Changed("watch-clones") {
				v, err := parseBool(watchClones)
				if err != nil {
					return err
				}
				cfg.Sync.WatchLocalClones = v
				changed = true
			}

			if changed {
				if err := cfg.Save(); err != nil {
					return err
				}
				fmt.Println("Updated. The daemon hot-reloads within ~10s; no restart needed.")
			}

			fmt.Printf("\n  max_cpus              %g   (host has %d cores)\n", cfg.Limits.MaxCPUs, runtime.NumCPU())
			fmt.Printf("  max_memory            %s   (host has %s)\n", cfg.Limits.MaxMemory,
				config.FormatBytes(config.HostMemoryBytes()))
			fmt.Printf("  max_disk_gb           %d\n", cfg.Limits.MaxDiskGB)
			fmt.Printf("  max_concurrent_jobs   %d\n", cfg.Limits.MaxConcurrentJobs)
			fmt.Printf("  job_timeout           %s\n", cfg.Limits.JobTimeout)
			fmt.Printf("\n  pause_below_battery   %d%%\n", cfg.Power.PauseBelowBatteryPct)
			fmt.Printf("  run_on_battery        %t\n", cfg.Power.RunOnBattery)
			fmt.Printf("  hold_sleep_assertion  %t\n", cfg.Power.HoldSleepAssertion)
			fmt.Printf("  only_when_idle        %t\n", cfg.Power.OnlyWhenIdle)
			fmt.Printf("  idle_user_cpu_percent %d%%   (sustained for %s)\n",
				cfg.Power.IdleUserCPUPercent, cfg.Power.IdleSustainedFor)
			fmt.Printf("  allow_clamshell_pmset %t\n", cfg.Power.AllowClamshellPmset)
			fmt.Printf("  clamshell_on_battery  %t   (floor %d%%)\n",
				cfg.Power.ClamshellOnBattery, cfg.Power.ClamshellBatteryFloorPct)
			fmt.Printf("\n  auto_relink           %t\n", cfg.Sync.AutoRelink)
			fmt.Printf("  watch_local_clones    %t\n", cfg.Sync.WatchLocalClones)
			fmt.Printf("\n  config: %s\n\n", config.Path())
			return nil
		},
	}
	cmd.Flags().Float64Var(&cpus, "cpus", 0, "CPU cores per job (docker --cpus)")
	cmd.Flags().StringVar(&memory, "memory", "", "memory per job, e.g. 8g")
	cmd.Flags().IntVar(&diskGB, "disk", 0, "disk cap in GB")
	cmd.Flags().IntVar(&concurrency, "concurrency", 0, "max concurrent jobs")
	cmd.Flags().StringVar(&timeout, "timeout", "", "hard per-job timeout, e.g. 2h")
	cmd.Flags().IntVar(&battPct, "battery-pause", 0, "pause pickup below this battery %% when unplugged")
	cmd.Flags().StringVar(&onBattery, "on-battery", "", "run jobs while unplugged (true/false)")
	cmd.Flags().StringVar(&holdSleep, "hold-sleep", "", "hold a sleep assertion while working (true/false)")
	cmd.Flags().StringVar(&onlyIdle, "only-when-idle", "", "pause job pickup while you are actively using the machine (true/false)")
	cmd.Flags().IntVar(&idleCPUPct, "idle-cpu-pct", 0, "user CPU %% that counts as 'you are busy' (with --only-when-idle)")
	cmd.Flags().StringVar(&clamshell, "clamshell", "", "managed lid-close toggle via pmset (true/false; run `homeplate power setup` first)")
	cmd.Flags().StringVar(&clamshellBatt, "clamshell-on-battery", "", "allow the lid-close toggle on battery too (true/false)")
	cmd.Flags().StringVar(&autoRelink, "auto-relink", "", "auto-repair renamed/transferred repo links (true/false)")
	cmd.Flags().StringVar(&watchClones, "watch-clones", "", "run never-pushed commits from local clones while offline (true/false)")
	return cmd
}

func parseBool(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "yes", "1", "on":
		return true, nil
	case "false", "no", "0", "off":
		return false, nil
	}
	return false, fmt.Errorf("expected true or false, got %q", s)
}

// ---------------- doctor ----------------

type checkResult struct {
	Name   string
	OK     bool
	Warn   bool
	Detail string
	Fix    string
}

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose Docker, power, connectivity, credentials, and label routing",
		RunE: func(c *cobra.Command, args []string) error {
			ctx, cancel := signalContext()
			defer cancel()
			return runDoctor(ctx)
		},
	}
}

func runDoctor(ctx context.Context) error {
	cfg, err := mustConfig()
	if err != nil {
		return err
	}
	var results []checkResult

	// --- container runtime ---
	if d, err := runner.NewDocker(); err != nil {
		results = append(results, checkResult{
			Name: "container runtime", Detail: err.Error(),
			Fix: "brew install --cask docker  (or `brew install colima && colima start`)",
		})
	} else if err := d.Available(ctx); err != nil {
		results = append(results, checkResult{
			Name: "container runtime", Detail: err.Error(),
			Fix: "start Docker Desktop, or run `colima start`",
		})
	} else {
		results = append(results, checkResult{
			Name: "container runtime", OK: true,
			Detail: fmt.Sprintf("%s, server %s", d.Bin, d.ServerVersion(ctx)),
		})
	}

	// --- act (Engine B) ---
	if a, err := offline.Find(); err != nil {
		results = append(results, checkResult{
			Name: "nektos/act (offline mode)", Warn: true, Detail: "not installed",
			Fix: "brew install act   (only needed for offline/degraded mode)",
		})
	} else {
		results = append(results, checkResult{
			Name: "nektos/act (offline mode)", OK: true, Detail: "act " + a.Version,
		})
	}

	// --- git ---
	if _, err := exec.LookPath("git"); err != nil {
		results = append(results, checkResult{Name: "git", Detail: "not found", Fix: "xcode-select --install"})
	} else {
		results = append(results, checkResult{Name: "git", OK: true, Detail: "found"})
	}

	// --- credential store ---
	if ring, err := keyring.Open(); err != nil {
		results = append(results, checkResult{Name: "credential store", Detail: err.Error()})
	} else {
		warn := strings.Contains(ring.Name(), "PLAINTEXT")
		results = append(results, checkResult{
			Name: "credential store", OK: !warn, Warn: warn, Detail: ring.Name(),
			Fix: map[bool]string{true: "unset HOMEPLATE_KEYRING to use the OS keychain"}[warn],
		})
	}

	// --- identities ---
	authStore, aerr := auth.OpenStore()
	if aerr != nil {
		results = append(results, checkResult{Name: "identities", Detail: aerr.Error()})
	} else {
		profiles := authStore.List()
		if len(profiles) == 0 {
			results = append(results, checkResult{
				Name: "identities", Detail: "none configured", Fix: "homeplate auth add personal",
			})
		} else {
			var names []string
			bad := 0
			for _, p := range profiles {
				names = append(names, p.Name)
				if _, err := authStore.Token(p.Name); err != nil {
					bad++
				}
			}
			results = append(results, checkResult{
				Name: "identities", OK: bad == 0, Detail: strings.Join(names, ", "),
				Fix: map[bool]string{true: "re-run `homeplate auth add <profile>`"}[bad > 0],
			})
		}
	}

	// --- connectivity ---
	mon := connectivity.NewMonitor()
	cs := mon.Check(ctx)
	results = append(results, checkResult{
		Name: "GitHub connectivity", OK: cs.State == connectivity.Online,
		Warn:   cs.State == connectivity.Degraded,
		Detail: cs.Reason,
		Fix:    map[bool]string{true: "Homeplate will run jobs offline via act and replay results when Actions recovers"}[cs.UseOffline()],
	})

	// --- power / clamshell ---
	rep := power.Describe(ctx, cfg.Power.HoldSleepAssertion)
	results = append(results, checkResult{
		Name: "power", OK: rep.State.OnAC(), Warn: !rep.State.OnAC(),
		Detail: power.FormatState(rep.State),
	})
	results = append(results, checkResult{
		Name: "lid-closed operation", OK: rep.Clamshell.WillKeepRunning, Warn: !rep.Clamshell.WillKeepRunning,
		Detail: rep.Clamshell.Reason, Fix: rep.Clamshell.Remedy,
	})

	// --- daemon ---
	if inst, err := service.New(config.Dir()); err == nil {
		installed, running, detail := inst.Status()
		results = append(results, checkResult{
			Name: "daemon", OK: installed && running, Warn: installed && !running, Detail: detail,
			Fix: map[bool]string{true: "homeplate service install"}[!installed],
		})
	}

	// --- label routing (the big one) ---
	results = append(results, routingCheck(ctx, cfg)...)

	// --- print ---
	fmt.Println()
	fails, warns := 0, 0
	for _, r := range results {
		mark := "FAIL"
		switch {
		case r.OK:
			mark = " ok "
		case r.Warn:
			mark = "warn"
			warns++
		default:
			fails++
		}
		fmt.Printf("  [%s]  %-26s %s\n", mark, r.Name, r.Detail)
		if r.Fix != "" && !r.OK {
			fmt.Printf("           %-26s -> %s\n", "", r.Fix)
		}
	}
	fmt.Println()
	switch {
	case fails > 0:
		fmt.Printf("%d problem(s), %d warning(s). Homeplate will not work correctly until the problems are fixed.\n\n", fails, warns)
	case warns > 0:
		fmt.Printf("Ready, with %d warning(s).\n\n", warns)
	default:
		fmt.Println("Everything checks out.")
		fmt.Println()
	}
	return nil
}

// routingCheck inspects each linked repo's workflows and reports honestly
// whether jobs will actually land on this machine.
func routingCheck(ctx context.Context, cfg *config.Config) []checkResult {
	var out []checkResult
	if len(cfg.Repos) == 0 {
		return []checkResult{{
			Name: "label routing", Warn: true, Detail: "no repos linked yet", Fix: "homeplate link",
		}}
	}

	myLabels := append(labels.Default(), labels.SelfHosted, "linux", "x64", runtime.GOARCH)

	for _, r := range cfg.Repos {
		if r.Scope != "repo" {
			continue
		}
		client, _, err := clientFor(r.Profile)
		if err != nil {
			out = append(out, checkResult{Name: "routing " + r.Slug, Warn: true, Detail: err.Error()})
			continue
		}
		files, err := client.ListWorkflowFiles(ctx, r.Slug, "")
		if err != nil {
			out = append(out, checkResult{
				Name: "routing " + r.Slug, Warn: true,
				Detail: "could not read .github/workflows: " + err.Error(),
			})
			continue
		}
		if len(files) == 0 {
			out = append(out, checkResult{Name: "routing " + r.Slug, Warn: true, Detail: "no workflows found"})
			continue
		}

		var allTargets []string
		for _, f := range files {
			content, err := fetchWorkflow(ctx, client, r.Slug, f.Path)
			if err != nil {
				continue
			}
			allTargets = append(allTargets, labels.ScanRunsOn(content)...)
		}
		advice := labels.Advise(labels.Dedupe(allTargets), myLabels)
		out = append(out, checkResult{
			Name:   "routing " + truncStr(r.Slug, 18),
			OK:     advice.Interceptable,
			Warn:   !advice.Interceptable,
			Detail: advice.Explanation,
			Fix:    advice.NextStep,
		})
	}
	return out
}

// fetchWorkflow downloads a workflow file's contents.
func fetchWorkflow(ctx context.Context, client *ghapi.Client, slug, path string) (string, error) {
	owner, name, err := ghapi.SplitSlug(slug)
	if err != nil {
		return "", err
	}
	var out struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if _, err := client.Get(ctx, fmt.Sprintf("/repos/%s/%s/contents/%s", owner, name, path), &out); err != nil {
		return "", err
	}
	return decodeBase64Content(out.Content, out.Encoding)
}
