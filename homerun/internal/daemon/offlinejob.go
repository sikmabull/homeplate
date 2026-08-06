package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/homerun-ci/homerun/internal/offline"
	"github.com/homerun-ci/homerun/internal/store"
)

// runOfflineJob materialises a working tree from the local mirror and runs the
// workflow with act.
//
// The whole point is that this works with NO network: the mirror already holds
// the commit (refreshed on every connected sync, or fed directly from the
// developer's own working clone), and act's container image is already pulled.
func (d *Daemon) runOfflineJob(ctx context.Context, job *store.Job) (*offline.Result, error) {
	act, err := offline.Find()
	if err != nil {
		return nil, err
	}

	mirror := &offline.Mirror{Slug: job.RepoSlug, Path: offline.MirrorPath(d.HomeDir, job.RepoSlug)}
	if _, err := os.Stat(filepath.Join(mirror.Path, "HEAD")); err != nil {
		return nil, fmt.Errorf("no local mirror for %s. Homerun mirrors linked repos on each "+
			"connected sync; run `homerun link` while online first", job.RepoSlug)
	}

	sha := job.CommitSHA
	if sha == "" && job.Ref != "" {
		sha, err = mirror.ResolveRef(ctx, job.Ref)
		if err != nil {
			return nil, err
		}
	}
	if !mirror.HasCommit(ctx, sha) {
		return nil, fmt.Errorf("commit %s is not in the local mirror of %s; "+
			"push it to the mirror or run `homerun run --repo-path <dir>`", short(sha), job.RepoSlug)
	}

	workDir := filepath.Join(d.HomeDir, "work", fmt.Sprintf("offline-%d", job.ID))
	cleanup, err := mirror.Checkout(ctx, workDir, sha)
	if err != nil {
		return nil, err
	}
	// The working tree is destroyed after every run, same as Engine A's
	// container: no state survives into the next job.
	defer cleanup()

	branch := strings.TrimPrefix(job.Ref, "refs/heads/")
	if branch == "" {
		branch = "main"
	}
	event := offline.Event{
		Name:       orDefault(job.EventName, "push"),
		Ref:        orDefault(job.Ref, "refs/heads/"+branch),
		SHA:        sha,
		Repository: job.RepoSlug,
		Actor:      "homerun",
		Branch:     branch,
	}

	logPath := filepath.Join(d.HomeDir, "logs", fmt.Sprintf("offline-%d.log", job.ID))
	opts := offline.RunOpts{
		WorkDir:     workDir,
		Event:       event,
		Workflow:    job.Workflow,
		Job:         job.JobName,
		Image:       d.Config.Engine.ActImage,
		Limits:      d.Config.Limits,
		ArtifactDir: filepath.Join(d.HomeDir, "artifacts", fmt.Sprint(job.ID)),
	}
	// A local secrets file is optional: without it, workflows that need
	// secrets will fail offline, and Homerun says so rather than pretending.
	secretsFile := filepath.Join(d.HomeDir, "secrets", strings.ReplaceAll(job.RepoSlug, "/", "__")+".env")
	if _, err := os.Stat(secretsFile); err == nil {
		opts.SecretsFile = secretsFile
	}

	if _, err := d.DB.SQL().ExecContext(ctx, `UPDATE jobs SET log_path=? WHERE id=?`, logPath, job.ID); err != nil {
		d.logf("could not record log path: %v", err)
	}

	res, err := act.Run(ctx, opts, logPath, nil)
	if res != nil && len(res.JobNames) > 0 && job.JobName == "" {
		_, _ = d.DB.SQL().ExecContext(ctx, `UPDATE jobs SET job_name=? WHERE id=?`,
			strings.Join(res.JobNames, ","), job.ID)
	}
	return res, err
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
