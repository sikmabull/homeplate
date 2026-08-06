//go:build !darwin

package power

import (
	"context"
	"fmt"
	"runtime"
)

// SudoersLine is macOS-only; other platforms use systemd inhibitors.
const SudoersLine = ""
const SudoersPath = ""

func errClamshellPlatform() error {
	return fmt.Errorf("managed lid-close toggle is macOS-only; %s uses the login manager's lid-switch inhibitor", runtime.GOOS)
}

func SetDisableSleep(ctx context.Context, on bool) error { return errClamshellPlatform() }
func EnsureClamshell(ctx context.Context, homeDir string, on bool) error {
	return errClamshellPlatform()
}
func RevertStaleClamshell(ctx context.Context, homeDir string) (bool, error) { return false, nil }
func InstallSudoersHelper(username string) (string, error)                   { return "", errClamshellPlatform() }
func RemoveSudoersHelper() error                                             { return nil }
