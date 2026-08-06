//go:build darwin

package power

import (
	"context"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// Read shells out to pmset, which is the supported, stable way to query power
// state without cgo. Output format:
//
//	Now drawing from 'AC Power'
//	 -InternalBattery-0 (id=...)  100%; charged; 0:00 remaining present: true
func Read(ctx context.Context) State {
	s := State{Source: SourceUnknown, BatteryPercent: -1}

	out, err := exec.CommandContext(ctx, "pmset", "-g", "batt").Output()
	if err != nil {
		return s
	}
	text := string(out)

	switch {
	case strings.Contains(text, "'AC Power'"):
		s.Source = SourceAC
	case strings.Contains(text, "'Battery Power'"):
		s.Source = SourceBattery
	}

	if m := regexp.MustCompile(`(\d{1,3})%`).FindStringSubmatch(text); m != nil {
		if pct, err := strconv.Atoi(m[1]); err == nil {
			s.BatteryPercent = pct
			s.HasBattery = true
		}
	}
	if strings.Contains(text, "InternalBattery") {
		s.HasBattery = true
	}

	s.LidClosed = lidClosed(ctx)
	s.ExternalDisplay = hasExternalDisplay(ctx)
	return s
}

// lidClosed reads AppleClamshellState from IORegistry. This is a documented
// IORegistry property, but it is absent on desktops, so absence means "no lid".
func lidClosed(ctx context.Context) bool {
	out, err := exec.CommandContext(ctx, "ioreg", "-r", "-k", "AppleClamshellState", "-d", "4").Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "AppleClamshellState") {
			return strings.Contains(line, "Yes") || strings.Contains(line, "true")
		}
	}
	return false
}

// hasExternalDisplay counts attached displays. On Apple Silicon laptops,
// clamshell operation historically required an external display; on modern
// macOS with an AC connection and a held assertion it usually does not, but we
// report what we can observe rather than assert a guarantee.
func hasExternalDisplay(ctx context.Context) bool {
	out, err := exec.CommandContext(ctx, "system_profiler", "-json", "SPDisplaysDataType").Output()
	if err != nil {
		return false
	}
	text := string(out)
	// Built-in panels are tagged spdisplays_display_type = built-in.
	total := strings.Count(text, "_spdisplays_displayID") + strings.Count(text, "spdisplays_display-serial-number")
	builtin := strings.Count(text, "spdisplays_builtin") + strings.Count(text, "Built-In")
	return total > builtin && total > 1
}

// caffeinateAssertion holds `caffeinate -s`, which maps to the IOKit
// PreventSystemSleep assertion. We deliberately shell out instead of using
// cgo + IOPMAssertionCreateWithName so that Homeplate stays a pure-Go,
// cross-compilable single binary. caffeinate(8) is part of the base OS.
//
//	-s  prevent system sleep (only effective on AC power)
//	-i  prevent idle sleep
//	-m  prevent disk idle sleep
//
// We use -s -i -m together: -s is the one that matters for long jobs, -i keeps
// the machine responsive, -m avoids disk spin-down stalls mid-build.
type caffeinateAssertion struct {
	cmd  *exec.Cmd
	once sync.Once
}

func acquireAssertion(reason string) (assertion, error) {
	cmd := exec.Command("caffeinate", "-s", "-i", "-m")
	cmd.Stdout = nil
	cmd.Stderr = nil
	// Detach stdin so caffeinate does not inherit a terminal.
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	// Reap the child when it exits so it does not become a zombie.
	a := &caffeinateAssertion{cmd: cmd}
	go func() { _ = cmd.Wait() }()
	return a, nil
}

func (c *caffeinateAssertion) release() error {
	var err error
	c.once.Do(func() {
		if c.cmd != nil && c.cmd.Process != nil {
			err = c.cmd.Process.Kill()
		}
	})
	return err
}

func (c *caffeinateAssertion) describe() string {
	pid := 0
	if c.cmd != nil && c.cmd.Process != nil {
		pid = c.cmd.Process.Pid
	}
	return "caffeinate -s -i -m (pid " + strconv.Itoa(pid) + ")"
}

