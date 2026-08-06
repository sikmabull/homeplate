package labels

import (
	"strings"
	"testing"
)

// TestHostedLabelsAreReserved documents the platform rule Homeplate is built
// around: a self-hosted runner cannot claim GitHub's hosted labels.
func TestHostedLabelsAreReserved(t *testing.T) {
	for _, l := range []string{"ubuntu-latest", "UBUNTU-LATEST", "macos-latest", "windows-latest", "ubuntu-22.04"} {
		if !IsReserved(l) {
			t.Errorf("IsReserved(%q) = false; Homeplate would wrongly promise interception", l)
		}
	}
	for _, l := range []string{"homeplate", "self-hosted", "linux", "my-custom-label"} {
		if IsReserved(l) {
			t.Errorf("IsReserved(%q) = true; a usable label was wrongly excluded", l)
		}
	}
}

// TestDefaultLabelsNeverClaimHostedNames is the guardrail that stops a future
// change from silently registering a reserved label.
func TestDefaultLabelsNeverClaimHostedNames(t *testing.T) {
	for _, l := range Default() {
		if IsReserved(l) {
			t.Errorf("default label set includes reserved label %q", l)
		}
	}
	found := false
	for _, l := range Default() {
		if l == Homeplate {
			found = true
		}
	}
	if !found {
		t.Errorf("default labels must include %q so `homeplate adopt` output routes here", Homeplate)
	}
}

// TestMatchesIsSupersetSemantics encodes GitHub's runs-on rule: the runner
// must carry EVERY requested label; extra runner labels are fine.
func TestMatchesIsSupersetSemantics(t *testing.T) {
	runner := []string{"self-hosted", "linux", "x64", "homeplate", "homeplate-linux"}

	cases := []struct {
		runsOn []string
		want   bool
		why    string
	}{
		{[]string{"self-hosted"}, true, "single label the runner has"},
		{[]string{"self-hosted", "linux", "x64"}, true, "all labels present"},
		{[]string{"homeplate"}, true, "homeplate label"},
		{[]string{"self-hosted", "linux", "gpu"}, false, "runner lacks gpu"},
		{[]string{"ubuntu-latest"}, false, "hosted label is never carried"},
		{nil, false, "empty runs-on matches nothing"},
	}
	for _, c := range cases {
		if got := Matches(runner, c.runsOn); got != c.want {
			t.Errorf("Matches(%v) = %v, want %v (%s)", c.runsOn, got, c.want, c.why)
		}
	}
}

