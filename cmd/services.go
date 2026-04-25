package cmd

import (
	agentservice "github.com/itsHabib/orchestra/internal/agents"
	"github.com/itsHabib/orchestra/internal/store"
	"github.com/itsHabib/orchestra/internal/store/filestore"
)

func newAgentService(workspace string) (store.Store, *agentservice.Service, error) {
	st := newAgentStore(workspace)
	svc, err := newAgentServiceFromStore(st)
	if err != nil {
		return nil, nil, err
	}
	return st, svc, nil
}

func newAgentStore(workspace string) store.Store {
	return filestore.New(workspace)
}

func newAgentServiceFromStore(st store.Store) (*agentservice.Service, error) {
	svc, err := agentservice.NewHostService(st)
	if err != nil {
		return nil, err
	}
	return svc, nil
}

func excludeCacheRecords(records []store.AgentRecord) func(key, agentID string) bool {
	cached := make(map[string]string, len(records))
	for i := range records {
		rec := &records[i]
		cached[rec.Key] = rec.AgentID
	}
	return func(key, agentID string) bool {
		return cached[key] == agentID
	}
}
