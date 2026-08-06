package offline

import (
	"strings"
	"testing"

	"github.com/homerun-ci/homerun/internal/config"
)

// realActOutput is captured verbatim from `act` v0.2.89 running on macOS,
// including the ANSI colour codes it emits when writing to a pipe. Parsing
// must survive them: a regression here silently loses every job name and step
// record, which in turn produces meaningless commit-status contexts.
const realActOutput = "\x1b[33m[CI/build] \x1b[0m\u2b50 Run Set up job\n" +
	"\x1b[33m[CI/build] \x1b[0m  \U0001f433  docker pull image=catthehacker/ubuntu:act-latest\n" +
	"\x1b[33m[CI/build] \x1b[0m  \u2705  Success - Set up job\n" +
	"\x1b[33m[CI/build] \x1b[0m\u2b50 Run Main Say hello\n" +
	"\x1b[33m|\x1b[0m hello from homerun offline mode\n" +
	"\x1b[33m[CI/build] \x1b[0m  \u2705  Success - Main Say hello [456.712834ms]\n" +
	"\x1b[33m[CI/build] \x1b[0m\U0001f3c1  Job succeeded\n"

// TestParseActHandlesANSI is a regression test for a real bug: act colourises
// piped output, and the un-stripped escape codes broke every anchored regex.
func TestParseActHandlesANSI(t *testing.T) {
	steps, jobs := parseAct(strings.NewReader(realActOutput))

	if len(jobs) != 1 || jobs[0] != "build" {
		t.Fatalf("job names = %v, want [build]; ANSI codes are breaking the parser", jobs)
	}
	if len(steps) == 0 {
		t.Fatal("no steps parsed from real act output")
	}
	var names []string
	for _, s := range steps {
		names = append(names, s.Name)
	}
	joined := strings.Join(names, "|")
	if !strings.Contains(joined, "Set up job") || !strings.Contains(joined, "Main Say hello") {
		t.Errorf("steps = %v, want the real step names", names)
	}
}

// TestStripANSI covers the helper directly.
func TestStripANSI(t *testing.T) {
	if got := stripANSI("\x1b[33m[CI/build] \x1b[0mhello"); got != "[CI/build] hello" {
		t.Errorf("stripANSI = %q", got)
	}
	if got := stripANSI("plain text"); got != "plain text" {
		t.Errorf("stripANSI mangled plain text: %q", got)
	}
}

// TestActArgsCarryResourceCaps proves Engine B honours the same limits as
// Engine A, rather than quietly running uncapped.
func TestActArgsCarryResourceCaps(t *testing.T) {
	a := &Act{Bin: "act"}
	args := a.Args(RunOpts{
		Event:  Event{Name: "push"},
		Limits: config.Limits{MaxCPUs: 3, MaxMemory: "4g"},
	}, "/tmp/event.json")

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--cpus=3") {
		t.Errorf("act args missing CPU cap:\n%s", joined)
	}
	if !strings.Contains(joined, "--memory=4g") {
		t.Errorf("act args missing memory cap:\n%s", joined)
	}
	if !strings.Contains(joined, "--memory-swap=4g") {
		t.Errorf("act args missing swap cap; memory limit could be bypassed:\n%s", joined)
	}
	if !strings.Contains(joined, "--rm") {
		t.Errorf("act args missing --rm; containers would leak:\n%s", joined)
	}
}

// TestActMapsHostedAndHomerunLabels: offline mode must run workflows whether
// or not they have been through `homerun adopt`.
func TestActMapsHostedAndHomerunLabels(t *testing.T) {
	a := &Act{Bin: "act"}
	joined := strings.Join(a.Args(RunOpts{Event: Event{Name: "push"}, Image: "img:tag"}, "/e.json"), " ")
	for _, label := range []string{"ubuntu-latest=img:tag", "homerun=img:tag", "self-hosted=img:tag"} {
		if !strings.Contains(joined, label) {
			t.Errorf("act platform mapping missing %q:\n%s", label, joined)
		}
	}
}

// TestEventPayloadMatchesGitHubShape ensures github.sha / github.repository
// resolve correctly inside an offline workflow.
func TestEventPayloadMatchesGitHubShape(t *testing.T) {
	e := Event{Name: "push", SHA: "abc123", Ref: "refs/heads/main",
		Repository: "acme/widgets", Actor: "alice", Branch: "main"}
	p := e.Payload()

	if p["after"] != "abc123" {
		t.Errorf("push payload `after` = %v, want the commit SHA", p["after"])
	}
	repo, ok := p["repository"].(map[string]any)
	if !ok {
		t.Fatal("payload has no repository object")
	}
	if repo["full_name"] != "acme/widgets" {
		t.Errorf("repository.full_name = %v", repo["full_name"])
	}
	if repo["name"] != "widgets" {
		t.Errorf("repository.name = %v, want widgets", repo["name"])
	}
	owner, _ := repo["owner"].(map[string]any)
	if owner == nil || owner["login"] != "acme" {
		t.Errorf("repository.owner.login = %v, want acme", owner)
	}
}

// TestRedactURL keeps tokens out of logs.
func TestRedactURL(t *testing.T) {
	in := "https://x-access-token:ghp_secret123@github.com/acme/widgets.git"
	got := RedactURL(in)
	if strings.Contains(got, "ghp_secret123") {
		t.Fatalf("SECURITY: token leaked in redacted URL: %s", got)
	}
	if !strings.Contains(got, "github.com/acme/widgets.git") {
		t.Errorf("RedactURL destroyed the useful part: %s", got)
	}
}

// TestAuthenticatedURL builds a token-carrying clone URL.
func TestAuthenticatedURL(t *testing.T) {
	got := AuthenticatedURL("github.com", "acme/widgets", "tok")
	if got != "https://x-access-token:tok@github.com/acme/widgets.git" {
		t.Errorf("AuthenticatedURL = %q", got)
	}
	if got := AuthenticatedURL("", "acme/widgets", ""); got != "https://github.com/acme/widgets.git" {
		t.Errorf("tokenless URL = %q", got)
	}
}

// TestScrubRemovesTokensFromGitOutput is a security regression test: git can
// echo the authenticated remote URL in its error text, which would otherwise
// write a live token straight into Homerun's logs.
func TestScrubRemovesTokensFromGitOutput(t *testing.T) {
	const token = "ghp_verysecrettoken1234567890"
	url := AuthenticatedURL("github.com", "acme/widgets", token)

	gitErr := "fatal: unable to access '" + url + "': The requested URL returned error: 403"
	got := scrub(gitErr, url)

	if strings.Contains(got, token) {
		t.Fatalf("SECURITY: token survived scrubbing:\n%s", got)
	}
	if !strings.Contains(got, "403") {
		t.Errorf("scrub destroyed the diagnostic detail: %s", got)
	}

	// A bare token appearing without the full URL must also be caught.
	if out := scrub("remote rejected "+token, url); strings.Contains(out, token) {
		t.Errorf("SECURITY: bare token survived scrubbing: %s", out)
	}
}

// TestTokenFromURL covers the credential extractor.
func TestTokenFromURL(t *testing.T) {
	if got := tokenFromURL("https://x-access-token:abc123@github.com/a/b.git"); got != "abc123" {
		t.Errorf("tokenFromURL = %q, want abc123", got)
	}
	if got := tokenFromURL("https://github.com/a/b.git"); got != "" {
		t.Errorf("tokenFromURL on a tokenless URL = %q, want empty", got)
	}
}
