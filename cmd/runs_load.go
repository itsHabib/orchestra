package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/itsHabib/orchestra/internal/store"
)

type runRecord struct {
	id         string
	active     bool
	dir        string
	state      *store.RunState
	modifiedAt time.Time
}

type runAgentRefs struct {
	allAgentIDs       map[string]struct{}
	protectedAgentIDs map[string]struct{}
}

func loadRunRecords(workspace string) ([]runRecord, error) {
	if workspace == "" {
		workspace = workspaceDir
	}
	var records []runRecord

	active, err := readRunStateFile(filepath.Join(workspace, "state.json"))
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		records = append(records, runRecord{
			id:         runIDForState(active.state, "active"),
			active:     true,
			dir:        workspace,
			state:      active.state,
			modifiedAt: active.modifiedAt,
		})
	}

	archiveDir := filepath.Join(workspace, "archive")
	entries, err := os.ReadDir(archiveDir)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			dir := filepath.Join(archiveDir, entry.Name())
			archived, err := readRunStateFile(filepath.Join(dir, "state.json"))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, err
			}
			records = append(records, runRecord{
				id:         runIDForState(archived.state, entry.Name()),
				dir:        dir,
				state:      archived.state,
				modifiedAt: archived.modifiedAt,
			})
		}
	}

	sortRunRecords(records)
	return records, nil
}

type runStateFile struct {
	state      *store.RunState
	modifiedAt time.Time
}

func readRunStateFile(path string) (*runStateFile, error) {
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
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if state.Teams == nil {
		state.Teams = make(map[string]store.TeamState)
	}
	return &runStateFile{state: &state, modifiedAt: info.ModTime().UTC()}, nil
}

func runIDForState(state *store.RunState, fallback string) string {
	if state != nil && state.RunID != "" {
		return state.RunID
	}
	return fallback
}

func sortRunRecords(records []runRecord) {
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

func (r runRecord) startedAt() time.Time {
	if r.state != nil && !r.state.StartedAt.IsZero() {
		return r.state.StartedAt.UTC()
	}
	return r.modifiedAt.UTC()
}

func findRunRecord(records []runRecord, id string) (runRecord, bool) {
	for _, record := range records {
		if record.id == id || (id == "active" && record.active) {
			return record, true
		}
	}
	return runRecord{}, false
}
