// Package labels encodes GitHub's runner label routing rules and Homerun's
// strategy for getting existing workflows onto local hardware.
//
// # THE CORE CONSTRAINT
//
// GitHub reserves the hosted-runner label names (ubuntu-latest, ubuntu-24.04,
// macos-latest, windows-latest, ...). A self-hosted runner cannot claim them:
// GitHub resolves those names to its own hosted pool before self-hosted
// matching happens, and the runner registration API rejects/ignores attempts
// to add them. That means a workflow saying `runs-on: ubuntu-latest` CANNOT be
// silently intercepted by a self-hosted runner without changing the workflow.
//
// Homerun therefore does not pretend. It offers, in order of preference:
//
//  1. PROBE      - actually attempt the hosted label at registration and read
//     back what GitHub stored. Reality beats documentation.
//  2. ADOPT      - `homerun adopt` opens a PR rewriting `runs-on:` to a
//     Homerun label across all workflows (the honest, supported
//     path; one PR, then never again).
//  3. GROUP      - org-level runner groups, so every repo in the org can use
//     the Homerun labels without per-repo registration.
package labels

import (
	"fmt"
	"runtime"
	"sort"
	"strings"
)

// Homerun is the label every Homerun runner carries. Workflows opt in with
// `runs-on: homerun` (or [self-hosted, homerun]).
const Homerun = "homerun"

// SelfHosted is applied implicitly by the runner agent and cannot be removed.
const SelfHosted = "self-hosted"

// Reserved lists the GitHub-hosted label names that a self-hosted runner may
// not claim. Sourced from GitHub's hosted runner images list.
var Reserved = map[string]bool{
	"ubuntu-latest":       true,
	"ubuntu-24.04":        true,
	"ubuntu-22.04":        true,
	"ubuntu-20.04":        true,
	"ubuntu-24.04-arm":    true,
	"ubuntu-22.04-arm":    true,
	"windows-latest":      true,
	"windows-2025":        true,
	"windows-2022":        true,
	"windows-2019":        true,
	"windows-11-arm":      true,
	"macos-latest":        true,
	"macos-15":            true,
	"macos-14":            true,
	"macos-13":            true,
	"macos-latest-large":  true,
	"macos-13-large":      true,
	"macos-14-large":      true,
	"macos-15-large":      true,
	"macos-latest-xlarge": true,
	"macos-14-xlarge":     true,
	"macos-15-xlarge":     true,
}

// IsReserved reports whether a label belongs to GitHub's hosted pool.
func IsReserved(l string) bool { return Reserved[strings.ToLower(strings.TrimSpace(l))] }

// Class is the runner cost/OS class used for pricing and image selection.
type Class string

const (
	ClassLinux   Class = "linux"
	ClassMacOS   Class = "macos"
	ClassWindows Class = "windows"
)

// ClassOf maps a runs-on label (or label list) to a cost class. Unrecognised
// labels default to Linux, which is the conservative choice for the savings
// counter because Linux is the cheapest class.
func ClassOf(runsOn string) Class {
	s := strings.ToLower(runsOn)
	switch {
	case strings.Contains(s, "macos"), strings.Contains(s, "darwin"), strings.Contains(s, "osx"):
		return ClassMacOS
	case strings.Contains(s, "windows"), strings.Contains(s, "win-"):
		return ClassWindows
	default:
		return ClassLinux
	}
}

// Default returns the labels Homerun registers for this machine.
//
// The runner agent always adds `self-hosted`, the OS, and the architecture.
// Homerun adds:
//
//	homerun            - the opt-in label used by `homerun adopt`
//	homerun-<os>       - e.g. homerun-macos, for OS-specific targeting
//	homerun-<os>-<arch>
//
// It deliberately does NOT add reserved hosted labels by default; see Probe.
func Default(extra ...string) []string {
	osName := runtime.GOOS
	if osName == "darwin" {
		osName = "macos"
	}
	arch := runtime.GOARCH
	out := []string{
		Homerun,
		fmt.Sprintf("%s-%s", Homerun, osName),
		fmt.Sprintf("%s-%s-%s", Homerun, osName, arch),
	}
	for _, e := range extra {
		if e = strings.TrimSpace(e); e != "" {
			out = append(out, e)
		}
	}
	return Dedupe(out)
}

