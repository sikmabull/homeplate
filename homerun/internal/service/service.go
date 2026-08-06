// Package service installs Homerun's daemon as a user-level background service.
//
// User-level, not system-level, on purpose: the daemon needs the user's
// keychain (macOS refuses keychain access to LaunchDaemons running as root),
// their Docker socket, and their git credentials. Running as root would break
// all three and grant the daemon far more privilege than it needs.
package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Label is the launchd label / systemd unit name.
const Label = "com.homerun.daemon"

// UnitName is the systemd user unit filename.
const UnitName = "homerun.service"

// Installer manages the platform service.
type Installer struct {
	// BinPath is the absolute path to the homerun binary.
	BinPath string
	// HomeDir is ~/.homerun.
	HomeDir string
}

// New builds an installer for the current executable.
func New(homeDir string) (*Installer, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return nil, err
	}
	return &Installer{BinPath: exe, HomeDir: homeDir}, nil
}

// Path returns the service definition file location.
func (i *Installer) Path() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "LaunchAgents", Label+".plist"), nil
	case "linux":
		return filepath.Join(home, ".config", "systemd", "user", UnitName), nil
	default:
		return "", fmt.Errorf("service installation is not supported on %s", runtime.GOOS)
	}
}

const plistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>

    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>daemon</string>
        <string>run</string>
    </array>

    <!-- Start at login and keep it running. -->
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <dict>
        <key>SuccessfulExit</key>
        <false/>
    </dict>

    <!-- Do not hammer the CPU if the daemon crash-loops. -->
    <key>ThrottleInterval</key>
    <integer>30</integer>

    <!-- Homerun jobs are background work; this keeps the Mac responsive.
         It does NOT cap CPU (that is Docker's job), it lowers scheduling
         priority so interactive apps always win. -->
    <key>ProcessType</key>
    <string>Background</string>
    <key>LowPriorityIO</key>
    <true/>
    <key>Nice</key>
    <integer>5</integer>

    <key>WorkingDirectory</key>
    <string>%s</string>
    <key>StandardOutPath</key>
    <string>%s</string>
    <key>StandardErrorPath</key>
    <string>%s</string>

    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>%s</string>
        <key>HOMERUN_HOME</key>
        <string>%s</string>
    </dict>
</dict>
</plist>
`

const systemdTemplate = `[Unit]
Description=Homerun - local GitHub Actions runner
Documentation=https://github.com/homerun-ci/homerun
After=network-online.target docker.service
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s daemon run
Restart=always
RestartSec=30
WorkingDirectory=%s
Environment=HOMERUN_HOME=%s
Environment=PATH=%s

# Background priority: builds must not make the desktop stutter.
Nice=5
IOSchedulingClass=idle
CPUSchedulingPolicy=batch

# The daemon itself needs very little; the JOB isolation is Docker's job.
NoNewPrivileges=true
PrivateTmp=true

[Install]
WantedBy=default.target
`

// Render produces the service definition text without touching the system.
// Kept separate from Install so the unit content is testable without loading
// anything into a developer's real launchd/systemd.
func (i *Installer) Render() (string, error) {
	logDir := filepath.Join(i.HomeDir, "logs")
	pathEnv := servicePATH()

	switch runtime.GOOS {
	case "darwin":
		return fmt.Sprintf(plistTemplate, Label, i.BinPath, i.HomeDir,
			filepath.Join(logDir, "daemon.out.log"),
			filepath.Join(logDir, "daemon.err.log"),
			pathEnv, i.HomeDir), nil
	case "linux":
		return fmt.Sprintf(systemdTemplate, i.BinPath, i.HomeDir, i.HomeDir, pathEnv), nil
	default:
		return "", fmt.Errorf("unsupported platform %s", runtime.GOOS)
	}
}

// servicePATH builds a PATH that includes Homebrew and Docker Desktop, which
// are outside launchd's minimal default PATH. Without this the daemon starts
// but cannot find `docker` or `act`, which looks like a Homerun bug.
func servicePATH() string {
	pathEnv := os.Getenv("PATH")
	if pathEnv == "" {
		pathEnv = "/usr/local/bin:/usr/bin:/bin"
	}
	for _, extra := range []string{"/usr/local/bin", "/opt/homebrew/bin"} {
		if !strings.Contains(pathEnv, extra) {
			pathEnv = extra + ":" + pathEnv
		}
	}
	return pathEnv
}

// Install writes the service definition and loads it.
func (i *Installer) Install() (path string, err error) {
	path, err = i.Path()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	logDir := filepath.Join(i.HomeDir, "logs")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return "", err
	}

	content, err := i.Render()
	if err != nil {
		return "", err
	}

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", err
	}
	return path, i.load(path)
}

func (i *Installer) load(path string) error {
	switch runtime.GOOS {
	case "darwin":
		// bootout first so re-installing picks up a changed plist. Failure is
		// expected on a first install, so it is ignored deliberately.
		uid := os.Getuid()
		domain := fmt.Sprintf("gui/%d", uid)
		_ = exec.Command("launchctl", "bootout", domain+"/"+Label).Run()
		if out, err := exec.Command("launchctl", "bootstrap", domain, path).CombinedOutput(); err != nil {
			// Older macOS uses `load`; try it before giving up.
			if out2, err2 := exec.Command("launchctl", "load", "-w", path).CombinedOutput(); err2 != nil {
				return fmt.Errorf("launchctl bootstrap failed: %s / load failed: %s",
					strings.TrimSpace(string(out)), strings.TrimSpace(string(out2)))
			}
		}
		_ = exec.Command("launchctl", "kickstart", "-k", domain+"/"+Label).Run()
		return nil
	case "linux":
		if out, err := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
			return fmt.Errorf("systemctl daemon-reload: %s", strings.TrimSpace(string(out)))
		}
		if out, err := exec.Command("systemctl", "--user", "enable", "--now", UnitName).CombinedOutput(); err != nil {
			return fmt.Errorf("systemctl enable --now: %s", strings.TrimSpace(string(out)))
		}
		// Without lingering, the user unit dies at logout, which defeats
		// "always on". This needs root, so it is advisory rather than fatal.
		_ = exec.Command("loginctl", "enable-linger", os.Getenv("USER")).Run()
		return nil
	}
	return nil
}

// Uninstall stops and removes the service.
func (i *Installer) Uninstall() error {
	path, err := i.Path()
	if err != nil {
		return err
	}
	switch runtime.GOOS {
	case "darwin":
		_ = exec.Command("launchctl", "bootout", fmt.Sprintf("gui/%d/%s", os.Getuid(), Label)).Run()
	case "linux":
		_ = exec.Command("systemctl", "--user", "disable", "--now", UnitName).Run()
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Status reports whether the service is loaded and running.
func (i *Installer) Status() (installed bool, running bool, detail string) {
	path, err := i.Path()
	if err != nil {
		return false, false, err.Error()
	}
	if _, err := os.Stat(path); err != nil {
		return false, false, "not installed (run `homerun init` or `homerun service install`)"
	}
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.Command("launchctl", "print", fmt.Sprintf("gui/%d/%s", os.Getuid(), Label)).CombinedOutput()
		if err != nil {
			return true, false, "installed but not loaded by launchd"
		}
		text := string(out)
		if strings.Contains(text, "state = running") {
			return true, true, "running under launchd"
		}
		if idx := strings.Index(text, "last exit code = "); idx >= 0 {
			line := text[idx : idx+40]
			return true, false, "loaded but not running; " + strings.TrimSpace(strings.Split(line, "\n")[0])
		}
		return true, false, "loaded, not currently running"
	case "linux":
		out, _ := exec.Command("systemctl", "--user", "is-active", UnitName).Output()
		state := strings.TrimSpace(string(out))
		return true, state == "active", "systemd user unit is " + state
	}
	return false, false, "unsupported platform"
}
