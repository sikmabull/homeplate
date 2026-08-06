package ghapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestSetRepoVariableCreate covers the fresh-create path: one POST, done.
func TestSetRepoVariableCreate(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	c := New("x")
	c.BaseURL = srv.URL
	if err := c.SetRepoVariable(context.Background(), "o/r", "RUNNER_LABEL", `["self-hosted"]`); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotPath != "/repos/o/r/actions/variables" {
		t.Errorf("unexpected request %s %s", gotMethod, gotPath)
	}
	if gotBody["name"] != "RUNNER_LABEL" || gotBody["value"] != `["self-hosted"]` {
		t.Errorf("unexpected body %v", gotBody)
	}
}

// TestSetRepoVariableConflictFallsBackToPut covers the update path: a 409 to
// POST must trigger a PUT to the named variable endpoint.
func TestSetRepoVariableConflictFallsBackToPut(t *testing.T) {
	var sawPut bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "Variable already exists"})
			return
		}
		if r.Method == http.MethodPut && r.URL.Path == "/repos/o/r/actions/variables/RUNNER_LABEL" {
			sawPut = true
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusTeapot)
	}))
	defer srv.Close()

	c := New("x")
	c.BaseURL = srv.URL
	if err := c.SetRepoVariable(context.Background(), "o/r", "RUNNER_LABEL", "v"); err != nil {
		t.Fatal(err)
	}
	if !sawPut {
		t.Error("409 on create did not fall back to PUT update")
	}
}
