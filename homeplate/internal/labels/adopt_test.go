package labels

import (
	"strings"
	"testing"
)

// TestAdoptWorkflowVariableRewritesHostedLabels is the core --variable
// guarantee: the original hosted label survives verbatim as the fallback.
func TestAdoptWorkflowVariableRewritesHostedLabels(t *testing.T) {
	in := `name: ci
on: [push]
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - run: go test ./...
  build:
    runs-on: macos-latest # pinned by policy
`
	res := AdoptWorkflowVariable(".github/workflows/ci.yml", in)
	if !res.Modified {
		t.Fatal("expected modifications")
	}
	if !strings.Contains(res.Content,
		"runs-on: ${{ vars.RUNNER_LABEL && fromJSON(vars.RUNNER_LABEL) || 'ubuntu-latest' }}") {
		t.Errorf("ubuntu-latest fallback missing:\n%s", res.Content)
	}
	if !strings.Contains(res.Content,
		"runs-on: ${{ vars.RUNNER_LABEL && fromJSON(vars.RUNNER_LABEL) || 'macos-latest' }} # pinned by policy") {
		t.Errorf("macos-latest fallback with preserved comment missing:\n%s", res.Content)
	}
	// The fallback must keep the workflow runnable on hosted runners, i.e. the
	// file still literally contains the original label.
	for _, want := range []string{"'ubuntu-latest'", "'macos-latest'"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("fallback %q not preserved", want)
		}
	}
}

// TestAdoptWorkflowVariableSkips documents what --variable must NOT touch:
// matrix expressions, custom labels, and already-adopted lines.
func TestAdoptWorkflowVariableSkips(t *testing.T) {
	in := `jobs:
  matrix-job:
    runs-on: ${{ matrix.os }}
  custom:
    runs-on: [self-hosted, my-gpu-box]
  done:
    runs-on: ${{ vars.RUNNER_LABEL && fromJSON(vars.RUNNER_LABEL) || 'ubuntu-latest' }}
`
	res := AdoptWorkflowVariable("w.yml", in)
	if res.Modified {
		t.Errorf("nothing should be rewritten, got:\n%s", res.Content)
	}
	if len(res.Changes) != 3 {
		t.Fatalf("expected 3 reported skips, got %d", len(res.Changes))
	}
	for _, ch := range res.Changes {
		if !ch.Skipped {
			t.Errorf("line %d was rewritten, want skip", ch.Line)
		}
	}
	if !strings.Contains(res.Changes[2].Reason, "already adopted") {
		t.Errorf("variable-form line should be reported as already adopted, got %q", res.Changes[2].Reason)
	}
}

// TestAdoptWorkflowVariableEscapesQuotes keeps the generated expression valid
// when the original value contains a single quote (e.g. runs-on: 'ubuntu-latest').
func TestAdoptWorkflowVariableEscapesQuotes(t *testing.T) {
	res := AdoptWorkflowVariable("w.yml", "jobs:\n  a:\n    runs-on: 'ubuntu-latest'\n")
	if !res.Modified {
		t.Fatal("expected a rewrite")
	}
	if !strings.Contains(res.Content, "|| 'ubuntu-latest'") {
		t.Errorf("YAML quotes should be stripped from the fallback:\n%s", res.Content)
	}
}

// TestAdoptWorkflowVariableIdempotent: applying the rewrite to its own output
// must change nothing, so `homeplate auto` can run twice safely.
func TestAdoptWorkflowVariableIdempotent(t *testing.T) {
	in := "jobs:\n  a:\n    runs-on: ubuntu-latest\n"
	first := AdoptWorkflowVariable("w.yml", in)
	second := AdoptWorkflowVariable("w.yml", first.Content)
	if second.Modified {
		t.Errorf("second pass rewrote again:\n%s", second.Content)
	}
}
