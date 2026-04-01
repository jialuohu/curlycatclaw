package skills

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInitPluginSkills_ReturnsAllSkills(t *testing.T) {
	skills := InitPluginSkills("/usr/bin/claude", "/tmp/isolated", []string{"context7"})
	if len(skills) != 5 {
		t.Fatalf("expected 5 plugin skills, got %d", len(skills))
	}

	names := make(map[string]bool)
	for _, s := range skills {
		names[s.Name] = true
	}

	for _, expected := range []string{"install_plugin", "uninstall_plugin", "list_plugins", "enable_plugin", "disable_plugin"} {
		if !names[expected] {
			t.Errorf("missing skill %q", expected)
		}
	}
}

func TestInstallPlugin_AllowlistRejects(t *testing.T) {
	skills := InitPluginSkills("/usr/bin/claude", "/tmp/isolated", []string{"context7", "playwright"})

	var installSkill *Skill
	for _, s := range skills {
		if s.Name == "install_plugin" {
			installSkill = s
			break
		}
	}
	if installSkill == nil {
		t.Fatal("install_plugin skill not found")
	}

	input, _ := json.Marshal(map[string]string{"name": "malicious-plugin"})
	_, err := installSkill.Execute(context.Background(), input)
	if err == nil {
		t.Fatal("expected error for non-allowed plugin")
	}
	if !strings.Contains(err.Error(), "not in the allowed list") {
		t.Errorf("error = %q, want it to mention allowed list", err.Error())
	}
}

func TestInstallPlugin_AllowlistAccepts(t *testing.T) {
	// We can't actually run `claude plugin install` but we can verify it
	// doesn't reject an allowed plugin name at the validation layer.
	// The exec will fail because the binary doesn't exist, which is fine.
	skills := InitPluginSkills("/nonexistent-binary", "/tmp/isolated", []string{"context7"})

	var installSkill *Skill
	for _, s := range skills {
		if s.Name == "install_plugin" {
			installSkill = s
			break
		}
	}

	input, _ := json.Marshal(map[string]string{"name": "context7"})
	_, err := installSkill.Execute(context.Background(), input)
	// Should fail with exec error, NOT allowlist error.
	if err == nil {
		t.Fatal("expected exec error (binary doesn't exist)")
	}
	if strings.Contains(err.Error(), "not in the allowed list") {
		t.Error("context7 should be in the allowed list")
	}
}

func TestInstallPlugin_EmptyName(t *testing.T) {
	skills := InitPluginSkills("/usr/bin/claude", "/tmp/isolated", []string{"context7"})

	var installSkill *Skill
	for _, s := range skills {
		if s.Name == "install_plugin" {
			installSkill = s
			break
		}
	}

	input, _ := json.Marshal(map[string]string{"name": ""})
	_, err := installSkill.Execute(context.Background(), input)
	if err == nil {
		t.Fatal("expected error for empty name")
	}
	if !strings.Contains(err.Error(), "required") {
		t.Errorf("error = %q, want mention of required", err.Error())
	}
}

func TestWriteReloadFlag(t *testing.T) {
	dir := t.TempDir()
	writeReloadFlag(dir)

	path := filepath.Join(dir, ".curlycatclaw-reload-needed")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("reload flag file should exist: %v", err)
	}
}

func TestBuildPluginEnv(t *testing.T) {
	result := buildPluginEnv("/isolated")
	hasHome := false
	hasMasterKey := false
	for _, e := range result {
		if e == "HOME=/isolated" {
			hasHome = true
		}
		if strings.HasPrefix(e, "CURLYCATCLAW_MASTER_KEY=") {
			hasMasterKey = true
		}
	}
	if !hasHome {
		t.Error("HOME=/isolated not found")
	}
	if hasMasterKey {
		t.Error("CURLYCATCLAW_MASTER_KEY should NOT be in plugin env")
	}
}

