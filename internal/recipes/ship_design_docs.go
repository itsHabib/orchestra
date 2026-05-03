// Package recipes assembles parameterized *config.Config objects for
// well-known orchestra workflows. Recipes side-step the yaml loader: callers
// pass typed parameters and get back a config the engine can run directly,
// so a workflow like "ship these N docs" doesn't require hand-authoring a
// yaml on every run.
package recipes

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/itsHabib/orchestra/internal/config"
)

// ShipFeatureSkillName is the orchestra skills cache key for the implementer
// skill the recipe attaches to every team. Callers must have run
// `orchestra skills upload ship-feature` against this name before launching
// the recipe; ValidateResourceReferences will reject the config otherwise.
const ShipFeatureSkillName = "ship-feature"

// SignalCompletionToolName is the customtools registry key for the host-side
// sentinel that tells the engine the team is done or blocked.
const SignalCompletionToolName = "signal_completion"

// Default values applied when ShipDesignDocsParams leaves the field zero.
const (
	defaultBranch         = "main"
	defaultModel          = "opus"
	defaultConcurrency    = 4
	defaultTimeoutMinutes = 90
	defaultLeadRole       = "Feature Implementer"
	defaultRepoMountPath  = "/workspace/repo"
	defaultPermissionMode = "acceptEdits"
	defaultMaxTurns       = 200
)

// ShipDesignDocsParams parameterizes the ship-design-docs recipe. Each
// DocPath becomes one tier-0 team driving the /ship-feature skill against
// that doc. Zero-value fields fall back to the recipe defaults; override only
// what you need.
type ShipDesignDocsParams struct {
	// DocPaths are repo-relative paths to design docs. Each becomes one
	// team. Required (must contain at least one entry).
	DocPaths []string

	// RepoURL is the https GitHub URL the agents will clone and push to.
	// Required.
	RepoURL string

	// DefaultBranch is the repository's default branch. Empty defaults
	// to "main".
	DefaultBranch string

	// Model selects the model used by every team. Empty defaults to
	// "opus" (claude-opus-4-7) — the implementer workflow benefits from
	// the larger model.
	Model string

	// Concurrency caps how many MA sessions run in parallel. Zero
	// defaults to 4 (well below MA's 60/min org limit).
	Concurrency int

	// Timeout is the per-team hard timeout. Zero defaults to 90 minutes.
	Timeout time.Duration

	// DisablePullRequests skips the engine's auto-open-PR step. Default
	// (false) means the engine opens a PR when a team's branch lands —
	// the §6.1 design specifies this as default-on for the workflow.
	DisablePullRequests bool

	// RunName is the orchestra project name embedded in the generated
	// config. Empty falls back to "ship-design-docs".
	RunName string
}

// ShipDesignDocs returns a *config.Config equivalent to §6.1 of the design
// doc: one tier-0 team per DocPath, each attaching the ship-feature skill +
// signal_completion custom tool, each driven by the implementer prompt. The
// returned config passes Config.Validate when the caller's params are
// well-formed; the caller must additionally pass it through
// ValidateResourceReferences to confirm the skill is registered.
//
// The pointer-receiver form keeps the call site allocation-free in the
// common case and satisfies the project's strict pointer-arg lint for any
// param struct over ~88 bytes.
func ShipDesignDocs(p *ShipDesignDocsParams) (*config.Config, error) {
	if p == nil {
		return nil, errors.New("recipes: ship-design-docs: nil params")
	}
	if len(p.DocPaths) == 0 {
		return nil, errors.New("recipes: ship-design-docs: at least one doc path is required")
	}
	if p.RepoURL == "" {
		return nil, errors.New("recipes: ship-design-docs: repo url is required")
	}
	if err := validateRepoURL(p.RepoURL); err != nil {
		return nil, err
	}

	branch := orDefault(p.DefaultBranch, defaultBranch)
	model := orDefault(p.Model, defaultModel)
	concurrency := p.Concurrency
	if concurrency == 0 {
		concurrency = defaultConcurrency
	}
	timeoutMinutes := int(p.Timeout / time.Minute)
	if timeoutMinutes == 0 {
		timeoutMinutes = defaultTimeoutMinutes
	}
	runName := orDefault(p.RunName, "ship-design-docs")

	agents := make([]config.Agent, 0, len(p.DocPaths))
	used := make(map[string]bool, len(p.DocPaths))
	for _, docPath := range p.DocPaths {
		if strings.TrimSpace(docPath) == "" {
			return nil, errors.New("recipes: ship-design-docs: empty doc path")
		}
		name := uniqueAgentName(agentNameForDoc(docPath), used)
		used[name] = true
		agents = append(agents, buildShipAgent(name, docPath))
	}

	cfg := &config.Config{
		Name: runName,
		Backend: config.Backend{
			Kind: "managed_agents",
			ManagedAgents: &config.ManagedAgentsBackend{
				Repository: &config.RepositorySpec{
					URL:           p.RepoURL,
					MountPath:     defaultRepoMountPath,
					DefaultBranch: branch,
				},
				OpenPullRequests: !p.DisablePullRequests,
			},
		},
		Defaults: config.Defaults{
			Model:                model,
			MaxTurns:             defaultMaxTurns,
			PermissionMode:       defaultPermissionMode,
			TimeoutMinutes:       timeoutMinutes,
			MAConcurrentSessions: concurrency,
		},
		Agents: agents,
	}
	cfg.ResolveDefaults()
	return cfg, nil
}

