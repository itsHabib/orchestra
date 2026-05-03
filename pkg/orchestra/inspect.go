package orchestra

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/itsHabib/orchestra/internal/spawner"
	"github.com/itsHabib/orchestra/internal/store"
	"github.com/itsHabib/orchestra/internal/store/filestore"
)

// RunSummary is the per-run row enumerated by [ListRuns]. Mirrors the
// fields the `orchestra runs ls` table needs, so callers can render
// run-history dashboards without re-reading state.json themselves.
//
// Experimental.
type RunSummary struct {
	// RunID is the recorded RunState.RunID, or "active"/<archive-dir-name>
	// when the persisted state is missing the field.
	RunID string
	// Project is the configured project name.
	Project string
	// StartedAt is the run's start time as recorded in the state file.
	// Falls back to the file's mtime when the persisted state has no
	// StartedAt.
	StartedAt time.Time
	// EndedAt is the latest team end time. Zero when no team has ended
	// (run is still in progress) or the persisted state predates this
	// field.
	EndedAt time.Time
	// Status is the aggregated run status: "done", "failed",
	// "canceled" (mapped from team status "terminated"), "in_progress",
	// or "unknown" when no teams are recorded.
	Status string
	// TotalCost is the sum of CostUSD across all teams.
	TotalCost float64
	// TeamCount is the number of teams in the run.
	TeamCount int
	// State is the underlying [RunState], surfaced so the CLI
	// `orchestra runs ls` renderer (and any caller that wants a richer
	// per-team view) can read team-level fields without a second
	// LoadRun call. Always non-nil for entries returned by [ListRuns].
	State *RunState
	// Active reports whether this entry represents the currently-running
	// run (i.e. the workspace's state.json) versus an archived run.
	Active bool
	// ModifiedAt is the file mtime of the underlying state.json. Used by
	// duration computations as the fallback "ended" timestamp when the
	// persisted state predates the EndedAt field.
	ModifiedAt time.Time
}

// SessionInfo is the per-team row enumerated by [ListSessions]. Carries
// just enough to identify a managed-agents session for steering or audit;
// callers wanting richer details can [LoadRun] for the full state.
//
// Experimental.
type SessionInfo struct {
	// SessionID is the MA session ID recorded in TeamState.SessionID.
	SessionID string
	// Team is the team name from the run config.
	Team string
	// RunID is the parent run's RunState.RunID. Empty when the active
	// state file does not yet record one.
	RunID string
	// StartedAt is the team's start time.
	StartedAt time.Time
	// Status is one of "active" (team status "running"), "archived"
	// (team status "done"), or "terminated" for any other terminal
	// status.
	Status string
	// TeamStatus is the raw TeamState.Status string ("running", "done",
	// "failed", "terminated", "pending", ...). Provided alongside the
	// coarser [SessionInfo.Status] alphabet so renderers that need the
	// exact run-state value can still read it without a [LoadRun] call.
	TeamStatus string
	// Steerable reports whether the team's session can accept steering
	// events (status "running" with a recorded session id) — the rows
	// `orchestra msg` and `orchestra interrupt` can target.
	Steerable bool
	// AgentID is the MA agent identifier recorded in TeamState.AgentID.
	AgentID string
	// LastEventID is the most recent event ID observed on the team's
	// session.
	LastEventID string
	// LastEventAt is the wall-clock time of the most recent observed
	// event. Zero when no event has been observed yet.
	LastEventAt time.Time
}

// ListRuns enumerates past and active runs in workspaceDir. Equivalent to
// `orchestra runs ls`. Reads only; safe to call concurrently with a live
// run in the same workspace — the active state.json is written via
// atomic rename, so observed snapshots are never torn.
//
// The returned slice is sorted newest-first (active first when present,
// then archived runs by StartedAt descending). Returns an empty slice
// (not an error) for a workspace with no runs.
//
// Experimental.
func ListRuns(workspaceDir string) ([]RunSummary, error) {
	records, err := scanRunRecords(workspaceDir)
	if err != nil {
		return nil, err
	}
	out := make([]RunSummary, 0, len(records))
	for _, r := range records {
		out = append(out, r.toSummary())
	}
	return out, nil
}