func TestEnsureMarketplace_SkipsWhenFresh(t *testing.T) {
	dir := t.TempDir()
	pluginDir := filepath.Join(dir, ".claude", "plugins")
	if err := os.MkdirAll(pluginDir, 0700); err != nil {
		t.Fatal(err)
	}
	// Create a fresh known_marketplaces.json (mod time = now).
	if err := os.WriteFile(filepath.Join(pluginDir, "known_marketplaces.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}

	// Should skip entirely (no CLI call needed, path is invalid anyway).
	err := ensureMarketplace("/nonexistent-binary", dir)
	if err != nil {
		t.Fatalf("expected nil error for fresh marketplace, got: %v", err)
	}
}

func TestEnsureMarketplace_FailsWhenMissingAndNoCLI(t *testing.T) {
	dir := t.TempDir()
	pluginDir := filepath.Join(dir, ".claude", "plugins")
	if err := os.MkdirAll(pluginDir, 0700); err != nil {
		t.Fatal(err)
	}
	// No known_marketplaces.json, invalid CLI path.
	err := ensureMarketplace("/nonexistent-binary", dir)
	if err == nil {
		t.Fatal("expected error when CLI is missing and marketplace not bootstrapped")
	}
	if !strings.Contains(err.Error(), "marketplace add") {
		t.Errorf("error = %q, want marketplace add mention", err.Error())
	}
}

func TestEnsureMarketplace_UpdatesWhenStale(t *testing.T) {
	dir := t.TempDir()
	pluginDir := filepath.Join(dir, ".claude", "plugins")
	if err := os.MkdirAll(pluginDir, 0700); err != nil {
		t.Fatal(err)
	}
	// Create a stale known_marketplaces.json (mod time = 25h ago).
	mktPath := filepath.Join(pluginDir, "known_marketplaces.json")
	if err := os.WriteFile(mktPath, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	staleTime := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(mktPath, staleTime, staleTime); err != nil {
		t.Fatal(err)
	}

	// Should try to update (will fail with invalid CLI, but that's non-fatal for update).
	err := ensureMarketplace("/nonexistent-binary", dir)
	// Update failure is non-fatal, so no error returned.
	if err != nil {
		t.Fatalf("expected nil error (stale update failure is non-fatal), got: %v", err)
	}
}

func TestCheckPluginCommand_Available(t *testing.T) {
	dir := t.TempDir()
	pluginsDir := filepath.Join(dir, ".claude", "plugins")
	installDir := filepath.Join(dir, "cache", "test-plugin")
	if err := os.MkdirAll(pluginsDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(installDir, 0700); err != nil {
		t.Fatal(err)
	}
	// Plugin that uses "ls" (always available).
	mcpData, _ := json.Marshal(map[string]any{
		"test": map[string]any{"command": "ls", "args": []string{"-la"}},
	})
	if err := os.WriteFile(filepath.Join(installDir, ".mcp.json"), mcpData, 0644); err != nil {
		t.Fatal(err)
	}
	manifest, _ := json.Marshal(map[string]any{
		"plugins": map[string]any{
			"test@mkt": []any{map[string]any{"installPath": installDir}},
		},
	})
	if err := os.WriteFile(filepath.Join(pluginsDir, "installed_plugins.json"), manifest, 0644); err != nil {
		t.Fatal(err)
	}
	warning := checkPluginCommand(dir, "test")
	if warning != "" {
		t.Errorf("expected no warning for available command, got: %s", warning)
	}
}

func TestCheckPluginCommand_Missing(t *testing.T) {
	dir := t.TempDir()
	pluginsDir := filepath.Join(dir, ".claude", "plugins")
	installDir := filepath.Join(dir, "cache", "test-plugin")
	if err := os.MkdirAll(pluginsDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(installDir, 0700); err != nil {
		t.Fatal(err)
	}
	// Plugin that uses a nonexistent command.
	mcpData, _ := json.Marshal(map[string]any{
		"test": map[string]any{"command": "nonexistent-runtime-xyz", "args": []string{}},
	})
	if err := os.WriteFile(filepath.Join(installDir, ".mcp.json"), mcpData, 0644); err != nil {
		t.Fatal(err)
	}
	manifest, _ := json.Marshal(map[string]any{
		"plugins": map[string]any{
			"test@mkt": []any{map[string]any{"installPath": installDir}},
		},
	})
	if err := os.WriteFile(filepath.Join(pluginsDir, "installed_plugins.json"), manifest, 0644); err != nil {
		t.Fatal(err)
	}
	warning := checkPluginCommand(dir, "test")
	if !strings.Contains(warning, "nonexistent-runtime-xyz") {
		t.Errorf("expected warning about missing command, got: %q", warning)
	}
}

func TestCheckPluginCommand_HTTPSkipped(t *testing.T) {
	dir := t.TempDir()
	pluginsDir := filepath.Join(dir, ".claude", "plugins")
	installDir := filepath.Join(dir, "cache", "test-plugin")
	if err := os.MkdirAll(pluginsDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(installDir, 0700); err != nil {
		t.Fatal(err)
	}
	// HTTP plugin (no command needed).
	mcpData, _ := json.Marshal(map[string]any{
		"test": map[string]any{"type": "http", "url": "https://example.com/mcp"},
	})
	if err := os.WriteFile(filepath.Join(installDir, ".mcp.json"), mcpData, 0644); err != nil {
		t.Fatal(err)
	}
	manifest, _ := json.Marshal(map[string]any{
		"plugins": map[string]any{
			"test@mkt": []any{map[string]any{"installPath": installDir}},
		},
	})
	if err := os.WriteFile(filepath.Join(pluginsDir, "installed_plugins.json"), manifest, 0644); err != nil {
		t.Fatal(err)
	}
	warning := checkPluginCommand(dir, "test")
	if warning != "" {
		t.Errorf("expected no warning for HTTP plugin, got: %s", warning)
	}
}

func TestInstallPlugin_EmptyAllowlist(t *testing.T) {
	// Empty allowlist means nothing can be installed.
	skills := InitPluginSkills("/usr/bin/claude", "/tmp/isolated", nil)

	var installSkill *Skill
	for _, s := range skills {
		if s.Name == "install_plugin" {
			installSkill = s
			break
		}
	}

	input, _ := json.Marshal(map[string]string{"name": "anything"})
	_, err := installSkill.Execute(context.Background(), input)
	if err == nil {
		t.Fatal("expected error with empty allowlist")
	}
	if !strings.Contains(err.Error(), "not in the allowed list") {
		t.Errorf("error = %q, want allowed list mention", err.Error())
	}
}
