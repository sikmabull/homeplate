//go:build !darwin && !linux

package power

import "context"

// Read cannot determine power state on unsupported platforms.
func Read(ctx context.Context) State {
	return State{Source: SourceUnknown, BatteryPercent: -1}
}

type noopAssertion struct{}

func (noopAssertion) release() error   { return nil }
func (noopAssertion) describe() string { return "none (unsupported platform)" }

func acquireAssertion(reason string) (assertion, error) { return noopAssertion{}, nil }

func assertionMechanism() string { return "none (unsupported platform)" }

func clamshell(ctx context.Context, s State, holdEnabled bool) ClamshellVerdict {
	return ClamshellVerdict{
		WillKeepRunning: false,
		Reason:          "this platform has no supported sleep-suppression mechanism",
	}
}

func platformNotes() []string {
	return []string{"Sleep suppression is implemented for macOS and Linux only."}
}
