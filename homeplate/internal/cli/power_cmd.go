package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/homeplate-ci/homeplate/internal/config"
	"github.com/homeplate-ci/homeplate/internal/power"
)

func newPowerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "power",
		Short: "Manage power/sleep behaviour and the lid-close helper",
	}

	var revert bool
	setup := &cobra.Command{
		Use:   "setup",
		Short: "Install the passwordless pmset helper for the managed lid-close toggle",
		Long: `The managed lid-close toggle (limit --clamshell true) needs root for exactly
one operation: pmset -a disablesleep 0|1. This command installs a single
NOPASSWD sudoers rule covering ONLY those two commands, so the daemon can
flip the toggle without ever prompting for a password.

Run it as yourself - it re-execs through sudo, so you get one password
prompt. Idempotent: re-running just rewrites the same rule.`,
		RunE: func(c *cobra.Command, args []string) error {
			ctx, cancel := signalContext()
			defer cancel()
			return runPowerSetup(ctx, revert)
		},
	}
	setup.Flags().BoolVar(&revert, "revert", false,
		"remove the sudoers helper and undo any stale lid-close override")
	cmd.AddCommand(setup)
	return cmd
}

func runPowerSetup(ctx context.Context, revert bool) error {
	if os.Geteuid() != 0 {
		return reexecPowerSetupViaSudo(revert)
	}

	homeDir := homeDirForSudoUser()

	if revert {
		reverted, err := power.RevertStaleClamshell(ctx, homeDir)
		if err != nil {
			fmt.Printf("warning: could not revert the lid-close override: %v\n", err)
		} else if reverted {
			fmt.Println("Reverted a stale lid-close override (pmset disablesleep back to 0).")
		}
		if err := power.RemoveSudoersHelper(); err != nil {
			return fmt.Errorf("removing sudoers helper: %w", err)
		}
		fmt.Printf("Removed %s.\n", power.SudoersPath)
		fmt.Println("The daemon can no longer toggle lid-close sleep without a password.")
		return nil
	}

	path, err := power.InstallSudoersHelper(os.Getenv("SUDO_USER"))
	if err != nil {
		return err
	}
	fmt.Printf("Installed %s\n", path)
	fmt.Println()
	fmt.Println("This grants the Homeplate user passwordless sudo for EXACTLY:")
	fmt.Println("    pmset -a disablesleep 0|1")
	fmt.Println("and nothing else. The daemon uses it to keep CI jobs alive with the lid")
	fmt.Println("closed, and reverts the setting when work drains, on exit, and on uninstall.")
	fmt.Println()
	fmt.Println("Enable the toggle with:  homeplate limit --clamshell true")
	fmt.Println("Undo everything with:    homeplate power setup --revert")
	return nil
}

// reexecPowerSetupViaSudo gives the user a single password prompt instead of
// demanding they retype the command with sudo.
func reexecPowerSetupViaSudo(revert bool) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{exe, "power", "setup"}
	if revert {
		args = append(args, "--revert")
	}
	fmt.Println("This step needs root once; re-running via sudo...")
	cmd := exec.Command("sudo", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// sudo strips the environment; make sure the helper targets the right
	// home when HOMEPLATE_HOME is customised.
	if v := os.Getenv("HOMEPLATE_HOME"); v != "" {
		cmd.Env = append(os.Environ(), "HOMEPLATE_HOME="+v)
	}
	return cmd.Run()
}

// homeDirForSudoUser finds the invoking user's Homeplate home even when we
// are running as root via sudo (root's own home would be the wrong place to
// look for the clamshell marker).
func homeDirForSudoUser() string {
	if v := os.Getenv("HOMEPLATE_HOME"); v != "" {
		return v
	}
	if su := os.Getenv("SUDO_USER"); su != "" {
		if u, err := user.Lookup(su); err == nil && u.HomeDir != "" {
			return filepath.Join(u.HomeDir, ".homeplate")
		}
	}
	return config.Dir()
}
