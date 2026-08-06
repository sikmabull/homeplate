package cli

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/homerun-ci/homerun/internal/ghapi"
	"github.com/homerun-ci/homerun/internal/labels"
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

func newAdoptCmd() *cobra.Command {
	var (
		dryRun bool
		branch string
		push   bool
	)
	cmd := &cobra.Command{
		Use:   "adopt <owner/repo>",
		Short: "Open a PR rewriting runs-on so jobs land on this machine",
		Long: `GitHub reserves the hosted runner labels (ubuntu-latest, macos-latest, ...).
A self-hosted runner CANNOT claim them, so Homerun cannot silently intercept
existing workflows. This is a GitHub platform rule, not a Homerun limitation.

` + "`homerun adopt`" + ` is the honest alternative: it opens ONE pull request that
rewrites every hosted ` + "`runs-on:`" + ` to Homerun's labels:

    runs-on: ubuntu-latest
    ->
    runs-on: [self-hosted, homerun, homerun-linux]

You review it, merge it once, and every future job runs on your hardware.

Matrix expressions (${{ matrix.os }}) are NOT rewritten automatically - they are
reported so you can decide, because blindly replacing a computed value would
break the matrix.`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			ctx, cancel := signalContext()
			defer cancel()
			return runAdopt(ctx, args[0], dryRun, branch, push)
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show the diff without creating anything")
	cmd.Flags().StringVar(&branch, "branch", "", "branch name for the PR (default homerun/adopt-<date>)")
	cmd.Flags().BoolVar(&push, "yes", false, "skip the confirmation prompt")
	return cmd
}

func runAdopt(ctx context.Context, slug string, dryRun bool, branch string, autoYes bool) error {
	client, _, err := clientFor(flagProfile)
	if err != nil {
		return err
	}
	repo, err := client.GetRepo(ctx, slug)
	if err != nil {
		return err
	}
	files, err := client.ListWorkflowFiles(ctx, slug, repo.DefaultBranch)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("%s has no .github/workflows directory", slug)
	}

	type change struct {
		path    string
		content string
		result  labels.RewriteResult
	}
	var changes []change
	skipped := 0

	for _, f := range files {
		if !strings.HasSuffix(f.Path, ".yml") && !strings.HasSuffix(f.Path, ".yaml") {
			continue
		}
		raw, err := fetchWorkflow(ctx, client, slug, f.Path)
		if err != nil {
			fmt.Printf("  ! could not read %s: %v\n", f.Path, err)
			continue
		}
		res := labels.AdoptWorkflow(f.Path, raw)
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
		fmt.Printf("Nothing to adopt in %s: no workflow uses a GitHub-hosted label.\n", slug)
		if skipped > 0 {
			fmt.Printf("(%d runs-on line(s) were skipped - see --dry-run for why)\n", skipped)
		}
		return nil
	}

	fmt.Printf("\nPlanned changes in %s:\n\n", slug)
	total := 0
	for _, ch := range changes {
		fmt.Printf("  %s\n", ch.path)
		for _, r := range ch.result.Changes {
			if r.Skipped {
				fmt.Printf("      line %-4d SKIP  %s\n", r.Line, r.Reason)
				continue
			}
			total++
			fmt.Printf("      line %-4d -     %s\n", r.Line, strings.TrimSpace(r.Old))
			fmt.Printf("               +     %s\n", strings.TrimSpace(r.New))
		}
		fmt.Println()
	}
	fmt.Printf("%d job target(s) will move to this machine.\n", total)

	if dryRun {
		fmt.Println("\n(dry run - nothing was changed)")
		return nil
	}
	if !autoYes {
		fmt.Print("\nOpen a pull request with these changes? [y/N] ")
		ans, _ := readLine()
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(ans)), "y") {
			fmt.Println("Cancelled.")
			return nil
		}
	}

	if branch == "" {
		branch = "homerun/adopt-" + time.Now().Format("2006-01-02")
	}

	// Resolve the base commit, create a branch, commit each file, open the PR.
	baseSHA, err := getRef(ctx, client, slug, repo.DefaultBranch)
	if err != nil {
		return err
	}
	if err := createRef(ctx, client, slug, branch, baseSHA); err != nil {
		return fmt.Errorf("creating branch %s: %w", branch, err)
	}

	for _, ch := range changes {
		fileSHA, err := getFileSHA(ctx, client, slug, ch.path, branch)
		if err != nil {
			return err
		}
		if err := putFile(ctx, client, slug, ch.path, branch, fileSHA,
			"ci: run this workflow on self-hosted Homerun runners", ch.content); err != nil {
			return fmt.Errorf("updating %s: %w", ch.path, err)
		}
		fmt.Printf("  committed %s\n", ch.path)
	}

	prURL, err := createPR(ctx, client, slug, branch, repo.DefaultBranch, total)
	if err != nil {
		return err
	}
	fmt.Printf("\nPull request opened:\n  %s\n", prURL)
	fmt.Println("\nMerge it and every future job for this repo runs on your machine at $0/min.")
	return nil
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

func createPR(ctx context.Context, c *ghapi.Client, slug, head, base string, jobCount int) (string, error) {
	owner, name, err := ghapi.SplitSlug(slug)
	if err != nil {
		return "", err
	}
	body := map[string]any{
		"title": "ci: run workflows on self-hosted Homerun runners",
		"head":  head,
		"base":  base,
		"body": fmt.Sprintf(`Moves %d job target(s) from GitHub-hosted runners to self-hosted runners
managed by [Homerun](https://github.com/homerun-ci/homerun).

**Why this PR exists:** GitHub reserves the hosted runner labels
(`+"`ubuntu-latest`"+`, `+"`macos-latest`"+`, ...). A self-hosted runner cannot claim
them, so `+"`runs-on:`"+` has to change explicitly. This is the supported path.

**What changes:** `+"`runs-on: ubuntu-latest`"+` becomes
`+"`runs-on: [self-hosted, homerun, homerun-linux]`"+`.

**Cost impact:** these jobs move from GitHub's per-minute billing to $0/min on
self-hosted hardware. Self-hosted runners are a first-class, supported GitHub
feature with no per-minute charge.

**Before merging, check:**
- A Homerun runner is online for this repo (`+"`homerun status`"+`).
- If nobody's machine is running Homerun, these jobs will queue instead of
  running. Keep a hosted fallback for anything release-critical.

Generated by `+"`homerun adopt`"+`.`, jobCount),
	}
	var out struct {
		HTMLURL string `json:"html_url"`
	}
	_, err = c.Post(ctx, fmt.Sprintf("/repos/%s/%s/pulls", owner, name), body, &out)
	return out.HTMLURL, err
}
