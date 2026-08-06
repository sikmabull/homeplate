// Package store is Homerun's durable state: the job queue, run results, and
// the replay ledger that makes reconnect idempotent.
//
// SQLite (pure-Go, no cgo) is used so the daemon can crash, the machine can
// sleep, and the queue survives. Every write that matters is wrapped in a
// transaction, and replay uses a UNIQUE constraint - not application logic -
// to guarantee "never double-post".
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Engine identifies which execution backend produced a result.
type Engine string

const (
	// EngineA is the official actions/runner wrapped by Homerun (connected).
	EngineA Engine = "connected"
	// EngineB is nektos/act (offline/standalone).
	EngineB Engine = "offline"
)

// JobState is the lifecycle of a queued job.
type JobState string

const (
	StateQueued    JobState = "queued"
	StateRunning   JobState = "running"
	StateSucceeded JobState = "succeeded"
	StateFailed    JobState = "failed"
	StateCancelled JobState = "cancelled"
	// StateInterrupted means the machine slept or the daemon died mid-job.
	StateInterrupted JobState = "interrupted"
)

// Terminal reports whether no further execution will occur.
func (s JobState) Terminal() bool {
	switch s {
	case StateSucceeded, StateFailed, StateCancelled, StateInterrupted:
		return true
	}
	return false
}

// Job is one workflow job executed (or to be executed) on this machine.
type Job struct {
	ID           int64
	ExternalID   string // stable dedupe key: engine+repo+sha+workflow+job
	Profile      string
	RepoSlug     string
	Engine       Engine
	Workflow     string // workflow file name, e.g. "ci.yml"
	WorkflowName string
	JobName      string
	CommitSHA    string
	Ref          string
	EventName    string
	RunsOn       string
	State        JobState
	ExitCode     int
	Reason       string // human-readable failure/interruption reason
	QueuedAt     time.Time
	StartedAt    *time.Time
	FinishedAt   *time.Time
	// BillableSeconds is wall-clock execution time used for the savings counter.
	BillableSeconds float64
	// RunnerClass is linux|macos|windows, used to price the saved minutes.
	RunnerClass string
	MachineID   string
	LogPath     string
	// Replayed marks that results have been pushed to GitHub.
	Replayed   bool
	ReplayedAt *time.Time
}

// Step is a per-step record with exit code and log offsets.
type Step struct {
	ID       int64
	JobID    int64
	Number   int
	Name     string
	ExitCode int
	Started  time.Time
	Finished time.Time
	Output   string
}

// ReplayKind is the type of artifact pushed back to GitHub on reconnect.
type ReplayKind string

const (
	ReplayStatus   ReplayKind = "status"
	ReplayCheckRun ReplayKind = "check_run"
	ReplayApprove  ReplayKind = "approve"
	ReplayMerge    ReplayKind = "merge"
	ReplayComment  ReplayKind = "comment"
)

// DB wraps the SQLite handle.
type DB struct {
	sql  *sql.DB
	path string
}

// Path is the default database location.
func Path(homeDir string) string { return filepath.Join(homeDir, "homerun.db") }

// Open creates or opens the database and applies migrations.
func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// _pragma busy_timeout keeps concurrent CLI+daemon access from failing
	// instantly on lock contention; WAL lets the CLI read while the daemon writes.
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite writes serialize anyway; a small pool avoids lock churn.
	sqlDB.SetMaxOpenConns(4)
	db := &DB{sql: sqlDB, path: path}
	if err := db.migrate(context.Background()); err != nil {
		sqlDB.Close()
		return nil, err
	}
	return db, nil
}

// Close releases the handle.
func (d *DB) Close() error { return d.sql.Close() }

// SQL exposes the raw handle for tests.
func (d *DB) SQL() *sql.DB { return d.sql }

