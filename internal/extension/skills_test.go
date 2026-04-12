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
	added      []config.MCPServerConfig
	addedNames []string
	removed    []string
	addErr     error
}

func (m *mockMCPAdder) AddServer(_ context.Context, cfg config.MCPServerConfig, _ func(string) (string, error)) error {
	if m.addErr != nil {
		return m.addErr
	}
	m.added = append(m.added, cfg)
	m.addedNames = append(m.addedNames, cfg.Name)
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
	ss := InitExtensionSkills(reg, mcpMgr, skillReg, reloadFunc, nil, nil, nil, nil)
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
	if len(mcpMgr.addedNames) != 1 || mcpMgr.addedNames[0] != "brave" {
		t.Fatalf("expected MCP AddServer called with brave, got: %v", mcpMgr.addedNames)
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
	ss := InitExtensionSkills(reg, mcpMgr, skillReg, nil, nil, nil, nil, nil)
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
	ss := InitExtensionSkills(reg, nil, skillReg, nil, nil, nil, nil, nil)
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
	connected      []string
	disconnected   []string
	oldCloserCalls int
	connectErr     error
	disconnectErr  error
	// connectFunc overrides default ConnectAndRegister behavior when non-nil.
	connectFunc func() ([]string, func(), error)
}

func (m *mockMCPHotReloader) ConnectAndRegister(_ context.Context, ext *Extension) ([]string, func(), error) {
	if m.connectFunc != nil {
		return m.connectFunc()
	}
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
	ss := InitExtensionSkills(reg, nil, skillReg, nil, hr, nil, nil, nil)
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
	ss := InitExtensionSkills(reg, nil, skillReg, reloadFunc, hr, nil, nil, nil)
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
	ss := InitExtensionSkills(reg, nil, skillReg, reloadFunc, hr, nil, nil, nil)

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
	ss := InitExtensionSkills(reg, nil, skillReg, reloadFunc, hr, nil, nil, nil)

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

func TestAddHTTPMCPExtension(t *testing.T) {
	reg, mcpMgr, _, ss := setupTest(t)
	skill := findSkill(ss, "add_extension")

	input := `{"name":"remote-api","type":"mcp","transport":"http","url":"http://localhost:18060/mcp","headers":{"X-Api-Key":"secret"}}`
	result, err := skill.Execute(context.Background(), json.RawMessage(input))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "immediately") {
		t.Fatalf("expected immediate availability, got: %s", result)
	}

	// Verify MCPServerConfig received correct fields.
	if len(mcpMgr.added) != 1 {
		t.Fatalf("expected 1 AddServer call, got %d", len(mcpMgr.added))
	}
	cfg := mcpMgr.added[0]
	if cfg.Transport != "http" {
		t.Errorf("config transport = %q, want http", cfg.Transport)
	}
	if cfg.URL != "http://localhost:18060/mcp" {
		t.Errorf("config url = %q, want http://localhost:18060/mcp", cfg.URL)
	}
	if cfg.Headers["X-Api-Key"] != "secret" {
		t.Error("expected headers passed through to MCPServerConfig")
	}
	if cfg.Command != "" {
		t.Errorf("config command should be empty for HTTP, got %q", cfg.Command)
	}

	// Verify persistence.
	got := reg.Get("remote-api")
	if got == nil {
		t.Fatal("expected extension to be persisted")
	}
	if got.Transport != "http" {
		t.Errorf("persisted transport = %q, want http", got.Transport)
	}
	if got.URL != "http://localhost:18060/mcp" {
		t.Errorf("persisted url = %q", got.URL)
	}
}

func TestAddHTTPMCPExtensionMissingURL(t *testing.T) {
	_, _, _, ss := setupTest(t)
	skill := findSkill(ss, "add_extension")

	input := `{"name":"no-url","type":"mcp","transport":"http"}`
	_, err := skill.Execute(context.Background(), json.RawMessage(input))
	if err == nil {
		t.Fatal("expected error for HTTP extension without URL")
	}
	if !strings.Contains(err.Error(), "url is required") {
		t.Fatalf("expected url-required error, got: %v", err)
	}
}

func TestAddHTTPMCPExtensionWithCommand(t *testing.T) {
	_, _, _, ss := setupTest(t)
	skill := findSkill(ss, "add_extension")

	input := `{"name":"bad","type":"mcp","transport":"http","url":"http://localhost:8080/mcp","command":"echo"}`
	_, err := skill.Execute(context.Background(), json.RawMessage(input))
	if err == nil {
		t.Fatal("expected error for HTTP extension with command")
	}
	if !strings.Contains(err.Error(), "command is not allowed") {
		t.Fatalf("expected command-not-allowed error, got: %v", err)
	}
}

func TestAddStdioMCPExtensionWithURL(t *testing.T) {
	_, _, _, ss := setupTest(t)
	skill := findSkill(ss, "add_extension")

	input := `{"name":"bad","type":"mcp","command":"echo","url":"http://localhost:8080"}`
	_, err := skill.Execute(context.Background(), json.RawMessage(input))
	if err == nil {
		t.Fatal("expected error for stdio extension with URL")
	}
	if !strings.Contains(err.Error(), "url is not allowed") {
		t.Fatalf("expected url-not-allowed error, got: %v", err)
	}
}

func TestAddHTTPMCPExtensionWithHeaders(t *testing.T) {
	reg, mcpMgr, _, ss := setupTest(t)
	skill := findSkill(ss, "add_extension")

	input := `{"name":"authed-api","type":"mcp","transport":"http","url":"https://api.example.com/mcp","headers":{"Authorization":"Bearer tok","X-Custom":"val"}}`
	if _, err := skill.Execute(context.Background(), json.RawMessage(input)); err != nil {
		t.Fatal(err)
	}

	cfg := mcpMgr.added[0]
	if cfg.Headers["Authorization"] != "Bearer tok" {
		t.Error("expected Authorization header")
	}
	if cfg.Headers["X-Custom"] != "val" {
		t.Error("expected X-Custom header")
	}

	got := reg.Get("authed-api")
	if got.Headers["Authorization"] != "Bearer tok" {
		t.Error("expected headers persisted")
	}
}

func TestListExtensionsHTTP(t *testing.T) {
	_, _, _, ss := setupTest(t)
	addSkill := findSkill(ss, "add_extension")
	listSkill := findSkill(ss, "list_extensions")

	// Add an HTTP extension.
	input := `{"name":"http-mcp","type":"mcp","transport":"http","url":"http://localhost:18060/mcp"}`
	if _, err := addSkill.Execute(context.Background(), json.RawMessage(input)); err != nil {
		t.Fatal(err)
	}

	result, err := listSkill.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "http") {
		t.Fatalf("expected http indicator in listing, got: %s", result)
	}
	if !strings.Contains(result, "http://localhost:18060/mcp") {
		t.Fatalf("expected URL in listing, got: %s", result)
	}
	// HTTP extensions should not show "Command:".
	lines := strings.Split(result, "\n")
	for i, line := range lines {
		if strings.Contains(line, "http-mcp") {
			// Next line should be URL, not Command.
			if i+1 < len(lines) && strings.Contains(lines[i+1], "Command:") {
				t.Fatal("HTTP extension should show URL, not Command")
			}
			break
		}
	}
}

