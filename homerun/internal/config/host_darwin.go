//go:build darwin

package config

import (
	"os/exec"
	"strconv"
	"strings"
)

// HostMemoryBytes returns total physical RAM via sysctl hw.memsize.
func HostMemoryBytes() int64 {
	out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		return 8 << 30
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil || v <= 0 {
		return 8 << 30
	}
	return v
}
