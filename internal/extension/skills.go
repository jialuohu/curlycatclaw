package extension

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"os"
	"path/filepath"

	"github.com/jialuohu/curlycatclaw/config"
	"github.com/jialuohu/curlycatclaw/internal/security"
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

// MCPHotReloader handles dynamic MCP extension tool registration without
// subprocess restart. When non-nil and mcpMgr is nil (MCP subprocess mode),
// MCP extensions are hot-reloaded instead of requiring a restart.
type MCPHotReloader interface {
	// ConnectAndRegister starts an MCP extension, discovers its tools,
	// and registers them as proxy tools on the running MCP server.
	// Returns tool descriptions and a closer for the displaced old session
	// (nil if no previous session existed). The caller should close the
	// old session after confirming the new one is working.
	ConnectAndRegister(ctx context.Context, ext *Extension) (toolDescs []string, oldSessionCloser func(), err error)
	// DisconnectAndUnregister removes proxy tools and closes the MCP
	// client session for the named extension. No-op if not tracked.
	DisconnectAndUnregister(name string) error
}

// InitExtensionSkills creates the built-in skills for runtime extension
// management: add_extension, remove_extension, list_extensions.
//
// mcpMgr may be nil (e.g. when running as an MCP server subprocess).
// reloadFunc is called after MCP extension mutations to trigger CLI
// subprocess respawn; it may be nil if no reload is needed.
// hotReloader, when non-nil, enables MCP extension hot-reload without
// subprocess restart (MCP server subprocess mode only).
func InitExtensionSkills(reg *Registry, mcpMgr MCPAdder, skillReg *skills.Registry, reloadFunc func(), hotReloader MCPHotReloader, credStore *security.CredentialStore, configServers []ConfigMCPServer) []*skills.Skill {
	ss := []*skills.Skill{
		addExtensionSkill(reg, mcpMgr, skillReg, reloadFunc, hotReloader),
		removeExtensionSkill(reg, mcpMgr, skillReg, reloadFunc, hotReloader),
		listExtensionsSkill(reg, configServers),
		loadPromptSkill(reg),
	}
	if credStore != nil {
		ss = append(ss, setExtensionEnvSkill(reg, credStore, reloadFunc, hotReloader))
		ss = append(ss, unsetExtensionEnvSkill(reg, credStore, reloadFunc, hotReloader))
	}
	return ss
}

func addExtensionSkill(reg *Registry, mcpMgr MCPAdder, skillReg *skills.Registry, reloadFunc func(), hotReloader MCPHotReloader) *skills.Skill {
	return &skills.Skill{
		Name:        "add_extension",
		Description: "Add a runtime extension (MCP server, exec skill, or prompt skill). MCP servers provide tools via the MCP protocol. Exec skills run a command as a subprocess with JSON input/output. Prompt skills are markdown instruction files (SKILL.md) that modify behavior.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"name":         {"type": "string", "description": "Unique name for the extension (alphanumeric, hyphens, underscores)"},
				"type":         {"type": "string", "enum": ["mcp", "exec", "prompt"], "description": "Extension type: mcp (MCP server), exec (standalone executable skill), or prompt (markdown instructions)"},
				"command":      {"type": "string", "description": "For mcp (stdio)/exec: command to run. For prompt: directory path containing SKILL.md"},
				"args":         {"type": "array", "items": {"type": "string"}, "description": "Command arguments (stdio only)"},
				"env":          {"type": "object", "additionalProperties": {"type": "string"}, "description": "Environment variables for the extension"},
				"description":  {"type": "string", "description": "What this tool does (required for exec type)"},
				"input_schema": {"type": "object", "description": "JSON Schema for exec skill input parameters"},
				"transport":    {"type": "string", "enum": ["stdio", "http"], "description": "MCP transport: stdio (default, spawns subprocess) or http (connects to running server)"},
				"url":          {"type": "string", "description": "HTTP endpoint URL (required when transport is http, e.g. http://localhost:8080/mcp)"},
				"headers":      {"type": "object", "additionalProperties": {"type": "string"}, "description": "HTTP request headers (e.g. API keys). Only used with http transport"}
			},
			"required": ["name", "type"]
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
				Transport   string            `json:"transport"`
				URL         string            `json:"url"`
				Headers     map[string]string `json:"headers"`
			}
			if err := json.Unmarshal(input, &params); err != nil {
				return "", fmt.Errorf("invalid input: %w", err)
			}

			if len(params.InputSchema) > 0 && !json.Valid(params.InputSchema) {
				return "", fmt.Errorf("input_schema is not valid JSON")
			}

			// If command contains spaces and no args were provided,
			// split it (Claude often passes "uvx foo" as one string).
			cmd := params.Command
			args := params.Args
			if len(args) == 0 && strings.Contains(cmd, " ") {
				parts := strings.Fields(cmd)
				cmd = parts[0]
				args = parts[1:]
			}

			ext := Extension{
				Name:        params.Name,
				Type:        params.Type,
				Command:     cmd,
				Args:        args,
				Env:         params.Env,
				Description: params.Description,
				InputSchema: params.InputSchema,
				AddedAt:     time.Now(),
				Transport:   params.Transport,
				URL:         params.URL,
				Headers:     params.Headers,
			}

			switch params.Type {
			case TypeMCP:
				return addMCPExtension(ctx, reg, mcpMgr, reloadFunc, hotReloader, ext)
			case TypeExec:
				return addExecExtension(reg, skillReg, ext)
			case TypePrompt:
				return addPromptExtension(reg, ext)
			default:
				return "", fmt.Errorf("unsupported extension type: %q", params.Type)
			}
		},
	}
}