func buildShipAgent(name, docPath string) config.Agent {
	return config.Agent{
		Name: name,
		Lead: config.Lead{Role: defaultLeadRole},
		Tasks: []config.Task{
			{
				Summary: "Ship the design doc",
				Details: fmt.Sprintf("Run /ship-feature against %s/%s",
					defaultRepoMountPath, docPath),
			},
		},
		Context: shipFeatureContext(docPath),
		Skills: []config.SkillRef{
			{Name: ShipFeatureSkillName, Type: "custom"},
		},
		CustomTools: []config.CustomToolRef{
			{Name: SignalCompletionToolName},
		},
	}
}

// shipFeatureContext is the per-team narrative the recipe injects into
// team.Context. It quotes §6.1 verbatim where the design doc was
// prescriptive — the agent reads this as Technical Context inside the
// engine-generated prompt.
//
// The narrative hardcodes defaultRepoMountPath because the recipe also
// always writes that constant into config.Backend.ManagedAgents.Repository.
// MountPath; the two are deliberately coupled. If the recipe ever exposes a
// per-call MountPath knob, this function must take it as a parameter to
// keep the prompt and the mount in sync.
func shipFeatureContext(docPath string) string {
	full := defaultRepoMountPath + "/" + docPath
	var b strings.Builder
	fmt.Fprintf(&b, "Apply the %s skill (attached to this session) to drive the\n", ShipFeatureSkillName)
	fmt.Fprintf(&b, "design doc at %s through to a merge-ready pull request.\n", full)
	b.WriteString("\n")
	b.WriteString("Drive it to: PR open, reviews requested (copilot, codex, claude),\n")
	b.WriteString("CI green, all required-changes acked.\n")
	b.WriteString("\n")
	fmt.Fprintf(&b, "When the PR is in the merge-ready state, call %s with\n", SignalCompletionToolName)
	b.WriteString("status=\"done\", pr_url=<the PR URL>, summary=<one-line summary>.\n")
	b.WriteString("\n")
	b.WriteString("If you hit a hard block — genuine ambiguity in the spec, an\n")
	b.WriteString("unresolvable review conflict, a CI failure outside your scope —\n")
	fmt.Fprintf(&b, "call %s with status=\"blocked\", reason=<a sentence the\n", SignalCompletionToolName)
	b.WriteString("human can act on>, then stop.\n")
	b.WriteString("\n")
	b.WriteString("The user can reach you via orchestra's steering channel; treat any\n")
	b.WriteString("incoming user.message as authoritative.\n")
	return b.String()
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// uniqueAgentName returns a team name that's not already in `used`. The first
// time `base` appears it's returned unchanged; subsequent collisions get a
// `-2`, `-3`, … suffix, skipping any suffixed candidate that's *also* taken
// (e.g. when DocPaths contains both `foo.md` and `foo-2.md` and a third
// `foo.md`, the third must become `foo-3` because `foo-2` is already a
// distinct team's name).
//
// Counter starts at 2 because the un-suffixed name covers the first
// occurrence; the first collision lands at counter==2.
func uniqueAgentName(base string, used map[string]bool) string {
	if !used[base] {
		return base
	}
	for counter := 2; ; counter++ {
		candidate := fmt.Sprintf("%s-%d", base, counter)
		if !used[candidate] {
			return candidate
		}
	}
}

// validateRepoURL rejects the most common shapes that survive the empty
// check but fail later in the engine: bare strings ("not-a-url"), non-https
// schemes (the MA backend only mounts https GitHub repositories), and URLs
// without a host. Surface formatting errors here so callers see a clear
// message instead of an opaque MA API failure 30 seconds into the run.
func validateRepoURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("recipes: ship-design-docs: invalid repo url %q: %w", raw, err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("recipes: ship-design-docs: repo url must be https, got scheme %q in %q", u.Scheme, raw)
	}
	if u.Host == "" {
		return fmt.Errorf("recipes: ship-design-docs: repo url has no host: %q", raw)
	}
	return nil
}

// agentNameForDoc derives a stable team name from a doc path. The team name
// is "ship-<slug>" where <slug> is the doc's basename stripped of its
// extension, lowercased, and with non-alphanumeric runs collapsed to "-".
// E.g. "docs/feat-flag-quiet.md" → "ship-feat-flag-quiet"; "FOO BAR.md" →
// "ship-foo-bar".
func agentNameForDoc(docPath string) string {
	base := path.Base(filepath.ToSlash(docPath))
	if dot := strings.LastIndex(base, "."); dot > 0 {
		base = base[:dot]
	}
	var b strings.Builder
	b.WriteString("ship-")
	prevHyphen := true
	for _, r := range strings.ToLower(base) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevHyphen = false
		default:
			if !prevHyphen {
				b.WriteRune('-')
				prevHyphen = true
			}
		}
	}
	out := strings.TrimRight(b.String(), "-")
	if out == "ship-" || out == "ship" {
		// Pathological inputs (only punctuation, etc.). Fall back to
		// a stable but obviously-synthetic name; the duplicate-name
		// check upstream will append a counter if needed.
		return "ship-doc"
	}
	return out
}
