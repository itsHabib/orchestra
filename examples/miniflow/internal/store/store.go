package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"miniflow/internal/model"

	_ "modernc.org/sqlite"
)

// Store provides persistence for workflows, runs, activity runs, and events.
type Store struct {
	db      *sql.DB
	claimMu sync.Mutex // serializes ClaimNextActivity to avoid SQLite lock contention
}

// NewStore opens a SQLite database at the given DSN, creates all required
// tables and indexes, and returns a ready-to-use Store.
func NewStore(dsn string) (*Store, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	// Enable WAL mode and foreign keys.
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("exec %s: %w", pragma, err)
		}
	}

	if err := createTables(db); err != nil {
		db.Close()
		return nil, err
	}

	return &Store{db: db}, nil
}

func createTables(db *sql.DB) error {
	ddl := `
	CREATE TABLE IF NOT EXISTS workflows (
		id TEXT PRIMARY KEY,
		name TEXT UNIQUE NOT NULL,
		definition TEXT NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS workflow_runs (
		id TEXT PRIMARY KEY,
		workflow_id TEXT NOT NULL REFERENCES workflows(id),
		status TEXT NOT NULL DEFAULT 'pending',
		input TEXT DEFAULT '{}',
		output TEXT DEFAULT '',
		current_step INTEGER DEFAULT 0,
		started_at DATETIME,
		completed_at DATETIME,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS activity_runs (
		id TEXT PRIMARY KEY,
		workflow_run_id TEXT NOT NULL REFERENCES workflow_runs(id),
		activity_name TEXT NOT NULL,
		activity_type TEXT NOT NULL DEFAULT '',
		step_index INTEGER NOT NULL,
		status TEXT NOT NULL DEFAULT 'pending',
		input TEXT DEFAULT '{}',
		output TEXT DEFAULT '',
		attempts INTEGER DEFAULT 0,
		max_retries INTEGER DEFAULT 3,
		timeout_seconds INTEGER DEFAULT 30,
		started_at DATETIME,
		completed_at DATETIME,
		last_heartbeat DATETIME,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		workflow_run_id TEXT NOT NULL REFERENCES workflow_runs(id),
		activity_run_id TEXT,
		event_type TEXT NOT NULL,
		payload TEXT DEFAULT '{}',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE INDEX IF NOT EXISTS idx_workflow_runs_status ON workflow_runs(status);
	CREATE INDEX IF NOT EXISTS idx_workflow_runs_workflow_id ON workflow_runs(workflow_id);
	CREATE INDEX IF NOT EXISTS idx_activity_runs_workflow_run_id ON activity_runs(workflow_run_id);
	CREATE INDEX IF NOT EXISTS idx_activity_runs_status ON activity_runs(status);
	CREATE INDEX IF NOT EXISTS idx_events_workflow_run_id ON events(workflow_run_id);
	`
	if _, err := db.Exec(ddl); err != nil {
		return fmt.Errorf("create tables: %w", err)
	}
	return nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// ---------------------------------------------------------------------------
// Workflows
// ---------------------------------------------------------------------------

// CreateWorkflow inserts a new workflow. The Definition field is marshalled
// to JSON for storage. Returns ErrDuplicateWorkflow if the name is taken.
func (s *Store) CreateWorkflow(w *model.Workflow) error {
	defJSON, err := json.Marshal(w.Definition)
	if err != nil {
		return fmt.Errorf("marshal definition: %w", err)
	}

	_, err = s.db.Exec(
		`INSERT INTO workflows (id, name, definition, created_at) VALUES (?, ?, ?, ?)`,
		w.ID, w.Name, string(defJSON), w.CreatedAt,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return ErrDuplicateWorkflow
		}
		return fmt.Errorf("insert workflow: %w", err)
	}
	return nil
}

// GetWorkflow retrieves a workflow by ID. Returns ErrNotFound if absent.
func (s *Store) GetWorkflow(id string) (*model.Workflow, error) {
	row := s.db.QueryRow(
		`SELECT id, name, definition, created_at FROM workflows WHERE id = ?`, id,
	)
	return scanWorkflow(row)
}

// GetWorkflowByName retrieves a workflow by its unique name. Returns
// ErrNotFound if absent.
func (s *Store) GetWorkflowByName(name string) (*model.Workflow, error) {
	row := s.db.QueryRow(
		`SELECT id, name, definition, created_at FROM workflows WHERE name = ?`, name,
	)
	return scanWorkflow(row)
}

