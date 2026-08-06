package cli

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/homeplate-ci/homeplate/internal/ghapi"
	"github.com/homeplate-ci/homeplate/internal/labels"
)

// decodeBase64Content decodes GitHub's contents API payload.
func decodeBase64Content(content, encoding string) (string, error) {
	if encoding != "base64" {
		return content, nil
	}
	// GitHub wraps base64 at 60 chars; the std decoder rejects newlines.
	clean := strings.NewReplacer("\n", "", "\r", "").Replace(content)
	b, err := base64.StdEncoding.DecodeString(clean)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// adoptOptions carries the adopt flags through the batch paths
// (--all, `homeplate scan --adopt`, `homeplate auto`).
type adoptOptions struct {
	dryRun   bool
	branch   string
	autoYes  bool
	variable bool
	// quiet suppresses human progress output (batch JSON mode); errors and
	// the returned PR URL are unaffected.
	quiet bool
}

func newAdoptCmd() *cobra.Command {
	var (
		dryRun   bool
		branch   string
		push     bool
		all      bool
		variable bool
	)
	cmd := &cobra.Command{
		Use:   "adopt [<owner/repo> ...]",
		Short: "Open a PR rewriting runs-on so jobs land on this machine",
		Long: `GitHub reserves the hosted runner labels (ubuntu-latest, macos-latest, ...).
A self-hosted runner CANNOT claim them, so Homeplate cannot silently intercept
existing workflows. This is a GitHub platform rule, not a Homeplate limitation.

` + "`homeplate adopt`" + ` is the honest alternative: it opens ONE pull request that
rewrites every hosted ` + "`runs-on:`" + ` to Homeplate's labels:

    runs-on: ubuntu-latest
    ->
    runs-on: [self-hosted, homeplate, homeplate-linux]

You review it, merge it once, and every future job runs on your hardware.

With --variable, the rewrite is non-invasive instead:

    runs-on: ${{ vars.RUNNER_LABEL && fromJSON(vars.RUNNER_LABEL) || 'ubuntu-latest' }}

The original hosted label stays as the fallback, so the repo flips between
hosted and this machine by setting/deleting the RUNNER_LABEL repo variable
(Settings -> Secrets and variables -> Actions) - no new commits required.

Matrix expressions (${{ matrix.os }}) are NOT rewritten automatically - they are
reported so you can decide, because blindly replacing a computed value would
break the matrix.`,
		Args: func(c *cobra.Command, args []string) error {
			if all {
				if len(args) > 0 {
					return fmt.Errorf("--all adopts every linked repo; do not pass repo arguments")
				}
				return nil
			}
			if len(args) == 0 {
				return fmt.Errorf("expected <owner/repo>, or pass --all")
			}
			return nil
		},
		RunE: func(c *cobra.Command, args []string) error {
			ctx, cancel := signalContext()
			defer cancel()
			opts := adoptOptions{dryRun: dryRun, branch: branch, autoYes: push, variable: variable}
			if all {
				opts.autoYes = true
				return runAdoptAll(ctx, opts)
			}
			client, _, err := clientFor(flagProfile)
			if err != nil {
				return err
			}
			for _, slug := range args {
				if _, err := adoptOne(ctx, client, slug, opts); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show the diff without creating anything")
	cmd.Flags().StringVar(&branch, "branch", "", "branch name for the PR (default homeplate/adopt-<date>)")
	cmd.Flags().BoolVar(&push, "yes", false, "skip the confirmation prompt")
	cmd.Flags().BoolVar(&all, "all", false, "adopt every linked repo (non-interactive, continues past failures)")
	cmd.Flags().BoolVar(&variable, "variable", false,
		"rewrite runs-on to read the RUNNER_LABEL repo variable (flip hosted<->local without new commits)")
	return cmd
}

// runAdoptAll adopts every linked repo-scope repo, reporting a PR URL or a
// per-repo skip reason. It never prompts and never stops at the first
// failure: a batch command that dies on repo 3 of 20 is useless to an agent.
func runAdoptAll(ctx context.Context, opts adoptOptions) error {
	cfg, err := mustConfig()
	if err != nil {
		return err
	}
	opened, skipped, failed := 0, 0, 0
	seen := 0
	for _, r := range cfg.Repos {
		if r.Scope != "repo" {
			continue
		}
		seen++
		client, _, err := clientFor(r.Profile)
		if err != nil {
			fmt.Printf("  %-40s SKIP  %v\n", r.Slug, err)
			skipped++
			continue
		}
		url, err := adoptOne(ctx, client, r.Slug, opts)
		switch {
		case err != nil:
			fmt.Printf("  %-40s FAIL  %v\n", r.Slug, err)
			failed++
		case url == "":
			skipped++
		default:
			fmt.Printf("  %-40s PR    %s\n", r.Slug, url)
			opened++
		}
	}
	if seen == 0 {
		fmt.Println("No linked repos. Run `homeplate link` (or `homeplate scan --link`) first.")
		return nil
	}
	fmt.Printf("\nadopt --all: %d PR(s) opened, %d skipped, %d failed.\n", opened, skipped, failed)
	if failed > 0 {
		return fmt.Errorf("%d repo(s) failed to adopt", failed)
	}
	return nil
}

// adoptOne runs the full adopt flow for a single repo and returns the PR URL.
// An empty URL with nil error means there was nothing to adopt (no hosted
// labels), which batch callers report as a skip. The caller is responsible
// for building the client, so batch paths can route per-repo profiles.
func adoptOne(ctx context.Context, client *ghapi.Client, slug string, opts adoptOptions) (string, error) {
	repo, err := client.GetRepo(ctx, slug)
	if err != nil {
		return "", err
	}
	files, err := client.ListWorkflowFiles(ctx, slug, repo.DefaultBranch)
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		return "", fmt.Errorf("%s has no .github/workflows directory", slug)
	}

	type change struct {
		path    string
		content string
		result  labels.RewriteResult
	}
	var changes []change
	skipped := 0

	rewrite := labels.AdoptWorkflow
	if opts.variable {
		rewrite = labels.AdoptWorkflowVariable
	}

	for _, f := range files {
		if !strings.HasSuffix(f.Path, ".yml") && !strings.HasSuffix(f.Path, ".yaml") {
			continue
		}
		raw, err := fetchWorkflow(ctx, client, slug, f.Path)
		if err != nil {
			fmt.Printf("  ! could not read %s: %v\n", f.Path, err)
			continue
		}
		res := rewrite(f.Path, raw)
		for _, ch := range res.Changes {
			if ch.Skipped {
				skipped++
			}
		}
		if res.Modified {
			changes = append(changes, change{path: f.Path, content: res.Content, result: res})
		}
	}

	if len(changes) == 0 {
		if !opts.quiet {
			fmt.Printf("Nothing to adopt in %s: no workflow uses a GitHub-hosted label.\n", slug)
			if skipped > 0 {
				fmt.Printf("(%d runs-on line(s) were skipped - see --dry-run for why)\n", skipped)
			}
		}
		return "", nil
	}

	total := 0
	for _, ch := range changes {
		for _, r := range ch.result.Changes {
			if !r.Skipped {
				total++
			}
		}
	}

	if !opts.quiet {
		fmt.Printf("\nPlanned changes in %s:\n\n", slug)
		for _, ch := range changes {
			fmt.Printf("  %s\n", ch.path)
			for _, r := range ch.result.Changes {
				if r.Skipped {
					fmt.Printf("      line %-4d SKIP  %s\n", r.Line, r.Reason)
					continue
				}
				fmt.Printf("      line %-4d -     %s\n", r.Line, strings.TrimSpace(r.Old))
				fmt.Printf("               +     %s\n", strings.TrimSpace(r.New))
			}
			fmt.Println()
		}
		fmt.Printf("%d job target(s) will move to this machine.\n", total)
	}

	if opts.dryRun {
		if !opts.quiet {
			fmt.Println("\n(dry run - nothing was changed)")
		}
		return "", nil
	}
	if !opts.autoYes {
		fmt.Print("\nOpen a pull request with these changes? [y/N] ")
		ans, _ := readLine()
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(ans)), "y") {
			fmt.Println("Cancelled.")
			return "", nil
		}
	}

	// In --variable mode the flip variable must exist before the PR merges,
	// or the fallback (hosted label) is what actually runs - which works, but
	// is not what the user asked for. Create-or-update it first.
	if opts.variable {
		if err := client.SetRepoVariable(ctx, slug, labels.RunnerLabelVariable, labels.RunnerLabelValue); err != nil {
			return "", fmt.Errorf("setting repo variable %s: %w", labels.RunnerLabelVariable, err)
		}
		if !opts.quiet {
			fmt.Printf("  repo variable %s set (delete it to fall back to hosted runners)\n",
				labels.RunnerLabelVariable)
		}
	}

	branch := opts.branch
	if branch == "" {
		branch = "homeplate/adopt-" + time.Now().Format("2006-01-02")
	}

	// Resolve the base commit, create a branch, commit each file, open the PR.
	baseSHA, err := getRef(ctx, client, slug, repo.DefaultBranch)
	if err != nil {
		return "", err
	}
	if err := createRef(ctx, client, slug, branch, baseSHA); err != nil {
		return "", fmt.Errorf("creating branch %s: %w", branch, err)
	}

	commitMsg := "ci: run this workflow on self-hosted Homeplate runners"
	if opts.variable {
		commitMsg = "ci: route this workflow via the RUNNER_LABEL repo variable"
	}
	for _, ch := range changes {
		fileSHA, err := getFileSHA(ctx, client, slug, ch.path, branch)
		if err != nil {
			return "", err
		}
		if err := putFile(ctx, client, slug, ch.path, branch, fileSHA, commitMsg, ch.content); err != nil {
			return "", fmt.Errorf("updating %s: %w", ch.path, err)
		}
		if !opts.quiet {
			fmt.Printf("  committed %s\n", ch.path)
		}
	}

	prURL, err := createPR(ctx, client, slug, branch, repo.DefaultBranch, total, opts.variable)
	if err != nil {
		return "", err
	}
	if !opts.quiet {
		fmt.Printf("\nPull request opened:\n  %s\n", prURL)
		if opts.variable {
			fmt.Printf("\nThis PR keeps %q as the fallback. To send jobs here, keep %s set;\n", "hosted labels", labels.RunnerLabelVariable)
			fmt.Println("to go back to GitHub-hosted runners, delete the variable - no revert PR needed.")
		} else {
			fmt.Println("\nMerge it and every future job for this repo runs on your machine.")
		}
	}
	return prURL, nil
}

func getRef(ctx context.Context, c *ghapi.Client, slug, ref string) (string, error) {
	owner, name, err := ghapi.SplitSlug(slug)
	if err != nil {
		return "", err
	}
	var out struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	_, err = c.Get(ctx, fmt.Sprintf("/repos/%s/%s/git/ref/heads/%s", owner, name, ref), &out)
	return out.Object.SHA, err
}

func createRef(ctx context.Context, c *ghapi.Client, slug, branch, sha string) error {
	owner, name, err := ghapi.SplitSlug(slug)
	if err != nil {
		return err
	}
	body := map[string]string{"ref": "refs/heads/" + branch, "sha": sha}
	_, err = c.Post(ctx, fmt.Sprintf("/repos/%s/%s/git/refs", owner, name), body, nil)
	if err != nil {
		var e *ghapi.Error
		if ghapi.AsError(err, &e) && strings.Contains(strings.ToLower(e.Message), "already exists") {
			return nil // idempotent: re-running adopt reuses the branch
		}
	}
	return err
}

func getFileSHA(ctx context.Context, c *ghapi.Client, slug, path, ref string) (string, error) {
	owner, name, err := ghapi.SplitSlug(slug)
	if err != nil {
		return "", err
	}
	var out struct {
		SHA string `json:"sha"`
	}
	_, err = c.Get(ctx, fmt.Sprintf("/repos/%s/%s/contents/%s?ref=%s", owner, name, path, ref), &out)
	return out.SHA, err
}

func putFile(ctx context.Context, c *ghapi.Client, slug, path, branch, sha, message, content string) error {
	owner, name, err := ghapi.SplitSlug(slug)
	if err != nil {
		return err
	}
	body := map[string]string{
		"message": message,
		"content": base64.StdEncoding.EncodeToString([]byte(content)),
		"branch":  branch,
		"sha":     sha,
	}
	_, err = c.Put(ctx, fmt.Sprintf("/repos/%s/%s/contents/%s", owner, name, path), body, nil)
	return err
}

func createPR(ctx context.Context, c *ghapi.Client, slug, head, base string, jobCount int, variable bool) (string, error) {
	owner, name, err := ghapi.SplitSlug(slug)
	if err != nil {
		return "", err
	}
	title := "ci: run workflows on self-hosted Homeplate runners"
	if variable {
		title = "ci: route workflows via the RUNNER_LABEL repo variable"
	}
	body := map[string]any{
		"title": title,
		"head":  head,
		"base":  base,
		"body":  adoptPRBody(jobCount, variable),
	}
	var out struct {
		HTMLURL string `json:"html_url"`
	}
	_, err = c.Post(ctx, fmt.Sprintf("/repos/%s/%s/pulls", owner, name), body, &out)
	return out.HTMLURL, err
}

// adoptPRBody explains the change to whoever reviews the PR, in the two
// dialects adopt speaks: the pin (fixed Homeplate labels) and the variable
// (flip hosted<->local from repo settings, no commits).
func adoptPRBody(jobCount int, variable bool) string {
	if variable {
		return fmt.Sprintf(`Routes %d job target(s) through the `+"`%s`"+` repository variable,
managed by [Homeplate](https://github.com/homeplate-ci/homeplate).

**Why this PR exists:** GitHub reserves the hosted runner labels
(`+"`ubuntu-latest`"+`, `+"`macos-latest`"+`, ...). A self-hosted runner cannot claim
them, so `+"`runs-on:`"+` has to change explicitly. This is the supported path.

**What changes:** `+"`runs-on: ubuntu-latest`"+` becomes

    runs-on: ${{ vars.%[2]s && fromJSON(vars.%[2]s) || 'ubuntu-latest' }}

**How the flip works:**
- This PR also sets the repo variable `+"`%[2]s`"+` to
  `+"`%[3]s`"+`, so jobs route to the self-hosted machine.
- The ORIGINAL hosted label stays in the expression as the fallback. To move
  a repo back to GitHub-hosted runners, delete (or empty) the variable under
  Settings -> Secrets and variables -> Actions. No revert PR, no new commit.
- Nothing about this PR can strand the repo: with the variable unset, the
  workflow behaves exactly as before.

**Cost impact:** while the variable is set, these jobs move from hosted
per-minute billing (up to $0.062/min for macOS) to self-hosted: $0.002/min on
private repos (GitHub's control-plane fee, since March 2026), $0 on public
repos. Self-hosted runners are a first-class, supported GitHub feature.

**Before merging, check:**
- A Homeplate runner is online for this repo (`+"`homeplate status`"+`).
- If nobody's machine is running Homeplate, jobs will queue while the variable
  is set. Delete the variable to fall back to hosted runners instantly.

Generated by `+"`homeplate adopt --variable`"+`.`, jobCount, labels.RunnerLabelVariable, labels.RunnerLabelValue)
	}
	return fmt.Sprintf(`Moves %d job target(s) from GitHub-hosted runners to self-hosted runners
managed by [Homeplate](https://github.com/homeplate-ci/homeplate).

**Why this PR exists:** GitHub reserves the hosted runner labels
(`+"`ubuntu-latest`"+`, `+"`macos-latest`"+`, ...). A self-hosted runner cannot claim
them, so `+"`runs-on:`"+` has to change explicitly. This is the supported path.

**What changes:** `+"`runs-on: ubuntu-latest`"+` becomes
`+"`runs-on: [self-hosted, homeplate, homeplate-linux]`"+`.

**Cost impact:** these jobs move from hosted per-minute billing (up to
$0.062/min for macOS) to self-hosted: $0.002/min on private repos (GitHub's
control-plane fee, since March 2026), $0 on public repos. Self-hosted runners
are a first-class, supported GitHub feature.

**Before merging, check:**
- A Homeplate runner is online for this repo (`+"`homeplate status`"+`).
- If nobody's machine is running Homeplate, these jobs will queue instead of
  running. Keep a hosted fallback for anything release-critical.

Generated by `+"`homeplate adopt`"+`.`, jobCount)
}
