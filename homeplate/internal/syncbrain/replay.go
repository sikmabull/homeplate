// Package syncbrain reconciles Engine A and Engine B with GitHub.
//
// Its single hard promise: replaying an offline result is IDEMPOTENT. Running
// replay twice, or crashing halfway through, must never produce two statuses,
// two approvals, or two merges. That guarantee is enforced by a UNIQUE
// constraint in the replay ledger (see internal/store), not by hoping.
//
// Its second hard promise: a replayed result NEVER impersonates a
// GitHub-hosted run. Every status/check says so in its description and its
// context string.
package syncbrain

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/homeplate-ci/homeplate/internal/config"
	"github.com/homeplate-ci/homeplate/internal/ghapi"
	"github.com/homeplate-ci/homeplate/internal/store"
)

// StatusContextPrefix namespaces every commit status Homeplate creates so it is
// visually distinct from GitHub's own "ci/..." contexts in the PR checks list.
const StatusContextPrefix = "homeplate"

// Disclaimer is the mandatory provenance string. It appears in the status
// description and in any check output.
func Disclaimer(at time.Time) string {
	return fmt.Sprintf("ran locally via Homeplate offline mode at %s", at.UTC().Format(time.RFC3339))
}

// StatusContext builds the context string for a job, e.g.
// "homeplate/ci.yml / build (offline)".
func StatusContext(j *store.Job) string {
	parts := []string{StatusContextPrefix}
	if j.Workflow != "" {
		parts = append(parts, strings.TrimSuffix(j.Workflow, ".yml"))
	}
	name := j.JobName
	if name == "" {
		name = "job"
	}
	ctx := strings.Join(parts, "/") + " / " + name
	if j.Engine == store.EngineB {
		ctx += " (offline)"
	}
	if len(ctx) > 255 {
		ctx = ctx[:255]
	}
	return ctx
}

// Clients resolves a GitHub client per auth profile, so one daemon can serve
// personal, work, and client-org identities simultaneously.
type Clients interface {
	For(profile string) (*ghapi.Client, error)
}

// Replayer pushes queued offline results to GitHub.
type Replayer struct {
	DB      *store.DB
	Clients Clients
	Config  *config.Config
	// Log receives human-readable progress.
	Log func(format string, args ...any)
	// Now is injectable for tests.
	Now func() time.Time
}

func (r *Replayer) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now().UTC()
}

func (r *Replayer) logf(format string, args ...any) {
	if r.Log != nil {
		r.Log(format, args...)
	}
}

// Outcome summarises one replay pass.
type Outcome struct {
	Considered int
	Posted     int
	Skipped    int
	Approved   int
	Merged     int
	Failed     int
	Errors     []error
}

// MaxAttempts caps retries for a single artifact so one permanently broken
// job (deleted repo, revoked token) cannot block the queue forever.
const MaxAttempts = 8

// ReplayAll pushes every pending offline result, oldest first.
//
// Ordering matters: statuses for an older commit must land before a newer one
// so the PR's check history reads correctly.
func (r *Replayer) ReplayAll(ctx context.Context) (Outcome, error) {
	var out Outcome

	jobs, err := r.DB.PendingReplay(ctx)
	if err != nil {
		return out, err
	}
	out.Considered = len(jobs)

	for _, j := range jobs {
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		default:
		}
		if err := r.replayJob(ctx, j, &out); err != nil {
			out.Failed++
			out.Errors = append(out.Errors, fmt.Errorf("job %d (%s %s): %w", j.ID, j.RepoSlug, j.CommitSHA[:min(7, len(j.CommitSHA))], err))
			continue
		}
	}
	return out, nil
}

