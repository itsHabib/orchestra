package spawner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"golang.org/x/text/unicode/norm"
)

// canonicalAgentSpec is the JSON-serializable shape used to compute the spec
// hash. Only fields we round-trip through the Managed Agents API are included,
// which keeps specHash and hashFromMAAgent symmetric.
type canonicalAgentSpec struct {
	Model      string           `json:"model"`
	System     string           `json:"system"`
	Tools      []Tool           `json:"tools,omitempty"`
	MCPServers []MCPServer      `json:"mcp_servers,omitempty"`
	Skills     []canonicalSkill `json:"skills,omitempty"`
}

type canonicalEnvSpec struct {
	Packages   PackageSpec `json:"packages"`
	Networking NetworkSpec `json:"networking"`
}

// canonicalSkill is the subset of Skill used for hashing. Arbitrary skill
// metadata is intentionally dropped: the MA API does not return it, so
// including it here would break round-trip equality with hashFromMAAgent.
type canonicalSkill struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	Type    string `json:"type,omitempty"`
}

func specHash(spec *AgentSpec) (string, error) {
	return hashCanonical(canonicalAgentSpec{
		Model:      spec.Model,
		System:     normalizePrompt(spec.SystemPrompt),
		Tools:      canonicalTools(spec.Tools),
		MCPServers: canonicalMCPServers(spec.MCPServers),
		Skills:     canonicalSkills(spec.Skills),
	})
}

func envSpecHash(spec *EnvSpec) (string, error) {
	return hashCanonical(canonicalEnvSpec{
		Packages:   canonicalPackages(&spec.Packages),
		Networking: canonicalNetwork(spec.Networking),
	})
}

