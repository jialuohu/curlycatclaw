package skills

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestReplaceHomeEnv(t *testing.T) {
	env := []string{"HOME=/old", "PATH=/usr/bin"}
	result := replaceHomeEnv(env, "/new")

	found := false
	for _, e := range result {
		if e == "HOME=/new" {
			found = true
		}
		if e == "HOME=/old" {
			t.Error("old HOME should be replaced")
		}
	}
	if !found {
		t.Error("HOME=/new not found")
	}
}

func TestReplaceHomeEnv_NoExisting(t *testing.T) {
	env := []string{"PATH=/usr/bin"}
	result := replaceHomeEnv(env, "/new")

	if len(result) != 2 {
		t.Errorf("len = %d, want 2", len(result))
	}
	found := false
	for _, e := range result {
		if e == "HOME=/new" {
			found = true
		}
	}
	if !found {
		t.Error("HOME=/new should be appended")
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