func (r *Replayer) replayJob(ctx context.Context, j *store.Job, out *Outcome) error {
	if j.CommitSHA == "" {
		// Nothing to attach a status to. Mark replayed so it stops being
		// retried, but say why.
		r.logf("job %d has no commit SHA; nothing to post to GitHub", j.ID)
		out.Skipped++
		return r.DB.MarkJobReplayed(ctx, j.ID)
	}

	client, err := r.Clients.For(j.Profile)
	if err != nil {
		return fmt.Errorf("no client for profile %q: %w", j.Profile, err)
	}

	target := StatusContext(j)

	attempts, err := r.DB.ReplayAttempts(ctx, j.ID, store.ReplayStatus, target)
	if err != nil {
		return err
	}
	if attempts >= MaxAttempts {
		r.logf("job %d exceeded %d replay attempts; giving up (see `homeplate logs %d`)", j.ID, MaxAttempts, j.ID)
		out.Skipped++
		return r.DB.MarkJobReplayed(ctx, j.ID)
	}

	done, err := r.DB.ClaimReplay(ctx, j.ID, store.ReplayStatus, target)
	if err != nil {
		return err
	}
	if done {
		// Already posted in a previous pass. This is the idempotency path.
		out.Skipped++
		return r.finish(ctx, j, out, client)
	}

	// Second idempotency layer: GitHub itself. If an identical status already
	// exists on the commit (e.g. the ledger was lost), do not post again.
	if existing, err := client.ListStatuses(ctx, j.RepoSlug, j.CommitSHA); err == nil {
		for _, e := range existing {
			if e.Context == target && e.State == string(stateFor(j)) {
				r.logf("status %q already present on %s; not duplicating", target, short(j.CommitSHA))
				_ = r.DB.CompleteReplay(ctx, j.ID, store.ReplayStatus, target, fmt.Sprint(e.ID))
				out.Skipped++
				return r.finish(ctx, j, out, client)
			}
		}
	}

	finishedAt := r.now()
	if j.FinishedAt != nil {
		finishedAt = *j.FinishedAt
	}

	st := ghapi.CommitStatus{
		State:       stateFor(j),
		Context:     target,
		Description: describe(j, finishedAt),
	}

	created, err := client.CreateStatus(ctx, j.RepoSlug, j.CommitSHA, st)
	if err != nil {
		_ = r.DB.FailReplay(ctx, j.ID, store.ReplayStatus, target, err)
		return err
	}
	out.Posted++
	r.logf("posted %s status %q on %s/%s", st.State, target, j.RepoSlug, short(j.CommitSHA))

	if err := r.DB.CompleteReplay(ctx, j.ID, store.ReplayStatus, target, fmt.Sprint(created.ID)); err != nil {
		return err
	}
	return r.finish(ctx, j, out, client)
}

// finish handles post-status actions (approve/merge) and marks the job done.
func (r *Replayer) finish(ctx context.Context, j *store.Job, out *Outcome, client *ghapi.Client) error {
	if j.State == store.StateSucceeded && r.Config.Sync.AutoApprove {
		if err := r.autoApproveAndMerge(ctx, j, out, client); err != nil {
			// Approval/merge failure must not block the status replay, which
			// is the valuable part. Log and continue.
			r.logf("auto-approve/merge for %s %s: %v", j.RepoSlug, short(j.CommitSHA), err)
		}
	}
	return r.DB.MarkJobReplayed(ctx, j.ID)
}

