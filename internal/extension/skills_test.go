package extension

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jialuohu/curlycatclaw/config"
	"github.com/jialuohu/curlycatclaw/skills"
)

// mockMCPAdder records calls for testing.
type mockMCPAdder struct {
	added   []string
	removed []string
	addErr  error
}

func (m *mockMCPAdder) AddServer(_ context.Context, cfg config.MCPServerConfig, _ func(string) (string, error)) error {
	if m.addErr != nil {
		return m.addErr
	}
	m.added = append(m.added, cfg.Name)
	return nil
}

func (m *mockMCPAdder) RemoveServer(name string) error {
	m.removed = append(m.removed, name)
	return nil
}

func setupTest(t *testing.T) (*Registry, *mockMCPAdder, *skills.Registry, []*skills.Skill) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "extensions.json")
	reg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	mcpMgr := &mockMCPAdder{}
	skillReg := skills.NewRegistry()
	reloadCalled := false
	reloadFunc := func() { reloadCalled = true }
	_ = reloadCalled
	ss := InitExtensionSkills(reg, mcpMgr, skillReg, reloadFunc, nil, nil, nil)
	return reg, mcpMgr, skillReg, ss
}

func findSkill(ss []*skills.Skill, name string) *skills.Skill {
	for _, s := range ss {
		if s.Name == name {
			return s
		}
	}
	return nil
}

func TestAddMCPExtension(t *testing.T) {
	reg, mcpMgr, _, ss := setupTest(t)
	skill := findSkill(ss, "add_extension")
	if skill == nil {
		t.Fatal("add_extension skill not found")
	}

	input := `{"name":"brave","type":"mcp","command":"npx","args":["-y","mcp-brave"],"env":{"KEY":"val"}}`
	result, err := skill.Execute(context.Background(), json.RawMessage(input))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "immediately") {
		t.Fatalf("expected immediate availability message, got: %s", result)
	}
	if len(mcpMgr.added) != 1 || mcpMgr.added[0] != "brave" {
		t.Fatalf("expected MCP AddServer called with brave, got: %v", mcpMgr.added)
	}
	if reg.Get("brave") == nil {
		t.Fatal("expected extension to be persisted")
	}
}

func TestAddMCPExtensionStartFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "extensions.json")
	reg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	mcpMgr := &mockMCPAdder{addErr: errors.New("connection refused")}
	skillReg := skills.NewRegistry()
	ss := InitExtensionSkills(reg, mcpMgr, skillReg, nil, nil, nil, nil)
	skill := findSkill(ss, "add_extension")

	input := `{"name":"broken","type":"mcp","command":"echo"}`
	_, err = skill.Execute(context.Background(), json.RawMessage(input))
	if err == nil {
		t.Fatal("expected error when MCP server fails to start")
	}
	if reg.Get("broken") != nil {
		t.Fatal("extension should not be persisted on MCP start failure")
	}
}

func TestAddMCPExtensionNilManager(t *testing.T) {
	path := filepath.Join(t.TempDir(), "extensions.json")
	reg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	skillReg := skills.NewRegistry()
	ss := InitExtensionSkills(reg, nil, skillReg, nil, nil, nil, nil)
	skill := findSkill(ss, "add_extension")

	input := `{"name":"remote","type":"mcp","command":"npx","args":["-y","mcp-server"]}`
	result, err := skill.Execute(context.Background(), json.RawMessage(input))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "next message") {
		t.Fatalf("expected deferred availability message when mcpMgr is nil, got: %s", result)
	}
	if reg.Get("remote") == nil {
		t.Fatal("expected extension to be persisted even with nil mcpMgr")
	}
}

