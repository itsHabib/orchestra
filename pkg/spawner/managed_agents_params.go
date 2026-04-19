package spawner

import (
	"encoding/json"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
)

func toAgentCreateParams(spec AgentSpec, key string) anthropic.BetaAgentNewParams {
	params := anthropic.BetaAgentNewParams{
		Name:       key,
		Model:      agentModelParam(spec.Model),
		System:     anthropic.String(spec.SystemPrompt),
		Tools:      agentCreateTools(spec.Tools),
		MCPServers: mcpServerParams(spec.MCPServers),
		Skills:     skillParams(spec.Skills),
		Metadata:   agentMetadata(spec),
	}
	if spec.Name != "" && spec.Name != key {
		params.Description = anthropic.String(spec.Name)
	}
	return params
}

func toAgentUpdateParams(spec AgentSpec, key string, version int64) anthropic.BetaAgentUpdateParams {
	params := anthropic.BetaAgentUpdateParams{
		Version:    version,
		Name:       anthropic.String(key),
		Model:      agentModelParam(spec.Model),
		System:     anthropic.String(spec.SystemPrompt),
		Tools:      agentUpdateTools(spec.Tools),
		MCPServers: mcpServerParams(spec.MCPServers),
		Skills:     skillParams(spec.Skills),
		Metadata:   agentMetadata(spec),
	}
	if spec.Name != "" && spec.Name != key {
		params.Description = anthropic.String(spec.Name)
	}
	return params
}

func toEnvCreateParams(spec EnvSpec, key string) anthropic.BetaEnvironmentNewParams {
	return anthropic.BetaEnvironmentNewParams{
		Name:     key,
		Config:   envConfigParams(spec),
		Metadata: envMetadata(spec),
	}
}

func agentModelParam(model string) anthropic.BetaManagedAgentsModelConfigParams {
	return anthropic.BetaManagedAgentsModelConfigParams{
		ID:    anthropic.BetaManagedAgentsModel(model),
		Speed: anthropic.BetaManagedAgentsModelConfigParamsSpeedStandard,
	}
}

func agentMetadata(spec AgentSpec) map[string]string {
	md := cloneStringMap(spec.Metadata)
	md[orchestraMetadataProject] = spec.Project
	md[orchestraMetadataRole] = spec.Role
	md[orchestraMetadataVersion] = orchestraVersionV2
	return md
}

func envMetadata(spec EnvSpec) map[string]string {
	md := cloneStringMap(spec.Metadata)
	md[orchestraMetadataProject] = spec.Project
	md[orchestraMetadataEnv] = spec.Name
	md[orchestraMetadataVersion] = orchestraVersionV2
	return md
}

func agentCreateTools(tools []Tool) []anthropic.BetaAgentNewParamsToolUnion {
	if len(tools) == 0 {
		return nil
	}
	var out []anthropic.BetaAgentNewParamsToolUnion
	if configs := builtinToolConfigs(tools); len(configs) > 0 {
		out = append(out, anthropic.BetaAgentNewParamsToolUnion{
			OfAgentToolset20260401: &anthropic.BetaManagedAgentsAgentToolset20260401Params{
				Type:    anthropic.BetaManagedAgentsAgentToolset20260401ParamsTypeAgentToolset20260401,
				Configs: configs,
			},
		})
	}
	for _, tool := range tools {
		switch normalizedToolType(tool) {
		case "custom":
			out = append(out, anthropic.BetaAgentNewParamsToolUnion{
				OfCustom: customToolParam(tool),
			})
		case "mcp_toolset":
			out = append(out, anthropic.BetaAgentNewParamsToolUnion{
				OfMCPToolset: mcpToolsetParam(tool),
			})
		}
	}
	return out
}

func agentUpdateTools(tools []Tool) []anthropic.BetaAgentUpdateParamsToolUnion {
	if len(tools) == 0 {
		return nil
	}
	var out []anthropic.BetaAgentUpdateParamsToolUnion
	if configs := builtinToolConfigs(tools); len(configs) > 0 {
		out = append(out, anthropic.BetaAgentUpdateParamsToolUnion{
			OfAgentToolset20260401: &anthropic.BetaManagedAgentsAgentToolset20260401Params{
				Type:    anthropic.BetaManagedAgentsAgentToolset20260401ParamsTypeAgentToolset20260401,
				Configs: configs,
			},
		})
	}
	for _, tool := range tools {
		switch normalizedToolType(tool) {
		case "custom":
			out = append(out, anthropic.BetaAgentUpdateParamsToolUnion{
				OfCustom: customToolParam(tool),
			})
		case "mcp_toolset":
			out = append(out, anthropic.BetaAgentUpdateParamsToolUnion{
				OfMCPToolset: mcpToolsetParam(tool),
			})
		}
	}
	return out
}

func builtinToolConfigs(tools []Tool) []anthropic.BetaManagedAgentsAgentToolConfigParams {
	var configs []anthropic.BetaManagedAgentsAgentToolConfigParams
	for _, tool := range tools {
		if normalizedToolType(tool) != "agent_toolset_20260401" || tool.Name == "" {
			continue
		}
		configs = append(configs, anthropic.BetaManagedAgentsAgentToolConfigParams{
			Name:    anthropic.BetaManagedAgentsAgentToolConfigParamsName(tool.Name),
			Enabled: anthropic.Bool(true),
		})
	}
	return configs
}

