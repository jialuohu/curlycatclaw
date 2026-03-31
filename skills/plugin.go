package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// InitPluginSkills returns skills for managing Claude Code plugins in an
// isolated home directory. The install skill validates plugin names against
// the allowedPlugins allowlist.
func InitPluginSkills(cliPath, isolatedHome string, allowedPlugins []string) []*Skill {
	allowed := make(map[string]bool, len(allowedPlugins))
	for _, name := range allowedPlugins {
		allowed[name] = true
	}

	return []*Skill{
		{
			Name:        "install_plugin",
			Description: "Install a Claude Code plugin. Only plugins in the allowed list can be installed.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","description":"Plugin name to install"}},"required":["name"]}`),
			Execute:     makePluginExecute(cliPath, isolatedHome, "install", allowed),
		},
		{
			Name:        "uninstall_plugin",
			Description: "Uninstall a Claude Code plugin.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","description":"Plugin name to uninstall"}},"required":["name"]}`),
			Execute:     makePluginExecute(cliPath, isolatedHome, "uninstall", nil),
		},
		{
			Name:        "list_plugins",
			Description: "List all installed Claude Code plugins.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
			Execute:     makePluginListExecute(cliPath, isolatedHome),
		},
		{
			Name:        "enable_plugin",
			Description: "Enable a previously disabled Claude Code plugin.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","description":"Plugin name to enable"}},"required":["name"]}`),
			Execute:     makePluginExecute(cliPath, isolatedHome, "enable", nil),
		},
		{
			Name:        "disable_plugin",
			Description: "Disable an installed Claude Code plugin without uninstalling it.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","description":"Plugin name to disable"}},"required":["name"]}`),
			Execute:     makePluginExecute(cliPath, isolatedHome, "disable", nil),
		},
	}
}

type pluginInput struct {
	Name string `json:"name"`
}

// makePluginExecute creates an Execute func for install/uninstall/enable/disable.
// If allowlist is non-nil (install), the plugin name is validated against it.
func makePluginExecute(cliPath, isolatedHome, action string, allowlist map[string]bool) func(ctx context.Context, input json.RawMessage) (string, error) {
	return func(ctx context.Context, input json.RawMessage) (string, error) {
		var params pluginInput
		if err := json.Unmarshal(input, &params); err != nil {
			return "", fmt.Errorf("invalid input: %w", err)
		}
		if params.Name == "" {
			return "", fmt.Errorf("plugin name is required")
		}

		for _, r := range params.Name {
			if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '@') {
				return "", fmt.Errorf("invalid plugin name %q: only alphanumeric, hyphens, underscores, and @ allowed", params.Name)
			}
		}

		// Validate against allowlist for install.
		if allowlist != nil && !allowlist[params.Name] {
			var allowed []string
			for name := range allowlist {
				allowed = append(allowed, name)
			}
			return "", fmt.Errorf("plugin %q is not in the allowed list. Allowed: %s", params.Name, strings.Join(allowed, ", "))
		}

		cmd := exec.CommandContext(ctx, cliPath, "plugin", action, params.Name)
		cmd.Env = buildPluginEnv(isolatedHome)

		output, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("plugin %s failed: %s: %w", action, string(output), err)
		}

		// Signal reload needed for mutation operations.
		writeReloadFlag(isolatedHome)

		return fmt.Sprintf("Plugin %s %sd successfully.\n%s", params.Name, action, strings.TrimSpace(string(output))), nil
	}
}

// makePluginListExecute creates an Execute func for listing plugins.
func makePluginListExecute(cliPath, isolatedHome string) func(ctx context.Context, input json.RawMessage) (string, error) {
	return func(ctx context.Context, _ json.RawMessage) (string, error) {
		cmd := exec.CommandContext(ctx, cliPath, "plugin", "list")
		cmd.Env = buildPluginEnv(isolatedHome)

		output, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("plugin list failed: %s: %w", string(output), err)
		}

		result := strings.TrimSpace(string(output))
		if result == "" {
			return "No plugins installed.", nil
		}
		return result, nil
	}
}

// writeReloadFlag creates the reload signal file.
func writeReloadFlag(isolatedHome string) {
	path := filepath.Join(isolatedHome, ".curlycatclaw-reload-needed")
	os.WriteFile(path, []byte("1"), 0644) //nolint:errcheck
}

// buildPluginEnv constructs a minimal environment for plugin subprocesses,
// preventing leakage of daemon secrets.
func buildPluginEnv(isolatedHome string) []string {
	env := make([]string, 0, 4)
	if v := os.Getenv("PATH"); v != "" {
		env = append(env, "PATH="+v)
	}
	env = append(env, "HOME="+isolatedHome)
	env = append(env, "TMPDIR=/tmp")
	if v := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"); v != "" {
		env = append(env, "CLAUDE_CODE_OAUTH_TOKEN="+v)
	}
	return env
}