func TestAddExecExtension(t *testing.T) {
	reg, _, skillReg, ss := setupTest(t)
	skill := findSkill(ss, "add_extension")

	input := `{"name":"my-tool","type":"exec","command":"/bin/echo","description":"echoes stuff"}`
	result, err := skill.Execute(context.Background(), json.RawMessage(input))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "ext__my-tool") {
		t.Fatalf("expected registry name in result, got: %s", result)
	}
	if skillReg.Get("ext__my-tool") == nil {
		t.Fatal("expected skill to be registered")
	}
	if reg.Get("my-tool") == nil {
		t.Fatal("expected extension to be persisted")
	}
}

func TestRemoveMCPExtension(t *testing.T) {
	reg, mcpMgr, _, ss := setupTest(t)

	// First add one.
	addSkill := findSkill(ss, "add_extension")
	input := `{"name":"test-mcp","type":"mcp","command":"echo"}`
	if _, err := addSkill.Execute(context.Background(), json.RawMessage(input)); err != nil {
		t.Fatal(err)
	}

	// Now remove it.
	removeSkill := findSkill(ss, "remove_extension")
	result, err := removeSkill.Execute(context.Background(), json.RawMessage(`{"name":"test-mcp"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "removed") {
		t.Fatalf("expected removal message, got: %s", result)
	}
	if len(mcpMgr.removed) != 1 || mcpMgr.removed[0] != "test-mcp" {
		t.Fatalf("expected MCP RemoveServer called, got: %v", mcpMgr.removed)
	}
	if reg.Get("test-mcp") != nil {
		t.Fatal("expected extension to be removed from registry")
	}
}

func TestRemoveExecExtension(t *testing.T) {
	reg, _, skillReg, ss := setupTest(t)

	addSkill := findSkill(ss, "add_extension")
	input := `{"name":"my-exec","type":"exec","command":"/bin/echo","description":"test"}`
	if _, err := addSkill.Execute(context.Background(), json.RawMessage(input)); err != nil {
		t.Fatal(err)
	}

	removeSkill := findSkill(ss, "remove_extension")
	if _, err := removeSkill.Execute(context.Background(), json.RawMessage(`{"name":"my-exec"}`)); err != nil {
		t.Fatal(err)
	}
	if skillReg.Get("ext__my-exec") != nil {
		t.Fatal("expected skill to be unregistered")
	}
	if reg.Get("my-exec") != nil {
		t.Fatal("expected extension to be removed from registry")
	}
}

func TestSkillRemoveNotFound(t *testing.T) {
	_, _, _, ss := setupTest(t)
	removeSkill := findSkill(ss, "remove_extension")
	_, err := removeSkill.Execute(context.Background(), json.RawMessage(`{"name":"nonexistent"}`))
	if err == nil {
		t.Fatal("expected error for nonexistent extension")
	}
}

func TestListExtensions(t *testing.T) {
	_, _, _, ss := setupTest(t)
	listSkill := findSkill(ss, "list_extensions")

	// Empty list.
	result, err := listSkill.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "No extensions") {
		t.Fatalf("expected empty message, got: %s", result)
	}

	// Add some extensions then list.
	addSkill := findSkill(ss, "add_extension")
	if _, err := addSkill.Execute(context.Background(), json.RawMessage(`{"name":"mcp1","type":"mcp","command":"echo"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := addSkill.Execute(context.Background(), json.RawMessage(`{"name":"exec1","type":"exec","command":"/bin/echo","description":"test"}`)); err != nil {
		t.Fatal(err)
	}

	result, err = listSkill.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "Runtime extensions") {
		t.Fatalf("expected runtime extensions section, got: %s", result)
	}
	if !strings.Contains(result, "mcp1") || !strings.Contains(result, "exec1") {
		t.Fatalf("expected both extensions listed, got: %s", result)
	}
}

func TestAddPromptExtension(t *testing.T) {
	reg, _, _, ss := setupTest(t)
	addSkill := findSkill(ss, "add_extension")

	// Create a skill directory with SKILL.md.
	skillDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("# Review Checklist\nDo this, then that."), 0644); err != nil {
		t.Fatal(err)
	}

	input := fmt.Sprintf(`{"name":"my-review","type":"prompt","command":%q,"description":"Code review skill"}`, skillDir)
	result, err := addSkill.Execute(context.Background(), json.RawMessage(input))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "Prompt skill") {
		t.Fatalf("expected prompt skill message, got: %s", result)
	}
	if reg.Get("my-review") == nil {
		t.Fatal("expected prompt extension to be persisted")
	}
}