// Dedupe removes duplicates, preserving order.
func Dedupe(in []string) []string {
	seen := map[string]bool{}
	out := in[:0:0]
	for _, s := range in {
		k := strings.ToLower(strings.TrimSpace(s))
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, strings.TrimSpace(s))
	}
	return out
}

// Matches implements GitHub's runs-on matching rule: a runner is eligible when
// it carries EVERY label listed in runs-on. Extra runner labels are fine, so
// a runner is a superset matcher, never an exact matcher.
func Matches(runnerLabels, runsOn []string) bool {
	have := map[string]bool{}
	for _, l := range runnerLabels {
		have[strings.ToLower(strings.TrimSpace(l))] = true
	}
	if len(runsOn) == 0 {
		return false
	}
	for _, want := range runsOn {
		w := strings.ToLower(strings.TrimSpace(want))
		if w == "" {
			continue
		}
		if !have[w] {
			return false
		}
	}
	return true
}

// ProbeResult is what GitHub actually did with a requested label set.
type ProbeResult struct {
	// Requested is what Homerun asked for at registration.
	Requested []string
	// Stored is what GitHub reports back on the registered runner.
	Stored []string
	// HostedLabelAccepted is true if a reserved hosted label survived
	// registration. If GitHub ever permits this, Homerun will notice without
	// a code change.
	HostedLabelAccepted bool
	// Dropped lists requested labels GitHub refused to store.
	Dropped []string
}

// Probe compares requested vs stored labels to learn GitHub's real behaviour.
func Probe(requested, stored []string) ProbeResult {
	res := ProbeResult{Requested: requested, Stored: stored}
	have := map[string]bool{}
	for _, s := range stored {
		have[strings.ToLower(s)] = true
	}
	for _, r := range requested {
		if !have[strings.ToLower(r)] {
			res.Dropped = append(res.Dropped, r)
			continue
		}
		if IsReserved(r) {
			res.HostedLabelAccepted = true
		}
	}
	sort.Strings(res.Dropped)
	return res
}

// RoutingAdvice is the plain-English explanation printed by `homerun doctor`.
type RoutingAdvice struct {
	// Interceptable is true when existing workflows need no edits.
	Interceptable bool
	// Explanation states the current reality.
	Explanation string
	// NextStep is the recommended action.
	NextStep string
}

// Advise produces routing guidance for a repo given its workflow labels.
func Advise(workflowRunsOn []string, runnerLabels []string) RoutingAdvice {
	var reserved, custom []string
	for _, r := range workflowRunsOn {
		if IsReserved(r) {
			reserved = append(reserved, r)
		} else {
			custom = append(custom, r)
		}
	}

	// Any workflow already targeting our labels routes today.
	routable := 0
	for _, r := range custom {
		if Matches(runnerLabels, []string{r}) {
			routable++
		}
	}

	switch {
	case len(reserved) == 0 && routable > 0:
		return RoutingAdvice{
			Interceptable: true,
			Explanation:   "every job already targets labels this machine carries",
			NextStep:      "nothing to do",
		}
	case len(reserved) > 0:
		return RoutingAdvice{
			Interceptable: false,
			Explanation: fmt.Sprintf(
				"%d job target(s) use GitHub-hosted labels (%s). GitHub resolves those to its own "+
					"hosted pool; a self-hosted runner cannot claim them, so these jobs will keep "+
					"billing GitHub minutes",
				len(reserved), strings.Join(Dedupe(reserved), ", ")),
			NextStep: "run `homerun adopt <owner/repo>` to open a PR rewriting runs-on to `homerun`",
		}
	default:
		return RoutingAdvice{
			Interceptable: false,
			Explanation:   "no workflow jobs target labels this machine carries",
			NextStep:      "run `homerun adopt <owner/repo>`",
		}
	}
}

// RunsOnReplacement is the label expression `homerun adopt` writes into YAML.
// [self-hosted, homerun] is used rather than bare `homerun` because it is
// explicit at review time about where the job will execute.
func RunsOnReplacement(class Class) string {
	switch class {
	case ClassMacOS:
		return "[self-hosted, homerun, homerun-macos]"
	case ClassWindows:
		return "[self-hosted, homerun, homerun-windows]"
	default:
		return "[self-hosted, homerun, homerun-linux]"
	}
}
