package store

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestEnqueueIsIdempotent proves the ExternalID dedupe key works: re-queuing
// the same logical job (same commit + workflow) never creates a duplicate.
func TestEnqueueIsIdempotent(t *testing.T) {
	ctx := context.Background()
	db := newDB(t)

	mk := func() *Job {
		return &Job{
			ExternalID: "offline|acme/widgets|abc123|ci.yml|build",
			Profile:    "personal", RepoSlug: "acme/widgets", Engine: EngineB,
			Workflow: "ci.yml", JobName: "build", CommitSHA: "abc123",
		}
	}

	j1 := mk()
	created, err := db.EnqueueJob(ctx, j1)
	if err != nil || !created {
		t.Fatalf("first enqueue: created=%v err=%v", created, err)
	}

	for i := 0; i < 5; i++ {
		j := mk()
		created, err := db.EnqueueJob(ctx, j)
		if err != nil {
			t.Fatalf("re-enqueue %d: %v", i, err)
		}
		if created {
			t.Fatalf("re-enqueue %d created a duplicate job", i)
		}
		if j.ID != j1.ID {
			t.Fatalf("re-enqueue returned id %d, want the existing %d", j.ID, j1.ID)
		}
	}

	queued, err := db.QueuedJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 {
		t.Fatalf("queue holds %d jobs, want 1", len(queued))
	}
}

