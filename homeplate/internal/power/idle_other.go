//go:build !darwin && !linux

package power

import (
	"context"
	"fmt"
	"runtime"
)

func userCPUPercent(ctx context.Context) (float64, error) {
	return 0, fmt.Errorf("user CPU sampling not supported on %s", runtime.GOOS)
}