func TestLoadPromptSkill(t *testing.T) {
	_, _, _, ss := setupTest(t)

	// Create and register a prompt skill.
	skillDir := t.TempDir()
	content := "# My Skill\n\nFollow these instructions."
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	addSkill := findSkill(ss, "add_extension")
	input := fmt.Sprintf(`{"name":"test-skill","type":"prompt","command":%q,"description":"Test"}`, skillDir)
	if _, err := addSkill.Execute(context.Background(), json.RawMessage(input)); err != nil {
		t.Fatal(err)
	}

	// Load the prompt skill.
	loadSkill := findSkill(ss, "load_prompt_skill")
	if loadSkill == nil {
		t.Fatal("load_prompt_skill not found")
	}
	result, err := loadSkill.Execute(context.Background(), json.RawMessage(`{"name":"test-skill"}`))
	if err != nil {
		t.Fatal(err)
	}
	if result != content {
		t.Fatalf("expected SKILL.md content, got: %s", result)
	}

	// Loading a non-existent skill should fail.
	_, err = loadSkill.Execute(context.Background(), json.RawMessage(`{"name":"nonexistent"}`))
	if err == nil {
		t.Fatal("expected error for nonexistent prompt skill")
	}

	// Loading an MCP extension should fail (wrong type).
	if _, err := addSkill.Execute(context.Background(), json.RawMessage(`{"name":"mcp-thing","type":"mcp","command":"echo"}`)); err != nil {
		t.Fatal(err)
	}
	_, err = loadSkill.Execute(context.Background(), json.RawMessage(`{"name":"mcp-thing"}`))
	if err == nil {
		t.Fatal("expected error for non-prompt extension")
	}
}

