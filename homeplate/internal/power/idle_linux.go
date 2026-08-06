//go:build linux

package power

import (
	"context"
	"os"
	"strconv"
	"strings"
	"time"
)

// userCPUPercent reads /proc/stat twice 1s apart and computes the user-space
// (non-nice) CPU share of the delta.
func userCPUPercent(ctx context.Context) (float64, error) {
	u1, t1, err := readStat()
	if err != nil {
		return 0, err
	}
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-time.After(1 * time.Second):
	}
	u2, t2, err := readStat()
	if err != nil {
		return 0, err
	}
	if t2 <= t1 {
		return 0, nil
	}
	return 100 * float64(u2-u1) / float64(t2-t1), nil
}

func readStat() (user, total uint64, err error) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		f := strings.Fields(line)[1:]
		var vals []uint64
		for _, s := range f {
			v, _ := strconv.ParseUint(s, 10, 64)
			vals = append(vals, v)
			total += v
		}
		if len(vals) > 0 {
			user = vals[0] // user, excluding nice (vals[1])
		}
		return user, total, nil
	}
	return 0, 0, strconv.ErrSyntax
}