func hashCanonical(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("spawner: canonical hash marshal: %w", err)
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func normalizePrompt(s string) string {
	return strings.ReplaceAll(norm.NFC.String(s), "\r\n", "\n")
}

func canonicalTools(tools []Tool) []Tool {
	out := make([]Tool, 0, len(tools))
	for _, tool := range tools {
		next := tool
		next.Type = normalizedToolType(tool)
		next.InputSchema = canonicalAny(tool.InputSchema)
		next.Metadata = cloneStringMap(tool.Metadata)
		out = append(out, next)
	}
	return out
}

func canonicalMCPServers(servers []MCPServer) []MCPServer {
	out := make([]MCPServer, 0, len(servers))
	for _, server := range servers {
		next := server
		next.Metadata = cloneStringMap(server.Metadata)
		out = append(out, next)
	}
	return out
}

func canonicalSkills(skills []Skill) []canonicalSkill {
	out := make([]canonicalSkill, 0, len(skills))
	for _, skill := range skills {
		out = append(out, canonicalSkill{
			Name:    skill.Name,
			Version: skill.Version,
			Type:    skill.Metadata["type"],
		})
	}
	return out
}

func canonicalPackages(packages *PackageSpec) PackageSpec {
	return PackageSpec{
		APT:   append([]string(nil), packages.APT...),
		Cargo: append([]string(nil), packages.Cargo...),
		Gem:   append([]string(nil), packages.Gem...),
		Go:    append([]string(nil), packages.Go...),
		NPM:   append([]string(nil), packages.NPM...),
		Pip:   append([]string(nil), packages.Pip...),
	}
}

func canonicalNetwork(network NetworkSpec) NetworkSpec {
	return NetworkSpec{
		Type:                 network.Type,
		AllowedHosts:         append([]string(nil), network.AllowedHosts...),
		AllowMCPServers:      network.AllowMCPServers,
		AllowPackageManagers: network.AllowPackageManagers,
	}
}

func canonicalAny(v any) any {
	switch typed := v.(type) {
	case nil:
		return nil
	case map[string]any:
		out := make(map[string]any, len(typed))
		for k, val := range typed {
			out[k] = canonicalAny(val)
		}
		return out
	case map[string]string:
		out := make(map[string]string, len(typed))
		for k, val := range typed {
			out[k] = val
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, val := range typed {
			out[i] = canonicalAny(val)
		}
		return out
	default:
		return typed
	}
}

func normalizedToolType(tool Tool) string {
	switch {
	case tool.Type != "":
		return tool.Type
	case isBuiltInTool(tool.Name):
		return "agent_toolset_20260401"
	default:
		return "custom"
	}
}

func isBuiltInTool(name string) bool {
	switch name {
	case "bash", "edit", "read", "write", "glob", "grep", "web_fetch", "web_search":
		return true
	default:
		return false
	}
}

// hashFromMAAgent reconstructs an AgentSpec from a Managed Agents resource and
// returns its canonical hash. The reconstruction only includes fields the MA
// API round-trips, which must match canonicalAgentSpec exactly — otherwise an
// adopted agent would fail an equality check and be needlessly updated.
func hashFromMAAgent(agent *anthropic.BetaManagedAgentsAgent) (string, error) {
	if agent == nil {
		return "", errors.New("spawner: nil managed agent")
	}
	spec := AgentSpec{
		Model:        agent.Model.ID,
		SystemPrompt: agent.System,
		Tools:        toolsFromMAAgent(agent.Tools),
		MCPServers:   mcpServersFromMAAgent(agent.MCPServers),
		Skills:       skillsFromMAAgent(agent.Skills),
	}
	return specHash(&spec)
}

func toolsFromMAAgent(tools []anthropic.BetaManagedAgentsAgentToolUnion) []Tool {
	var out []Tool
	for i := range tools {
		tool := &tools[i]
		switch tool.Type {
		case "agent_toolset_20260401":
			for j := range tool.Configs.OfBetaManagedAgentsAgentToolConfigArray {
				cfg := &tool.Configs.OfBetaManagedAgentsAgentToolConfigArray[j]
				if cfg.Enabled {
					out = append(out, Tool{Name: string(cfg.Name), Type: "agent_toolset_20260401"})
				}
			}
		case "mcp_toolset":
			out = append(out, Tool{Name: tool.MCPServerName, Type: "mcp_toolset"})
		case "custom":
			out = append(out, Tool{
				Name:        tool.Name,
				Type:        "custom",
				Description: tool.Description,
				InputSchema: map[string]any{
					"properties": tool.InputSchema.Properties,
					"required":   tool.InputSchema.Required,
					"type":       string(tool.InputSchema.Type),
				},
			})
		}
	}
	return out
}

func mcpServersFromMAAgent(servers []anthropic.BetaManagedAgentsMCPServerURLDefinition) []MCPServer {
	out := make([]MCPServer, 0, len(servers))
	for i := range servers {
		server := &servers[i]
		out = append(out, MCPServer{
			Name: server.Name,
			Type: string(server.Type),
			URL:  server.URL,
		})
	}
	return out
}

func skillsFromMAAgent(skills []anthropic.BetaManagedAgentsAgentSkillUnion) []Skill {
	out := make([]Skill, 0, len(skills))
	for i := range skills {
		skill := &skills[i]
		out = append(out, Skill{
			Name:     skill.SkillID,
			Version:  skill.Version,
			Metadata: map[string]string{"type": skill.Type},
		})
	}
	return out
}

func hashFromMAEnv(env *anthropic.BetaEnvironment) (string, error) {
	if env == nil {
		return "", errors.New("spawner: nil managed environment")
	}
	spec := EnvSpec{
		Packages: PackageSpec{
			APT:   append([]string(nil), env.Config.Packages.Apt...),
			Cargo: append([]string(nil), env.Config.Packages.Cargo...),
			Gem:   append([]string(nil), env.Config.Packages.Gem...),
			Go:    append([]string(nil), env.Config.Packages.Go...),
			NPM:   append([]string(nil), env.Config.Packages.Npm...),
			Pip:   append([]string(nil), env.Config.Packages.Pip...),
		},
		Networking: NetworkSpec{
			Type:                 env.Config.Networking.Type,
			AllowedHosts:         append([]string(nil), env.Config.Networking.AllowedHosts...),
			AllowMCPServers:      env.Config.Networking.AllowMCPServers,
			AllowPackageManagers: env.Config.Networking.AllowPackageManagers,
		},
	}
	return envSpecHash(&spec)
}