// TestNextQueuedIsAtomic ensures two concurrent workers cannot claim the same
// job - otherwise a job would run twice and bill twice.
func TestNextQueuedIsAtomic(t *testing.T) {
	ctx := context.Background()
	db := newDB(t)

	const n = 25
	for i := 0; i < n; i++ {
		j := &Job{
			ExternalID: fmt.Sprintf("offline|acme/w|sha%d|ci.yml|build", i),
			Profile:    "p", RepoSlug: "acme/w", Engine: EngineB,
		}
		if _, err := db.EnqueueJob(ctx, j); err != nil {
			t.Fatal(err)
		}
	}

	var mu sync.Mutex
	claimed := map[int64]int{}
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				j, err := db.NextQueued(ctx, EngineB)
				if err != nil || j == nil {
					return
				}
				mu.Lock()
				claimed[j.ID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(claimed) != n {
		t.Errorf("claimed %d distinct jobs, want %d", len(claimed), n)
	}
	for id, times := range claimed {
		if times != 1 {
			t.Errorf("job %d was claimed %d times; concurrent workers would run it twice", id, times)
		}
	}
}

// TestReplayLedgerIsIdempotent exercises the storage-level guarantee that
// backs offline replay.
func TestReplayLedgerIsIdempotent(t *testing.T) {
	ctx := context.Background()
	db := newDB(t)

	j := &Job{ExternalID: "x", Profile: "p", RepoSlug: "a/b", Engine: EngineB}
	if _, err := db.EnqueueJob(ctx, j); err != nil {
		t.Fatal(err)
	}

	done, err := db.ClaimReplay(ctx, j.ID, ReplayStatus, "ctx-1")
	if err != nil || done {
		t.Fatalf("first claim: done=%v err=%v, want done=false", done, err)
	}
	// Not completed yet, so a re-claim must still report "not done" (allowing
	// a retry) rather than blocking forever.
	done, err = db.ClaimReplay(ctx, j.ID, ReplayStatus, "ctx-1")
	if err != nil || done {
		t.Fatalf("re-claim before completion: done=%v err=%v, want done=false", done, err)
	}

	if err := db.CompleteReplay(ctx, j.ID, ReplayStatus, "ctx-1", "999"); err != nil {
		t.Fatal(err)
	}
	done, err = db.ClaimReplay(ctx, j.ID, ReplayStatus, "ctx-1")
	if err != nil || !done {
		t.Fatalf("claim after completion: done=%v err=%v, want done=true", done, err)
	}

	// A different target on the same job is tracked independently.
	done, _ = db.ClaimReplay(ctx, j.ID, ReplayStatus, "ctx-2")
	if done {
		t.Error("a different status context must not be considered already-posted")
	}
}

// TestInterruptedJobsAreNeverDropped covers the sleep-mid-job requirement.
func TestInterruptedJobsAreNeverDropped(t *testing.T) {
	ctx := context.Background()
	db := newDB(t)

	// One connected job and one offline job, both left "running" by a crash.
	for i, eng := range []Engine{EngineA, EngineB} {
		j := &Job{ExternalID: fmt.Sprintf("j%d", i), Profile: "p", RepoSlug: "a/b", Engine: eng}
		if _, err := db.EnqueueJob(ctx, j); err != nil {
			t.Fatal(err)
		}
		if _, err := db.NextQueued(ctx, eng); err != nil {
			t.Fatal(err)
		}
	}

	n, err := db.MarkInterrupted(ctx, "machine slept mid-job")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("marked %d jobs interrupted, want 2", n)
	}

	// Offline jobs are automatically re-queued; connected ones defer to
	// GitHub's own re-run path.
	m, err := db.RequeueInterrupted(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if m != 1 {
		t.Fatalf("re-queued %d jobs, want 1 (offline only)", m)
	}

	queued, _ := db.QueuedJobs(ctx)
	if len(queued) != 1 || queued[0].Engine != EngineB {
		t.Errorf("expected exactly one re-queued offline job, got %+v", queued)
	}

	// The connected job must still be visible with its reason, not vanished.
	recent, _ := db.RecentJobs(ctx, 10)
	foundReason := false
	for _, j := range recent {
		if j.Engine == EngineA && j.State == StateInterrupted && j.Reason != "" {
			foundReason = true
		}
	}
	if !foundReason {
		t.Error("interrupted connected job lost its failure reason; it must be explainable")
	}
}

// TestBillableSecondsRecorded is what the savings counter depends on.
func TestBillableSecondsRecorded(t *testing.T) {
	ctx := context.Background()
	db := newDB(t)

	j := &Job{ExternalID: "billing", Profile: "p", RepoSlug: "a/b", Engine: EngineB}
	if _, err := db.EnqueueJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	if _, err := db.NextQueued(ctx, EngineB); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if err := db.FinishJob(ctx, j.ID, StateSucceeded, 0, ""); err != nil {
		t.Fatal(err)
	}

	got, err := db.Job(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.BillableSeconds <= 0 {
		t.Fatalf("billable_seconds = %v; the savings counter would always read $0",
			got.BillableSeconds)
	}
	if got.FinishedAt == nil {
		t.Error("finished_at not recorded")
	}
}

// TestStatsCountsToday sanity-checks the status dashboard numbers.
func TestStatsCountsToday(t *testing.T) {
	ctx := context.Background()
	db := newDB(t)

	for i := 0; i < 3; i++ {
		j := &Job{ExternalID: fmt.Sprintf("s%d", i), Profile: "p", RepoSlug: "a/b", Engine: EngineB}
		db.EnqueueJob(ctx, j)
		db.NextQueued(ctx, EngineB)
		state := StateSucceeded
		if i == 2 {
			state = StateFailed
		}
		db.FinishJob(ctx, j.ID, state, 0, "")
	}

	st, err := db.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.SucceededToday != 2 {
		t.Errorf("SucceededToday = %d, want 2", st.SucceededToday)
	}
	if st.FailedToday != 1 {
		t.Errorf("FailedToday = %d, want 1", st.FailedToday)
	}
	if st.PendingReplay != 3 {
		t.Errorf("PendingReplay = %d, want 3", st.PendingReplay)
	}
}

// TestRunnerRegistrationIsIdempotent covers re-running `homeplate link`.
func TestRunnerRegistrationIsIdempotent(t *testing.T) {
	ctx := context.Background()
	db := newDB(t)

	for i := 0; i < 4; i++ {
		if err := db.RecordRunner(ctx, "personal", "repo", "acme/widgets", "runner-1",
			[]string{"homeplate", "homeplate-linux"}, "/tmp/w"); err != nil {
			t.Fatalf("RecordRunner %d: %v", i, err)
		}
	}
	rows, err := db.Runners(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("recorded %d runner rows, want 1 (registration must be idempotent)", len(rows))
	}
	if len(rows[0].Labels) != 2 {
		t.Errorf("labels = %v, want 2", rows[0].Labels)
	}
}

// TestStepsAreUpsertedNotDuplicated covers step recording on retry.
func TestStepsAreUpsertedNotDuplicated(t *testing.T) {
	ctx := context.Background()
	db := newDB(t)
	j := &Job{ExternalID: "steps", Profile: "p", RepoSlug: "a/b", Engine: EngineB}
	db.EnqueueJob(ctx, j)

	for i := 0; i < 3; i++ {
		if err := db.AddStep(ctx, &Step{JobID: j.ID, Number: 1, Name: "checkout", ExitCode: i}); err != nil {
			t.Fatal(err)
		}
	}
	steps, err := db.Steps(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 {
		t.Fatalf("got %d steps, want 1 (upsert on job+number)", len(steps))
	}
	if steps[0].ExitCode != 2 {
		t.Errorf("exit code = %d, want the latest value 2", steps[0].ExitCode)
	}
}