func (r *Replayer) autoApproveAndMerge(ctx context.Context, j *store.Job, out *Outcome, client *ghapi.Client) error {
	prs, err := client.PRsForCommit(ctx, j.RepoSlug, j.CommitSHA)
	if err != nil {
		return err
	}
	for _, pr := range prs {
		if pr.State != "open" || pr.Draft {
			continue
		}
		// Never auto-act on a fork PR: that is untrusted third-party code.
		if pr.IsFork() {
			r.logf("skipping auto-approve for %s#%d: PR is from a fork", j.RepoSlug, pr.Number)
			continue
		}
		// The head must still be the commit we actually tested.
		if !strings.EqualFold(pr.Head.SHA, j.CommitSHA) {
			r.logf("skipping %s#%d: head moved from %s to %s since the local run",
				j.RepoSlug, pr.Number, short(j.CommitSHA), short(pr.Head.SHA))
			continue
		}
		// Every other Homeplate job on this commit must also have passed.
		ok, why, err := r.allLocalChecksPassed(ctx, j.RepoSlug, j.CommitSHA)
		if err != nil {
			return err
		}
		if !ok {
			r.logf("skipping %s#%d: %s", j.RepoSlug, pr.Number, why)
			continue
		}

		target := fmt.Sprintf("pr-%d", pr.Number)
		done, err := r.DB.ClaimReplay(ctx, j.ID, store.ReplayApprove, target)
		if err != nil {
			return err
		}
		if !done {
			body := fmt.Sprintf("Approved by Homeplate: all required checks %s.\n\n"+
				"This approval is based on a run that happened on a local machine, not on "+
				"GitHub-hosted infrastructure.", Disclaimer(r.now()))
			if err := client.ApprovePR(ctx, j.RepoSlug, pr.Number, body); err != nil {
				// Self-approval is rejected by GitHub for your own PRs. That is
				// expected for solo developers, not an error worth retrying.
				if isSelfApproval(err) {
					r.logf("cannot approve %s#%d (GitHub forbids approving your own PR); "+
						"the passing status is still posted", j.RepoSlug, pr.Number)
					_ = r.DB.CompleteReplay(ctx, j.ID, store.ReplayApprove, target, "self-approval-not-allowed")
				} else {
					_ = r.DB.FailReplay(ctx, j.ID, store.ReplayApprove, target, err)
					return err
				}
			} else {
				out.Approved++
				_ = r.DB.CompleteReplay(ctx, j.ID, store.ReplayApprove, target, "approved")
				r.logf("approved %s#%d", j.RepoSlug, pr.Number)
			}
		}

		if !r.Config.Sync.AutoMerge {
			continue
		}
		mergeTarget := fmt.Sprintf("pr-%d-merge", pr.Number)
		mdone, err := r.DB.ClaimReplay(ctx, j.ID, store.ReplayMerge, mergeTarget)
		if err != nil {
			return err
		}
		if mdone {
			continue
		}
		fresh, err := client.GetPR(ctx, j.RepoSlug, pr.Number)
		if err != nil {
			return err
		}
		if fresh.MergedAt != nil {
			_ = r.DB.CompleteReplay(ctx, j.ID, store.ReplayMerge, mergeTarget, "already-merged")
			continue
		}
		if fresh.Mergeable != nil && !*fresh.Mergeable {
			r.logf("not merging %s#%d: GitHub reports it is not mergeable (%s)",
				j.RepoSlug, pr.Number, fresh.MergeableState)
			continue
		}
		res, err := client.MergePR(ctx, j.RepoSlug, pr.Number, ghapi.MergeRequest{
			SHA:         j.CommitSHA, // refuse to merge anything but the tested commit
			MergeMethod: r.Config.Sync.MergeMethod,
			CommitTitle: fmt.Sprintf("%s (#%d)", fresh.Title, pr.Number),
			CommitMessage: fmt.Sprintf("Merged by Homeplate after a local run.\n%s",
				Disclaimer(r.now())),
		})
		if err != nil {
			_ = r.DB.FailReplay(ctx, j.ID, store.ReplayMerge, mergeTarget, err)
			return err
		}
		out.Merged++
		_ = r.DB.CompleteReplay(ctx, j.ID, store.ReplayMerge, mergeTarget, res.SHA)
		r.logf("merged %s#%d as %s", j.RepoSlug, pr.Number, short(res.SHA))
	}
	return nil
}

// allLocalChecksPassed verifies every Homeplate job recorded for a commit passed.
func (r *Replayer) allLocalChecksPassed(ctx context.Context, slug, sha string) (bool, string, error) {
	jobs, err := r.DB.RecentJobs(ctx, 500)
	if err != nil {
		return false, "", err
	}
	found := 0
	for _, j := range jobs {
		if !strings.EqualFold(j.RepoSlug, slug) || !strings.EqualFold(j.CommitSHA, sha) {
			continue
		}
		found++
		if j.State != store.StateSucceeded {
			return false, fmt.Sprintf("local job %q is %s, not succeeded", j.JobName, j.State), nil
		}
	}
	if found == 0 {
		return false, "no local job results recorded for this commit", nil
	}
	return true, "", nil
}

func stateFor(j *store.Job) ghapi.StatusState {
	switch j.State {
	case store.StateSucceeded:
		return ghapi.StatusSuccess
	case store.StateFailed:
		return ghapi.StatusFailure
	case store.StateInterrupted:
		// "error" (not "failure") is semantically right: the job did not
		// produce a verdict, the machine went to sleep.
		return ghapi.StatusError
	default:
		return ghapi.StatusPending
	}
}

// describe builds the 140-char status description. The provenance disclaimer
// is mandatory and is placed FIRST so it survives truncation.
func describe(j *store.Job, at time.Time) string {
	prefix := Disclaimer(at)
	detail := ""
	switch j.State {
	case store.StateSucceeded:
		detail = "passed"
	case store.StateFailed:
		detail = fmt.Sprintf("failed (exit %d)", j.ExitCode)
	case store.StateInterrupted:
		detail = "interrupted: " + j.Reason
	}
	dur := time.Duration(j.BillableSeconds) * time.Second
	s := fmt.Sprintf("%s - %s in %s", prefix, detail, dur.Round(time.Second))
	if len(s) > 140 {
		s = s[:137] + "..."
	}
	return s
}

func isSelfApproval(err error) bool {
	var e *ghapi.Error
	if !errors.As(err, &e) {
		return false
	}
	msg := strings.ToLower(e.Message)
	if strings.Contains(msg, "can not approve your own pull request") ||
		strings.Contains(msg, "cannot approve your own") {
		return true
	}
	for _, sub := range e.Errors {
		if strings.Contains(strings.ToLower(sub.Message), "approve your own") {
			return true
		}
	}
	return false
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
