// Package power handles battery/AC awareness and sleep suppression.
//
// Homerun makes two distinct promises, and it is important not to conflate them:
//
//  1. "Don't idle-sleep while work exists" - achievable on both platforms
//     with a sleep assertion (caffeinate on macOS, systemd-inhibit on Linux).
//  2. "Keep running with the lid closed" - NOT universally achievable on
//     macOS. Report(), not marketing copy, is the source of truth.
package power

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Source is where the machine's power comes from.
type Source string

const (
	SourceAC      Source = "ac"
	SourceBattery Source = "battery"
	SourceUnknown Source = "unknown"
)

// State is a point-in-time power snapshot.
type State struct {
	Source Source
	// BatteryPercent is 0-100, or -1 when there is no battery (desktop).
	BatteryPercent int
	// HasBattery is false on desktops/servers.
	HasBattery bool
	// LidClosed is best-effort; false when undetectable.
	LidClosed bool
	// ExternalDisplay is best-effort and matters for macOS clamshell.
	ExternalDisplay bool
}

// OnAC is a convenience predicate.
func (s State) OnAC() bool { return s.Source == SourceAC || !s.HasBattery }

// ClamshellVerdict is the honest answer to "will jobs keep running with the
// lid closed right now?"
type ClamshellVerdict struct {
	// WillKeepRunning is the headline YES/NO shown by `homerun status`.
	WillKeepRunning bool
	// Reason is a plain-English explanation, always populated.
	Reason string
	// Remedy is an actionable suggestion when WillKeepRunning is false.
	Remedy string
}

// String renders the verdict the way `homerun status` prints it.
func (v ClamshellVerdict) String() string {
	yn := "NO"
	if v.WillKeepRunning {
		yn = "YES"
	}
	s := fmt.Sprintf("%s because %s", yn, v.Reason)
	if !v.WillKeepRunning && v.Remedy != "" {
		s += "\n    fix: " + v.Remedy
	}
	return s
}

// Manager owns the platform sleep assertion and reference-counts holders, so
// concurrent jobs share a single assertion and the last one to finish releases it.
type Manager struct {
	mu       sync.Mutex
	holds    int
	assert   assertion
	reason   string
	acquired time.Time
}

// assertion is the platform-specific sleep suppression handle.
type assertion interface {
	release() error
	describe() string
}

// NewManager creates a sleep-assertion manager.
func NewManager() *Manager { return &Manager{} }

// Hold acquires (or increments) the sleep assertion. The returned release
// function is safe to call multiple times.
func (m *Manager) Hold(reason string) (release func(), err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.holds == 0 {
		a, err := acquireAssertion(reason)
		if err != nil {
			return func() {}, err
		}
		m.assert = a
		m.reason = reason
		m.acquired = time.Now()
	}
	m.holds++

	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			m.holds--
			if m.holds <= 0 {
				m.holds = 0
				if m.assert != nil {
					_ = m.assert.release()
					m.assert = nil
					m.reason = ""
				}
			}
		})
	}, nil
}

// Held reports whether a sleep assertion is currently active.
func (m *Manager) Held() (bool, string, time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.holds == 0 || m.assert == nil {
		return false, "", 0
	}
	return true, m.assert.describe(), time.Since(m.acquired)
}

// ReleaseAll drops the assertion regardless of refcount (daemon shutdown).
func (m *Manager) ReleaseAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.holds = 0
	if m.assert != nil {
		_ = m.assert.release()
		m.assert = nil
	}
}

// Policy decides whether new jobs may be picked up.
type Policy struct {
	// PauseBelowPct pauses pickup on battery below this level. 0 disables.
	PauseBelowPct int
	// RunOnBattery permits any pickup while unplugged.
	RunOnBattery bool
}

// Decision is the outcome of evaluating Policy against State.
type Decision struct {
	Allow  bool
	Reason string
}

// Evaluate applies the battery policy. Running jobs are never killed by this;
// it gates *pickup* only, so a job started on AC finishes even if unplugged.
func (p Policy) Evaluate(s State) Decision {
	if !s.HasBattery || s.Source == SourceAC {
		return Decision{Allow: true, Reason: "on AC power"}
	}
	if s.Source == SourceUnknown {
		// Fail open, but say so: refusing all work because we cannot read a
		// battery would be worse than running.
		return Decision{Allow: true, Reason: "power source undetectable; assuming it is safe to run"}
	}
	if !p.RunOnBattery {
		return Decision{Allow: false, Reason: "on battery and run_on_battery = false"}
	}
	if p.PauseBelowPct > 0 && s.BatteryPercent >= 0 && s.BatteryPercent < p.PauseBelowPct {
		return Decision{
			Allow: false,
			Reason: fmt.Sprintf("battery %d%% is below pause_below_battery_pct = %d%% and unplugged",
				s.BatteryPercent, p.PauseBelowPct),
		}
	}
	return Decision{Allow: true, Reason: fmt.Sprintf("on battery at %d%%", s.BatteryPercent)}
}

// Report bundles everything `homerun status` and `homerun doctor` print.
type Report struct {
	State     State
	Clamshell ClamshellVerdict
	Assertion string
	Notes     []string
}

// Describe builds the full power report for this host.
func Describe(ctx context.Context, holdEnabled bool) Report {
	st := Read(ctx)
	r := Report{State: st, Clamshell: clamshell(ctx, st, holdEnabled)}
	r.Assertion = assertionMechanism()
	r.Notes = platformNotes()
	return r
}

// FormatState renders a one-line power summary.
func FormatState(s State) string {
	var b strings.Builder
	switch s.Source {
	case SourceAC:
		b.WriteString("AC power")
	case SourceBattery:
		b.WriteString("battery")
	default:
		b.WriteString("unknown power source")
	}
	if s.HasBattery && s.BatteryPercent >= 0 {
		fmt.Fprintf(&b, " (%d%%)", s.BatteryPercent)
	}
	return b.String()
}