const schema = `
CREATE TABLE IF NOT EXISTS jobs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  external_id TEXT NOT NULL UNIQUE,
  profile TEXT NOT NULL,
  repo_slug TEXT NOT NULL,
  engine TEXT NOT NULL,
  workflow TEXT NOT NULL DEFAULT '',
  workflow_name TEXT NOT NULL DEFAULT '',
  job_name TEXT NOT NULL DEFAULT '',
  commit_sha TEXT NOT NULL DEFAULT '',
  ref TEXT NOT NULL DEFAULT '',
  event_name TEXT NOT NULL DEFAULT '',
  runs_on TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL,
  exit_code INTEGER NOT NULL DEFAULT 0,
  reason TEXT NOT NULL DEFAULT '',
  queued_at TIMESTAMP NOT NULL,
  started_at TIMESTAMP,
  finished_at TIMESTAMP,
  billable_seconds REAL NOT NULL DEFAULT 0,
  runner_class TEXT NOT NULL DEFAULT 'linux',
  machine_id TEXT NOT NULL DEFAULT '',
  log_path TEXT NOT NULL DEFAULT '',
  replayed INTEGER NOT NULL DEFAULT 0,
  replayed_at TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_jobs_state ON jobs(state);
CREATE INDEX IF NOT EXISTS idx_jobs_replay ON jobs(engine, state, replayed);
CREATE INDEX IF NOT EXISTS idx_jobs_repo ON jobs(repo_slug, commit_sha);
CREATE INDEX IF NOT EXISTS idx_jobs_finished ON jobs(finished_at);

CREATE TABLE IF NOT EXISTS steps (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  job_id INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
  number INTEGER NOT NULL,
  name TEXT NOT NULL,
  exit_code INTEGER NOT NULL DEFAULT 0,
  started_at TIMESTAMP,
  finished_at TIMESTAMP,
  output TEXT NOT NULL DEFAULT '',
  UNIQUE(job_id, number)
);

-- The replay ledger. The UNIQUE constraint is the idempotency guarantee:
-- posting the same (job, kind, target) twice is impossible at the storage
-- layer, not merely unlikely at the application layer.
CREATE TABLE IF NOT EXISTS replays (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  job_id INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
  kind TEXT NOT NULL,
  target TEXT NOT NULL,
  remote_id TEXT NOT NULL DEFAULT '',
  succeeded INTEGER NOT NULL DEFAULT 0,
  attempts INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMP NOT NULL,
  updated_at TIMESTAMP NOT NULL,
  UNIQUE(job_id, kind, target)
);

CREATE TABLE IF NOT EXISTS runners (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  profile TEXT NOT NULL,
  scope TEXT NOT NULL,
  slug TEXT NOT NULL,
  runner_name TEXT NOT NULL,
  labels TEXT NOT NULL DEFAULT '',
  remote_id INTEGER NOT NULL DEFAULT 0,
  work_dir TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL DEFAULT 'registered',
  created_at TIMESTAMP NOT NULL,
  UNIQUE(profile, scope, slug, runner_name)
);

CREATE TABLE IF NOT EXISTS meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
`

