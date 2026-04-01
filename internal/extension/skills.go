package extension

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jialuohu/curlycatclaw/config"
	"github.com/jialuohu/curlycatclaw/internal/skillloader"
	"github.com/jialuohu/curlycatclaw/skills"
)

// ExecSkillPrefix is prepended to exec extension names when registering
// in the skills registry to avoid collisions with other skill sources.
const ExecSkillPrefix = "ext__"

// MCPAdder abstracts the MCP manager methods needed for dynamic server
// management. Nil is valid (MCP server subprocess mode).
type MCPAdder interface {
	AddServer(ctx context.Context, cfg config.MCPServerConfig, envResolver func(string) (string, error)) error
	RemoveServer(name string) error
}

// InitExtensionSkills creates the built-in skills for runtime extension
// management: add_extension, remove_extension, list_extensions.
//
// mcpMgr may be nil (e.g. when running as an MCP server subprocess).
// reloadFunc is called after MCP extension mutations to trigger CLI
// subprocess respawn; it may be nil if no reload is needed.
func InitExtensionSkills(reg *Registry, mcpMgr MCPAdder, skillReg *skills.Registry, reloadFunc func()) []*skills.Skill {
	return []*skills.Skill{
		addExtensionSkill(reg, mcpMgr, skillReg, reloadFunc),
		removeExtensionSkill(reg, mcpMgr, skillReg, reloadFunc),
		listExtensionsSkill(reg),
	}
}