func assertionMechanism() string { return "caffeinate(8) -s -i -m (IOKit PreventSystemSleep)" }

// clamshell gives the honest per-host answer.
func clamshell(ctx context.Context, s State, holdEnabled bool) ClamshellVerdict {
	if !s.HasBattery {
		return ClamshellVerdict{
			WillKeepRunning: true,
			Reason:          "this Mac has no lid (desktop); sleep assertion is all that is needed",
		}
	}
	if !holdEnabled {
		return ClamshellVerdict{
			WillKeepRunning: false,
			Reason:          "hold_sleep_assertion is disabled in config.toml",
			Remedy:          "homeplate limit --hold-sleep true",
		}
	}
	if disableSleepActive(ctx) {
		return ClamshellVerdict{
			WillKeepRunning: true,
			Reason:          "pmset disablesleep is active system-wide, so the lid switch is ignored",
		}
	}
	if !s.OnAC() {
		return ClamshellVerdict{
			WillKeepRunning: false,
			Reason: "this is a MacBook on battery power. Closing the lid on battery sleeps " +
				"the machine regardless of any sleep assertion Homeplate can hold",
			Remedy: "plug in AC power (and attach an external display for true clamshell mode)",
		}
	}

	// THE HONEST ANSWER, and it is not the flattering one.
	//
	// A caffeinate/IOPMAssertion sleep assertion does NOT survive a lid close.
	// The kernel (IOPMrootDomain::shouldSleepOnClamshellClosed) sleeps on lid
	// close unless the machine is in "desktop mode" - which requires AC power
	// AND an external display - or unless the system-wide SleepDisabled bit is
	// set. Overriding sleep on lid close needs the Apple-private entitlement
	// com.apple.private.iokit.assertonlidclose, which third-party software
	// cannot obtain. Claiming otherwise would be exactly the kind of faked
	// behaviour this project refuses to ship.
	if s.ExternalDisplay {
		return ClamshellVerdict{
			WillKeepRunning: true,
			Reason: "on AC power with an external display attached, which puts this Mac in " +
				"clamshell (desktop) mode - the documented Apple configuration where closing " +
				"the lid does not sleep the machine",
		}
	}
	return ClamshellVerdict{
		WillKeepRunning: false,
		Reason: "on AC power, but with no external display. A sleep assertion (caffeinate -s) " +
			"prevents IDLE sleep only; it does not survive the lid switch. macOS requires " +
			"AC power AND an external display to enter clamshell mode",
		Remedy: "attach an external display (Apple's documented clamshell requirement: AC + " +
			"external display + external keyboard/mouse), OR keep the lid open, OR run " +
			"`homeplate power setup` then set allow_clamshell_pmset = true for the managed " +
			"lid-close toggle (system-wide while jobs run, auto-reverted after)",
	}
}

// disableSleepActive checks the global pmset override.
func disableSleepActive(ctx context.Context) bool {
	out, err := exec.CommandContext(ctx, "pmset", "-g").Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "SleepDisabled" {
			return f[1] == "1"
		}
	}
	return false
}

func platformNotes() []string {
	notes := []string{
		"macOS: `caffeinate -s` prevents IDLE sleep; it does NOT override the lid switch.",
		"macOS: lid-closed operation requires clamshell mode = AC power + external display " +
			"(+ external keyboard/mouse), per Apple's documented requirements.",
		"macOS: no third-party process can hold a lid-close sleep override; that needs the " +
			"Apple-private entitlement com.apple.private.iokit.assertonlidclose.",
	}
	if os.Geteuid() != 0 {
		notes = append(notes,
			"macOS: `sudo pmset -a disablesleep 1` does block lid-close sleep (it sets the "+
				"system-wide SleepDisabled bit), but it is undocumented, requires root, and "+
				"applies to the whole system. Homeplate manages it as a toggle - set while "+
				"jobs run, reverted after - and only with allow_clamshell_pmset = true plus "+
				"the one-time sudoers helper (`homeplate power setup`).")
	}
	return notes
}
