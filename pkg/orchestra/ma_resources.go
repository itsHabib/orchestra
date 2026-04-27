package orchestra

import (
	"context"
	"fmt"

	"github.com/itsHabib/orchestra/internal/customtools"
	"github.com/itsHabib/orchestra/internal/event"
	"github.com/itsHabib/orchestra/internal/skills"
	"github.com/itsHabib/orchestra/internal/spawner"
)

// teamResources is the pre-resolved skill/tool bundle resolveTeamResources
// returns. Keyed by team name; a team with no skills or no custom tools
// simply has no entry under that name (a nil slice read is safe).
type teamResources struct {
	Skills      map[string][]spawner.Skill
	CustomTools map[string][]spawner.Tool
}

// resolveTeamResources reads the live skills cache and customtools registry,
// validates every team.Skills/CustomTools entry against them, and returns
// pre-resolved slices keyed by team name.
//
// Resolution happens once at run-construction time so the agent-creation hot
// path is a map read. Unknown skill/tool names under managed_agents fail
// fast here — a missing skill_id at session-create time would surface as an
// opaque API error 30 seconds into the run.
func resolveTeamResources(ctx context.Context, cfg *Config, emitter event.Emitter) (*teamResources, error) {
	skillEntries, err := loadSkillEntries(ctx)
	if err != nil {
		return nil, err
	}
	skillNames := nameSetFromSkillEntries(skillEntries)
	toolNames := nameSetFromCustomToolDefs()

	validation := cfg.ValidateResourceReferences(skillNames, toolNames)
	for _, w := range validation.Warnings {
		emitter.Emit(Event{
			Kind:    EventWarn,
			Tier:    -1,
			Team:    w.Team,
			Message: w.String(),
		})
	}
	if !validation.Valid() {
		return nil, validation.Err()
	}

	out := &teamResources{
		Skills:      make(map[string][]spawner.Skill),
		CustomTools: make(map[string][]spawner.Tool),
	}
	for i := range cfg.Teams {
		t := &cfg.Teams[i]
		if err := populateTeamResources(t, skillEntries, out); err != nil {
			return nil, fmt.Errorf("team %q: %w", t.Name, err)
		}
	}
	return out, nil
}

func populateTeamResources(t *Team, skillEntries map[string]skills.Entry, out *teamResources) error {
	if len(t.Skills) > 0 {
		resolved, err := resolveSkillsForTeam(t, skillEntries)
		if err != nil {
			return err
		}
		out.Skills[t.Name] = resolved
	}
	if len(t.CustomTools) > 0 {
		resolved, err := resolveCustomToolsForTeam(t)
		if err != nil {
			return err
		}
		out.CustomTools[t.Name] = resolved
	}
	return nil
}

func nameSetFromSkillEntries(entries map[string]skills.Entry) map[string]bool {
	out := make(map[string]bool, len(entries))
	for name := range entries {
		out[name] = true
	}
	return out
}

func nameSetFromCustomToolDefs() map[string]bool {
	defs := customtools.Definitions()
	out := make(map[string]bool, len(defs))
	for i := range defs {
		out[defs[i].Name] = true
	}
	return out
}

// loadSkillEntries reads the on-disk skills cache directly (no API client
// required for read-only resolution). The returned map is keyed by skill name
// — Entry.SkillID is the registered ID we'll feed to the agent spec.
func loadSkillEntries(ctx context.Context) (map[string]skills.Entry, error) {
	cache := skills.NewFileCache(skills.DefaultCachePath())
	entries, err := cache.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("read skills cache: %w", err)
	}
	// Drop legacy rows whose SkillID is empty (e.g. cache files from before
	// the Files-API → native-skill switch). They'd validate as "unknown"
	// otherwise, which is what we want — but pruning here makes the
	// validator's "name not registered" message accurate instead of
	// confusing the user with "you registered it but it's broken."
	live := make(map[string]skills.Entry, len(entries))
	for name, e := range entries {
		if e.SkillID == "" {
			continue
		}
		live[name] = e
	}
	return live, nil
}

func resolveSkillsForTeam(t *Team, entries map[string]skills.Entry) ([]spawner.Skill, error) {
	out := make([]spawner.Skill, 0, len(t.Skills))
	for j := range t.Skills {
		ref := &t.Skills[j]
		entry, ok := entries[ref.Name]
		if !ok {
			// ValidateResourceReferences should have caught this under MA;
			// a defensive error keeps a future caller skipping validation
			// from producing a session with a missing skill_id.
			return nil, fmt.Errorf("skill %q is not registered", ref.Name)
		}
		version := ref.Version
		if version == "" {
			version = entry.LatestVersion
		}
		kind := ref.Type
		if kind == "" {
			kind = "custom"
		}
		// spawner.Skill.Name is the MA-API skill_id (the value returned
		// by Beta.Skills.New), not the orchestra-side skill name the
		// user typed in their config. The MA SDK's skillParams reads
		// this field as SkillID — calling it Name here is the SDK's
		// inversion, not ours.
		out = append(out, spawner.Skill{
			Name:     entry.SkillID,
			Version:  version,
			Metadata: map[string]string{"type": kind},
		})
	}
	return out, nil
}

func resolveCustomToolsForTeam(t *Team) ([]spawner.Tool, error) {
	out := make([]spawner.Tool, 0, len(t.CustomTools))
	for j := range t.CustomTools {
		ref := &t.CustomTools[j]
		handler, ok := customtools.Lookup(ref.Name)
		if !ok {
			return nil, fmt.Errorf("custom tool %q has no registered handler", ref.Name)
		}
		def := handler.Tool()
		out = append(out, spawner.Tool{
			Name:        def.Name,
			Type:        "custom",
			Description: def.Description,
			InputSchema: def.InputSchema,
		})
	}
	return out, nil
}

// agentsSkillCopy clones a slice of spawner.Skill so the AgentSpec a team
// hands to EnsureAgent doesn't share the slice header with the resolved-once
// map on orchestrationRun. EnsureAgent doesn't mutate the slice today, but
// the engine has been bitten by aliased slices before — the copy is cheap.
func agentsSkillCopy(in []spawner.Skill) []spawner.Skill {
	if len(in) == 0 {
		return nil
	}
	out := make([]spawner.Skill, len(in))
	for i := range in {
		out[i] = in[i]
		if len(in[i].Metadata) > 0 {
			md := make(map[string]string, len(in[i].Metadata))
			for k, v := range in[i].Metadata {
				md[k] = v
			}
			out[i].Metadata = md
		}
	}
	return out
}

// agentsToolCopy clones a slice of spawner.Tool, including a deep copy of
// each tool's InputSchema map and Metadata map so the AgentSpec the team
// hands to EnsureAgent doesn't share map headers with the resolved-once
// store on orchestrationRun. The SDK doesn't currently mutate either map,
// but treating the resolved store as immutable across teams keeps a future
// MA-side mutation from cross-contaminating sibling teams.
func agentsToolCopy(in []spawner.Tool) []spawner.Tool {
	if len(in) == 0 {
		return nil
	}
	out := make([]spawner.Tool, len(in))
	for i := range in {
		out[i] = in[i]
		if schema, ok := in[i].InputSchema.(map[string]any); ok && len(schema) > 0 {
			cloned := make(map[string]any, len(schema))
			for k, v := range schema {
				cloned[k] = v
			}
			out[i].InputSchema = cloned
		}
		if len(in[i].Metadata) > 0 {
			md := make(map[string]string, len(in[i].Metadata))
			for k, v := range in[i].Metadata {
				md[k] = v
			}
			out[i].Metadata = md
		}
	}
	return out
}