// LoadRun reads a past run's full state by run ID. Pass "active" to
// load the currently-running run; when no run is in flight, the most
// recent archived run wins. Returns an error wrapping [fs.ErrNotExist]
// when the run is unknown, so callers can use
// errors.Is(err, fs.ErrNotExist).
//
// Reads only; safe to call concurrently with a live run.
//
// Experimental.
func LoadRun(workspaceDir, runID string) (*RunState, error) {
	if runID == "" {
		return nil, fmt.Errorf("orchestra: load run %q: %w", runID, fs.ErrNotExist)
	}
	records, err := scanRunRecords(workspaceDir)
	if err != nil {
		return nil, err
	}
	for i := range records {
		if records[i].id == runID {
			return records[i].state, nil
		}
	}
	if runID == "active" && len(records) > 0 {
		// Prefer the explicitly-active record. scanRunRecords usually puts
		// it at records[0] (newest-first sort), but a stale archive with a
		// later StartedAt would sort ahead of it; scan explicitly so the
		// alias always resolves to the live run when one exists, falling
		// back to the most recent archive otherwise.
		for i := range records {
			if records[i].active {
				return records[i].state, nil
			}
		}
		return records[0].state, nil
	}
	return nil, fmt.Errorf("orchestra: run %q: %w", runID, fs.ErrNotExist)
}

// ListSessions enumerates per-team session info for the active run in
// workspaceDir. Returns an empty slice (not an error) for local-backend
// workspaces or when no run is active.
//
// All teams are returned, including those whose [SessionInfo.SessionID]
// is empty (pending teams, terminated teams without a session). Callers
// that only want steerable rows can filter by [SessionInfo.Steerable].
//
// Order is by team name for stable rendering.
//
// Experimental.
func ListSessions(workspaceDir string) ([]SessionInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultInspectTimeout)
	defer cancel()
	state, err := filestore.ReadActiveRunState(ctx, workspaceDir)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("orchestra: read active run state: %w", err)
	}
	if state.Backend != "" && state.Backend != BackendManagedAgents {
		return nil, nil
	}
	rows := spawner.ListTeamSessions(state)
	out := make([]SessionInfo, 0, len(rows))
	for _, row := range rows {
		ts, ok := state.Agents[row.Team]
		var startedAt time.Time
		if ok {
			startedAt = ts.StartedAt
		}
		out = append(out, SessionInfo{
			SessionID:   row.SessionID,
			Team:        row.Team,
			RunID:       state.RunID,
			StartedAt:   startedAt,
			Status:      sessionInfoStatus(row.Status),
			TeamStatus:  row.Status,
			Steerable:   row.Steerable,
			AgentID:     row.AgentID,
			LastEventID: row.LastEventID,
			LastEventAt: row.LastEventAt,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Team < out[j].Team })
	return out, nil
}

// defaultInspectTimeout caps the state-file read used by ListSessions —
// the call is just a single state.json read so the budget is small.
const defaultInspectTimeout = 5 * time.Second

// sessionInfoStatus maps a TeamState.Status into the public SessionInfo
// status alphabet documented on [SessionInfo.Status]. Non-terminal states
// like "pending" are preserved as-is so SDK consumers can distinguish
// not-yet-started teams from teams that finished or failed.
func sessionInfoStatus(teamStatus string) string {
	switch teamStatus {
	case "running":
		return "active"
	case "done":
		return "archived"
	case "failed", "canceled":
		return "terminated"
	default:
		return teamStatus
	}
}

// runMetadataRecord mirrors the cmd/runs_load runRecord just enough to
// serve [ListRuns] and [LoadRun]. Kept private so callers consume only
// the SDK-typed [RunSummary] / [RunState].
type runMetadataRecord struct {
	id         string
	active     bool
	state      *store.RunState
	modifiedAt time.Time
}

