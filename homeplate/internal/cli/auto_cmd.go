package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/homeplate-ci/homeplate/internal/auth"
	"github.com/homeplate-ci/homeplate/internal/config"
	"github.com/homeplate-ci/homeplate/internal/ghapi"
	"github.com/homeplate-ci/homeplate/internal/offline"
	"github.com/homeplate-ci/homeplate/internal/repofind"
	"github.com/homeplate-ci/homeplate/internal/runner"
	"github.com/homeplate-ci/homeplate/internal/service"
)

// TokenEnvVar lets an agent authenticate without ever touching a prompt:
// export a fine-grained PAT and `homeplate auto` does the rest.
const TokenEnvVar = "HOMEPLATE_GITHUB_TOKEN"

// agentProfileName is where the env-var token is stored.
const agentProfileName = "agent"

func newAutoCmd() *cobra.Command {
	var (
		dryRun   bool
		allowPub bool
	)
	cmd := &cobra.Command{
		Use:   "auto",
		Short: "One-shot setup: auth, link local clones, adopt, install the daemon",
		Long: `Fully non-interactive setup for agents and fresh machines. It NEVER prompts;
when something is missing it fails with the exact remediation.

  homeplate auto                          set up everything, end to end
  homeplate auto --dry-run                report what it WOULD do
  homeplate auto --profile work           use a specific identity

Authentication is resolved in this order:
  1. --profile <name>          an existing ` + "`homeplate auth add`" + ` profile
  2. ` + TokenEnvVar + `       a fine-grained PAT, stored as profile "agent"
  3. exactly one profile       use it
  4. otherwise                 fail with instructions

Steps: config + database init, auth, Docker/act preflight, scan the disk for
working copies, link every repo the identity administers (private only),
open an adopt PR per repo, install the launchd/systemd daemon.

Safe to run twice: linking, adopting, and service installation are all
idempotent.`,
		RunE: func(c *cobra.Command, args []string) error {
			ctx, cancel := signalContext()
			defer cancel()
			return runAuto(ctx, dryRun, allowPub)
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "plan only: change nothing on disk or on GitHub")
	cmd.Flags().BoolVar(&allowPub, "i-understand-public-repo-risk", false,
		"allow linking PUBLIC repos (fork PRs = strangers' code on your machine)")
	return cmd
}

// autoReport is the machine-readable summary emitted with --json.
type autoReport struct {
	Identity     string   `json:"identity"`
	Profile      string   `json:"profile"`
	ConfigPath   string   `json:"config_path"`
	DockerOK     bool     `json:"docker_ok"`
	DockerDetail string   `json:"docker_detail"`
	ActOK        bool     `json:"act_ok"`
	Found        int      `json:"working_copies_found"`
	Linked       []string `json:"linked"`
	Skipped      []string `json:"skipped"`
	LinkErrors   []string `json:"link_errors,omitempty"`
	PRs          []string `json:"adopt_prs"`
	AdoptErrors  []string `json:"adopt_errors,omitempty"`
	Service      string   `json:"service_state"`
	DryRun       bool     `json:"dry_run"`
}

func runAuto(ctx context.Context, dryRun, allowPub bool) error {
	rep := &autoReport{DryRun: dryRun}
	step := func(format string, a ...any) {
		if !flagJSON {
			fmt.Printf(format+"\n", a...)
		}
	}

	// ---- (a) config + store init ----
	home := config.Dir()
	if !dryRun {
		if err := os.MkdirAll(home, 0o700); err != nil {
			return err
		}
		for _, sub := range []string{"logs", "work", "mirrors", "runner", "artifacts", "secrets"} {
			if err := os.MkdirAll(filepath.Join(home, sub), 0o700); err != nil {
				return err
			}
		}
	}
	cfg, err := mustConfig()
	if err != nil {
		return err
	}
	rep.ConfigPath = config.Path()
	if _, err := os.Stat(config.Path()); os.IsNotExist(err) && !dryRun {
		if err := cfg.Save(); err != nil {
			return err
		}
	}
	step("1. config ready: %s", rep.ConfigPath)

	if !dryRun {
		db, err := openStore()
		if err != nil {
			return err
		}
		if err := ensureMachineID(ctx, db); err != nil {
			db.Close()
			return err
		}
		db.Close()
	}
	step("2. job queue ready")

	// ---- (b) auth, never interactive ----
	client, profile, err := autoAuth(ctx, dryRun)
	if err != nil {
		return err
	}
	rep.Identity = "@" + profile.Login
	rep.Profile = profile.Name
	step("3. authenticated as @%s (profile %q)", profile.Login, profile.Name)

	// ---- (c) preflight ----
	if d, err := runner.NewDocker(); err != nil {
		rep.DockerDetail = "not installed"
	} else if err := d.Available(ctx); err != nil {
		rep.DockerDetail = err.Error()
	} else {
		rep.DockerOK = true
		rep.DockerDetail = fmt.Sprintf("%s (server %s)", d.Bin, d.ServerVersion(ctx))
	}
	if !rep.DockerOK && !dryRun {
		return fmt.Errorf(`container runtime unavailable: %s

  Homeplate runs Linux jobs in throwaway Docker containers; without a running
  runtime there is nothing to set up.

  Fix:  brew install --cask docker   (then start Docker Desktop)
   or:  brew install colima && colima start
  Then re-run: homeplate auto`, rep.DockerDetail)
	}
	step("4. container runtime: %s", rep.DockerDetail)
	if a, err := offline.Find(); err != nil {
		step("   nektos/act: not found - offline mode (Engine B) disabled; `brew install act` to enable")
	} else {
		rep.ActOK = true
		step("   nektos/act: %s (offline mode available)", a.Version)
	}

	// ---- (d) scan ----
	found, err := repofind.Scan(ctx, repofind.DefaultRoots(), 5)
	if err != nil {
		return err
	}
	rep.Found = len(found)
	step("5. found %d working copy(ies) with GitHub remotes", rep.Found)

	// ---- (e) link everything the identity admins ----
	var toAdopt []string
	for _, f := range found {
		if _, ok := cfg.FindRepo(f.Slug); ok {
			rep.Skipped = append(rep.Skipped, f.Slug+" (already linked)")
			toAdopt = append(toAdopt, f.Slug)
			continue
		}
		if dryRun {
			repo, err := client.GetRepo(ctx, f.Slug)
			switch {
			case err != nil:
				rep.Skipped = append(rep.Skipped, f.Slug+" (unreadable: "+err.Error()+")")
			case !repo.Permissions.Admin:
				rep.Skipped = append(rep.Skipped, f.Slug+" (not an admin)")
			case repo.IsPublic() && !allowPub:
				rep.Skipped = append(rep.Skipped, f.Slug+" (PUBLIC; pass "+PublicRepoFlag+")")
			default:
				rep.Linked = append(rep.Linked, f.Slug+" (would link)")
				toAdopt = append(toAdopt, f.Slug)
			}
			continue
		}
		sels, err := linkExplicit(ctx, client, profile, []string{f.Slug}, allowPub, "", nil)
		if err != nil {
			rep.LinkErrors = append(rep.LinkErrors, f.Slug+": "+oneLine(err))
			step("   ! %s: %s", f.Slug, oneLine(err))
			continue
		}
		for _, sel := range sels {
			sel.LocalPath = f.Path
			cfg.AddRepo(sel)
			rep.Linked = append(rep.Linked, f.Slug)
			toAdopt = append(toAdopt, f.Slug)
			step("   linked %s", f.Slug)
		}
	}
	if !dryRun {
		if err := cfg.Save(); err != nil {
			return err
		}
	}
	step("6. linked %d repo(s), %d skipped, %d failed", len(rep.Linked), len(rep.Skipped), len(rep.LinkErrors))

	// ---- (f) adopt ----
	for _, slug := range toAdopt {
		url, err := adoptOne(ctx, client, slug, adoptOptions{autoYes: true, dryRun: dryRun, quiet: flagJSON})
		switch {
		case err != nil:
			rep.AdoptErrors = append(rep.AdoptErrors, slug+": "+oneLine(err))
			step("   ! adopt %s: %s", slug, oneLine(err))
		case url != "":
			rep.PRs = append(rep.PRs, url)
		}
	}
	step("7. adopt: %d PR(s) opened, %d error(s)", len(rep.PRs), len(rep.AdoptErrors))

	// ---- (g) service ----
	if dryRun {
		rep.Service = "would install"
	} else {
		inst, err := service.New(home)
		if err != nil {
			rep.Service = "unavailable: " + oneLine(err)
		} else if path, err := inst.Install(); err != nil {
			rep.Service = "install failed: " + oneLine(err)
			step("   ! service install failed: %v (foreground fallback: homeplate daemon run)", err)
		} else {
			rep.Service = "installed: " + path
		}
	}
	step("8. daemon service: %s", rep.Service)

	// ---- (h) summary ----
	if flagJSON {
		return json.NewEncoder(os.Stdout).Encode(rep)
	}
	fmt.Println()
	fmt.Println(strings.Repeat("=", 52))
	fmt.Printf("identity:   %s (profile %q)\n", rep.Identity, rep.Profile)
	fmt.Printf("linked:     %d repo(s)\n", len(rep.Linked))
	for _, s := range rep.Linked {
		fmt.Printf("            %s\n", s)
	}
	fmt.Printf("PRs opened: %d\n", len(rep.PRs))
	for _, u := range rep.PRs {
		fmt.Printf("            %s\n", u)
	}
	fmt.Printf("service:    %s\n", rep.Service)
	fmt.Println()
	fmt.Println("Watch it work:  homeplate status")
	fmt.Println("Daemon log:     homeplate logs --follow")
	return nil
}

// autoAuth resolves the identity without ever prompting:
// --profile, then HOMEPLATE_GITHUB_TOKEN, then a single existing profile.
func autoAuth(ctx context.Context, dryRun bool) (*ghapi.Client, *auth.Profile, error) {
	authStore, err := auth.OpenStore()
	if err != nil {
		return nil, nil, err
	}

	if flagProfile != "" {
		client, p, err := clientFor(flagProfile)
		if err != nil {
			return nil, nil, err
		}
		return client, p, nil
	}

	if tok := strings.TrimSpace(os.Getenv(TokenEnvVar)); tok != "" {
		client := ghNew(tok)
		user, _, err := client.Whoami(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("%s is set but the token was rejected: %w", TokenEnvVar, err)
		}
		p := &auth.Profile{
			Name: agentProfileName, Login: user.Login, Host: "github.com", Kind: auth.KindPAT,
		}
		if !dryRun {
			if err := authStore.Save(p, tok); err != nil {
				return nil, nil, err
			}
		}
		client.Profile = p.Name
		return client, p, nil
	}

	profiles := authStore.List()
	if len(profiles) == 1 {
		return clientFor(profiles[0].Name)
	}

	var names []string
	for _, p := range profiles {
		names = append(names, p.Name)
	}
	if len(names) == 0 {
		return nil, nil, fmt.Errorf(`no GitHub identity available and auto cannot prompt.

  Fix either way:
    export %s=<fine-grained PAT>     (token needs repo + admin rights)
    homeplate auth add personal      (interactive, one time)
  Then re-run: homeplate auto`, TokenEnvVar)
	}
	return nil, nil, fmt.Errorf(`multiple identities exist (%s) and auto cannot prompt.

  Fix:  homeplate auto --profile <name>`, strings.Join(names, ", "))
}

// oneLine flattens a multi-line error for table/JSON output.
func oneLine(err error) string {
	return strings.Join(strings.Fields(err.Error()), " ")
}
