package labels

import (
	"fmt"
	"regexp"
	"strings"
)

// runsOnRe matches a `runs-on:` line and captures indentation and value.
// Deliberately line-based rather than a full YAML round-trip: re-emitting YAML
// destroys comments, anchors, and formatting in workflow files that humans
// maintain. A surgical line rewrite produces a reviewable one-line-per-job diff.
var runsOnRe = regexp.MustCompile(`(?m)^(\s*)runs-on:\s*(.+?)\s*$`)

// Rewrite is a single planned change to a workflow file.
type Rewrite struct {
	Line    int
	Old     string
	New     string
	Class   Class
	Skipped bool
	Reason  string
}

// RewriteResult is the outcome of adopting one workflow file.
type RewriteResult struct {
	Path     string
	Content  string
	Changes  []Rewrite
	Modified bool
}

// AdoptWorkflow rewrites hosted `runs-on:` values to Homerun labels.
//
// Matrix expressions (${{ matrix.os }}) are intentionally NOT rewritten: the
// value is computed at run time and blindly replacing it would break the
// matrix. They are reported as skipped so `homerun adopt` can tell the user
// exactly which jobs still cost money.
func AdoptWorkflow(path, content string) RewriteResult {
	res := RewriteResult{Path: path, Content: content}
	lines := strings.Split(content, "\n")

	for i, line := range lines {
		m := runsOnRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		indent, value := m[1], strings.TrimSpace(m[2])
		// Strip trailing comment for analysis, preserve it on output.
		comment := ""
		if idx := strings.Index(value, " #"); idx >= 0 {
			comment = value[idx:]
			value = strings.TrimSpace(value[:idx])
		}

		switch {
		case strings.Contains(value, "${{"):
			res.Changes = append(res.Changes, Rewrite{
				Line: i + 1, Old: line, Skipped: true,
				Reason: "expression/matrix value - rewrite by hand or change the matrix `os:` list",
			})
			continue
		case strings.Contains(strings.ToLower(value), Homerun):
			res.Changes = append(res.Changes, Rewrite{
				Line: i + 1, Old: line, Skipped: true, Reason: "already adopted",
			})
			continue
		}

		targets := parseRunsOnValue(value)
		if len(targets) == 0 {
			continue
		}
		anyReserved := false
		for _, t := range targets {
			if IsReserved(t) {
				anyReserved = true
			}
		}
		if !anyReserved {
			res.Changes = append(res.Changes, Rewrite{
				Line: i + 1, Old: line, Skipped: true,
				Reason: "not a GitHub-hosted label; left alone",
			})
			continue
		}

		class := ClassLinux
		for _, t := range targets {
			if c := ClassOf(t); c != ClassLinux {
				class = c
			}
		}
		newLine := fmt.Sprintf("%sruns-on: %s%s", indent, RunsOnReplacement(class), comment)
		res.Changes = append(res.Changes, Rewrite{
			Line: i + 1, Old: line, New: newLine, Class: class,
		})
		lines[i] = newLine
		res.Modified = true
	}

	if res.Modified {
		res.Content = strings.Join(lines, "\n")
	}
	return res
}

// parseRunsOnValue handles the three YAML spellings of runs-on:
//
//	runs-on: ubuntu-latest
//	runs-on: [self-hosted, linux]
//	runs-on: {group: g, labels: [a]}    (group form; reported, not rewritten)
func parseRunsOnValue(v string) []string {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	if strings.HasPrefix(v, "{") {
		// Runner-group form; extracting labels safely needs real YAML parsing.
		return nil
	}
	if strings.HasPrefix(v, "[") {
		inner := strings.TrimSuffix(strings.TrimPrefix(v, "["), "]")
		var out []string
		for _, part := range strings.Split(inner, ",") {
			part = strings.Trim(strings.TrimSpace(part), `"'`)
			if part != "" {
				out = append(out, part)
			}
		}
		return out
	}
	return []string{strings.Trim(v, `"'`)}
}

// ScanRunsOn extracts every runs-on target in a workflow, for `doctor` and
// for the savings estimator.
func ScanRunsOn(content string) []string {
	var out []string
	for _, m := range runsOnRe.FindAllStringSubmatch(content, -1) {
		value := strings.TrimSpace(m[2])
		if idx := strings.Index(value, " #"); idx >= 0 {
			value = strings.TrimSpace(value[:idx])
		}
		out = append(out, parseRunsOnValue(value)...)
	}
	return Dedupe(out)
}
