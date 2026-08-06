package power

import (
	"strings"
	"testing"
)

// TestBatteryPolicyPausesBelowThreshold is the documented battery behaviour:
// pause pickup below 20% when unplugged, resume on AC.
func TestBatteryPolicyPausesBelowThreshold(t *testing.T) {
	pol := Policy{PauseBelowPct: 20, RunOnBattery: true}

	cases := []struct {
		name  string
		state State
		allow bool
	}{
		{"AC at 5% still runs", State{Source: SourceAC, BatteryPercent: 5, HasBattery: true}, true},
		{"battery at 50% runs", State{Source: SourceBattery, BatteryPercent: 50, HasBattery: true}, true},
		{"battery at 20% runs (threshold is exclusive)", State{Source: SourceBattery, BatteryPercent: 20, HasBattery: true}, true},
		{"battery at 19% pauses", State{Source: SourceBattery, BatteryPercent: 19, HasBattery: true}, false},
		{"battery at 1% pauses", State{Source: SourceBattery, BatteryPercent: 1, HasBattery: true}, false},
		{"desktop always runs", State{Source: SourceAC, HasBattery: false, BatteryPercent: -1}, true},
	}
	for _, c := range cases {
		got := pol.Evaluate(c.state)
		if got.Allow != c.allow {
			t.Errorf("%s: Allow = %v, want %v (reason: %s)", c.name, got.Allow, c.allow, got.Reason)
		}
		if got.Reason == "" {
			t.Errorf("%s: every decision must carry a reason for `homerun status`", c.name)
		}
	}
}

// TestRunOnBatteryDisabled covers the stricter opt-out.
func TestRunOnBatteryDisabled(t *testing.T) {
	pol := Policy{PauseBelowPct: 20, RunOnBattery: false}
	d := pol.Evaluate(State{Source: SourceBattery, BatteryPercent: 100, HasBattery: true})
	if d.Allow {
		t.Error("run_on_battery=false must pause even at 100%")
	}
	if !strings.Contains(d.Reason, "run_on_battery") {
		t.Errorf("reason should name the setting, got %q", d.Reason)
	}
}

// TestUnknownPowerFailsOpen: refusing all work because a battery is
// unreadable would be worse than running.
func TestUnknownPowerFailsOpen(t *testing.T) {
	pol := Policy{PauseBelowPct: 20, RunOnBattery: true}
	d := pol.Evaluate(State{Source: SourceUnknown, BatteryPercent: -1, HasBattery: true})
	if !d.Allow {
		t.Error("unknown power state should fail open")
	}
	if !strings.Contains(strings.ToLower(d.Reason), "undetectable") {
		t.Errorf("the uncertainty must be disclosed, got %q", d.Reason)
	}
}

// TestZeroThresholdDisablesCheck covers the documented "0 disables" rule.
func TestZeroThresholdDisablesCheck(t *testing.T) {
	pol := Policy{PauseBelowPct: 0, RunOnBattery: true}
	if !pol.Evaluate(State{Source: SourceBattery, BatteryPercent: 2, HasBattery: true}).Allow {
		t.Error("pause_below_battery_pct=0 must disable the battery gate")
	}
}

// TestClamshellVerdictAlwaysExplains enforces the honesty requirement that
// `homerun status` prints YES/NO *because ...*, never a bare claim.
func TestClamshellVerdictAlwaysExplains(t *testing.T) {
	cases := []ClamshellVerdict{
		{WillKeepRunning: true, Reason: "on AC"},
		{WillKeepRunning: false, Reason: "on battery", Remedy: "plug in"},
	}
	for _, v := range cases {
		s := v.String()
		if !strings.Contains(s, "because") {
			t.Errorf("verdict %q must explain itself", s)
		}
		if v.WillKeepRunning && !strings.HasPrefix(s, "YES") {
			t.Errorf("expected YES prefix, got %q", s)
		}
		if !v.WillKeepRunning {
			if !strings.HasPrefix(s, "NO") {
				t.Errorf("expected NO prefix, got %q", s)
			}
			if v.Remedy != "" && !strings.Contains(s, "fix:") {
				t.Errorf("a NO verdict with a remedy must show the fix, got %q", s)
			}
		}
	}
}

// TestSleepAssertionRefcounts ensures concurrent jobs share one assertion and
// the last release actually drops it.
func TestSleepAssertionRefcounts(t *testing.T) {
	m := NewManager()

	r1, err := m.Hold("job 1")
	if err != nil {
		t.Skipf("cannot acquire a sleep assertion in this environment: %v", err)
	}
	held, _, _ := m.Held()
	if !held {
		t.Fatal("assertion not held after first Hold")
	}

	r2, err := m.Hold("job 2")
	if err != nil {
		t.Fatal(err)
	}

	r1()
	if held, _, _ = m.Held(); !held {
		t.Error("assertion released while a second holder still exists")
	}

	r1() // double release must be safe
	if held, _, _ = m.Held(); !held {
		t.Error("a duplicate release dropped the assertion early")
	}

	r2()
	if held, _, _ = m.Held(); held {
		t.Error("assertion still held after the last release")
	}
}

// TestFormatState renders a readable power summary.
func TestFormatState(t *testing.T) {
	if got := FormatState(State{Source: SourceAC, HasBattery: true, BatteryPercent: 87}); got != "AC power (87%)" {
		t.Errorf("FormatState = %q", got)
	}
	if got := FormatState(State{Source: SourceBattery, HasBattery: true, BatteryPercent: 42}); got != "battery (42%)" {
		t.Errorf("FormatState = %q", got)
	}
}

// TestOnACTreatsDesktopsCorrectly: a machine with no battery is always "on AC".
func TestOnACTreatsDesktopsCorrectly(t *testing.T) {
	if !(State{Source: SourceUnknown, HasBattery: false}).OnAC() {
		t.Error("a battery-less machine must be treated as on AC")
	}
}
