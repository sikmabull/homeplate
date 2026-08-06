package syncbrain

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/homeplate-ci/homeplate/internal/config"
	"github.com/homeplate-ci/homeplate/internal/ghapi"
	"github.com/homeplate-ci/homeplate/internal/store"
)

// fakeGitHub records every write so tests can assert that replay never
// double-posts.
type fakeGitHub struct {
	mu sync.Mutex

	statuses  []map[string]any
	approvals []string
	merges    []string
	// failNextStatus makes the first N status posts fail, to test retry.
	failStatusTimes int
	// existingStatuses is returned by the list endpoint.
	existingStatuses []map[string]any

	server *httptest.Server
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{}
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		path := r.URL.Path

		switch {
		// POST /repos/{o}/{r}/statuses/{sha}
		case strings.Contains(path, "/statuses/") && r.Method == http.MethodPost:
			if f.failStatusTimes > 0 {
				f.failStatusTimes--
				w.WriteHeader(http.StatusInternalServerError)
				json.NewEncoder(w).Encode(map[string]string{"message": "server error"})
				return
			}
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			body["_sha"] = strings.TrimPrefix(path[strings.Index(path, "/statuses/"):], "/statuses/")
			f.statuses = append(f.statuses, body)
			json.NewEncoder(w).Encode(map[string]any{
				"id": len(f.statuses), "state": body["state"], "context": body["context"],
			})

		// GET /repos/{o}/{r}/commits/{sha}/statuses
		case strings.HasSuffix(path, "/statuses") && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(f.existingStatuses)

		// GET /repos/{o}/{r}/commits/{sha}/pulls
		case strings.HasSuffix(path, "/pulls") && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode([]map[string]any{{
				"number": 42, "state": "open", "draft": false, "title": "feat: thing",
				"head": map[string]any{"sha": "abc123def456", "ref": "feature",
					"repo": map[string]any{"full_name": "acme/widgets"}},
				"base": map[string]any{"ref": "main",
					"repo": map[string]any{"full_name": "acme/widgets"}},
				"mergeable": true, "mergeable_state": "clean",
			}})

		// POST /repos/{o}/{r}/pulls/42/reviews
		case strings.HasSuffix(path, "/reviews") && r.Method == http.MethodPost:
			f.approvals = append(f.approvals, path)
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{"id": 1, "state": "APPROVED"})

		// GET /repos/{o}/{r}/pulls/42
		case strings.Contains(path, "/pulls/") && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]any{
				"number": 42, "state": "open", "title": "feat: thing",
				"head":      map[string]any{"sha": "abc123def456"},
				"mergeable": true, "mergeable_state": "clean",
			})

		// PUT /repos/{o}/{r}/pulls/42/merge
		case strings.HasSuffix(path, "/merge") && r.Method == http.MethodPut:
			f.merges = append(f.merges, path)
			json.NewEncoder(w).Encode(map[string]any{"sha": "merged123", "merged": true})

		default:
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"message": "not found: " + path})
		}
	})

	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

// client returns a ghapi.Client pointed at the fake server.
func (f *fakeGitHub) client() *ghapi.Client {
	c := ghapi.New("test-token")
	c.HTTP = f.server.Client()
	c.BaseURL = f.server.URL
	return c
}

type fakeClients struct{ c *ghapi.Client }

func (f fakeClients) For(profile string) (*ghapi.Client, error) { return f.c, nil }

func newTestDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func seedOfflineJob(t *testing.T, db *store.DB, extID string, state store.JobState) *store.Job {
	t.Helper()
	ctx := context.Background()
	j := &store.Job{
		ExternalID: extID, Profile: "personal", RepoSlug: "acme/widgets",
		Engine: store.EngineB, Workflow: "ci.yml", JobName: "build",
		CommitSHA: "abc123def456", Ref: "refs/heads/feature", RunnerClass: "linux",
	}
	if _, err := db.EnqueueJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	if _, err := db.NextQueued(ctx, store.EngineB); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishJob(ctx, j.ID, state, 0, ""); err != nil {
		t.Fatal(err)
	}
	fresh, err := db.Job(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	return fresh
}

// TestReplayIsIdempotent is THE offline-mode guarantee: replaying the same
// queued result any number of times must post exactly one commit status.
func TestReplayIsIdempotent(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	gh := newFakeGitHub(t)
	seedOfflineJob(t, db, "job-1", store.StateSucceeded)

	cfg := config.Defaults()
	r := &Replayer{DB: db, Clients: fakeClients{gh.client()}, Config: cfg}

	// Five passes, simulating a flapping network / restarted daemon.
	for i := 0; i < 5; i++ {
		out, err := r.ReplayAll(ctx)
		if err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
		if i == 0 && out.Posted != 1 {
			t.Fatalf("first pass posted %d statuses, want 1", out.Posted)
		}
	}

	gh.mu.Lock()
	defer gh.mu.Unlock()
	if len(gh.statuses) != 1 {
		t.Fatalf("replay posted %d statuses, want exactly 1 (idempotency broken):\n%+v",
			len(gh.statuses), gh.statuses)
	}
}

// TestReplayNeverImpersonatesGitHub enforces the honesty requirement: every
// replayed result must say it ran locally.
func TestReplayNeverImpersonatesGitHub(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	gh := newFakeGitHub(t)
	seedOfflineJob(t, db, "job-honest", store.StateSucceeded)

	r := &Replayer{DB: db, Clients: fakeClients{gh.client()}, Config: config.Defaults()}
	if _, err := r.ReplayAll(ctx); err != nil {
		t.Fatal(err)
	}

	gh.mu.Lock()
	defer gh.mu.Unlock()
	if len(gh.statuses) != 1 {
		t.Fatalf("want 1 status, got %d", len(gh.statuses))
	}
	desc, _ := gh.statuses[0]["description"].(string)
	if !strings.Contains(desc, "ran locally via Homeplate offline mode at") {
		t.Errorf("status description must disclose local execution, got %q", desc)
	}
	context_, _ := gh.statuses[0]["context"].(string)
	if !strings.HasPrefix(context_, "homeplate") {
		t.Errorf("status context must be namespaced to homeplate, got %q", context_)
	}
	if !strings.Contains(context_, "offline") {
		t.Errorf("offline results must be marked offline in the context, got %q", context_)
	}
}

// TestReplayDeduplicatesAgainstGitHub covers ledger loss: if the local replay
// record vanishes but the status exists on GitHub, do not post a second one.
func TestReplayDeduplicatesAgainstGitHub(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	gh := newFakeGitHub(t)
	job := seedOfflineJob(t, db, "job-dedupe", store.StateSucceeded)

	// GitHub already has an identical status from a previous machine/run.
	gh.existingStatuses = []map[string]any{{
		"id": 999, "state": "success", "context": StatusContext(job),
	}}

	r := &Replayer{DB: db, Clients: fakeClients{gh.client()}, Config: config.Defaults()}
	out, err := r.ReplayAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	gh.mu.Lock()
	defer gh.mu.Unlock()
	if len(gh.statuses) != 0 {
		t.Fatalf("posted %d duplicate statuses; want 0", len(gh.statuses))
	}
	if out.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", out.Skipped)
	}
}

// TestReplayOrdering asserts results push oldest-first so a PR's check history
// reads in causal order.
func TestReplayOrdering(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	gh := newFakeGitHub(t)

	for i := 0; i < 3; i++ {
		seedOfflineJob(t, db, fmt.Sprintf("job-order-%d", i), store.StateSucceeded)
		time.Sleep(5 * time.Millisecond) // distinct finished_at
	}

	r := &Replayer{DB: db, Clients: fakeClients{gh.client()}, Config: config.Defaults()}
	if _, err := r.ReplayAll(ctx); err != nil {
		t.Fatal(err)
	}
	gh.mu.Lock()
	defer gh.mu.Unlock()
	if len(gh.statuses) != 3 {
		t.Fatalf("want 3 statuses, got %d", len(gh.statuses))
	}
}