func TestRemoveHTTPMCPExtension(t *testing.T) {
	reg, mcpMgr, _, ss := setupTest(t)
	addSkill := findSkill(ss, "add_extension")
	removeSkill := findSkill(ss, "remove_extension")

	input := `{"name":"rm-http","type":"mcp","transport":"http","url":"http://localhost:18060/mcp"}`
	if _, err := addSkill.Execute(context.Background(), json.RawMessage(input)); err != nil {
		t.Fatal(err)
	}

	result, err := removeSkill.Execute(context.Background(), json.RawMessage(`{"name":"rm-http"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "removed") {
		t.Fatalf("expected removal message, got: %s", result)
	}
	if len(mcpMgr.removed) != 1 || mcpMgr.removed[0] != "rm-http" {
		t.Fatalf("expected RemoveServer called, got: %v", mcpMgr.removed)
	}
	if reg.Get("rm-http") != nil {
		t.Fatal("expected HTTP extension to be removed")
	}
}

func TestAddHTTPMCPExtensionConnectionFailureGuidesManageService(t *testing.T) {
	path := filepath.Join(t.TempDir(), "extensions.json")
	reg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	mcpMgr := &mockMCPAdder{addErr: errors.New("connection refused")}
	skillReg := skills.NewRegistry()
	ss := InitExtensionSkills(reg, mcpMgr, skillReg, nil, nil, nil, nil, nil)
	skill := findSkill(ss, "add_extension")

	// HTTP extension that can't connect should NOT error — should return guidance.
	input := `{"name":"unreachable","type":"mcp","transport":"http","url":"http://localhost:99999/mcp"}`
	result, err := skill.Execute(context.Background(), json.RawMessage(input))
	if err != nil {
		t.Fatalf("HTTP extension connection failure should not error, got: %v", err)
	}
	if !strings.Contains(result, "manage_service") {
		t.Fatalf("expected manage_service guidance in response, got: %s", result)
	}
	if !strings.Contains(result, "not reachable") {
		t.Fatalf("expected 'not reachable' message, got: %s", result)
	}
	// Extension should still be persisted (so it reconnects when server starts).
	if reg.Get("unreachable") == nil {
		t.Fatal("HTTP extension should be persisted even when connection fails")
	}
}

func TestAddHTTPMCPExtensionAutoStartSuccess(t *testing.T) {
	// API mode: mcpMgr first fails, autoStarter starts the service,
	// retry succeeds. Tests the caller-layer auto-start recovery.
	path := filepath.Join(t.TempDir(), "extensions.json")
	reg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	mcpMgr := &mockMCPAdder{} // will override addErr dynamically
	mcpMgr.addErr = errors.New("connection refused")
	skillReg := skills.NewRegistry()

	autoStartCalled := false
	autoStarter := ServiceAutoStarter(func(_ context.Context, name string, _ *ServiceRegInfo) (string, error) {
		autoStartCalled = true
		// Simulate: service started successfully, now mcpMgr.AddServer succeeds.
		mcpMgr.addErr = nil
		return "started and healthy", nil
	})

	ss := InitExtensionSkills(reg, mcpMgr, skillReg, nil, nil, nil, nil, autoStarter)
	skill := findSkill(ss, "add_extension")

	input := `{"name":"auto-api","type":"mcp","transport":"http","url":"http://localhost:18060/mcp","image":"test/image","ports":{"18060":"18060"}}`
	result, err := skill.Execute(context.Background(), json.RawMessage(input))
	if err != nil {
		t.Fatalf("auto-start should recover from connection failure, got: %v", err)
	}
	if !autoStartCalled {
		t.Fatal("expected autoStarter to be called")
	}
	if !strings.Contains(result, "auto-started") {
		t.Fatalf("expected auto-started in message, got: %s", result)
	}
	if !strings.Contains(result, "immediately") {
		t.Fatalf("expected tools to be immediately available after retry, got: %s", result)
	}
}

func TestAddHTTPMCPExtensionCLIModeAutoStartSuccess(t *testing.T) {
	// CLI mode: hot-reload initially fails (server down), autoStarter starts service,
	// hot-reload retry succeeds. Tests the enhanceHTTPResult path.
	path := filepath.Join(t.TempDir(), "extensions.json")
	reg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	skillReg := skills.NewRegistry()

	connectAttempt := 0
	hr := &mockMCPHotReloader{
		connectFunc: func() ([]string, func(), error) {
			connectAttempt++
			if connectAttempt == 1 {
				return nil, nil, errors.New("connection refused")
			}
			// Second attempt after auto-start: success.
			return []string{"tool1", "tool2"}, nil, nil
		},
	}

	autoStartCalled := false
	autoStarter := ServiceAutoStarter(func(_ context.Context, name string, _ *ServiceRegInfo) (string, error) {
		autoStartCalled = true
		return "started", nil
	})

	reloadCalled := false
	reloadFunc := func() { reloadCalled = true }
	ss := InitExtensionSkills(reg, nil, skillReg, reloadFunc, hr, nil, nil, autoStarter)
	skill := findSkill(ss, "add_extension")

	input := `{"name":"auto-cli","type":"mcp","transport":"http","url":"http://localhost:18060/mcp","image":"test/image","ports":{"18060":"18060"}}`
	result, err := skill.Execute(context.Background(), json.RawMessage(input))
	if err != nil {
		t.Fatalf("CLI mode auto-start should succeed, got: %v", err)
	}
	if !autoStartCalled {
		t.Fatal("expected autoStarter to be called")
	}
	if !strings.Contains(result, "immediately") {
		t.Fatalf("expected immediate tool availability after auto-start + hot-reload, got: %s", result)
	}
	// reloadFunc should be called for HTTP extensions (ensures next turn gets fresh tools).
	if !reloadCalled {
		t.Fatal("reloadFunc should always be called for HTTP extensions")
	}
}

func TestAddHTTPMCPExtensionCLIModeAutoStartFailure(t *testing.T) {
	// CLI mode: autoStarter fails. Should return guidance with manage_service steps.
	path := filepath.Join(t.TempDir(), "extensions.json")
	reg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	skillReg := skills.NewRegistry()
	hr := &mockMCPHotReloader{connectErr: errors.New("connection refused")}

	autoStarter := ServiceAutoStarter(func(_ context.Context, name string, _ *ServiceRegInfo) (string, error) {
		return "", fmt.Errorf("service %q not registered", name)
	})

	reloadCalled := false
	reloadFunc := func() { reloadCalled = true }
	ss := InitExtensionSkills(reg, nil, skillReg, reloadFunc, hr, nil, nil, autoStarter)
	skill := findSkill(ss, "add_extension")

	input := `{"name":"fail-auto","type":"mcp","transport":"http","url":"http://localhost:18060/mcp","image":"test/image","ports":{"18060":"18060"}}`
	result, err := skill.Execute(context.Background(), json.RawMessage(input))
	if err != nil {
		t.Fatalf("CLI mode auto-start failure should not error, got: %v", err)
	}
	if !strings.Contains(result, "manage_service") {
		t.Fatalf("expected manage_service guidance on auto-start failure, got: %s", result)
	}
	// reloadFunc should be called as fallback.
	if !reloadCalled {
		t.Fatal("reloadFunc should be called when auto-start fails")
	}
}

func TestAddHTTPMCPExtensionCLIModeNoAutoStarter(t *testing.T) {
	// CLI mode with no autoStarter (nil). HTTP extension should get generic guidance.
	path := filepath.Join(t.TempDir(), "extensions.json")
	reg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	skillReg := skills.NewRegistry()
	hr := &mockMCPHotReloader{connectErr: errors.New("connection refused")}

	reloadCalled := false
	reloadFunc := func() { reloadCalled = true }
	// Pass nil autoStarter.
	ss := InitExtensionSkills(reg, nil, skillReg, reloadFunc, hr, nil, nil, nil)
	skill := findSkill(ss, "add_extension")

	input := `{"name":"no-auto","type":"mcp","transport":"http","url":"http://localhost:18060/mcp"}`
	result, err := skill.Execute(context.Background(), json.RawMessage(input))
	if err != nil {
		t.Fatalf("CLI mode without autoStarter should not error, got: %v", err)
	}
	if !strings.Contains(result, "not reachable") {
		t.Fatalf("expected 'not reachable' generic guidance, got: %s", result)
	}
	if !strings.Contains(result, "Start the server manually") {
		t.Fatalf("expected manual start guidance, got: %s", result)
	}
	if !reloadCalled {
		t.Fatal("reloadFunc should be called (no auto-start to defer for)")
	}
}

func TestAddHTTPMCPExtensionAutoRegisterAndStart(t *testing.T) {
	// API mode: service not in catalog, image provided → auto-register → auto-start → retry → tools available.
	path := filepath.Join(t.TempDir(), "extensions.json")
	reg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	mcpMgr := &mockMCPAdder{}
	mcpMgr.addErr = errors.New("connection refused")
	skillReg := skills.NewRegistry()

	registered := false
	autoStarter := ServiceAutoStarter(func(_ context.Context, name string, regInfo *ServiceRegInfo) (string, error) {
		if regInfo == nil || regInfo.Image == "" {
			return "", fmt.Errorf("service %q not registered", name)
		}
		registered = true
		// Simulate: registered + started, now connection succeeds.
		mcpMgr.addErr = nil
		return "registered and started", nil
	})

	ss := InitExtensionSkills(reg, mcpMgr, skillReg, nil, nil, nil, nil, autoStarter)
	skill := findSkill(ss, "add_extension")

	input := `{"name":"xhs","type":"mcp","transport":"http","url":"http://localhost:18060/mcp","image":"xpzouying/xiaohongshu-mcp","ports":{"18060":"18060"}}`
	result, err := skill.Execute(context.Background(), json.RawMessage(input))
	if err != nil {
		t.Fatalf("auto-register should succeed, got: %v", err)
	}
	if !registered {
		t.Fatal("expected auto-register to be called with regInfo")
	}
	if !strings.Contains(result, "immediately") {
		t.Fatalf("expected tools available immediately after auto-register + start, got: %s", result)
	}
	if !strings.Contains(result, "auto-started") {
		t.Fatalf("expected auto-started message, got: %s", result)
	}
}

func TestAddHTTPMCPExtensionAutoRegisterCLIMode(t *testing.T) {
	// CLI mode: service not in catalog, image provided → auto-register → start → hot-reload retry → immediate tools.
	path := filepath.Join(t.TempDir(), "extensions.json")
	reg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	skillReg := skills.NewRegistry()

	connectAttempt := 0
	hr := &mockMCPHotReloader{
		connectFunc: func() ([]string, func(), error) {
			connectAttempt++
			if connectAttempt == 1 {
				return nil, nil, errors.New("connection refused")
			}
			return []string{"xhs_search", "xhs_explore"}, nil, nil
		},
	}

	registered := false
	autoStarter := ServiceAutoStarter(func(_ context.Context, name string, regInfo *ServiceRegInfo) (string, error) {
		if regInfo != nil && regInfo.Image != "" {
			registered = true
			return "registered and started", nil
		}
		return "", fmt.Errorf("service %q not registered", name)
	})

	reloadCalled := false
	reloadFunc := func() { reloadCalled = true }
	ss := InitExtensionSkills(reg, nil, skillReg, reloadFunc, hr, nil, nil, autoStarter)
	skill := findSkill(ss, "add_extension")

	input := `{"name":"xhs-cli","type":"mcp","transport":"http","url":"http://localhost:18060/mcp","image":"xpzouying/xiaohongshu-mcp","ports":{"18060":"18060"}}`
	result, err := skill.Execute(context.Background(), json.RawMessage(input))
	if err != nil {
		t.Fatalf("CLI auto-register should succeed, got: %v", err)
	}
	if !registered {
		t.Fatal("expected auto-register with image")
	}
	if !strings.Contains(result, "immediately") {
		t.Fatalf("expected immediate tools after auto-register + hot-reload, got: %s", result)
	}
	if !reloadCalled {
		t.Fatal("reloadFunc should always be called for HTTP extensions")
	}
}

func TestAddHTTPMCPExtensionAutoRegisterFails(t *testing.T) {
	// API mode: image provided but auto-register fails (e.g. ALLOWED_IMAGES rejection).
	path := filepath.Join(t.TempDir(), "extensions.json")
	reg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	mcpMgr := &mockMCPAdder{addErr: errors.New("connection refused")}
	skillReg := skills.NewRegistry()

	autoStarter := ServiceAutoStarter(func(_ context.Context, name string, regInfo *ServiceRegInfo) (string, error) {
		return "", fmt.Errorf("auto-register service %q failed: image not allowed", name)
	})

	ss := InitExtensionSkills(reg, mcpMgr, skillReg, nil, nil, nil, nil, autoStarter)
	skill := findSkill(ss, "add_extension")

	input := `{"name":"blocked","type":"mcp","transport":"http","url":"http://localhost:18060/mcp","image":"evil/image"}`
	result, err := skill.Execute(context.Background(), json.RawMessage(input))
	if err != nil {
		t.Fatalf("register failure should return guidance not error, got: %v", err)
	}
	if !strings.Contains(result, "manage_service") {
		t.Fatalf("expected manage_service guidance after register failure, got: %s", result)
	}
}

func TestAddHTTPMCPExtensionAutoRegisterStartButUnreachable(t *testing.T) {
	// CLI mode: register + start OK but MCP still unreachable after hot-reload retry.
	path := filepath.Join(t.TempDir(), "extensions.json")
	reg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	skillReg := skills.NewRegistry()
	hr := &mockMCPHotReloader{connectErr: errors.New("connection refused")}

	autoStarter := ServiceAutoStarter(func(_ context.Context, name string, regInfo *ServiceRegInfo) (string, error) {
		return "registered and started", nil
	})

	reloadCalled := false
	reloadFunc := func() { reloadCalled = true }
	ss := InitExtensionSkills(reg, nil, skillReg, reloadFunc, hr, nil, nil, autoStarter)
	skill := findSkill(ss, "add_extension")

	input := `{"name":"unreachable-after-start","type":"mcp","transport":"http","url":"http://localhost:18060/mcp","image":"xpzouying/xiaohongshu-mcp"}`
	result, err := skill.Execute(context.Background(), json.RawMessage(input))
	if err != nil {
		t.Fatalf("should not error, got: %v", err)
	}
	// Service started but MCP still not reachable — should have companion service info.
	if !strings.Contains(result, "auto-started") {
		t.Fatalf("expected auto-started info in result, got: %s", result)
	}
	// Should NOT contain "immediately" since hot-reload retry failed.
	if strings.Contains(result, "immediately") {
		t.Fatal("should not claim immediate availability when MCP is still unreachable")
	}
	// Deferred reloadFunc should fire since auto-start didn't achieve immediate tools.
	if !reloadCalled {
		t.Fatal("reloadFunc should be called when hot-reload retry fails")
	}
}

func TestAddHTTPMCPExtensionNoImageFallsBack(t *testing.T) {
	// API mode: no image provided → existing guidance behavior (no auto-register).
	path := filepath.Join(t.TempDir(), "extensions.json")
	reg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	mcpMgr := &mockMCPAdder{addErr: errors.New("connection refused")}
	skillReg := skills.NewRegistry()

	autoStarter := ServiceAutoStarter(func(_ context.Context, name string, _ *ServiceRegInfo) (string, error) {
		// No image → service not registered error.
		return "", fmt.Errorf("service %q not registered", name)
	})

	ss := InitExtensionSkills(reg, mcpMgr, skillReg, nil, nil, nil, nil, autoStarter)
	skill := findSkill(ss, "add_extension")

	// No image field with autoStarter present — should return validation error
	// telling the bot to include the image field.
	input := `{"name":"no-image","type":"mcp","transport":"http","url":"http://localhost:18060/mcp"}`
	_, err = skill.Execute(context.Background(), json.RawMessage(input))
	if err == nil {
		t.Fatal("expected validation error for HTTP extension without image when autoStarter is present")
	}
	if !strings.Contains(err.Error(), "image") {
		t.Fatalf("expected error about missing image field, got: %v", err)
	}
}

func TestAddMCPExtensionHTTPAutoDetect(t *testing.T) {
	reg, mcpMgr, _, ss := setupTest(t)
	skill := findSkill(ss, "add_extension")

	// Pass an HTTP URL as "command" (common LLM mistake) — should auto-convert to http transport.
	input := `{"name":"auto-http","type":"mcp","command":"http://localhost:18060/mcp"}`
	result, err := skill.Execute(context.Background(), json.RawMessage(input))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "immediately") {
		t.Fatalf("expected immediate availability, got: %s", result)
	}

	// Verify the MCPServerConfig got transport=http and url, not command.
	if len(mcpMgr.added) != 1 {
		t.Fatalf("expected 1 AddServer call, got %d", len(mcpMgr.added))
	}
	cfg := mcpMgr.added[0]
	if cfg.Transport != "http" {
		t.Errorf("expected auto-detected transport=http, got %q", cfg.Transport)
	}
	if cfg.URL != "http://localhost:18060/mcp" {
		t.Errorf("expected url from command, got %q", cfg.URL)
	}
	if cfg.Command != "" {
		t.Errorf("expected empty command after auto-detect, got %q", cfg.Command)
	}

	// Verify persistence has correct fields.
	got := reg.Get("auto-http")
	if got == nil {
		t.Fatal("expected extension to persist")
	}
	if got.Transport != "http" {
		t.Errorf("persisted transport = %q, want http", got.Transport)
	}
	if got.URL != "http://localhost:18060/mcp" {
		t.Errorf("persisted url = %q", got.URL)
	}
}