// scanRunRecords reads the active state.json (if present) and every
// archive subdirectory's state.json into a sorted list (newest first).
// Mirrors cmd/runs_load.go's loadRunRecords without the cmd-side typing.
func scanRunRecords(workspaceDir string) ([]runMetadataRecord, error) {
	if workspaceDir == "" {
		return nil, errors.New("orchestra: empty workspace dir")
	}
	var records []runMetadataRecord

	active, err := readRunMetadata(filepath.Join(workspaceDir, "state.json"))
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		records = append(records, runMetadataRecord{
			id:         runIDOrFallback(active.state, "active"),
			active:     true,
			state:      active.state,
			modifiedAt: active.modifiedAt,
		})
	}

	archiveDir := filepath.Join(workspaceDir, "archive")
	entries, err := os.ReadDir(archiveDir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			path := filepath.Join(archiveDir, entry.Name(), "state.json")
			rec, err := readRunMetadata(path)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, err
			}
			records = append(records, runMetadataRecord{
				id:         runIDOrFallback(rec.state, entry.Name()),
				state:      rec.state,
				modifiedAt: rec.modifiedAt,
			})
		}
	}

	sortRunMetadataRecords(records)
	return records, nil
}

type runMetadataFile struct {
	state      *store.RunState
	modifiedAt time.Time
}

func readRunMetadata(path string) (*runMetadataFile, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var state store.RunState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if state.Agents == nil {
		state.Agents = make(map[string]store.AgentState)
	}
	return &runMetadataFile{state: &state, modifiedAt: info.ModTime().UTC()}, nil
}

func runIDOrFallback(state *store.RunState, fallback string) string {
	if state != nil && state.RunID != "" {
		return state.RunID
	}
	return fallback
}

func sortRunMetadataRecords(records []runMetadataRecord) {
	sort.SliceStable(records, func(i, j int) bool {
		iStarted := records[i].startedAt()
		jStarted := records[j].startedAt()
		if !iStarted.Equal(jStarted) {
			return iStarted.After(jStarted)
		}
		if records[i].active != records[j].active {
			return records[i].active
		}
		return records[i].id > records[j].id
	})
}

func (r runMetadataRecord) startedAt() time.Time {
	if r.state != nil && !r.state.StartedAt.IsZero() {
		return r.state.StartedAt.UTC()
	}
	return r.modifiedAt.UTC()
}

func (r runMetadataRecord) toSummary() RunSummary {
	state := r.state
	summary := RunSummary{
		RunID:      r.id,
		StartedAt:  r.startedAt(),
		State:      state,
		Active:     r.active,
		ModifiedAt: r.modifiedAt,
	}
	if state == nil {
		return summary
	}
	summary.Project = state.Project
	summary.Status = aggregateRunSummaryStatus(state, r.active)
	summary.TeamCount = len(state.Agents)
	for name := range state.Agents {
		summary.TotalCost += state.Agents[name].CostUSD
	}
	summary.EndedAt = latestTeamEndTime(state)
	return summary
}

func aggregateRunSummaryStatus(state *store.RunState, active bool) string {
	if state == nil || len(state.Agents) == 0 {
		return "unknown"
	}
	counts := make(map[string]int)
	for name := range state.Agents {
		counts[firstNonEmpty(state.Agents[name].Status, "pending")]++
	}
	switch {
	case counts["failed"] > 0:
		return "failed"
	case counts["terminated"] == len(state.Agents):
		return "canceled"
	case counts["done"] == len(state.Agents):
		return "done"
	case active || counts["running"] > 0 || counts["idle"] > 0 || counts["pending"] > 0:
		return "in_progress"
	default:
		return "unknown"
	}
}

func latestTeamEndTime(state *store.RunState) time.Time {
	var end time.Time
	if state == nil {
		return end
	}
	for name := range state.Agents {
		ended := state.Agents[name].EndedAt
		if ended.After(end) {
			end = ended
		}
	}
	if end.IsZero() {
		return end
	}
	return end.UTC()
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
