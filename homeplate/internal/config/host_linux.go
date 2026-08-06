//go:build linux

package config

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// HostMemoryBytes reads MemTotal from /proc/meminfo.
func HostMemoryBytes() int64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 8 << 30
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			break
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			break
		}
		return kb * 1024
	}
	return 8 << 30
}
