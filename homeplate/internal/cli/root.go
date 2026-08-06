// Package cli implements the homeplate command line.
package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/homeplate-ci/homeplate/internal/config"
	"github.com/homeplate-ci/homeplate/internal/store"
)

// Version is set at build time with -ldflags "-X .../internal/cli.Version=x.y.z".
var Version = "0.1.0-dev"

// Commit is the build commit, also injected at link time.
var Commit = "dev"

var (
	flagProfile string
	flagJSON    bool
	flagVerbose bool
)

// Execute runs the CLI.
func Execute() error {
	root := &cobra.Command{
		Use:   "homeplate",
		Short: "Run your GitHub Actions on your own machine",
		Long: `Homeplate turns this machine into your GitHub Actions runner.

GitHub keeps orchestration, PR checks, and logs.
Your hardware does the compute ($0.002/min on private repos vs up to $0.062/min
for hosted macOS - about 31x cheaper, and you own the silicon).

Quick start:
  homeplate auto                    one-shot setup: auth, link local clones, adopt, daemon
  homeplate init                    authenticate, pick repos, install the daemon
  homeplate status                  what is running, what it saved you
  homeplate doctor                  diagnose Docker, power, routing, connectivity`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       fmt.Sprintf("%s (%s)", Version, Commit),
	}

	root.PersistentFlags().StringVar(&flagProfile, "profile", "", "GitHub identity to use (see `homeplate auth list`)")
	root.PersistentFlags().BoolVar(&flagJSON, "json", false, "machine-readable output")
	root.PersistentFlags().BoolVarP(&flagVerbose, "verbose", "v", false, "verbose output")

	root.AddCommand(
		newInitCmd(),
		newAutoCmd(),
		newAuthCmd(),
		newScanCmd(),
		newLinkCmd(),
		newStatusCmd(),
		newLimitCmd(),
		newLogsCmd(),
		newPauseCmd(),
		newResumeCmd(),
		newDoctorCmd(),
		newAdoptCmd(),
		newPowerCmd(),
		newRunCmd(),
		newDaemonCmd(),
		newServiceCmd(),
		newMenubarCmd(),
	)
	return root.Execute()
}

// signalContext cancels on SIGINT/SIGTERM so the daemon can release its sleep
// assertion and deregister runners instead of being hard-killed.
func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		cancel()
	}()
	return ctx, cancel
}

// openStore opens the SQLite database, creating ~/.homeplate as needed.
func openStore() (*store.DB, error) {
	home := config.Dir()
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, err
	}
	return store.Open(store.Path(home))
}

// mustConfig loads config with defaults.
func mustConfig() (*config.Config, error) {
	return config.Load()
}
