// idle.go - host "is the human busy?" sampling for only_when_idle.
//
// There is no portable "user is typing" signal without cgo, so Homeplate uses
// the spec's proxy: sustained USER-space CPU. Interactive use (browsers,
// editors, video calls) shows up as user CPU; a machine running only
// Homeplate's niced/containerised jobs shows up mostly as nice/system time.
package power

import (
	"context"
	"sync"
	"time"
)

// BusyTracker keeps a sliding record of host user-CPU samples and answers
// "has user CPU been above threshold continuously for the last d?".
type BusyTracker struct {
	Threshold float64 // percent, e.g. 40

	mu        sync.Mutex
	busySince time.Time // zero = currently below threshold
	ready     bool
}

// NewBusyTracker builds a tracker for a threshold in percent.
func NewBusyTracker(thresholdPct int) *BusyTracker {
	t := &BusyTracker{Threshold: float64(thresholdPct)}
	if t.Threshold <= 0 {
		t.Threshold = 40
	}
	return t
}

// Sample takes one reading. Safe to call on any cadence; every 15-30s is
// plenty. Sampling errors are treated as "not busy" (fail open).
func (t *BusyTracker) Sample(ctx context.Context) {
	pct, err := userCPUPercent(ctx)
	t.mu.Lock()
	defer t.mu.Unlock()
	if err != nil {
		t.busySince = time.Time{}
		t.ready = true
		return
	}
	t.ready = true
	if pct >= t.Threshold {
		if t.busySince.IsZero() {
			t.busySince = time.Now()
		}
	} else {
		t.busySince = time.Time{}
	}
}

// BusyFor reports how long user CPU has continuously exceeded the threshold.
// Zero means "not currently busy".
func (t *BusyTracker) BusyFor() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.busySince.IsZero() {
		return 0
	}
	return time.Since(t.busySince)
}

// ShouldYield implements only_when_idle: true when the human has been busy
// for at least sustained.
func (t *BusyTracker) ShouldYield(sustained time.Duration) bool {
	if sustained <= 0 {
		sustained = 5 * time.Minute
	}
	return t.BusyFor() >= sustained
}
