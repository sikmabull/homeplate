package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/homerun-ci/homerun/internal/auth"
	"github.com/homerun-ci/homerun/internal/config"
	"github.com/homerun-ci/homerun/internal/daemon"
	"github.com/homerun-ci/homerun/internal/offline"
	"github.com/homerun-ci/homerun/internal/runner"
	"github.com/homerun-ci/homerun/internal/service"
	"github.com/homerun-ci/homerun/internal/store"
)

func readLine() (string, error) {
	return bufio.NewReader(os.Stdin).ReadString('\n')
}

func newInitCmd() *cobra.Command {
	var (
		skipService bool
		pat         bool
		clientID    string
	)
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Set up Homerun: authenticate, pick repos, install the daemon",
		RunE: func(c *cobra.Command, args []string) error {
			ctx, cancel := signalContext()
			defer cancel()

			home := config.Dir()
			if err := os.MkdirAll(home, 0o700); err != nil {
				return err
			}
			for _, sub := range []string{"logs", "work", "mirrors", "runner", "artifacts", "secrets"} {
				if err := os.MkdirAll(filepath.Join(home, sub), 0o700); err != nil {
					return err
				}
			}

			fmt.Println("Homerun - your machine, your CI, $0 per minute")
			fmt.Println(strings.Repeat("=", 52))

			// 1. Config with host-sized defaults.
			cfg, err := mustConfig()
			if err != nil {
				return err
			}
			if _, err := os.Stat(config.Path()); os.IsNotExist(err) {
				if err := cfg.Save(); err != nil {
					return err
				}
				fmt.Printf("\n1. Created %s\n", config.Path())
			} else {
				fmt.Printf("\n1. Using existing %s\n", config.Path())
			}
			fmt.Printf("   caps: %g cpus, %s memory, %d concurrent job(s)\n",
				cfg.Limits.MaxCPUs, cfg.Limits.MaxMemory, cfg.Limits.MaxConcurrentJobs)

			// 2. Database.
			db, err := openStore()
			if err != nil {
				return err
			}
			defer db.Close()
			if err := ensureMachineID(ctx, db); err != nil {
				return err
			}
			fmt.Printf("2. Job queue ready at %s\n", store.Path(home))

			// 3. Docker preflight - fail early and clearly.
			fmt.Print("3. Container runtime... ")
			if d, err := runner.NewDocker(); err != nil {
				fmt.Println("NOT FOUND")
				fmt.Println("   Homerun needs Docker (or Podman/Colima) for clean-room Linux jobs.")
				fmt.Println("   Install:  brew install --cask docker")
				fmt.Println("   Then re-run: homerun init")
			} else if err := d.Available(ctx); err != nil {
				fmt.Println("NOT RUNNING")
				fmt.Printf("   %v\n   Start Docker Desktop, then re-run `homerun doctor`.\n", err)
			} else {
				fmt.Printf("%s (server %s)\n", d.Bin, d.ServerVersion(ctx))
			}

			// 4. Authentication.
			authStore, err := auth.OpenStore()
			if err != nil {
				return err
			}
			if len(authStore.List()) == 0 {
				fmt.Println("\n4. Authenticate with GitHub")
				name := "personal"
				fmt.Printf("   Profile name [%s]: ", name)
				if line, _ := readLine(); strings.TrimSpace(line) != "" {
					name = strings.TrimSpace(line)
				}
				var token string
				var kind auth.Kind
				var scopes []string
				if pat {
					token, err = readSecret("   Paste a fine-grained PAT: ")
					kind = auth.KindPAT
				} else {
					token, scopes, err = runDeviceFlow(ctx, clientID, "", nil)
					kind = auth.KindDeviceFlow
					if err != nil {
						// Device flow needs a client_id; fall back gracefully
						// rather than dead-ending the user.
						fmt.Printf("\n   Device flow unavailable: %v\n", err)
						fmt.Println("   Falling back to token paste-in.")
						token, err = readSecret("   Paste a fine-grained PAT: ")
						kind = auth.KindPAT
					}
				}
				if err != nil {
					return err
				}
				client := ghNew(token)
				user, hdrScopes, err := client.Whoami(ctx)
				if err != nil {
					return fmt.Errorf("verifying token: %w", err)
				}
				if len(hdrScopes) > 0 {
					scopes = hdrScopes
				}
				if err := authStore.Save(&auth.Profile{
					Name: name, Login: user.Login, Host: "github.com", Kind: kind, Scopes: scopes,
				}, token); err != nil {
					return err
				}
				fmt.Printf("   Authenticated as @%s (stored in %s)\n", user.Login, authStore.KeyringName())
			} else {
				fmt.Printf("\n4. Already authenticated: %d identity(ies)\n", len(authStore.List()))
			}

			// 5. Link repos.
			if len(cfg.Repos) == 0 {
				fmt.Println("\n5. Pick repos to run jobs for:")
				fmt.Println("   (run `homerun link` now, or later)")
			} else {
				fmt.Printf("\n5. %d repo(s) already linked\n", len(cfg.Repos))
			}

			// 6. Daemon.
			if !skipService {
				fmt.Print("6. Installing background daemon... ")
				inst, err := service.New(home)
				if err != nil {
					fmt.Printf("skipped (%v)\n", err)
				} else if path, err := inst.Install(); err != nil {
					fmt.Printf("FAILED: %v\n", err)
					fmt.Println("   You can run it in the foreground: homerun daemon run")
				} else {
					fmt.Printf("installed\n   %s\n", path)
				}
			}

			fmt.Println("\n" + strings.Repeat("=", 52))
			fmt.Println("Next:")
			fmt.Println("  homerun link                 pick repos")
			fmt.Println("  homerun adopt <owner/repo>   route existing workflows here")
			fmt.Println("  homerun status               see what it's saving you")
			fmt.Println("  homerun doctor               diagnose anything odd")
			return nil
		},
	}
	cmd.Flags().BoolVar(&skipService, "no-service", false, "do not install the background daemon")
	cmd.Flags().BoolVar(&pat, "pat", false, "authenticate with a pasted token instead of device flow")
	cmd.Flags().StringVar(&clientID, "client-id", "", "OAuth App client_id for device flow")
	return cmd
}

