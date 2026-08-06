package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/homeplate-ci/homeplate/internal/ghapi"
)

// TestAdoptOneVariable exercises the full --variable flow against a mock
// GitHub: variable create-or-update, branch, file commit, PR - and asserts
// the workflow rewrite keeps the original hosted label as the fallback.
func TestAdoptOneVariable(t *testing.T) {
	workflow := "name: ci\non: [push]\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: go test ./...\n"
	var sawVariable bool
	var committedContent string
	var prTitle string

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r", func(w http.ResponseWriter, req *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"full_name": "o/r", "default_branch": "main", "private": true,
		})
	})
	mux.HandleFunc("/repos/o/r/contents/.github/workflows", func(w http.ResponseWriter, req *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]string{
			{"name": "ci.yml", "path": ".github/workflows/ci.yml", "type": "file"},
		})
	})
	mux.HandleFunc("/repos/o/r/contents/.github/workflows/ci.yml", func(w http.ResponseWriter, req *http.Request) {
		switch req.Method {
		case http.MethodGet:
			if strings.Contains(req.URL.RawQuery, "ref=homeplate") {
				_ = json.NewEncoder(w).Encode(map[string]string{"sha": "filesha"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{
				"content": base64.StdEncoding.EncodeToString([]byte(workflow)), "encoding": "base64",
			})
		case http.MethodPut:
			var body map[string]string
			_ = json.NewDecoder(req.Body).Decode(&body)
			b, _ := base64.StdEncoding.DecodeString(body["content"])
			committedContent = string(b)
			_ = json.NewEncoder(w).Encode(map[string]any{})
		}
	})
	mux.HandleFunc("/repos/o/r/actions/variables", func(w http.ResponseWriter, req *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(req.Body).Decode(&body)
		if body["name"] != "RUNNER_LABEL" {
			t.Errorf("unexpected variable %q", body["name"])
		}
		sawVariable = true
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("/repos/o/r/git/ref/heads/main", func(w http.ResponseWriter, req *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"object": map[string]string{"sha": "basesha"}})
	})
	mux.HandleFunc("/repos/o/r/git/refs", func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{})
	})
	mux.HandleFunc("/repos/o/r/pulls", func(w http.ResponseWriter, req *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(req.Body).Decode(&body)
		prTitle, _ = body["title"].(string)
		prBody, _ := body["body"].(string)
		if !strings.Contains(prBody, "RUNNER_LABEL") || !strings.Contains(prBody, "delete") {
			t.Errorf("variable PR body should explain the flip mechanism, got:\n%s", prBody)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"html_url": "https://github.com/o/r/pull/1"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := ghapi.New("x")
	c.BaseURL = srv.URL

	url, err := adoptOne(context.Background(), c, "o/r", adoptOptions{autoYes: true, variable: true, quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if url != "https://github.com/o/r/pull/1" {
		t.Errorf("unexpected PR URL %q", url)
	}
	if !sawVariable {
		t.Error("RUNNER_LABEL repo variable was never set")
	}
	want := "runs-on: ${{ vars.RUNNER_LABEL && fromJSON(vars.RUNNER_LABEL) || 'ubuntu-latest' }}"
	if !strings.Contains(committedContent, want) {
		t.Errorf("committed workflow lacks the fallback expression %q:\n%s", want, committedContent)
	}
	if !strings.Contains(prTitle, "RUNNER_LABEL") {
		t.Errorf("unexpected PR title %q", prTitle)
	}
}
