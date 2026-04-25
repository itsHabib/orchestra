package agents

import (
	"encoding/json"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
)

func toAgentCreateParams(spec *AgentSpec, key string) anthropic.BetaAgentNewParams {
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

func toAgentUpdateParams(spec *AgentSpec, key string, version int64) anthropic.BetaAgentUpdateParams {
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

func agentModelParam(model string) anthropic.BetaManagedAgentsModelConfigParams {
	return anthropic.BetaManagedAgentsModelConfigParams{
		ID:    model,
		Speed: anthropic.BetaManagedAgentsModelConfigParamsSpeedStandard,
	}
}

func agentMetadata(spec *AgentSpec) map[string]string {
	md := cloneStringMap(spec.Metadata)
	md[orchestraMetadataProject] = spec.Project
	md[orchestraMetadataRole] = spec.Role
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
			PermissionPolicy: anthropic.BetaManagedAgentsAgentToolConfigParamsPermissionPolicyUnion{
				OfAlwaysAllow: &anthropic.BetaManagedAgentsAlwaysAllowPolicyParam{
					Type: anthropic.BetaManagedAgentsAlwaysAllowPolicyTypeAlwaysAllow,
				},
			},
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
