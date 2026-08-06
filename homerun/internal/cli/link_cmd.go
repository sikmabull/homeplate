package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/homerun-ci/homerun/internal/auth"
	"github.com/homerun-ci/homerun/internal/config"
	"github.com/homerun-ci/homerun/internal/ghapi"
	"github.com/homerun-ci/homerun/internal/labels"
	"github.com/homerun-ci/homerun/internal/offline"
)

// PublicRepoFlag is the mandatory opt-in for linking public repositories.
const PublicRepoFlag = "--i-understand-public-repo-risk"

func newLinkCmd() *cobra.Command {
	var (
		all       bool
		orgs      bool
		allowPub  bool
		runnerGrp string
		extraLbls []string
		noMirror  bool
	)
	cmd := &cobra.Command{
		Use:   "link [owner/repo ...]",
		Short: "Pick which repos and orgs this machine runs jobs for",
		Long: `Enumerates every repo and org this identity can administer, lets you pick,
then registers Homerun as a self-hosted runner for each selection.

Re-running is safe: linking is idempotent.

SECURITY: linking a PUBLIC repository requires ` + PublicRepoFlag + `.
On a public repo, anyone can open a pull request, and (subject to your repo's
Actions settings) that pull request's code can execute on YOUR machine.`,
		RunE: func(c *cobra.Command, args []string) error {
			ctx, cancel := signalContext()
			defer cancel()

			client, profile, err := clientFor(flagProfile)
			if err != nil {
				return err
			}
			cfg, err := mustConfig()
			if err != nil {
				return err
			}

			var selections []config.LinkedRepo

			if len(args) > 0 {
				selections, err = linkExplicit(ctx, client, profile, args, allowPub, runnerGrp, extraLbls)
			} else {
				selections, err = linkInteractive(ctx, client, profile, all, orgs, allowPub, runnerGrp, extraLbls)
			}
			if err != nil {
				return err
			}
			if len(selections) == 0 {
				fmt.Println("Nothing linked.")
				return nil
			}

			added, updated := 0, 0
			for _, sel := range selections {
				if cfg.AddRepo(sel) {
					added++
				} else {
					updated++
				}
			}
			if err := cfg.Save(); err != nil {
				return err
			}

			fmt.Printf("\nLinked %d new, updated %d existing.\n", added, updated)
			for _, sel := range selections {
				fmt.Printf("  %-40s %s  labels: %s\n", sel.Slug, sel.Scope, strings.Join(sel.Labels, ","))
			}

			// Mirror repos so Engine B can run them offline later.
			if !noMirror {
				mirrorSelections(ctx, client, profile, selections)
			}

			fmt.Println("\nIMPORTANT - workflow routing:")
			fmt.Println("  GitHub reserves the hosted labels (ubuntu-latest, macos-latest, ...).")
			fmt.Println("  A self-hosted runner cannot claim them, so existing workflows will")
			fmt.Println("  keep running on GitHub's paid runners until you adopt them:")
			fmt.Printf("\n    homerun adopt %s\n", selections[0].Slug)
			fmt.Println("\n  That opens one PR rewriting `runs-on:` to Homerun's labels.")
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "link every repo/org this identity can administer")
	cmd.Flags().BoolVar(&orgs, "orgs", false, "include org-level runner registration")
	cmd.Flags().BoolVar(&allowPub, "i-understand-public-repo-risk", false,
		"allow linking PUBLIC repos (fork PRs = strangers' code on your machine)")
	cmd.Flags().StringVar(&runnerGrp, "runner-group", "", "org runner group to register into")
	cmd.Flags().StringSliceVar(&extraLbls, "label", nil, "extra runner labels")
	cmd.Flags().BoolVar(&noMirror, "no-mirror", false, "skip creating local bare clones for offline mode")
	return cmd
}

func linkExplicit(ctx context.Context, client *ghapi.Client, profile *auth.Profile,
	slugs []string, allowPub bool, group string, extra []string) ([]config.LinkedRepo, error) {

	var out []config.LinkedRepo
	for _, slug := range slugs {
		if !strings.Contains(slug, "/") {
			// No slash: treat as an org.
			if err := verifyOrgRunnerAccess(ctx, client, slug); err != nil {
				return nil, err
			}
			out = append(out, config.LinkedRepo{
				Slug: slug, Scope: "org", Profile: profile.Name,
				Labels: labels.Default(extra...), RunnerGroup: group, LinkedAt: time.Now().UTC(),
			})
			continue
		}
		repo, err := client.GetRepo(ctx, slug)
		if err != nil {
			return nil, fmt.Errorf("cannot read %s: %w", slug, err)
		}
		if !repo.Permissions.Admin {
			return nil, fmt.Errorf("you are not an admin of %s; runner registration requires admin", slug)
		}
		if repo.IsPublic() && !allowPub {
			return nil, publicRepoError(slug)
		}
		if repo.IsPublic() {
			printPublicWarning(slug)
		}
		if err := verifyRepoRunnerAccess(ctx, client, slug); err != nil {
			return nil, err
		}
		out = append(out, config.LinkedRepo{
			Slug: slug, Scope: "repo", Profile: profile.Name, Public: repo.IsPublic(),
			Labels: labels.Default(extra...), RunnerGroup: group, LinkedAt: time.Now().UTC(),
			Mirror: offline.MirrorPath(config.Dir(), slug),
		})
	}
	return out, nil
}

func linkInteractive(ctx context.Context, client *ghapi.Client, profile *auth.Profile,
	all, includeOrgs, allowPub bool, group string, extra []string) ([]config.LinkedRepo, error) {

	fmt.Printf("Enumerating repos you can administer as @%s...\n", profile.Login)
	repos, err := client.ListRepos(ctx)
	if err != nil {
		return nil, err
	}
	var admin []ghapi.Repo
	for _, r := range repos {
		if r.Permissions.Admin && !r.Archived {
			admin = append(admin, r)
		}
	}
	sort.Slice(admin, func(i, j int) bool {
		if admin[i].PushedAt == nil || admin[j].PushedAt == nil {
			return admin[i].FullName < admin[j].FullName
		}
		return admin[i].PushedAt.After(*admin[j].PushedAt)
	})

	var orgList []ghapi.Org
	if includeOrgs {
		orgList, err = client.ListOrgs(ctx)
		if err != nil {
			fmt.Printf("  (could not enumerate orgs: %v)\n", err)
		}
	}

	if len(admin) == 0 && len(orgList) == 0 {
		return nil, fmt.Errorf("no repos or orgs found that @%s can administer", profile.Login)
	}

	type choice struct {
		slug   string
		scope  string
		public bool
	}
	var choices []choice
	for _, o := range orgList {
		choices = append(choices, choice{slug: o.Login, scope: "org"})
	}
	for _, r := range admin {
		choices = append(choices, choice{slug: r.FullName, scope: "repo", public: r.IsPublic()})
	}

	var picked []int
	if all {
		for i := range choices {
			if choices[i].public && !allowPub {
				fmt.Printf("  skipping PUBLIC repo %s (pass %s to include it)\n", choices[i].slug, PublicRepoFlag)
				continue
			}
			picked = append(picked, i)
		}
	} else {
		fmt.Println()
		for i, ch := range choices {
			vis := "private"
			if ch.public {
				vis = "PUBLIC"
			}
			if ch.scope == "org" {
				vis = "org-wide"
			}
			fmt.Printf("  %3d) %-45s %s\n", i+1, ch.slug, vis)
		}
		fmt.Println("\nEnter numbers to link (e.g. 1,3,5 or 1-4), or 'all', or blank to cancel:")
		fmt.Print("> ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		picked = parseSelection(strings.TrimSpace(line), len(choices))
	}

	var out []config.LinkedRepo
	for _, i := range picked {
		ch := choices[i]
		if ch.public && !allowPub {
			return nil, publicRepoError(ch.slug)
		}
		if ch.public {
			printPublicWarning(ch.slug)
		}

		// Verify registration actually works BEFORE recording the link, so a
		// permissions problem surfaces here and not at 3am in the daemon log.
		var verr error
		if ch.scope == "org" {
			verr = verifyOrgRunnerAccess(ctx, client, ch.slug)
		} else {
			verr = verifyRepoRunnerAccess(ctx, client, ch.slug)
		}
		if verr != nil {
			fmt.Printf("  ! %s: %v\n", ch.slug, verr)
			continue
		}
		fmt.Printf("  registered runner access for %s\n", ch.slug)

		lr := config.LinkedRepo{
			Slug: ch.slug, Scope: ch.scope, Profile: profile.Name, Public: ch.public,
			Labels: labels.Default(extra...), RunnerGroup: group, LinkedAt: time.Now().UTC(),
		}
		if ch.scope == "repo" {
			lr.Mirror = offline.MirrorPath(config.Dir(), ch.slug)
		}
		out = append(out, lr)
	}
	return out, nil
}

// verifyRepoRunnerAccess proves the token can mint a registration token.
//
// Minting is harmless (the token simply expires unused) and is the only
// reliable way to know registration will work: GitHub returns 404, not 403,
// when a token lacks admin rights, so permission cannot be inferred from repo
// metadata alone.
func verifyRepoRunnerAccess(ctx context.Context, client *ghapi.Client, slug string) error {
	_, err := client.RepoRegistrationToken(ctx, slug)
	return err
}

func verifyOrgRunnerAccess(ctx context.Context, client *ghapi.Client, org string) error {
	_, err := client.OrgRegistrationToken(ctx, org)
	return err
}

func publicRepoError(slug string) error {
	return fmt.Errorf(`refusing to link PUBLIC repo %s.

  On a public repository, anyone can open a pull request. Depending on your
  repo's Actions settings, that pull request's code may execute on THIS machine
  with access to your network. That is a real risk, not a formality.

  If you understand and accept it:
      homerun link %s %s

  Safer alternative: Settings -> Actions -> General -> "Require approval for
  all external contributors" before linking.`, slug, slug, PublicRepoFlag)
}

func printPublicWarning(slug string) {
	fmt.Printf(`
  !! %s is PUBLIC.
     Fork pull requests can run STRANGERS' CODE on this machine.
     Homerun runs jobs in throwaway containers with no host network, but a
     container is not a security boundary against a determined attacker.
     Recommended: require approval for all outside collaborators in
     Settings -> Actions -> General.

`, slug)
}

func parseSelection(input string, max int) []int {
	input = strings.TrimSpace(strings.ToLower(input))
	if input == "" {
		return nil
	}
	if input == "all" {
		out := make([]int, max)
		for i := range out {
			out[i] = i
		}
		return out
	}
	seen := map[int]bool{}
	var out []int
	for _, part := range strings.Split(input, ",") {
		part = strings.TrimSpace(part)
		if lo, hi, ok := strings.Cut(part, "-"); ok {
			a, err1 := strconv.Atoi(strings.TrimSpace(lo))
			b, err2 := strconv.Atoi(strings.TrimSpace(hi))
			if err1 != nil || err2 != nil {
				continue
			}
			for i := a; i <= b; i++ {
				if i >= 1 && i <= max && !seen[i-1] {
					seen[i-1] = true
					out = append(out, i-1)
				}
			}
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < 1 || n > max || seen[n-1] {
			continue
		}
		seen[n-1] = true
		out = append(out, n-1)
	}
	return out
}

// mirrorSelections creates bare clones so offline mode has real commits.
func mirrorSelections(ctx context.Context, client *ghapi.Client, profile *auth.Profile, sels []config.LinkedRepo) {
	st, err := auth.OpenStore()
	if err != nil {
		return
	}
	token, err := st.Token(profile.Name)
	if err != nil {
		return
	}
	fmt.Println("\nMirroring repos locally so offline mode can run real commits...")
	for _, s := range sels {
		if s.Scope != "repo" {
			continue
		}
		url := offline.AuthenticatedURL(profile.Host, s.Slug, token)
		if _, err := offline.EnsureMirror(ctx, config.Dir(), s.Slug, url, func(m string) {
			fmt.Println("  " + m)
		}); err != nil {
			fmt.Printf("  ! %s: %v\n", s.Slug, err)
		}
	}
}