// TestAdoptRewritesHostedLabels covers the core of `homeplate adopt`.
func TestAdoptRewritesHostedLabels(t *testing.T) {
	in := `name: CI
on: [push]
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: make test
  mac:
    runs-on: macos-latest
    steps:
      - run: xcodebuild
`
	res := AdoptWorkflow(".github/workflows/ci.yml", in)
	if !res.Modified {
		t.Fatal("workflow with hosted labels was not modified")
	}
	if strings.Contains(res.Content, "runs-on: ubuntu-latest") {
		t.Error("ubuntu-latest was not rewritten")
	}
	if !strings.Contains(res.Content, "runs-on: [self-hosted, homeplate, homeplate-linux]") {
		t.Errorf("linux job not rewritten correctly:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, "runs-on: [self-hosted, homeplate, homeplate-macos]") {
		t.Errorf("macos job must map to the macos label:\n%s", res.Content)
	}
	// Indentation must survive: YAML is whitespace-significant.
	for _, line := range strings.Split(res.Content, "\n") {
		if strings.Contains(line, "runs-on:") && !strings.HasPrefix(line, "    runs-on:") {
			t.Errorf("indentation lost on line %q", line)
		}
	}
}

// TestAdoptSkipsMatrixExpressions: silently rewriting a computed value would
// break the workflow, so it must be reported instead.
func TestAdoptSkipsMatrixExpressions(t *testing.T) {
	in := `jobs:
  test:
    strategy:
      matrix:
        os: [ubuntu-latest, macos-latest]
    runs-on: ${{ matrix.os }}
`
	res := AdoptWorkflow("ci.yml", in)
	if res.Modified {
		t.Error("a matrix expression must NOT be rewritten automatically")
	}
	found := false
	for _, c := range res.Changes {
		if c.Skipped && strings.Contains(c.Reason, "matrix") {
			found = true
		}
	}
	if !found {
		t.Error("the skipped matrix line must be reported so the user knows it still costs money")
	}
}

// TestAdoptIsIdempotent: running adopt twice must not double-rewrite.
func TestAdoptIsIdempotent(t *testing.T) {
	in := "jobs:\n  b:\n    runs-on: ubuntu-latest\n"
	once := AdoptWorkflow("ci.yml", in)
	twice := AdoptWorkflow("ci.yml", once.Content)
	if twice.Modified {
		t.Errorf("second adopt pass modified an already-adopted file:\n%s", twice.Content)
	}
}

// TestAdoptLeavesCustomLabelsAlone: someone else's self-hosted fleet must not
// be hijacked.
func TestAdoptLeavesCustomLabelsAlone(t *testing.T) {
	in := "jobs:\n  b:\n    runs-on: [self-hosted, gpu, cuda-12]\n"
	res := AdoptWorkflow("ci.yml", in)
	if res.Modified {
		t.Error("a non-hosted runs-on must be left untouched")
	}
}

// TestAdoptPreservesComments keeps diffs reviewable.
func TestAdoptPreservesComments(t *testing.T) {
	in := "jobs:\n  b:\n    runs-on: ubuntu-latest # pinned deliberately\n"
	res := AdoptWorkflow("ci.yml", in)
	if !strings.Contains(res.Content, "# pinned deliberately") {
		t.Errorf("trailing comment was destroyed:\n%s", res.Content)
	}
}

// TestScanRunsOnFindsAllForms covers the doctor's routing analysis.
func TestScanRunsOnFindsAllForms(t *testing.T) {
	in := `jobs:
  a:
    runs-on: ubuntu-latest
  b:
    runs-on: [self-hosted, linux]
  c:
    runs-on: "macos-13"
`
	got := ScanRunsOn(in)
	want := map[string]bool{"ubuntu-latest": true, "self-hosted": true, "linux": true, "macos-13": true}
	if len(got) != len(want) {
		t.Errorf("ScanRunsOn found %v, want %d distinct targets", got, len(want))
	}
	for _, g := range got {
		if !want[g] {
			t.Errorf("unexpected target %q", g)
		}
	}
}

// TestAdviseIsHonestAboutHostedLabels: doctor must never claim interception
// works when it cannot.
func TestAdviseIsHonestAboutHostedLabels(t *testing.T) {
	runnerLabels := append(Default(), SelfHosted, "linux", "x64")

	adv := Advise([]string{"ubuntu-latest"}, runnerLabels)
	if adv.Interceptable {
		t.Error("Advise claimed a hosted label is interceptable; that is false")
	}
	if !strings.Contains(adv.NextStep, "adopt") {
		t.Errorf("advice should point at `homeplate adopt`, got %q", adv.NextStep)
	}

	adv = Advise([]string{Homeplate}, runnerLabels)
	if !adv.Interceptable {
		t.Error("a workflow already targeting the homeplate label should route today")
	}
}

// TestProbeDetectsDroppedLabels backs the "verify reality" strategy: if GitHub
// ever accepts a hosted label, Homeplate notices without a code change.
func TestProbeDetectsDroppedLabels(t *testing.T) {
	res := Probe([]string{"homeplate", "ubuntu-latest"}, []string{"self-hosted", "homeplate"})
	if res.HostedLabelAccepted {
		t.Error("hosted label was reported accepted when GitHub dropped it")
	}
	if len(res.Dropped) != 1 || res.Dropped[0] != "ubuntu-latest" {
		t.Errorf("Dropped = %v, want [ubuntu-latest]", res.Dropped)
	}

	res = Probe([]string{"homeplate", "ubuntu-latest"}, []string{"homeplate", "ubuntu-latest"})
	if !res.HostedLabelAccepted {
		t.Error("Probe failed to notice GitHub accepting a hosted label")
	}
}

// TestDedupePreservesOrder keeps label lists stable for registration.
func TestDedupePreservesOrder(t *testing.T) {
	got := Dedupe([]string{"a", "B", "a", "b", "c", ""})
	if strings.Join(got, ",") != "a,B,c" {
		t.Errorf("Dedupe = %v, want [a B c] (case-insensitive, order preserved)", got)
	}
}