func addExtensionSkill(reg *Registry, mcpMgr MCPAdder, skillReg *skills.Registry, reloadFunc func()) *skills.Skill {
	return &skills.Skill{
		Name:        "add_extension",
		Description: "Add a runtime extension (MCP server or exec skill). MCP servers provide tools via the MCP protocol. Exec skills run a command as a subprocess with JSON input/output.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"name":         {"type": "string", "description": "Unique name for the extension (alphanumeric, hyphens, underscores)"},
				"type":         {"type": "string", "enum": ["mcp", "exec"], "description": "Extension type: mcp (MCP server) or exec (standalone executable skill)"},
				"command":      {"type": "string", "description": "Command to run (e.g. npx, /path/to/binary)"},
				"args":         {"type": "array", "items": {"type": "string"}, "description": "Command arguments"},
				"env":          {"type": "object", "additionalProperties": {"type": "string"}, "description": "Environment variables for the extension"},
				"description":  {"type": "string", "description": "What this tool does (required for exec type)"},
				"input_schema": {"type": "object", "description": "JSON Schema for exec skill input parameters"}
			},
			"required": ["name", "type", "command"]
		}`),
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			var params struct {
				Name        string            `json:"name"`
				Type        Type              `json:"type"`
				Command     string            `json:"command"`
				Args        []string          `json:"args"`
				Env         map[string]string `json:"env"`
				Description string            `json:"description"`
				InputSchema json.RawMessage   `json:"input_schema"`
			}
			if err := json.Unmarshal(input, &params); err != nil {
				return "", fmt.Errorf("invalid input: %w", err)
			}

			if len(params.InputSchema) > 0 && !json.Valid(params.InputSchema) {
				return "", fmt.Errorf("input_schema is not valid JSON")
			}

			ext := Extension{
				Name:        params.Name,
				Type:        params.Type,
				Command:     params.Command,
				Args:        params.Args,
				Env:         params.Env,
				Description: params.Description,
				InputSchema: params.InputSchema,
				AddedAt:     time.Now(),
			}

			switch params.Type {
			case TypeMCP:
				return addMCPExtension(ctx, reg, mcpMgr, reloadFunc, ext)
			case TypeExec:
				return addExecExtension(reg, skillReg, ext)
			default:
				return "", fmt.Errorf("unsupported extension type: %q", params.Type)
			}
		},
	}
}

func addMCPExtension(ctx context.Context, reg *Registry, mcpMgr MCPAdder, reloadFunc func(), ext Extension) (string, error) {
	if mcpMgr != nil {
		cfg := config.MCPServerConfig{
			Name:    ext.Name,
			Command: ext.Command,
			Args:    ext.Args,
			Env:     ext.Env,
		}
		if err := mcpMgr.AddServer(ctx, cfg, nil); err != nil {
			return "", fmt.Errorf("failed to start MCP server: %w", err)
		}
	}

	if err := reg.Add(ext); err != nil {
		if mcpMgr != nil {
			if rmErr := mcpMgr.RemoveServer(ext.Name); rmErr != nil {
				slog.Warn("extension: rollback RemoveServer failed, MCP process may be orphaned",
					"name", ext.Name, "err", rmErr)
			}
		}
		return "", fmt.Errorf("failed to persist extension: %w", err)
	}

	if reloadFunc != nil {
		reloadFunc()
	}

	if mcpMgr != nil {
		return fmt.Sprintf("Extension %q added (MCP server). Tools are available immediately.", ext.Name), nil
	}
	return fmt.Sprintf("Extension %q added (MCP server). Tools will be available on the next message.", ext.Name), nil
}

func addExecExtension(reg *Registry, skillReg *skills.Registry, ext Extension) (string, error) {
	registryName := ExecSkillPrefix + ext.Name

	schema := ext.InputSchema
	if len(schema) == 0 {
		schema = json.RawMessage(`{"type":"object"}`)
	}

	adapter := skillloader.NewExecAdapter(ext.Command, ext.Args, "", ext.Env, 30*time.Second)
	skill := &skills.Skill{
		Name:        registryName,
		Description: ext.Description,
		InputSchema: schema,
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			user := skills.GetUser(ctx)
			return adapter.Execute(ctx, input, user)
		},
	}

	skillReg.Register(skill)

	if err := reg.Add(ext); err != nil {
		skillReg.Unregister(registryName)
		return "", fmt.Errorf("failed to persist extension: %w", err)
	}

	return fmt.Sprintf("Extension %q added (exec skill, registered as %q). Available immediately.", ext.Name, registryName), nil
}

func removeExtensionSkill(reg *Registry, mcpMgr MCPAdder, skillReg *skills.Registry, reloadFunc func()) *skills.Skill {
	return &skills.Skill{
		Name:        "remove_extension",
		Description: "Remove a runtime extension by name.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"name": {"type": "string", "description": "Name of the extension to remove"}
			},
			"required": ["name"]
		}`),
		Execute: func(_ context.Context, input json.RawMessage) (string, error) {
			var params struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(input, &params); err != nil {
				return "", fmt.Errorf("invalid input: %w", err)
			}

			ext := reg.Get(params.Name)
			if ext == nil {
				return "", fmt.Errorf("extension %q not found", params.Name)
			}

			// Remove from persistence first (rolls back on disk-write failure),
			// then mutate runtime state.
			if err := reg.Remove(params.Name); err != nil {
				return "", fmt.Errorf("failed to remove extension: %w", err)
			}

			switch ext.Type {
			case TypeMCP:
				if mcpMgr != nil {
					if err := mcpMgr.RemoveServer(params.Name); err != nil {
						slog.Warn("extension: MCP server removal failed (persisted removal succeeded)",
							"name", params.Name, "err", err)
					}
				}
				if reloadFunc != nil {
					reloadFunc()
				}

			case TypeExec:
				skillReg.Unregister(ExecSkillPrefix + params.Name)
			}

			return fmt.Sprintf("Extension %q removed.", params.Name), nil
		},
	}
}

func listExtensionsSkill(reg *Registry) *skills.Skill {
	return &skills.Skill{
		Name:        "list_extensions",
		Description: "List all runtime-added extensions.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Execute: func(_ context.Context, _ json.RawMessage) (string, error) {
			all := reg.All()
			if len(all) == 0 {
				return "No runtime extensions registered.", nil
			}

			var sb strings.Builder
			fmt.Fprintf(&sb, "%d extension(s):\n\n", len(all))
			for _, ext := range all {
				fmt.Fprintf(&sb, "- **%s** (type: %s)\n", ext.Name, ext.Type)
				fmt.Fprintf(&sb, "  Command: `%s", ext.Command)
				if len(ext.Args) > 0 {
					fmt.Fprintf(&sb, " %s", strings.Join(ext.Args, " "))
				}
				sb.WriteString("`\n")
				if ext.Description != "" {
					fmt.Fprintf(&sb, "  Description: %s\n", ext.Description)
				}
				fmt.Fprintf(&sb, "  Added: %s\n", ext.AddedAt.Format(time.RFC3339))
			}
			return sb.String(), nil
		},
	}
}
