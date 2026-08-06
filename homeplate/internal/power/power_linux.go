//go:build linux

package power

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// Read inspects /sys/class/power_supply, the kernel's stable interface.
func Read(ctx context.Context) State {
	s := State{Source: SourceUnknown, BatteryPercent: -1}
	entries, err := os.ReadDir("/sys/class/power_supply")
	if err != nil {
		// No power supply class at all: almost certainly a server/container.
		return State{Source: SourceAC, BatteryPercent: -1}
	}
	sawAC, acOnline := false, false
	for _, e := range entries {
		base := filepath.Join("/sys/class/power_supply", e.Name())
		typ := strings.TrimSpace(readFile(filepath.Join(base, "type")))
		switch typ {
		case "Mains", "USB", "ADP":
			sawAC = true
			if strings.TrimSpace(readFile(filepath.Join(base, "online"))) == "1" {
				acOnline = true
			}
		case "Battery":
			s.HasBattery = true
			if v, err := strconv.Atoi(strings.TrimSpace(readFile(filepath.Join(base, "capacity")))); err == nil {
				s.BatteryPercent = v
			}
		}
	}
	switch {
	case !s.HasBattery:
		s.Source = SourceAC
	case sawAC && acOnline:
		s.Source = SourceAC
	case sawAC && !acOnline:
		s.Source = SourceBattery
	}
	return s
}

func readFile(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return string(b)
}

// inhibitAssertion holds a systemd-inhibit lock.
//
// Linux is genuinely better than macOS here: logind exposes `handle-lid-switch`
// as an inhibitable event, so a userspace process CAN legitimately block
// lid-close suspend - no private entitlement, no root. That is why Homeplate's
// clamshell verdict can be YES on Linux while being honest about NO on a
// MacBook without an external display.
//
//	idle              - block idle-triggered sleep
//	sleep             - block explicit suspend requests
//	handle-lid-switch - block logind suspending on lid close
//	shutdown          - block shutdown while a job is mid-flight
type inhibitAssertion struct {
	cmd  *exec.Cmd
	once sync.Once
}

// InhibitWhat is the event set Homeplate blocks while jobs are running.
const InhibitWhat = "idle:sleep:handle-lid-switch:shutdown"

func acquireAssertion(reason string) (assertion, error) {
	if _, err := exec.LookPath("systemd-inhibit"); err != nil {
		return nil, err
	}
	if reason == "" {
		reason = "Homeplate is running CI jobs"
	}
	cmd := exec.Command("systemd-inhibit",
		"--what="+InhibitWhat,
		"--who=homeplate",
		"--why="+reason,
		"--mode=block",
		"sleep", "infinity")
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() { _ = cmd.Wait() }()
	return &inhibitAssertion{cmd: cmd}, nil
}

func (i *inhibitAssertion) release() error {
	var err error
	i.once.Do(func() {
		if i.cmd != nil && i.cmd.Process != nil {
			err = i.cmd.Process.Kill()
		}
	})
	return err
}

func (i *inhibitAssertion) describe() string {
	pid := 0
	if i.cmd != nil && i.cmd.Process != nil {
		pid = i.cmd.Process.Pid
	}
	return "systemd-inhibit --what=" + InhibitWhat + " (pid " + strconv.Itoa(pid) + ")"
}

func assertionMechanism() string {
	return "systemd-inhibit --what=" + InhibitWhat + " --mode=block"
}

func clamshell(ctx context.Context, s State, holdEnabled bool) ClamshellVerdict {
	if !s.HasBattery {
		return ClamshellVerdict{WillKeepRunning: true, Reason: "no lid detected (desktop or server)"}
	}
	if !holdEnabled {
		return ClamshellVerdict{
			WillKeepRunning: false,
			Reason:          "hold_sleep_assertion is disabled in config.toml",
			Remedy:          "homeplate limit --hold-sleep true",
		}
	}
	// logind honours a `handle-lid-switch` inhibitor lock, which Homeplate takes
	// while work exists. This is a real, supported override - unlike macOS.
	if handle := logindLidSetting(); handle == "ignore" {
		return ClamshellVerdict{
			WillKeepRunning: true,
			Reason:          "logind HandleLidSwitch=ignore, so closing the lid does nothing",
		}
	}
	if !s.OnAC() {
		return ClamshellVerdict{
			WillKeepRunning: true,
			Reason: "Homeplate holds a systemd-inhibit lock including handle-lid-switch while jobs " +
				"run, so logind will not suspend on lid close. Note you are on battery, so the " +
				"battery policy may pause new job pickup first",
		}
	}
	return ClamshellVerdict{
		WillKeepRunning: true,
		Reason: "on AC with a systemd-inhibit lock covering handle-lid-switch; logind will not " +
			"suspend on lid close while a job is running",
	}
}

func logindLidSetting() string {
	for _, p := range []string{"/etc/systemd/logind.conf", "/etc/systemd/logind.conf.d/homeplate.conf"} {
		for _, line := range strings.Split(readFile(p), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "HandleLidSwitch=") {
				return strings.TrimPrefix(line, "HandleLidSwitch=")
			}
		}
	}
	return "suspend (systemd default)"
}

func platformNotes() []string {
	return []string{
		"Linux: systemd-inhibit blocks idle sleep, explicit suspend, and lid-close suspend.",
		"Linux: an inhibitor lock only applies while Homeplate holds it, i.e. while work exists. " +
			"With an empty queue the machine sleeps normally, which is intended.",
		"Linux: for unconditional lid-open-forever behaviour, set HandleLidSwitch=ignore in " +
			"/etc/systemd/logind.conf.",
	}
}