func addMCPExtension(ctx context.Context, reg *Registry, mcpMgr MCPAdder, reloadFunc func(), hotReloader MCPHotReloader, ext Extension) (string, error) {
	if mcpMgr != nil {
		cfg := config.MCPServerConfig{
			Name:      ext.Name,
			Command:   ext.Command,
			Args:      ext.Args,
			Env:       ext.Env,
			Transport: ext.Transport,
			URL:       ext.URL,
			Headers:   ext.Headers,
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

	// Direct API mode: tools are available immediately via mcpMgr.
	if mcpMgr != nil {
		if reloadFunc != nil {
			reloadFunc()
		}
		slog.Info("extension: MCP server added", "name", ext.Name, "transport", ext.Transport, "command", ext.Command, "url", ext.URL)
		return fmt.Sprintf("Extension %q added (MCP server). Tools are available immediately.", ext.Name), nil
	}

	// CLI subprocess mode: try hot-reload first, fall back to subprocess restart.
	if hotReloader != nil {
		toolDescs, oldCloser, err := hotReloader.ConnectAndRegister(ctx, &ext)
		if err == nil {
			if oldCloser != nil {
				oldCloser()
			}
			slog.Info("extension: MCP server hot-reloaded", "name", ext.Name, "tools", len(toolDescs))
			msg := fmt.Sprintf("Extension %q added (MCP server). Tools are available immediately", ext.Name)
			if len(toolDescs) > 0 {
				msg += ": " + strings.Join(toolDescs, ", ")
			}
			return msg + ".", nil
		}
		slog.Warn("extension: hot-reload failed, falling back to subprocess restart",
			"name", ext.Name, "err", err)
	}

	if reloadFunc != nil {
		reloadFunc()
	}
	slog.Info("extension: MCP server added (CLI mode, pending reload)", "name", ext.Name, "transport", ext.Transport, "command", ext.Command, "url", ext.URL)
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

	slog.Info("extension: exec skill added", "name", ext.Name, "registry_name", registryName, "command", ext.Command)
	return fmt.Sprintf("Extension %q added (exec skill, registered as %q). Available immediately.", ext.Name, registryName), nil
}

func removeExtensionSkill(reg *Registry, mcpMgr MCPAdder, skillReg *skills.Registry, reloadFunc func(), hotReloader MCPHotReloader) *skills.Skill {
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

			if IsDefault(params.Name) {
				return "", fmt.Errorf("extension %q is a pre-installed default and cannot be removed", params.Name)
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
					if reloadFunc != nil {
						reloadFunc()
					}
				} else if hotReloader != nil {
					if err := hotReloader.DisconnectAndUnregister(params.Name); err != nil {
						slog.Warn("extension: hot-unload failed, falling back to subprocess restart",
							"name", params.Name, "err", err)
						if reloadFunc != nil {
							reloadFunc()
						}
					}
				} else if reloadFunc != nil {
					reloadFunc()
				}

			case TypeExec:
				skillReg.Unregister(ExecSkillPrefix + params.Name)
			}

			slog.Info("extension: removed", "name", params.Name, "type", ext.Type)
			return fmt.Sprintf("Extension %q removed.", params.Name), nil
		},
	}
}

// ConfigMCPServer is the minimal info needed to display config-based MCP servers
// in list_extensions. Avoids importing the config package.
type ConfigMCPServer struct {
	Name      string
	Command   string
	Transport string
	URL       string
}