func TestRemovePromptExtension(t *testing.T) {
	reg, _, _, ss := setupTest(t)

	skillDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("# Skill"), 0644); err != nil {
		t.Fatal(err)
	}

	addSkill := findSkill(ss, "add_extension")
	input := fmt.Sprintf(`{"name":"rm-prompt","type":"prompt","command":%q,"description":"test"}`, skillDir)
	if _, err := addSkill.Execute(context.Background(), json.RawMessage(input)); err != nil {
		t.Fatal(err)
	}

	removeSkill := findSkill(ss, "remove_extension")
	result, err := removeSkill.Execute(context.Background(), json.RawMessage(`{"name":"rm-prompt"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "removed") {
		t.Fatalf("expected removal message, got: %s", result)
	}
	if reg.Get("rm-prompt") != nil {
		t.Fatal("expected prompt extension to be removed")
	}
}

func TestAddDuplicateExtension(t *testing.T) {
	_, _, _, ss := setupTest(t)
	addSkill := findSkill(ss, "add_extension")

	input := `{"name":"dup","type":"mcp","command":"echo"}`
	if _, err := addSkill.Execute(context.Background(), json.RawMessage(input)); err != nil {
		t.Fatal(err)
	}
	_, err := addSkill.Execute(context.Background(), json.RawMessage(input))
	if err == nil {
		t.Fatal("expected duplicate error")
	}
}

// mockMCPHotReloader records hot-reload calls for testing.
type mockMCPHotReloader struct {
	connected     []string
	disconnected  []string
	oldCloserCalls int
	connectErr    error
	disconnectErr error
}

func (m *mockMCPHotReloader) ConnectAndRegister(_ context.Context, ext *Extension) ([]string, func(), error) {
	if m.connectErr != nil {
		return nil, nil, m.connectErr
	}
	m.connected = append(m.connected, ext.Name)
	closer := func() { m.oldCloserCalls++ }
	return []string{ext.Name + "__tool1 — a tool"}, closer, nil
}

func (m *mockMCPHotReloader) DisconnectAndUnregister(name string) error {
	if m.disconnectErr != nil {
		return m.disconnectErr
	}
	m.disconnected = append(m.disconnected, name)
	return nil
}

func TestAddMCPExtensionHotReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "extensions.json")
	reg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	skillReg := skills.NewRegistry()
	hr := &mockMCPHotReloader{}
	ss := InitExtensionSkills(reg, nil, skillReg, nil, hr, nil, nil)
	skill := findSkill(ss, "add_extension")

	input := `{"name":"hot","type":"mcp","command":"echo"}`
	result, err := skill.Execute(context.Background(), json.RawMessage(input))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "immediately") {
		t.Fatalf("expected immediate availability, got: %s", result)
	}
	if len(hr.connected) != 1 || hr.connected[0] != "hot" {
		t.Fatalf("expected ConnectAndRegister called with 'hot', got: %v", hr.connected)
	}
}

func TestAddMCPExtensionHotReloadFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "extensions.json")
	reg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	skillReg := skills.NewRegistry()
	hr := &mockMCPHotReloader{connectErr: errors.New("connection refused")}
	reloadCalled := false
	reloadFunc := func() { reloadCalled = true }
	ss := InitExtensionSkills(reg, nil, skillReg, reloadFunc, hr, nil, nil)
	skill := findSkill(ss, "add_extension")

	input := `{"name":"fallback","type":"mcp","command":"echo"}`
	result, err := skill.Execute(context.Background(), json.RawMessage(input))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "next message") {
		t.Fatalf("expected deferred availability on fallback, got: %s", result)
	}
	if !reloadCalled {
		t.Fatal("expected reloadFunc to be called on hot-reload failure")
	}
}

func TestRemoveMCPExtensionHotReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "extensions.json")
	reg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	skillReg := skills.NewRegistry()
	hr := &mockMCPHotReloader{}
	reloadCalled := false
	reloadFunc := func() { reloadCalled = true }
	ss := InitExtensionSkills(reg, nil, skillReg, reloadFunc, hr, nil, nil)

	// Add first.
	addSkill := findSkill(ss, "add_extension")
	if _, err := addSkill.Execute(context.Background(), json.RawMessage(`{"name":"rm-hot","type":"mcp","command":"echo"}`)); err != nil {
		t.Fatal(err)
	}

	// Remove.
	removeSkill := findSkill(ss, "remove_extension")
	result, err := removeSkill.Execute(context.Background(), json.RawMessage(`{"name":"rm-hot"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "removed") {
		t.Fatalf("expected removal message, got: %s", result)
	}
	if len(hr.disconnected) != 1 || hr.disconnected[0] != "rm-hot" {
		t.Fatalf("expected DisconnectAndUnregister called, got: %v", hr.disconnected)
	}
	if reloadCalled {
		t.Fatal("reloadFunc should not be called when hot-reload succeeds")
	}
}

func TestRemoveMCPExtensionHotReloadFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "extensions.json")
	reg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	skillReg := skills.NewRegistry()
	hr := &mockMCPHotReloader{disconnectErr: errors.New("session gone")}
	reloadCalled := false
	reloadFunc := func() { reloadCalled = true }
	ss := InitExtensionSkills(reg, nil, skillReg, reloadFunc, hr, nil, nil)

	addSkill := findSkill(ss, "add_extension")
	if _, err := addSkill.Execute(context.Background(), json.RawMessage(`{"name":"rm-fail","type":"mcp","command":"echo"}`)); err != nil {
		t.Fatal(err)
	}

	removeSkill := findSkill(ss, "remove_extension")
	if _, err := removeSkill.Execute(context.Background(), json.RawMessage(`{"name":"rm-fail"}`)); err != nil {
		t.Fatal(err)
	}
	if !reloadCalled {
		t.Fatal("expected reloadFunc called on hot-unload failure")
	}
}
