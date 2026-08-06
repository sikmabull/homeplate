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

// AdoptWorkflow rewrites hosted `runs-on:` values to Homeplate labels.
//
// Matrix expressions (${{ matrix.os }}) are intentionally NOT rewritten: the
// value is computed at run time and blindly replacing it would break the
// matrix. They are reported as skipped so `homeplate adopt` can tell the user
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
		case strings.Contains(strings.ToLower(value), Homeplate):
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

// RunnerLabelVariable is the repo-level Actions variable used by
// `homeplate adopt --variable`. The value is a JSON array of labels, read at
// run time via fromJSON(), so flipping a repo between hosted and self-hosted
// runners is a repo-settings change, not a commit.
const RunnerLabelVariable = "RUNNER_LABEL"

// RunnerLabelValue is the default value written to RunnerLabelVariable: the
// Linux Homeplate label set, as a JSON array string.
const RunnerLabelValue = `["self-hosted","homeplate","homeplate-linux"]`

// variableExpression builds the runs-on value used in --variable mode:
//
//	${{ vars.RUNNER_LABEL && fromJSON(vars.RUNNER_LABEL) || <original> }}
//
// When RUNNER_LABEL is set, the job routes to Homeplate; when it is deleted
// or emptied, the job falls back to its ORIGINAL hosted target, so merging
// the adopt PR never strands a repo on a machine that is off. The fallback
// preserves the original's shape: a scalar stays a scalar string, and the
// [a, b] array form becomes fromJSON('["a","b"]') so it stays a label list.
func variableExpression(original string, targets []string) string {
	// GitHub expression strings escape a single quote by doubling it.
	esc := func(s string) string { return strings.ReplaceAll(s, "'", "''") }
	if strings.HasPrefix(original, "[") {
		var b strings.Builder
		b.WriteString("[")
		for i, t := range targets {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, "%q", t)
		}
		b.WriteString("]")
		return "${{ vars." + RunnerLabelVariable + " && fromJSON(vars." + RunnerLabelVariable +
			") || fromJSON('" + esc(b.String()) + "') }}"
	}
	scalar := strings.Trim(original, `"'`)
	return "${{ vars." + RunnerLabelVariable + " && fromJSON(vars." + RunnerLabelVariable +
		") || '" + esc(scalar) + "' }}"
}

// AdoptWorkflowVariable is the non-invasive alternative to AdoptWorkflow:
// instead of pinning Homeplate labels, every hosted `runs-on:` becomes an
// expression that reads the RUNNER_LABEL repo variable and falls back to the
// original hosted label when the variable is unset.
//
// The skip rules match AdoptWorkflow: matrix/expression values and custom
// labels are left alone and reported, never silently rewritten.
func AdoptWorkflowVariable(path, content string) RewriteResult {
	res := RewriteResult{Path: path, Content: content}
	lines := strings.Split(content, "\n")

	for i, line := range lines {
		m := runsOnRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		indent, value := m[1], strings.TrimSpace(m[2])
		comment := ""
		if idx := strings.Index(value, " #"); idx >= 0 {
			comment = value[idx:]
			value = strings.TrimSpace(value[:idx])
		}

		if strings.Contains(value, "vars."+RunnerLabelVariable) {
			res.Changes = append(res.Changes, Rewrite{
				Line: i + 1, Old: line, Skipped: true, Reason: "already adopted (variable form)",
			})
			continue
		}
		if strings.Contains(value, "${{") {
			res.Changes = append(res.Changes, Rewrite{
				Line: i + 1, Old: line, Skipped: true,
				Reason: "expression/matrix value - rewrite by hand or change the matrix `os:` list",
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
		newLine := fmt.Sprintf("%sruns-on: %s%s", indent, variableExpression(value, targets), comment)
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