func listExtensionsSkill(reg *Registry, configServers []ConfigMCPServer) *skills.Skill {
	return &skills.Skill{
		Name:        "list_extensions",
		Description: "List all extensions and MCP tool servers.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Execute: func(_ context.Context, _ json.RawMessage) (string, error) {
			all := reg.All()
			total := len(all) + len(configServers)
			if total == 0 {
				return "No extensions or MCP servers registered.", nil
			}

			var sb strings.Builder

			if len(configServers) > 0 {
				sb.WriteString("**Config MCP servers** (from config.toml):\n\n")
				for _, srv := range configServers {
					if srv.Transport == "http" {
						fmt.Fprintf(&sb, "- **%s** (type: mcp, config, http)\n", srv.Name)
						fmt.Fprintf(&sb, "  URL: `%s`\n", srv.URL)
					} else {
						fmt.Fprintf(&sb, "- **%s** (type: mcp, config)\n", srv.Name)
						fmt.Fprintf(&sb, "  Command: `%s`\n", srv.Command)
					}
				}
				sb.WriteString("\n")
			}

			if len(all) > 0 {
				sb.WriteString("**Runtime extensions** (added via chat):\n\n")
				for _, ext := range all {
					if ext.Transport == "http" {
						fmt.Fprintf(&sb, "- **%s** (type: %s, http)\n", ext.Name, ext.Type)
						fmt.Fprintf(&sb, "  URL: `%s`\n", ext.URL)
					} else {
						fmt.Fprintf(&sb, "- **%s** (type: %s)\n", ext.Name, ext.Type)
						fmt.Fprintf(&sb, "  Command: `%s", ext.Command)
						if len(ext.Args) > 0 {
							fmt.Fprintf(&sb, " %s", strings.Join(ext.Args, " "))
						}
						sb.WriteString("`\n")
					}
					if ext.Description != "" {
						fmt.Fprintf(&sb, "  Description: %s\n", ext.Description)
					}
					fmt.Fprintf(&sb, "  Added: %s\n", ext.AddedAt.Format(time.RFC3339))
				}
			}

			return sb.String(), nil
		},
	}
}

func addPromptExtension(reg *Registry, ext Extension) (string, error) {
	if err := reg.Add(ext); err != nil {
		return "", fmt.Errorf("failed to persist extension: %w", err)
	}
	slog.Info("extension: prompt skill added", "name", ext.Name, "path", ext.Command)
	return fmt.Sprintf("Prompt skill %q added. Claude will see it in the available skills list and can load it with load_prompt_skill.", ext.Name), nil
}

// isDangerousEnvKey returns true if the key matches a prefix that could
// enable library injection (LD_PRELOAD, LD_LIBRARY_PATH, DYLD_*).
func isDangerousEnvKey(key string) bool {
	upper := strings.ToUpper(key)
	for _, prefix := range []string{"LD_PRELOAD", "LD_LIBRARY_PATH", "DYLD_"} {
		if strings.HasPrefix(upper, prefix) {
			return true
		}
	}
	return false
}

// credKeyName builds the credential store key for an extension env var.
func credKeyName(extName, envKey string) string {
	return "ext_" + extName + "_" + envKey
}

