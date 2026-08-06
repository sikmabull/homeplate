package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/homeplate-ci/homeplate/internal/config"
	"github.com/homeplate-ci/homeplate/internal/ghapi"
	"github.com/homeplate-ci/homeplate/internal/offline"
	"github.com/homeplate-ci/homeplate/internal/repofind"
	"github.com/homeplate-ci/homeplate/internal/store"
)

// ghNew is a tiny indirection so tests can stub client construction.
var ghNew = ghapi.New

func newRunCmd() *cobra.Command {
	var (
		repoPath  string
		workflow  string
		job       string
		event     string
		queueOnly bool
	)
	cmd := &cobra.Command{
		Use:   "run [owner/repo]",
		Short: "Run a workflow locally right now (Engine B / offline mode)",
		Long: `Runs a workflow on this machine with nektos/act, without waiting for GitHub
to dispatch it. Results are stored in the local queue and replayed to GitHub as
commit statuses when connectivity returns.

This is what powers the offline demo: commit while unplugged, run the job
locally, reconnect, and watch the check appear.

Engine B is BETA - act is not byte-identical to GitHub's runner. See README.`,
		RunE: func(c *cobra.Command, args []string) error {
			ctx, cancel := signalContext()
			defer cancel()

			cfg, err := mustConfig()
			if err != nil {
				return err
			}
			db, err := openStore()
			if err != nil {
				return err
			}
			defer db.Close()

			// Determine the repo slug and commit to run against.
			slug := ""
			if len(args) > 0 {
				slug = args[0]
			}
			if repoPath == "" {
				repoPath = "."
			}
			abs, err := filepath.Abs(repoPath)
			if err != nil {
				return err
			}
			if slug == "" {
				slug, err = inferSlug(ctx, abs)
				if err != nil {
					return fmt.Errorf("could not infer the repo: %w (pass owner/repo explicitly)", err)
				}
			}

			sha, err := gitOutput(ctx, abs, "rev-parse", "HEAD")
			if err != nil {
				return fmt.Errorf("%s is not a git repo: %w", abs, err)
			}
			branch, _ := gitOutput(ctx, abs, "rev-parse", "--abbrev-ref", "HEAD")

			// Feed the local working clone into the mirror so the commit is
			// available even though it was never pushed.
			if _, err := offline.LocalRepoMirror(ctx, config.Dir(), slug, abs); err != nil {
				fmt.Printf("note: could not mirror the local clone (%v); continuing\n", err)
			}

			profile := flagProfile
			if profile == "" {
				if _, p, err := clientFor(""); err == nil {
					profile = p.Name
				}
			}

			extID := fmt.Sprintf("%s|%s|%s|%s|%s", store.EngineB, slug, sha, workflow, job)
			machineID, _ := db.GetMeta(ctx, "machine_id")
			j := &store.Job{
				ExternalID:  extID,
				Profile:     profile,
				RepoSlug:    slug,
				Engine:      store.EngineB,
				Workflow:    workflow,
				JobName:     job,
				CommitSHA:   sha,
				Ref:         "refs/heads/" + branch,
				EventName:   event,
				State:       store.StateQueued,
				RunnerClass: "linux",
				MachineID:   machineID,
				QueuedAt:    time.Now().UTC(),
			}
			created, err := db.EnqueueJob(ctx, j)
			if err != nil {
				return err
			}
			if !created {
				fmt.Printf("This exact run is already in the queue as job %d (state: %s).\n", j.ID, j.State)
				if j.State.Terminal() {
					fmt.Println("Re-running the same commit+workflow is deduplicated. Use `homeplate logs`.")
					return nil
				}
			}
			fmt.Printf("Queued job %d: %s @ %s\n", j.ID, slug, short(sha))

			if queueOnly {
				fmt.Println("Queued only (--queue-only). The daemon will pick it up.")
				return nil
			}

			// Run it right here so the user sees live output.
			act, err := offline.Find()
			if err != nil {
				return err
			}
			claimed, err := db.NextQueued(ctx, store.EngineB)
			if err != nil {
				return err
			}
			if claimed == nil || claimed.ID != j.ID {
				fmt.Println("The daemon claimed this job first; follow it with `homeplate logs`.")
				return nil
			}

			logPath := filepath.Join(config.Dir(), "logs", fmt.Sprintf("offline-%d.log", j.ID))
			opts := offline.RunOpts{
				WorkDir:  abs,
				Workflow: workflow,
				Job:      job,
				Image:    cfg.Engine.ActImage,
				Limits:   cfg.Limits,
				Event: offline.Event{
					Name: event, SHA: sha, Ref: "refs/heads/" + branch,
					Repository: slug, Actor: "homeplate", Branch: branch,
				},
				ArtifactDir: filepath.Join(config.Dir(), "artifacts", fmt.Sprint(j.ID)),
			}
			fmt.Printf("Running with act (caps: %g cpus, %s memory)...\n\n",
				cfg.Limits.MaxCPUs, cfg.Limits.MaxMemory)

			res, runErr := act.Run(ctx, opts, logPath, os.Stdout)

			state := store.StateFailed
			exit, reason := 1, ""
			if runErr != nil {
				reason = runErr.Error()
			} else if res != nil {
				exit = res.ExitCode
				if res.Succeeded {
					state = store.StateSucceeded
				}
				for i := range res.Steps {
					res.Steps[i].JobID = j.ID
					_ = db.AddStep(ctx, &res.Steps[i])
				}
			}
			if _, err := db.SQL().ExecContext(ctx, `UPDATE jobs SET log_path=? WHERE id=?`, logPath, j.ID); err != nil {
				return err
			}
			// act discovers the job id(s) from the workflow; record them so
			// `homeplate logs` and the replayed status context are meaningful.
			if res != nil && len(res.JobNames) > 0 && job == "" {
				if _, err := db.SQL().ExecContext(ctx, `UPDATE jobs SET job_name=? WHERE id=?`,
					strings.Join(res.JobNames, ","), j.ID); err != nil {
					return err
				}
			}
			if err := db.FinishJob(ctx, j.ID, state, exit, reason); err != nil {
				return err
			}

			fmt.Printf("\nJob %d: %s\n", j.ID, state)
			fmt.Println("Result stored locally. It will be posted to GitHub as a commit status")
			fmt.Println("on the next sync, clearly labelled as having run locally via Homeplate.")
			return nil
		},
	}
	cmd.Flags().StringVar(&repoPath, "repo-path", ".", "path to the local git working copy")
	cmd.Flags().StringVarP(&workflow, "workflow", "W", "", "workflow file (e.g. .github/workflows/ci.yml)")
	cmd.Flags().StringVarP(&job, "job", "j", "", "job id to run")
	cmd.Flags().StringVar(&event, "event", "push", "event name to simulate")
	cmd.Flags().BoolVar(&queueOnly, "queue-only", false, "queue without running now")
	return cmd
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	full := append([]string{"-C", dir}, args...)
	out, err := exec.CommandContext(ctx, "git", full...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// inferSlug derives owner/repo from the origin remote. Host-agnostic:
// github.com and GHES remotes parse identically.
func inferSlug(ctx context.Context, dir string) (string, error) {
	url, err := gitOutput(ctx, dir, "remote", "get-url", "origin")
	if err != nil {
		return "", err
	}
	_, slug, ok := repofind.ParseRemoteURL(url)
	if !ok {
		return "", fmt.Errorf("origin %q is not a recognisable GitHub/GHES URL", url)
	}
	return slug, nil
}

func short(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}