// ListWorkflows returns all registered workflows ordered by creation time.
func (s *Store) ListWorkflows() ([]model.Workflow, error) {
	rows, err := s.db.Query(
		`SELECT id, name, definition, created_at FROM workflows ORDER BY created_at ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("list workflows: %w", err)
	}
	defer rows.Close()

	var workflows []model.Workflow
	for rows.Next() {
		w, err := scanWorkflowRow(rows)
		if err != nil {
			return nil, err
		}
		workflows = append(workflows, *w)
	}
	return workflows, rows.Err()
}

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func scanWorkflow(row *sql.Row) (*model.Workflow, error) {
	var w model.Workflow
	var defJSON string
	if err := row.Scan(&w.ID, &w.Name, &defJSON, &w.CreatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scan workflow: %w", err)
	}
	if err := json.Unmarshal([]byte(defJSON), &w.Definition); err != nil {
		return nil, fmt.Errorf("unmarshal definition: %w", err)
	}
	return &w, nil
}

func scanWorkflowRow(rows *sql.Rows) (*model.Workflow, error) {
	var w model.Workflow
	var defJSON string
	if err := rows.Scan(&w.ID, &w.Name, &defJSON, &w.CreatedAt); err != nil {
		return nil, fmt.Errorf("scan workflow: %w", err)
	}
	if err := json.Unmarshal([]byte(defJSON), &w.Definition); err != nil {
		return nil, fmt.Errorf("unmarshal definition: %w", err)
	}
	return &w, nil
}

// ---------------------------------------------------------------------------
// Workflow Runs
// ---------------------------------------------------------------------------

// CreateRun inserts a new workflow run.
func (s *Store) CreateRun(r *model.WorkflowRun) error {
	_, err := s.db.Exec(
		`INSERT INTO workflow_runs (id, workflow_id, status, input, output, current_step, started_at, completed_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.WorkflowID, r.Status, r.Input, r.Output, r.CurrentStep,
		nullTime(r.StartedAt), nullTime(r.CompletedAt), r.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert run: %w", err)
	}
	return nil
}

// GetRun retrieves a workflow run by ID. Returns ErrNotFound if absent.
func (s *Store) GetRun(id string) (*model.WorkflowRun, error) {
	row := s.db.QueryRow(
		`SELECT id, workflow_id, status, input, output, current_step,
		        started_at, completed_at, created_at
		 FROM workflow_runs WHERE id = ?`, id,
	)

	var r model.WorkflowRun
	var startedAt, completedAt sql.NullTime
	if err := row.Scan(
		&r.ID, &r.WorkflowID, &r.Status, &r.Input, &r.Output, &r.CurrentStep,
		&startedAt, &completedAt, &r.CreatedAt,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scan run: %w", err)
	}
	r.StartedAt = fromNullTime(startedAt)
	r.CompletedAt = fromNullTime(completedAt)
	return &r, nil
}

// ListRuns returns workflow runs, optionally filtered by status. If status is
// empty, all runs are returned. Results are limited to the given count (0 =
// no limit).
func (s *Store) ListRuns(status string, limit int) ([]model.WorkflowRun, error) {
	query := `SELECT id, workflow_id, status, input, output, current_step,
	                  started_at, completed_at, created_at
	           FROM workflow_runs`
	var args []any

	if status != "" {
		query += ` WHERE status = ?`
		args = append(args, status)
	}
	query += ` ORDER BY created_at DESC`
	if limit > 0 {
		query += fmt.Sprintf(` LIMIT %d`, limit)
	}

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	defer rows.Close()

	var runs []model.WorkflowRun
	for rows.Next() {
		var r model.WorkflowRun
		var startedAt, completedAt sql.NullTime
		if err := rows.Scan(
			&r.ID, &r.WorkflowID, &r.Status, &r.Input, &r.Output, &r.CurrentStep,
			&startedAt, &completedAt, &r.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan run row: %w", err)
		}
		r.StartedAt = fromNullTime(startedAt)
		r.CompletedAt = fromNullTime(completedAt)
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

// UpdateRun updates a workflow run (all mutable fields).
func (s *Store) UpdateRun(r *model.WorkflowRun) error {
	res, err := s.db.Exec(
		`UPDATE workflow_runs SET status=?, input=?, output=?, current_step=?,
		        started_at=?, completed_at=?
		 WHERE id=?`,
		r.Status, r.Input, r.Output, r.CurrentStep,
		nullTime(r.StartedAt), nullTime(r.CompletedAt), r.ID,
	)
	if err != nil {
		return fmt.Errorf("update run: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------------------
// Activity Runs
// ---------------------------------------------------------------------------

// CreateActivityRun inserts a new activity run.
func (s *Store) CreateActivityRun(ar *model.ActivityRun) error {
	_, err := s.db.Exec(
		`INSERT INTO activity_runs
		   (id, workflow_run_id, activity_name, activity_type, step_index, status, input, output,
		    attempts, max_retries, timeout_seconds, started_at, completed_at, last_heartbeat, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ar.ID, ar.WorkflowRunID, ar.ActivityName, ar.ActivityType, ar.StepIndex,
		ar.Status, ar.Input, ar.Output,
		ar.Attempts, ar.MaxRetries, ar.TimeoutSeconds,
		nullTime(ar.StartedAt), nullTime(ar.CompletedAt), nullTime(ar.LastHeartbeat),
		ar.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert activity run: %w", err)
	}
	return nil
}

// GetActivityRun retrieves an activity run by ID. Returns ErrNotFound if absent.
func (s *Store) GetActivityRun(id string) (*model.ActivityRun, error) {
	row := s.db.QueryRow(
		`SELECT id, workflow_run_id, activity_name, activity_type, step_index, status, input, output,
		        attempts, max_retries, timeout_seconds, started_at, completed_at, last_heartbeat, created_at
		 FROM activity_runs WHERE id = ?`, id,
	)
	return scanActivityRun(row)
}

// GetActivityRunsByRunID returns all activity runs belonging to a workflow run.
func (s *Store) GetActivityRunsByRunID(runID string) ([]model.ActivityRun, error) {
	rows, err := s.db.Query(
		`SELECT id, workflow_run_id, activity_name, activity_type, step_index, status, input, output,
		        attempts, max_retries, timeout_seconds, started_at, completed_at, last_heartbeat, created_at
		 FROM activity_runs WHERE workflow_run_id = ? ORDER BY step_index ASC`, runID,
	)
	if err != nil {
		return nil, fmt.Errorf("list activity runs: %w", err)
	}
	defer rows.Close()

	var ars []model.ActivityRun
	for rows.Next() {
		var ar model.ActivityRun
		var startedAt, completedAt, lastHeartbeat sql.NullTime
		if err := rows.Scan(
			&ar.ID, &ar.WorkflowRunID, &ar.ActivityName, &ar.ActivityType, &ar.StepIndex,
			&ar.Status, &ar.Input, &ar.Output,
			&ar.Attempts, &ar.MaxRetries, &ar.TimeoutSeconds,
			&startedAt, &completedAt, &lastHeartbeat, &ar.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan activity run row: %w", err)
		}
		ar.StartedAt = fromNullTime(startedAt)
		ar.CompletedAt = fromNullTime(completedAt)
		ar.LastHeartbeat = fromNullTime(lastHeartbeat)
		ars = append(ars, ar)
	}
	return ars, rows.Err()
}

// UpdateActivityRun updates an activity run (all mutable fields).
func (s *Store) UpdateActivityRun(ar *model.ActivityRun) error {
	res, err := s.db.Exec(
		`UPDATE activity_runs SET status=?, input=?, output=?,
		        attempts=?, max_retries=?, timeout_seconds=?,
		        started_at=?, completed_at=?, last_heartbeat=?
		 WHERE id=?`,
		ar.Status, ar.Input, ar.Output,
		ar.Attempts, ar.MaxRetries, ar.TimeoutSeconds,
		nullTime(ar.StartedAt), nullTime(ar.CompletedAt), nullTime(ar.LastHeartbeat),
		ar.ID,
	)
	if err != nil {
		return fmt.Errorf("update activity run: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ClaimNextActivity atomically finds the oldest pending activity run and
// marks it as running with started_at set to now. Returns (nil, nil) if
// no pending activity runs exist.
func (s *Store) ClaimNextActivity() (*model.ActivityRun, error) {
	s.claimMu.Lock()
	defer s.claimMu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	row := tx.QueryRow(
		`SELECT id, workflow_run_id, activity_name, activity_type, step_index, status, input, output,
		        attempts, max_retries, timeout_seconds, started_at, completed_at, last_heartbeat, created_at
		 FROM activity_runs
		 WHERE status = ?
		 ORDER BY created_at ASC
		 LIMIT 1`,
		model.StatusPending,
	)

	var ar model.ActivityRun
	var startedAt, completedAt, lastHeartbeat sql.NullTime
	if err := row.Scan(
		&ar.ID, &ar.WorkflowRunID, &ar.ActivityName, &ar.ActivityType, &ar.StepIndex,
		&ar.Status, &ar.Input, &ar.Output,
		&ar.Attempts, &ar.MaxRetries, &ar.TimeoutSeconds,
		&startedAt, &completedAt, &lastHeartbeat, &ar.CreatedAt,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("scan claim: %w", err)
	}
	ar.StartedAt = fromNullTime(startedAt)
	ar.CompletedAt = fromNullTime(completedAt)
	ar.LastHeartbeat = fromNullTime(lastHeartbeat)

	now := time.Now()
	ar.Status = model.StatusRunning
	ar.StartedAt = &now

	_, err = tx.Exec(
		`UPDATE activity_runs SET status = ?, started_at = ? WHERE id = ?`,
		model.StatusRunning, now, ar.ID,
	)
	if err != nil {
		return nil, fmt.Errorf("update claim: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit claim: %w", err)
	}
	return &ar, nil
}

func scanActivityRun(row *sql.Row) (*model.ActivityRun, error) {
	var ar model.ActivityRun
	var startedAt, completedAt, lastHeartbeat sql.NullTime
	if err := row.Scan(
		&ar.ID, &ar.WorkflowRunID, &ar.ActivityName, &ar.ActivityType, &ar.StepIndex,
		&ar.Status, &ar.Input, &ar.Output,
		&ar.Attempts, &ar.MaxRetries, &ar.TimeoutSeconds,
		&startedAt, &completedAt, &lastHeartbeat, &ar.CreatedAt,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scan activity run: %w", err)
	}
	ar.StartedAt = fromNullTime(startedAt)
	ar.CompletedAt = fromNullTime(completedAt)
	ar.LastHeartbeat = fromNullTime(lastHeartbeat)
	return &ar, nil
}

// ---------------------------------------------------------------------------
// Events
// ---------------------------------------------------------------------------

// AppendEvent inserts a new event. The ID field is ignored (auto-incremented).
func (s *Store) AppendEvent(e *model.Event) error {
	res, err := s.db.Exec(
		`INSERT INTO events (workflow_run_id, activity_run_id, event_type, payload, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		e.WorkflowRunID, nullString(e.ActivityRunID), e.EventType, e.Payload, e.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert event: %w", err)
	}
	id, _ := res.LastInsertId()
	e.ID = id
	return nil
}

// ListEvents returns all events for a given workflow run, ordered by creation.
func (s *Store) ListEvents(runID string) ([]model.Event, error) {
	rows, err := s.db.Query(
		`SELECT id, workflow_run_id, activity_run_id, event_type, payload, created_at
		 FROM events WHERE workflow_run_id = ? ORDER BY id ASC`, runID,
	)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()

	var events []model.Event
	for rows.Next() {
		var ev model.Event
		var activityRunID sql.NullString
		if err := rows.Scan(
			&ev.ID, &ev.WorkflowRunID, &activityRunID,
			&ev.EventType, &ev.Payload, &ev.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		if activityRunID.Valid {
			ev.ActivityRunID = activityRunID.String
		}
		events = append(events, ev)
	}
	return events, rows.Err()
}

// ---------------------------------------------------------------------------
// Stats
// ---------------------------------------------------------------------------

// Stats returns a map of workflow run counts grouped by status.
func (s *Store) Stats() (map[string]int, error) {
	rows, err := s.db.Query(
		`SELECT status, COUNT(*) FROM workflow_runs GROUP BY status`,
	)
	if err != nil {
		return nil, fmt.Errorf("stats: %w", err)
	}
	defer rows.Close()

	m := make(map[string]int)
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return nil, fmt.Errorf("scan stats: %w", err)
		}
		m[status] = count
	}
	return m, rows.Err()
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// nullTime converts a *time.Time to sql.NullTime for storage.
func nullTime(t *time.Time) sql.NullTime {
	if t == nil {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: *t, Valid: true}
}

// fromNullTime converts sql.NullTime back to *time.Time.
func fromNullTime(nt sql.NullTime) *time.Time {
	if !nt.Valid {
		return nil
	}
	t := nt.Time
	return &t
}

// nullString converts a string to sql.NullString, treating "" as NULL.
func nullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}