func (d *DB) migrate(ctx context.Context) error {
	if _, err := d.sql.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

// SetMeta stores a key/value pair (machine id, last sync time, etc).
func (d *DB) SetMeta(ctx context.Context, key, value string) error {
	_, err := d.sql.ExecContext(ctx,
		`INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		key, value)
	return err
}

// GetMeta reads a key, returning "" when absent.
func (d *DB) GetMeta(ctx context.Context, key string) (string, error) {
	var v string
	err := d.sql.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// EnqueueJob inserts a job, or returns the existing one if ExternalID matches.
// This is what makes offline re-runs of the same commit not pile up duplicates.
func (d *DB) EnqueueJob(ctx context.Context, j *Job) (created bool, err error) {
	if j.ExternalID == "" {
		return false, errors.New("store: job ExternalID required")
	}
	if j.QueuedAt.IsZero() {
		j.QueuedAt = time.Now().UTC()
	}
	if j.State == "" {
		j.State = StateQueued
	}
	res, err := d.sql.ExecContext(ctx, `
		INSERT INTO jobs(external_id, profile, repo_slug, engine, workflow, workflow_name, job_name,
			commit_sha, ref, event_name, runs_on, state, queued_at, runner_class, machine_id, log_path)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(external_id) DO NOTHING`,
		j.ExternalID, j.Profile, j.RepoSlug, string(j.Engine), j.Workflow, j.WorkflowName, j.JobName,
		j.CommitSHA, j.Ref, j.EventName, j.RunsOn, string(j.State), j.QueuedAt, j.RunnerClass,
		j.MachineID, j.LogPath)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		existing, err := d.JobByExternalID(ctx, j.ExternalID)
		if err != nil {
			return false, err
		}
		*j = *existing
		return false, nil
	}
	id, err := res.LastInsertId()
	if err != nil {
		return true, err
	}
	j.ID = id
	return true, nil
}

const jobCols = `id, external_id, profile, repo_slug, engine, workflow, workflow_name, job_name,
	commit_sha, ref, event_name, runs_on, state, exit_code, reason, queued_at, started_at, finished_at,
	billable_seconds, runner_class, machine_id, log_path, replayed, replayed_at`

func scanJob(rows interface{ Scan(...any) error }) (*Job, error) {
	var j Job
	var engine, state string
	var started, finished, replayedAt sql.NullTime
	var replayed int
	err := rows.Scan(&j.ID, &j.ExternalID, &j.Profile, &j.RepoSlug, &engine, &j.Workflow, &j.WorkflowName,
		&j.JobName, &j.CommitSHA, &j.Ref, &j.EventName, &j.RunsOn, &state, &j.ExitCode, &j.Reason,
		&j.QueuedAt, &started, &finished, &j.BillableSeconds, &j.RunnerClass, &j.MachineID, &j.LogPath,
		&replayed, &replayedAt)
	if err != nil {
		return nil, err
	}
	j.Engine = Engine(engine)
	j.State = JobState(state)
	if started.Valid {
		t := started.Time
		j.StartedAt = &t
	}
	if finished.Valid {
		t := finished.Time
		j.FinishedAt = &t
	}
	if replayedAt.Valid {
		t := replayedAt.Time
		j.ReplayedAt = &t
	}
	j.Replayed = replayed != 0
	return &j, nil
}

// JobByExternalID looks up a job by its dedupe key.
func (d *DB) JobByExternalID(ctx context.Context, extID string) (*Job, error) {
	row := d.sql.QueryRowContext(ctx, `SELECT `+jobCols+` FROM jobs WHERE external_id=?`, extID)
	return scanJob(row)
}

// Job fetches by primary key.
func (d *DB) Job(ctx context.Context, id int64) (*Job, error) {
	row := d.sql.QueryRowContext(ctx, `SELECT `+jobCols+` FROM jobs WHERE id=?`, id)
	return scanJob(row)
}

func (d *DB) queryJobs(ctx context.Context, where string, args ...any) ([]*Job, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT `+jobCols+` FROM jobs `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// NextQueued claims the oldest queued job for execution, atomically flipping it
// to running so two daemon loops cannot claim the same job.
func (d *DB) NextQueued(ctx context.Context, engine Engine) (*Job, error) {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	row := tx.QueryRowContext(ctx,
		`SELECT `+jobCols+` FROM jobs WHERE state=? AND engine=? ORDER BY queued_at ASC LIMIT 1`,
		string(StateQueued), string(engine))
	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET state=?, started_at=? WHERE id=?`,
		string(StateRunning), now, j.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	j.State = StateRunning
	j.StartedAt = &now
	return j, nil
}

// FinishJob records terminal state, exit code, and billable time.
//
// The elapsed time is computed in Go rather than with SQLite's julianday(),
// because the driver binds time.Time in a format julianday() cannot parse and
// silently yields NULL - which then violates the NOT NULL constraint on
// billable_seconds and, worse, would zero out the savings counter.
func (d *DB) FinishJob(ctx context.Context, id int64, state JobState, exitCode int, reason string) error {
	now := time.Now().UTC()

	var started sql.NullTime
	if err := d.sql.QueryRowContext(ctx, `SELECT started_at FROM jobs WHERE id=?`, id).Scan(&started); err != nil {
		return err
	}
	seconds := 0.0
	if started.Valid {
		if d := now.Sub(started.Time).Seconds(); d > 0 {
			seconds = d
		}
	}

	_, err := d.sql.ExecContext(ctx, `
		UPDATE jobs
		SET state=?, exit_code=?, reason=?, finished_at=?, billable_seconds=?
		WHERE id=?`,
		string(state), exitCode, reason, now, seconds, id)
	return err
}

// MarkInterrupted flips any job left in `running` back to interrupted. Called
// on daemon start: if we are starting up and a job says "running", the machine
// slept or the daemon was killed mid-job. Never silently drop it.
func (d *DB) MarkInterrupted(ctx context.Context, reason string) (int64, error) {
	now := time.Now().UTC()
	res, err := d.sql.ExecContext(ctx,
		`UPDATE jobs SET state=?, reason=?, finished_at=? WHERE state=?`,
		string(StateInterrupted), reason, now, string(StateRunning))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RequeueInterrupted puts interrupted offline jobs back in the queue, which is
// the documented behaviour for Engine B (Engine A defers to GitHub's re-run).
func (d *DB) RequeueInterrupted(ctx context.Context) (int64, error) {
	res, err := d.sql.ExecContext(ctx,
		`UPDATE jobs SET state=?, reason='', started_at=NULL, finished_at=NULL
		 WHERE state=? AND engine=?`,
		string(StateQueued), string(StateInterrupted), string(EngineB))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PendingReplay returns finished offline jobs that have not been pushed to
// GitHub yet, oldest first so replay preserves causal order.
func (d *DB) PendingReplay(ctx context.Context) ([]*Job, error) {
	return d.queryJobs(ctx,
		`WHERE engine=? AND replayed=0 AND state IN (?,?,?) ORDER BY finished_at ASC, id ASC`,
		string(EngineB), string(StateSucceeded), string(StateFailed), string(StateInterrupted))
}

// QueuedJobs lists everything waiting.
func (d *DB) QueuedJobs(ctx context.Context) ([]*Job, error) {
	return d.queryJobs(ctx, `WHERE state=? ORDER BY queued_at ASC`, string(StateQueued))
}

// RunningJobs lists in-flight work.
func (d *DB) RunningJobs(ctx context.Context) ([]*Job, error) {
	return d.queryJobs(ctx, `WHERE state=? ORDER BY started_at ASC`, string(StateRunning))
}

// RecentJobs returns the newest jobs for `homerun status`/`logs`.
func (d *DB) RecentJobs(ctx context.Context, limit int) ([]*Job, error) {
	if limit <= 0 {
		limit = 20
	}
	return d.queryJobs(ctx, `ORDER BY id DESC LIMIT ?`, limit)
}

// JobsSince returns jobs finished at or after t (savings counter).
func (d *DB) JobsSince(ctx context.Context, t time.Time) ([]*Job, error) {
	return d.queryJobs(ctx, `WHERE finished_at IS NOT NULL AND finished_at >= ? ORDER BY finished_at ASC`, t.UTC())
}

// AddStep records a step result.
func (d *DB) AddStep(ctx context.Context, s *Step) error {
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO steps(job_id, number, name, exit_code, started_at, finished_at, output)
		VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(job_id, number) DO UPDATE SET
			name=excluded.name, exit_code=excluded.exit_code,
			finished_at=excluded.finished_at, output=excluded.output`,
		s.JobID, s.Number, s.Name, s.ExitCode, s.Started, s.Finished, s.Output)
	return err
}

// Steps returns a job's steps in order.
func (d *DB) Steps(ctx context.Context, jobID int64) ([]Step, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT id, job_id, number, name, exit_code, started_at, finished_at, output
		 FROM steps WHERE job_id=? ORDER BY number ASC`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Step
	for rows.Next() {
		var s Step
		var started, finished sql.NullTime
		if err := rows.Scan(&s.ID, &s.JobID, &s.Number, &s.Name, &s.ExitCode, &started, &finished, &s.Output); err != nil {
			return nil, err
		}
		if started.Valid {
			s.Started = started.Time
		}
		if finished.Valid {
			s.Finished = finished.Time
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ClaimReplay reserves a replay slot. It returns done=true when this exact
// (job, kind, target) has already been pushed successfully, which is how
// reconnect replay stays idempotent across daemon restarts and partial failures.
func (d *DB) ClaimReplay(ctx context.Context, jobID int64, kind ReplayKind, target string) (done bool, err error) {
	now := time.Now().UTC()
	var succeeded int
	err = d.sql.QueryRowContext(ctx,
		`SELECT succeeded FROM replays WHERE job_id=? AND kind=? AND target=?`,
		jobID, string(kind), target).Scan(&succeeded)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = d.sql.ExecContext(ctx,
			`INSERT INTO replays(job_id, kind, target, attempts, created_at, updated_at)
			 VALUES(?,?,?,0,?,?)`, jobID, string(kind), target, now, now)
		return false, err
	case err != nil:
		return false, err
	}
	return succeeded != 0, nil
}

// CompleteReplay marks a replay row successful with the remote object id.
func (d *DB) CompleteReplay(ctx context.Context, jobID int64, kind ReplayKind, target, remoteID string) error {
	_, err := d.sql.ExecContext(ctx,
		`UPDATE replays SET succeeded=1, remote_id=?, last_error='', attempts=attempts+1, updated_at=?
		 WHERE job_id=? AND kind=? AND target=?`,
		remoteID, time.Now().UTC(), jobID, string(kind), target)
	return err
}

// FailReplay records an attempt failure for backoff and `homerun status`.
func (d *DB) FailReplay(ctx context.Context, jobID int64, kind ReplayKind, target string, cause error) error {
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}
	if len(msg) > 2000 {
		msg = msg[:2000]
	}
	_, err := d.sql.ExecContext(ctx,
		`UPDATE replays SET attempts=attempts+1, last_error=?, updated_at=?
		 WHERE job_id=? AND kind=? AND target=?`,
		msg, time.Now().UTC(), jobID, string(kind), target)
	return err
}

// ReplayAttempts returns how many times a replay has been tried.
func (d *DB) ReplayAttempts(ctx context.Context, jobID int64, kind ReplayKind, target string) (int, error) {
	var n int
	err := d.sql.QueryRowContext(ctx,
		`SELECT attempts FROM replays WHERE job_id=? AND kind=? AND target=?`,
		jobID, string(kind), target).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return n, err
}

// MarkJobReplayed flips the job's replayed flag once all its artifacts landed.
func (d *DB) MarkJobReplayed(ctx context.Context, jobID int64) error {
	now := time.Now().UTC()
	_, err := d.sql.ExecContext(ctx, `UPDATE jobs SET replayed=1, replayed_at=? WHERE id=?`, now, jobID)
	return err
}

// RecordRunner persists a runner registration for cleanup and status display.
func (d *DB) RecordRunner(ctx context.Context, profile, scope, slug, name string, labels []string, workDir string) error {
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO runners(profile, scope, slug, runner_name, labels, work_dir, state, created_at)
		VALUES(?,?,?,?,?,?,'registered',?)
		ON CONFLICT(profile, scope, slug, runner_name) DO UPDATE SET
			labels=excluded.labels, work_dir=excluded.work_dir, state='registered'`,
		profile, scope, slug, name, strings.Join(labels, ","), workDir, time.Now().UTC())
	return err
}

// SetRunnerState updates a runner row.
func (d *DB) SetRunnerState(ctx context.Context, profile, slug, name, state string) error {
	_, err := d.sql.ExecContext(ctx,
		`UPDATE runners SET state=? WHERE profile=? AND slug=? AND runner_name=?`,
		state, profile, slug, name)
	return err
}

// RunnerRow is a stored runner registration.
type RunnerRow struct {
	Profile   string
	Scope     string
	Slug      string
	Name      string
	Labels    []string
	WorkDir   string
	State     string
	CreatedAt time.Time
}

// Runners lists all recorded runner registrations.
func (d *DB) Runners(ctx context.Context) ([]RunnerRow, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT profile, scope, slug, runner_name, labels, work_dir, state, created_at FROM runners ORDER BY slug`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunnerRow
	for rows.Next() {
		var r RunnerRow
		var labels string
		if err := rows.Scan(&r.Profile, &r.Scope, &r.Slug, &r.Name, &labels, &r.WorkDir, &r.State, &r.CreatedAt); err != nil {
			return nil, err
		}
		if labels != "" {
			r.Labels = strings.Split(labels, ",")
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Stats summarises the queue for `homerun status`.
type Stats struct {
	Queued         int
	Running        int
	SucceededToday int
	FailedToday    int
	PendingReplay  int
}

// Stats computes queue counters. "Today" is local-midnight based, matching
// what a user sees on their own clock.
func (d *DB) Stats(ctx context.Context) (Stats, error) {
	var s Stats
	now := time.Now()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).UTC()

	q := func(dst *int, query string, args ...any) error {
		return d.sql.QueryRowContext(ctx, query, args...).Scan(dst)
	}
	if err := q(&s.Queued, `SELECT COUNT(*) FROM jobs WHERE state=?`, string(StateQueued)); err != nil {
		return s, err
	}
	if err := q(&s.Running, `SELECT COUNT(*) FROM jobs WHERE state=?`, string(StateRunning)); err != nil {
		return s, err
	}
	if err := q(&s.SucceededToday, `SELECT COUNT(*) FROM jobs WHERE state=? AND finished_at>=?`,
		string(StateSucceeded), midnight); err != nil {
		return s, err
	}
	if err := q(&s.FailedToday, `SELECT COUNT(*) FROM jobs WHERE state IN (?,?) AND finished_at>=?`,
		string(StateFailed), string(StateInterrupted), midnight); err != nil {
		return s, err
	}
	if err := q(&s.PendingReplay, `SELECT COUNT(*) FROM jobs WHERE engine=? AND replayed=0 AND state IN (?,?)`,
		string(EngineB), string(StateSucceeded), string(StateFailed)); err != nil {
		return s, err
	}
	return s, nil
}

// EncodeJSON is a helper for storing structured blobs in meta.
func EncodeJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}
