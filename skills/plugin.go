package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
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
		{
			Name:        "add_marketplace",
			Description: "Add a plugin marketplace source (GitHub repo). Lets you install plugins from third-party collections. Example: 'jarrodwatts/claude-hud' or 'nextlevelbuilder/ui-ux-pro-max-skill'.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"source":{"type":"string","description":"GitHub repo in owner/repo format (e.g. jarrodwatts/claude-hud)"}},"required":["source"]}`),
			Execute:     makeMarketplaceAddExecute(cliPath, isolatedHome),
		},
		{
			Name:        "remove_marketplace",
			Description: "Remove a previously added plugin marketplace source.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","description":"Marketplace name to remove (e.g. claude-hud)"}},"required":["name"]}`),
			Execute:     makeMarketplaceRemoveExecute(cliPath, isolatedHome),
		},
		{
			Name:        "list_marketplaces",
			Description: "List all configured plugin marketplaces.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
			Execute:     makeMarketplaceListExecute(cliPath, isolatedHome),
		},
		{
			Name:        "update_plugin",
			Description: "Update an installed plugin to the latest version, or update all installed plugins if no name given.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","description":"Plugin name to update (omit to update all)"}}}`),
			Execute:     makePluginUpdateExecute(cliPath, isolatedHome),
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
			isAlpha := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
			isDigit := r >= '0' && r <= '9'
			isSafe := r == '-' || r == '_' || r == '@'
			if !isAlpha && !isDigit && !isSafe {
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

		// Bootstrap marketplace on first install attempt (lazy, idempotent).
		// Runs after allowlist check to avoid network calls for rejected plugins.
		if action == "install" {
			if err := ensureMarketplace(cliPath, isolatedHome); err != nil {
				return "", fmt.Errorf("marketplace bootstrap failed: %w", err)
			}
			// Auto-update stale plugins (non-blocking, warn on failure).
			ensurePluginsUpdated(cliPath, isolatedHome)
		}

		cmd := exec.CommandContext(ctx, cliPath, "plugin", action, params.Name)
		cmd.Env = buildPluginEnv(isolatedHome)

		output, err := cmd.CombinedOutput()
		if err != nil {
			outStr := strings.TrimSpace(string(output))
			// If plugin not found, hint at the marketplace workflow.
			if action == "install" && (strings.Contains(outStr, "not found") || strings.Contains(outStr, "No matching")) {
				return "", fmt.Errorf("plugin %s not found in any registered marketplace. "+
					"You MUST now: 1) Use WebSearch to find the plugin's GitHub marketplace repo, "+
					"2) Use add_marketplace to register it, "+
					"3) Retry install_plugin. "+
					"Do NOT ask the user for the repo URL, search for it yourself. "+
					"Original error: %s", params.Name, outStr)
			}
			return "", fmt.Errorf("plugin %s failed: %s: %w", action, outStr, err)
		}

		// Signal reload needed for mutation operations.
		writeReloadFlag(isolatedHome)

		msg := fmt.Sprintf("Plugin %s %sd successfully.\n%s", params.Name, action, strings.TrimSpace(string(output)))

		if action == "install" {
			msg += "\n\nThe plugin's tools will be available starting with the user's next message."
			msg += " Tell the user the plugin is ready and they can start using it."

			// Check if the plugin's MCP server command is available.
			if warning := checkPluginCommand(isolatedHome, params.Name); warning != "" {
				msg += "\n\n" + warning
			}
		}

		return msg, nil
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

// makeMarketplaceAddExecute creates an Execute func for adding marketplace sources.
func makeMarketplaceAddExecute(cliPath, isolatedHome string) func(ctx context.Context, input json.RawMessage) (string, error) {
	return func(ctx context.Context, input json.RawMessage) (string, error) {
		var params struct {
			Source string `json:"source"`
		}
		if err := json.Unmarshal(input, &params); err != nil {
			return "", fmt.Errorf("invalid input: %w", err)
		}
		if params.Source == "" {
			return "", fmt.Errorf("marketplace source is required (e.g. owner/repo)")
		}

		// Basic validation: should look like owner/repo.
		if !strings.Contains(params.Source, "/") {
			return "", fmt.Errorf("marketplace source should be in owner/repo format (e.g. jarrodwatts/claude-hud)")
		}

		if err := runMarketplaceCmd(cliPath, isolatedHome, "add", params.Source); err != nil {
			return "", fmt.Errorf("marketplace add failed: %w", err)
		}

		return fmt.Sprintf("Marketplace %q added successfully.\nYou can now install plugins from this marketplace. Use install_plugin to install them.", params.Source), nil
	}
}

// makeMarketplaceRemoveExecute creates an Execute func for removing marketplace sources.
// Blocks removal of the default marketplace and auto-uninstalls plugins from the marketplace.
func makeMarketplaceRemoveExecute(cliPath, isolatedHome string) func(ctx context.Context, input json.RawMessage) (string, error) {
	return func(ctx context.Context, input json.RawMessage) (string, error) {
		var params struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(input, &params); err != nil {
			return "", fmt.Errorf("invalid input: %w", err)
		}
		if params.Name == "" {
			return "", fmt.Errorf("marketplace name is required")
		}

		// Block removal of the default marketplace.
		if params.Name == "claude-plugins-official" {
			return "", fmt.Errorf("cannot remove the default marketplace (claude-plugins-official)")
		}

		// Auto-uninstall plugins belonging to this marketplace.
		removed := uninstallMarketplacePlugins(ctx, cliPath, isolatedHome, params.Name)

		if err := runMarketplaceCmd(cliPath, isolatedHome, "remove", params.Name); err != nil {
			return "", fmt.Errorf("marketplace remove failed: %w", err)
		}

		// Signal reload since plugins were removed.
		if len(removed) > 0 {
			writeReloadFlag(isolatedHome)
		}

		msg := fmt.Sprintf("Marketplace %q removed successfully.", params.Name)
		if len(removed) > 0 {
			msg += fmt.Sprintf("\nAlso uninstalled %d plugin(s): %s", len(removed), strings.Join(removed, ", "))
		}
		return msg, nil
	}
}

// uninstallMarketplacePlugins finds and uninstalls all plugins from a given marketplace.
func uninstallMarketplacePlugins(ctx context.Context, cliPath, isolatedHome, marketplace string) []string {
	manifestPath := filepath.Join(isolatedHome, ".claude", "plugins", "installed_plugins.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil
	}
	var manifest struct {
		Plugins map[string][]struct {
			InstallPath string `json:"installPath"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil
	}

	suffix := "@" + marketplace
	var removed []string
	for key := range manifest.Plugins {
		if strings.HasSuffix(key, suffix) {
			pluginName := strings.TrimSuffix(key, suffix)
			cmd := exec.CommandContext(ctx, cliPath, "plugin", "uninstall", pluginName)
			cmd.Env = buildPluginEnv(isolatedHome)
			if output, err := cmd.CombinedOutput(); err != nil {
				slog.Warn("failed to uninstall marketplace plugin",
					"plugin", pluginName, "marketplace", marketplace,
					"err", err, "output", strings.TrimSpace(string(output)))
			} else {
				removed = append(removed, pluginName)
			}
		}
	}
	return removed
}

// makeMarketplaceListExecute creates an Execute func for listing marketplaces.
func makeMarketplaceListExecute(cliPath, isolatedHome string) func(ctx context.Context, input json.RawMessage) (string, error) {
	return func(ctx context.Context, _ json.RawMessage) (string, error) {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, cliPath, "plugin", "marketplace", "list")
		cmd.Env = buildPluginEnv(isolatedHome)
		output, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("marketplace list failed: %s: %w", strings.TrimSpace(string(output)), err)
		}
		result := strings.TrimSpace(string(output))
		if result == "" {
			return "No marketplaces configured.", nil
		}
		return result, nil
	}
}

// checkPluginCommand reads the newly installed plugin's .mcp.json to find
// what command it needs, and checks if that command is available. Returns a
// warning string if the command is missing, empty string if all good.
// makePluginUpdateExecute creates an Execute func for updating plugins.
// If name is provided, updates that plugin. If empty, updates all installed plugins.
func makePluginUpdateExecute(cliPath, isolatedHome string) func(ctx context.Context, input json.RawMessage) (string, error) {
	return func(ctx context.Context, input json.RawMessage) (string, error) {
		var params struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(input, &params); err != nil {
			return "", fmt.Errorf("invalid input: %w", err)
		}

		if params.Name != "" {
			// Update a specific plugin.
			uCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(uCtx, cliPath, "plugin", "update", params.Name)
			cmd.Env = buildPluginEnv(isolatedHome)
			output, err := cmd.CombinedOutput()
			if err != nil {
				return "", fmt.Errorf("plugin update failed: %s: %w", strings.TrimSpace(string(output)), err)
			}
			writeReloadFlag(isolatedHome)
			return fmt.Sprintf("Plugin %s updated successfully.\n%s\nRestart takes effect on the next message.", params.Name, strings.TrimSpace(string(output))), nil
		}

		// Update all installed plugins using full manifest keys (name@marketplace).
		keys := installedPluginKeys(isolatedHome)
		if len(keys) == 0 {
			return "No plugins installed to update.", nil
		}

		var updated, failed []string
		for _, key := range keys {
			uCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			cmd := exec.CommandContext(uCtx, cliPath, "plugin", "update", key)
			cmd.Env = buildPluginEnv(isolatedHome)
			if _, err := cmd.CombinedOutput(); err != nil {
				// Extract short name for display.
				name := strings.SplitN(key, "@", 2)[0]
				failed = append(failed, name)
			} else {
				name := strings.SplitN(key, "@", 2)[0]
				updated = append(updated, name)
			}
			cancel()
		}

		if len(updated) > 0 {
			writeReloadFlag(isolatedHome)
		}

		msg := fmt.Sprintf("Updated %d plugin(s): %s", len(updated), strings.Join(updated, ", "))
		if len(failed) > 0 {
			msg += fmt.Sprintf("\nFailed to update %d: %s", len(failed), strings.Join(failed, ", "))
		}
		if len(updated) > 0 {
			msg += "\nRestart takes effect on the next message."
		}
		return msg, nil
	}
}

// installedPluginKeys returns the full manifest keys (e.g. "context7@claude-plugins-official")
// of all installed plugins. The CLI's `plugin update` command needs the full key.
func installedPluginKeys(isolatedHome string) []string {
	manifestPath := filepath.Join(isolatedHome, ".claude", "plugins", "installed_plugins.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil
	}
	var manifest struct {
		Plugins map[string][]struct{} `json:"plugins"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil
	}
	var keys []string
	for key := range manifest.Plugins {
		keys = append(keys, key)
	}
	return keys
}

const pluginMaxAge = 7 * 24 * time.Hour

var pluginUpdateMu sync.Mutex

// ensurePluginsUpdated checks if any installed plugins are stale (>7 days since
// lastUpdated) and updates them. Called lazily on plugin install. Non-blocking:
// failures are logged, not returned.
func ensurePluginsUpdated(cliPath, isolatedHome string) {
	manifestPath := filepath.Join(isolatedHome, ".claude", "plugins", "installed_plugins.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return
	}

	var manifest struct {
		Plugins map[string][]struct {
			LastUpdated string `json:"lastUpdated"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return
	}

	now := time.Now()
	var stale []string
	for key, installs := range manifest.Plugins {
		if len(installs) == 0 || installs[0].LastUpdated == "" {
			continue
		}
		updated, err := time.Parse(time.RFC3339, installs[0].LastUpdated)
		if err != nil {
			continue
		}
		if now.Sub(updated) > pluginMaxAge {
			name := strings.SplitN(key, "@", 2)[0]
			stale = append(stale, name)
		}
	}

	if len(stale) == 0 {
		return
	}

	pluginUpdateMu.Lock()
	defer pluginUpdateMu.Unlock()

	for _, name := range stale {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		cmd := exec.CommandContext(ctx, cliPath, "plugin", "update", name)
		cmd.Env = buildPluginEnv(isolatedHome)
		if output, err := cmd.CombinedOutput(); err != nil {
			slog.Warn("plugin auto-update failed", "plugin", name, "err", err,
				"output", strings.TrimSpace(string(output)))
		} else {
			slog.Info("plugin auto-updated", "plugin", name)
		}
		cancel()
	}
}

func checkPluginCommand(isolatedHome, pluginName string) string {
	manifestPath := filepath.Join(isolatedHome, ".claude", "plugins", "installed_plugins.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return ""
	}
	var manifest struct {
		Plugins map[string][]struct {
			InstallPath string `json:"installPath"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return ""
	}

	// Find the plugin's installPath (match by prefix since manifest key includes @marketplace).
	var installPath string
	for key, installs := range manifest.Plugins {
		if strings.HasPrefix(key, pluginName+"@") || key == pluginName {
			if len(installs) > 0 && installs[0].InstallPath != "" {
				installPath = installs[0].InstallPath
				break
			}
		}
	}
	if installPath == "" {
		return ""
	}

	mcpData, err := os.ReadFile(filepath.Join(installPath, ".mcp.json"))
	if err != nil {
		return ""
	}

	// Parse .mcp.json to find command (handles both flat and nested mcpServers format).
	var servers map[string]struct {
		Command string `json:"command"`
		Type    string `json:"type"`
	}
	if err := json.Unmarshal(mcpData, &servers); err != nil {
		// Try nested mcpServers format.
		var nested struct {
			MCPServers map[string]struct {
				Command string `json:"command"`
				Type    string `json:"type"`
			} `json:"mcpServers"`
		}
		if err := json.Unmarshal(mcpData, &nested); err != nil {
			return ""
		}
		servers = nested.MCPServers
	}

	for _, srv := range servers {
		if srv.Type == "http" || srv.Command == "" {
			continue // HTTP servers don't need a local command
		}
		if _, err := exec.LookPath(srv.Command); err != nil {
			return fmt.Sprintf("WARNING: This plugin requires '%s' which is not installed. "+
				"The plugin's MCP server will fail to start. "+
				"Tell the user they need to add '%s' to the Docker image.", srv.Command, srv.Command)
		}
	}
	return ""
}

// writeReloadFlag creates the reload signal file.
func writeReloadFlag(isolatedHome string) {
	path := filepath.Join(isolatedHome, ".curlycatclaw-reload-needed")
	os.WriteFile(path, []byte("1"), 0644) //nolint:errcheck
}

var defaultMarketplaces = []string{"anthropics/claude-plugins-official"}

const marketplaceMaxAge = 24 * time.Hour

var marketplaceMu sync.Mutex

// ensureMarketplace registers the default marketplace if missing, or updates
// it if stale (>24h since last update). Called lazily on plugin install.
// Uses a mutex to prevent concurrent git clones/pulls from racing.
// Assumes known_marketplaces.json is written atomically by the Claude CLI
// (verified against Claude Code v1.0.x).
func ensureMarketplace(cliPath, isolatedHome string) error {
	knownMkt := filepath.Join(isolatedHome, ".claude", "plugins", "known_marketplaces.json")

	// Fast path: marketplace exists and is fresh. No lock needed.
	if info, err := os.Stat(knownMkt); err == nil {
		if time.Since(info.ModTime()) < marketplaceMaxAge {
			return nil
		}
	}

	marketplaceMu.Lock()
	defer marketplaceMu.Unlock()

	// Re-check under lock (another goroutine may have just updated).
	info, err := os.Stat(knownMkt)
	if err != nil {
		// Missing: bootstrap (clone).
		for _, source := range defaultMarketplaces {
			if err := runMarketplaceCmd(cliPath, isolatedHome, "add", source); err != nil {
				return err
			}
		}
		slog.Info("marketplace bootstrapped", "home", isolatedHome)
		return nil
	}

	// Exists but stale: update (pull).
	if time.Since(info.ModTime()) >= marketplaceMaxAge {
		if err := runMarketplaceCmd(cliPath, isolatedHome, "update", ""); err != nil {
			slog.Warn("marketplace update failed, using stale data", "err", err)
		} else {
			slog.Info("marketplace updated", "home", isolatedHome)
		}
	}
	return nil
}

func runMarketplaceCmd(cliPath, isolatedHome, action, arg string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	args := []string{"plugin", "marketplace", action}
	if arg != "" {
		args = append(args, arg)
	}
	cmd := exec.CommandContext(ctx, cliPath, args...)
	cmd.Env = buildPluginEnv(isolatedHome)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("marketplace %s %s: %s: %w", action, arg, strings.TrimSpace(string(output)), err)
	}
	return nil
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