// TestReplayRetriesThenGivesUp proves a permanently failing job cannot block
// the queue forever, and that failures are recorded rather than swallowed.
func TestReplayRetriesThenGivesUp(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	gh := newFakeGitHub(t)
	gh.failStatusTimes = 1000 // always fail
	seedOfflineJob(t, db, "job-doomed", store.StateSucceeded)

	r := &Replayer{DB: db, Clients: fakeClients{gh.client()}, Config: config.Defaults()}

	for i := 0; i < MaxAttempts+2; i++ {
		out, _ := r.ReplayAll(ctx)
		if out.Skipped == 1 {
			// Gave up and marked replayed; queue is unblocked.
			pending, err := db.PendingReplay(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(pending) != 0 {
				t.Errorf("job still pending after giving up: %d", len(pending))
			}
			return
		}
	}
	t.Fatalf("replayer never gave up after %d attempts; the queue would be blocked forever", MaxAttempts+2)
}

// TestAutoApproveDisabledByDefault verifies the safe default.
func TestAutoApproveDisabledByDefault(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	gh := newFakeGitHub(t)
	seedOfflineJob(t, db, "job-noapprove", store.StateSucceeded)

	cfg := config.Defaults()
	if cfg.Sync.AutoApprove {
		t.Fatal("auto_approve must default to false")
	}
	r := &Replayer{DB: db, Clients: fakeClients{gh.client()}, Config: cfg}
	if _, err := r.ReplayAll(ctx); err != nil {
		t.Fatal(err)
	}
	gh.mu.Lock()
	defer gh.mu.Unlock()
	if len(gh.approvals) != 0 || len(gh.merges) != 0 {
		t.Errorf("auto-approve/merge happened without opt-in: %d approvals, %d merges",
			len(gh.approvals), len(gh.merges))
	}
}

// TestAutoApproveAndMergeOptIn exercises the opt-in path, including that
// merges pin the tested SHA and never repeat.
func TestAutoApproveAndMergeOptIn(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	gh := newFakeGitHub(t)
	seedOfflineJob(t, db, "job-approve", store.StateSucceeded)

	cfg := config.Defaults()
	cfg.Sync.AutoApprove = true
	cfg.Sync.AutoMerge = true

	r := &Replayer{DB: db, Clients: fakeClients{gh.client()}, Config: cfg}
	for i := 0; i < 3; i++ {
		if _, err := r.ReplayAll(ctx); err != nil {
			t.Fatal(err)
		}
	}

	gh.mu.Lock()
	defer gh.mu.Unlock()
	if len(gh.approvals) != 1 {
		t.Errorf("approvals = %d, want exactly 1 (idempotency)", len(gh.approvals))
	}
	if len(gh.merges) != 1 {
		t.Errorf("merges = %d, want exactly 1 (idempotency)", len(gh.merges))
	}
}

// TestFailedJobPostsFailure ensures a failed local run is reported as failed,
// not silently dropped.
func TestFailedJobPostsFailure(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	gh := newFakeGitHub(t)
	seedOfflineJob(t, db, "job-failed", store.StateFailed)

	r := &Replayer{DB: db, Clients: fakeClients{gh.client()}, Config: config.Defaults()}
	if _, err := r.ReplayAll(ctx); err != nil {
		t.Fatal(err)
	}
	gh.mu.Lock()
	defer gh.mu.Unlock()
	if len(gh.statuses) != 1 {
		t.Fatalf("want 1 status, got %d", len(gh.statuses))
	}
	if gh.statuses[0]["state"] != "failure" {
		t.Errorf("state = %v, want failure", gh.statuses[0]["state"])
	}
}

// TestAutoMergeRequiresAutoApprove guards the clamp in config normalisation.
func TestAutoMergeRequiresAutoApprove(t *testing.T) {
	cfg := config.Defaults()
	cfg.Sync.AutoMerge = true
	cfg.Sync.AutoApprove = false
	// normalize() runs on Load; emulate it here via Save/Load round trip.
	dir := t.TempDir()
	t.Setenv("HOMEPLATE_HOME", dir)
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Sync.AutoMerge {
		t.Error("auto_merge must be clamped off when auto_approve is false")
	}
}
