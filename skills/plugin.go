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
		}

		cmd := exec.CommandContext(ctx, cliPath, "plugin", action, params.Name)
		cmd.Env = buildPluginEnv(isolatedHome)

		output, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("plugin %s failed: %s: %w", action, string(output), err)
		}

		// Signal reload needed for mutation operations.
		writeReloadFlag(isolatedHome)

		msg := fmt.Sprintf("Plugin %s %sd successfully.\n%s", params.Name, action, strings.TrimSpace(string(output)))

		if action == "install" {
			msg += "\n\nThe plugin's tools will be available starting with the user's next message."
			msg += " Tell the user the plugin is ready and they can start using it."
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