func setExtensionEnvSkill(reg *Registry, credStore *security.CredentialStore, reloadFunc func(), hotReloader MCPHotReloader) *skills.Skill {
	return &skills.Skill{
		Name:        "set_extension_env",
		Description: "Set an environment variable (e.g. API key) for an MCP extension. The value is encrypted at rest. The extension will be reloaded to pick up the change.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"name":  {"type": "string", "description": "Extension name"},
				"key":   {"type": "string", "description": "Environment variable name (e.g. CORE_API_KEY)"},
				"value": {"type": "string", "description": "Environment variable value"}
			},
			"required": ["name", "key", "value"]
		}`),
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			var params struct {
				Name  string `json:"name"`
				Key   string `json:"key"`
				Value string `json:"value"`
			}
			if err := json.Unmarshal(input, &params); err != nil {
				return "", fmt.Errorf("invalid input: %w", err)
			}
			if params.Key == "" {
				return "", fmt.Errorf("key is required")
			}
			// Reject env keys that would be filtered at spawn time anyway.
			if isDangerousEnvKey(params.Key) {
				return "", fmt.Errorf("env key %q is blocked (dangerous prefix)", params.Key)
			}

			ext := reg.Get(params.Name)
			if ext == nil {
				return "", fmt.Errorf("extension %q not found", params.Name)
			}

			// Encrypt and store the value.
			credKey := credKeyName(params.Name, params.Key)
			if err := credStore.Set(credKey, params.Value); err != nil {
				return "", fmt.Errorf("failed to store credential: %w", err)
			}

			// Update the extension's env to reference the encrypted value.
			ref := "encrypted:ref:" + credKey
			if err := reg.Update(params.Name, func(e *Extension) {
				if e.Env == nil {
					e.Env = make(map[string]string)
				}
				e.Env[params.Key] = ref
			}); err != nil {
				// Rollback: remove the credential we just stored.
				_ = credStore.Delete(credKey)
				return "", fmt.Errorf("failed to update extension: %w", err)
			}

			msg := hotReloadEnvChange(ctx, reg, params.Name, ext.Type, hotReloader, reloadFunc)

			slog.Info("extension: env var set (encrypted)", "extension", params.Name, "key", params.Key)
			return fmt.Sprintf("Set %s for %s (encrypted). %s", params.Key, params.Name, msg), nil
		},
	}
}

func unsetExtensionEnvSkill(reg *Registry, credStore *security.CredentialStore, reloadFunc func(), hotReloader MCPHotReloader) *skills.Skill {
	return &skills.Skill{
		Name:        "unset_extension_env",
		Description: "Remove an environment variable from an MCP extension and delete its encrypted credential.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"name": {"type": "string", "description": "Extension name"},
				"key":  {"type": "string", "description": "Environment variable name to remove"}
			},
			"required": ["name", "key"]
		}`),
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			var params struct {
				Name string `json:"name"`
				Key  string `json:"key"`
			}
			if err := json.Unmarshal(input, &params); err != nil {
				return "", fmt.Errorf("invalid input: %w", err)
			}

			ext := reg.Get(params.Name)
			if ext == nil {
				return "", fmt.Errorf("extension %q not found", params.Name)
			}

			// Update registry first, then delete credential. This order
			// ensures a registry write failure doesn't leave a dangling
			// encrypted:ref: pointing at a deleted credential.
			if err := reg.Update(params.Name, func(e *Extension) {
				delete(e.Env, params.Key)
			}); err != nil {
				return "", fmt.Errorf("failed to update extension: %w", err)
			}

			// Delete the encrypted credential (ignore not-found).
			credKey := credKeyName(params.Name, params.Key)
			if err := credStore.Delete(credKey); err != nil && !errors.Is(err, security.ErrNotFound) {
				slog.Warn("extension: orphaned credential after env unset",
					"extension", params.Name, "key", params.Key, "err", err)
			}

			msg := hotReloadEnvChange(ctx, reg, params.Name, ext.Type, hotReloader, reloadFunc)

			slog.Info("extension: env var removed", "extension", params.Name, "key", params.Key)
			return fmt.Sprintf("Removed %s from %s. %s", params.Key, params.Name, msg), nil
		},
	}
}

// hotReloadEnvChange handles reconnection after an MCP extension's env changes.
// Uses connect-new-first: connects with new env, then closes old session.
// Returns a user-facing status message.
func hotReloadEnvChange(ctx context.Context, reg *Registry, name string, extType Type, hotReloader MCPHotReloader, reloadFunc func()) string {
	if extType != TypeMCP {
		return "" // exec extensions pick up env per-invocation, no reload needed
	}

	if hotReloader != nil {
		// Connect-new-first: start new session with updated env before closing old.
		updated := reg.Get(name)
		if updated != nil {
			_, oldCloser, err := hotReloader.ConnectAndRegister(ctx, updated)
			if err == nil {
				// New session is live and tools are registered (AddTool overwrites by name).
				// Close the displaced OLD session (returned by ConnectAndRegister).
				if oldCloser != nil {
					oldCloser()
				}
				return "Extension reloaded immediately."
			}
			slog.Warn("extension: hot-reload env change failed, falling back to restart",
				"name", name, "err", err)
		}
	}

	if reloadFunc != nil {
		reloadFunc()
	}
	return "The extension will reload on the next message."
}

func loadPromptSkill(reg *Registry) *skills.Skill {
	return &skills.Skill{
		Name:        "load_prompt_skill",
		Description: "Load a prompt-based skill's SKILL.md instructions. Call this when you need to follow a prompt skill's workflow.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"name": {"type": "string", "description": "Name of the prompt skill to load"}
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
				return "", fmt.Errorf("prompt skill %q not found", params.Name)
			}
			if ext.Type != TypePrompt {
				return "", fmt.Errorf("extension %q is type %q, not a prompt skill", params.Name, ext.Type)
			}

			skillPath := filepath.Join(ext.Command, "SKILL.md")
			data, err := os.ReadFile(skillPath)
			if err != nil {
				return "", fmt.Errorf("failed to read SKILL.md: %w", err)
			}
			slog.Info("extension: prompt skill loaded", "name", params.Name, "size_bytes", len(data))
			return string(data), nil
		},
	}
}
