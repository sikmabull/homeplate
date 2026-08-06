package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newMenubarCmd is the MVP stub for the menu-bar app. The brand kit already
// ships the template images (assets/menubar in the repo, and the daemon has
// all the state a tray needs via `homeplate status --json`); the native
// systray app lands within the week. Until then this command exists so
// scripts and docs have a stable target.
func newMenubarCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "menubar",
		Short: "Menu-bar app (stub: coming this week)",
		RunE: func(c *cobra.Command, args []string) error {
			fmt.Println("The Homeplate menu-bar app is not built yet (MVP stub).")
			fmt.Println()
			fmt.Println("Everything it will show is available today:")
			fmt.Println("  homeplate status          queue, caps, sleep state, net $ saved")
			fmt.Println("  homeplate status --json   the same, machine-readable")
			fmt.Println("  homeplate pause / resume  job pickup control")
			fmt.Println()
			fmt.Println("Template icons are ready (macOS template image, 22px pentagon)")
			fmt.Println("in the brand kit. The systray app is the next milestone.")
			return nil
		},
	}
}
