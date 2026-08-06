//go:build darwin

package power

import (
	"context"
	"os/exec"
	"regexp"
	"strconv"
)

// userCPUPercent parses `top -l 2`: the first sample on macOS is cumulative
// since boot, the second is a real interval. Output line looks like:
//
//	CPU usage: 12.34% user, 5.67% sys, 81.98% idle
var reCPUUser = regexp.MustCompile(`([\d.]+)% user`)

func userCPUPercent(ctx context.Context) (float64, error) {
	out, err := exec.CommandContext(ctx, "top", "-l", "2", "-n", "0", "-s", "2").Output()
	if err != nil {
		return 0, err
	}
	// Use the LAST CPU usage line (the interval sample, not the boot average).
	var last string
	for _, m := range reCPUUser.FindAllStringSubmatch(string(out), -1) {
		last = m[1]
	}
	if last == "" {
		return 0, exec.ErrNotFound
	}
	return strconv.ParseFloat(last, 64)
}