// ensureMachineID stamps a stable identifier used in job fingerprints.
func ensureMachineID(ctx context.Context, db *store.DB) error {
	id, err := db.GetMeta(ctx, "machine_id")
	if err != nil {
		return err
	}
	if id != "" {
		return nil
	}
	host, _ := os.Hostname()
	id = fmt.Sprintf("%s-%d", host, time.Now().Unix())
	return db.SetMeta(ctx, "machine_id", id)
}

// ---------------- daemon ----------------

func newDaemonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run or inspect the background daemon",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "run",
		Short: "Run the daemon in the foreground (launchd/systemd use this)",
		RunE: func(c *cobra.Command, args []string) error {
			ctx, cancel := signalContext()
			defer cancel()
			return runDaemon(ctx)
		},
	})
	return cmd
}

func runDaemon(ctx context.Context) error {
	cfg, err := mustConfig()
	if err != nil {
		return err
	}
	db, err := openStore()
	if err != nil {
		return err
	}
	defer db.Close()

	// Build one GitHub client per identity: this is how a single daemon
	// multiplexes personal + work + client-org queues on one machine.
	authStore, err := auth.OpenStore()
	if err != nil {
		return err
	}
	clients := daemon.NewClientMap()
	for _, p := range authStore.List() {
		token, err := authStore.Token(p.Name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "homerun: profile %q has no usable token: %v\n", p.Name, err)
			continue
		}
		c := ghNew(token)
		if p.Host != "" && p.Host != "github.com" {
			c = c.WithHost(p.Host)
		}
		c.Profile = p.Name
		clients.Add(p.Name, c)
	}
	if len(clients.Profiles()) == 0 {
		return fmt.Errorf("no usable identities; run `homerun auth add <name>`")
	}

	d := daemon.New(cfg, db, config.Dir(), clients, os.Stdout)
	if dk, err := runner.NewDocker(); err == nil {
		d.Docker = dk
	}
	d.SetTargets(daemon.TargetsFromConfig(cfg))

	// Refresh mirrors in the background so offline mode always has commits.
	go refreshMirrors(ctx, cfg, authStore)

	return d.Run(ctx)
}

// refreshMirrors keeps local bare clones current while online. This is what
// makes offline mode able to run real commits.
func refreshMirrors(ctx context.Context, cfg *config.Config, authStore *auth.Store) {
	tick := time.NewTicker(15 * time.Minute)
	defer tick.Stop()
	for {
		for _, r := range cfg.Repos {
			if r.Scope != "repo" {
				continue
			}
			p, err := authStore.Get(r.Profile)
			if err != nil {
				continue
			}
			token, err := authStore.Token(r.Profile)
			if err != nil {
				continue
			}
			url := offline.AuthenticatedURL(p.Host, r.Slug, token)
			_, _ = offline.EnsureMirror(ctx, config.Dir(), r.Slug, url, nil)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// ---------------- service ----------------

func newServiceCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "service", Short: "Install or remove the auto-start daemon"}

	cmd.AddCommand(&cobra.Command{
		Use:   "install",
		Short: "Install the launchd agent / systemd user unit",
		RunE: func(c *cobra.Command, args []string) error {
			inst, err := service.New(config.Dir())
			if err != nil {
				return err
			}
			path, err := inst.Install()
			if err != nil {
				return err
			}
			fmt.Printf("Installed and started: %s\n", path)
			fmt.Println("It will start automatically at login.")
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "uninstall",
		Short: "Stop and remove the daemon service",
		RunE: func(c *cobra.Command, args []string) error {
			inst, err := service.New(config.Dir())
			if err != nil {
				return err
			}
			if err := inst.Uninstall(); err != nil {
				return err
			}
			fmt.Println("Daemon stopped and removed.")
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Show daemon service state",
		RunE: func(c *cobra.Command, args []string) error {
			inst, err := service.New(config.Dir())
			if err != nil {
				return err
			}
			installed, running, detail := inst.Status()
			fmt.Printf("installed: %t  running: %t\n%s\n", installed, running, detail)
			return nil
		},
	})
	return cmd
}
