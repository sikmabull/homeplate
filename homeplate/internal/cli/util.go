package cli

import (
	"context"
	"os/exec"
	"runtime"
	"time"

	"github.com/homeplate-ci/homeplate/internal/repofind"
)

// openBrowser is best-effort convenience. Device flow works fine without it.
func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	default:
		return nil
	}
	return cmd.Start()
}

// findLocalClone records where on disk a repo lives, so the daemon can run
// never-pushed commits while offline. Cheap check first (cwd), then a
// bounded filesystem scan.
func findLocalClone(slug string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if p, ok := repofind.FindForSlug(ctx, slug); ok {
		return p
	}
	return ""
}