func customToolParam(tool Tool) *anthropic.BetaManagedAgentsCustomToolParams {
	return &anthropic.BetaManagedAgentsCustomToolParams{
		Name:        tool.Name,
		Description: tool.Description,
		Type:        anthropic.BetaManagedAgentsCustomToolParamsTypeCustom,
		InputSchema: customToolInputSchema(tool.InputSchema),
	}
}

func mcpToolsetParam(tool Tool) *anthropic.BetaManagedAgentsMCPToolsetParams {
	server := firstNonEmpty(tool.Metadata["mcp_server_name"], tool.Name)
	return &anthropic.BetaManagedAgentsMCPToolsetParams{
		MCPServerName: server,
		Type:          anthropic.BetaManagedAgentsMCPToolsetParamsTypeMCPToolset,
	}
}

func mcpServerParams(servers []MCPServer) []anthropic.BetaManagedAgentsURLMCPServerParams {
	if len(servers) == 0 {
		return nil
	}
	out := make([]anthropic.BetaManagedAgentsURLMCPServerParams, 0, len(servers))
	for _, server := range servers {
		out = append(out, anthropic.BetaManagedAgentsURLMCPServerParams{
			Name: server.Name,
			Type: anthropic.BetaManagedAgentsURLMCPServerParamsTypeURL,
			URL:  server.URL,
		})
	}
	return out
}

func skillParams(skills []Skill) []anthropic.BetaManagedAgentsSkillParamsUnion {
	if len(skills) == 0 {
		return nil
	}
	out := make([]anthropic.BetaManagedAgentsSkillParamsUnion, 0, len(skills))
	for _, skill := range skills {
		kind := skill.Metadata["type"]
		if kind == "anthropic" {
			out = append(out, anthropic.BetaManagedAgentsSkillParamsUnion{
				OfAnthropic: &anthropic.BetaManagedAgentsAnthropicSkillParams{
					SkillID: skill.Name,
					Type:    anthropic.BetaManagedAgentsAnthropicSkillParamsTypeAnthropic,
					Version: optionalString(skill.Version),
				},
			})
			continue
		}
		out = append(out, anthropic.BetaManagedAgentsSkillParamsUnion{
			OfCustom: &anthropic.BetaManagedAgentsCustomSkillParams{
				SkillID: skill.Name,
				Type:    anthropic.BetaManagedAgentsCustomSkillParamsTypeCustom,
				Version: optionalString(skill.Version),
			},
		})
	}
	return out
}

func envConfigParams(spec EnvSpec) anthropic.BetaCloudConfigParams {
	return anthropic.BetaCloudConfigParams{
		Packages:   packageParams(spec.Packages),
		Networking: networkParams(spec.Networking),
	}
}

func packageParams(packages PackageSpec) anthropic.BetaPackagesParams {
	return anthropic.BetaPackagesParams{
		Type:  anthropic.BetaPackagesParamsTypePackages,
		Apt:   append([]string(nil), packages.APT...),
		Cargo: append([]string(nil), packages.Cargo...),
		Gem:   append([]string(nil), packages.Gem...),
		Go:    append([]string(nil), packages.Go...),
		Npm:   append([]string(nil), packages.NPM...),
		Pip:   append([]string(nil), packages.Pip...),
	}
}

func networkParams(network NetworkSpec) anthropic.BetaCloudConfigParamsNetworkingUnion {
	if strings.EqualFold(network.Type, "unrestricted") || strings.EqualFold(network.Type, "open") {
		unrestricted := anthropic.NewBetaUnrestrictedNetworkParam()
		return anthropic.BetaCloudConfigParamsNetworkingUnion{OfUnrestricted: &unrestricted}
	}
	return anthropic.BetaCloudConfigParamsNetworkingUnion{
		OfLimited: &anthropic.BetaLimitedNetworkParams{
			AllowedHosts:         append([]string(nil), network.AllowedHosts...),
			AllowMCPServers:      anthropic.Bool(network.AllowMCPServers),
			AllowPackageManagers: anthropic.Bool(network.AllowPackageManagers),
		},
	}
}

func customToolInputSchema(input any) anthropic.BetaManagedAgentsCustomToolInputSchemaParam {
	var schema struct {
		Properties map[string]any `json:"properties"`
		Required   []string       `json:"required"`
		Type       string         `json:"type"`
	}
	if input != nil {
		if b, err := json.Marshal(input); err == nil {
			_ = json.Unmarshal(b, &schema)
		}
	}
	if schema.Type == "" {
		schema.Type = "object"
	}
	return anthropic.BetaManagedAgentsCustomToolInputSchemaParam{
		Properties: schema.Properties,
		Required:   schema.Required,
		Type:       anthropic.BetaManagedAgentsCustomToolInputSchemaType(schema.Type),
	}
}

func optionalString(s string) param.Opt[string] {
	if s == "" {
		return param.Opt[string]{}
	}
	return anthropic.String(s)
}

func cloneStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
