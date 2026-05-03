package orchestra

import (
	"github.com/itsHabib/orchestra/internal/messaging"
	"github.com/itsHabib/orchestra/internal/store"
)

func teamNamesFromConfig(teams []Team) []string {
	names := make([]string, len(teams))
	for i := range teams {
		names[i] = teams[i].Name
	}
	return names
}

func inboxLookupFromParticipants(participants []messaging.Participant) map[string]string {
	lookup := make(map[string]string, len(participants))
	for _, p := range participants {
		lookup[p.Name] = p.FolderName()
	}
	return lookup
}

func (r *orchestrationRun) seedBootstrapMessages(team *Team, state *store.RunState) error {
	for _, dep := range team.DependsOn {
		summary := r.dependencySummary(dep, state)
		if summary == "" {
			continue
		}
		if err := r.bus.Send(r.bootstrapMessage(dep, team.Name, summary)); err != nil {
			return err
		}
	}
	return nil
}

func (r *orchestrationRun) dependencySummary(dep string, state *store.RunState) string {
	depResult, err := r.ws.ReadResult(dep)
	if err != nil || depResult == nil {
		return ""
	}
	if depResult.Result != "" {
		return depResult.Result
	}
	if ts, ok := state.Agents[dep]; ok {
		return ts.ResultSummary
	}
	return ""
}

func (r *orchestrationRun) bootstrapMessage(dep, teamName, summary string) *messaging.Message {
	return &messaging.Message{
		ID:        "bootstrap-" + dep + "-to-" + teamName,
		Sender:    "orchestrator",
		Recipient: r.inboxLookup[teamName],
		Type:      messaging.MsgBootstrap,
		Subject:   "Results from " + dep + " (completed)",
		Content:   summary,
		Timestamp: r.runService.Now(),
		Read:      false,
	}
}
