package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/jialuohu/curlycatclaw/config"
	"github.com/jialuohu/curlycatclaw/internal/telegram"
)

func TestTruncate(t *testing.T) {
	cases := []struct {
		input string
		max   int
		want  string
	}{
		{"hello", 10, "hello"},
		{"hello", 5, "hello"},
		{"hello world", 5, "hello..."},
		{"", 5, ""},
		{"ab", 1, "a..."},
	}
	for _, tc := range cases {
		got := truncate(tc.input, tc.max)
		if got != tc.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tc.input, tc.max, got, tc.want)
		}
	}
}

func TestRequiresConfirmation(t *testing.T) {
	a := &Actor{
		cfg: &config.Config{
			ConfirmTools: []string{"cancel_reminder", "filesystem__delete"},
		},
	}

	cases := []struct {
		tool string
		want bool
	}{
		{"cancel_reminder", true},
		{"filesystem__delete_file", true},
		{"web_search", false},
		{"save_note", false},
		{"", false},
	}
	for _, tc := range cases {
		got := a.requiresConfirmation(tc.tool)
		if got != tc.want {
			t.Errorf("requiresConfirmation(%q) = %v, want %v", tc.tool, got, tc.want)
		}
	}
}

func TestRequiresConfirmation_EmptyList(t *testing.T) {
	a := &Actor{cfg: &config.Config{}}

	if a.requiresConfirmation("anything") {
		t.Error("empty ConfirmTools list should never require confirmation")
	}
}

// mockTelegramTransport captures outgoing messages for test assertions.
type mockTelegramTransport struct {
	inbox   chan telegram.OutgoingMessage
	updates chan telegram.IncomingMessage
}

func newMockTG() *mockTelegramTransport {
	return &mockTelegramTransport{
		inbox:   make(chan telegram.OutgoingMessage, 16),
		updates: make(chan telegram.IncomingMessage, 16),
	}
}

func (m *mockTelegramTransport) Inbox() chan<- telegram.OutgoingMessage {
	return m.inbox
}

func (m *mockTelegramTransport) Updates() <-chan telegram.IncomingMessage {
	return m.updates
}

func TestHandleProjectCommand_List(t *testing.T) {
	tg := newMockTG()
	a := &Actor{
		cfg: &config.Config{
			Projects: []config.ProjectConfig{
				{Name: "myapp", Path: "/home/user/myapp"},
				{Name: "backend", Path: "/home/user/backend"},
			},
		},
		tg:             tg,
		activeProjects: make(map[userKey]string),
	}

	err := a.handleProjectCommand(telegram.IncomingMessage{
		UserID: 42,
		ChatID: 100,
		Text:   "/project",
	})
	if err != nil {
		t.Fatalf("handleProjectCommand: %v", err)
	}

	select {
	case msg := <-tg.inbox:
		if msg.ChatID != 100 {
			t.Errorf("ChatID = %d, want 100", msg.ChatID)
		}
		if !contains(msg.Text, "myapp") || !contains(msg.Text, "backend") {
			t.Errorf("response should list projects, got: %s", msg.Text)
		}
	default:
		t.Error("expected a message to be sent")
	}
}

func TestHandleProjectCommand_Select(t *testing.T) {
	tg := newMockTG()
	a := &Actor{
		cfg: &config.Config{
			Projects: []config.ProjectConfig{
				{Name: "myapp", Path: "/home/user/myapp"},
			},
		},
		tg:             tg,
		activeProjects: make(map[userKey]string),
	}

	err := a.handleProjectCommand(telegram.IncomingMessage{
		UserID: 42,
		ChatID: 100,
		Text:   "/project myapp",
	})
	if err != nil {
		t.Fatalf("handleProjectCommand: %v", err)
	}

	key := userKey{UserID: 42, ChatID: 100}
	a.projectsMu.RLock()
	proj := a.activeProjects[key]
	a.projectsMu.RUnlock()

	if proj != "myapp" {
		t.Errorf("active project = %q, want %q", proj, "myapp")
	}
}

func TestHandleProjectCommand_Off(t *testing.T) {
	tg := newMockTG()
	a := &Actor{
		cfg: &config.Config{
			Projects: []config.ProjectConfig{
				{Name: "myapp", Path: "/home/user/myapp"},
			},
		},
		tg:             tg,
		activeProjects: make(map[userKey]string),
	}

	key := userKey{UserID: 42, ChatID: 100}
	a.projectsMu.Lock()
	a.activeProjects[key] = "myapp"
	a.projectsMu.Unlock()

	err := a.handleProjectCommand(telegram.IncomingMessage{
		UserID: 42,
		ChatID: 100,
		Text:   "/project off",
	})
	if err != nil {
		t.Fatalf("handleProjectCommand: %v", err)
	}

	a.projectsMu.RLock()
	proj := a.activeProjects[key]
	a.projectsMu.RUnlock()

	if proj != "" {
		t.Errorf("active project should be cleared, got %q", proj)
	}
}

func TestHandleProjectCommand_UnknownProject(t *testing.T) {
	tg := newMockTG()
	a := &Actor{
		cfg: &config.Config{
			Projects: []config.ProjectConfig{
				{Name: "myapp", Path: "/home/user/myapp"},
			},
		},
		tg:             tg,
		activeProjects: make(map[userKey]string),
	}

	err := a.handleProjectCommand(telegram.IncomingMessage{
		UserID: 42,
		ChatID: 100,
		Text:   "/project nosuchproject",
	})
	if err != nil {
		t.Fatalf("handleProjectCommand: %v", err)
	}

	select {
	case msg := <-tg.inbox:
		if !contains(msg.Text, "Unknown project") {
			t.Errorf("expected unknown project message, got: %s", msg.Text)
		}
	default:
		t.Error("expected error message to be sent")
	}
}

func TestGetActiveProject(t *testing.T) {
	a := &Actor{
		cfg: &config.Config{
			Projects: []config.ProjectConfig{
				{Name: "myapp", Path: "/home/user/myapp"},
			},
		},
		activeProjects: make(map[userKey]string),
		projectsMu:     sync.RWMutex{},
	}

	// No project set.
	if proj := a.getActiveProject(42, 100); proj != nil {
		t.Errorf("expected nil when no project set, got %v", proj)
	}

	// Set a project.
	key := userKey{UserID: 42, ChatID: 100}
	a.projectsMu.Lock()
	a.activeProjects[key] = "myapp"
	a.projectsMu.Unlock()

	proj := a.getActiveProject(42, 100)
	if proj == nil {
		t.Fatal("expected non-nil project config")
	}
	if proj.Name != "myapp" {
		t.Errorf("project name = %q, want %q", proj.Name, "myapp")
	}
	if proj.Path != "/home/user/myapp" {
		t.Errorf("project path = %q, want %q", proj.Path, "/home/user/myapp")
	}
}

func TestBuildMCPConfig_IncludesPlugins(t *testing.T) {
	// Create an isolated home with a plugin MCP config.
	isolatedHome := t.TempDir()
	pluginDir := filepath.Join(isolatedHome, ".claude", "plugins", "test-plugin")
	if err := os.MkdirAll(pluginDir, 0700); err != nil {
		t.Fatal(err)
	}

	pluginMCP := map[string]any{
		"mcpServers": map[string]any{
			"my-server": map[string]any{
				"command": "npx",
				"args":    []string{"-y", "test-server"},
			},
		},
	}
	data, _ := json.Marshal(pluginMCP)
	if err := os.WriteFile(filepath.Join(pluginDir, ".mcp.json"), data, 0644); err != nil {
		t.Fatal(err)
	}

	a := &Actor{
		cfg: &config.Config{
			Claude: config.ClaudeConfig{
				IsolatedHome: isolatedHome,
				CLIPath:      "/usr/bin/claude",
			},
			Storage: config.StorageConfig{DBPath: "/tmp/test.db"},
		},
		configPath: "/tmp/config.toml",
	}

	result := a.buildMCPConfig(42, 100)

	var parsed struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("unmarshal MCP config: %v", err)
	}

	// Should have curlycatclaw-skills + test-plugin__my-server.
	if _, ok := parsed.MCPServers["curlycatclaw-skills"]; !ok {
		t.Error("missing curlycatclaw-skills server")
	}
	if _, ok := parsed.MCPServers["test-plugin__my-server"]; !ok {
		t.Error("missing test-plugin__my-server from plugin")
	}
}

func TestBuildMCPConfig_PassesIsolatedHomeEnv(t *testing.T) {
	a := &Actor{
		cfg: &config.Config{
			Claude: config.ClaudeConfig{
				IsolatedHome: "/tmp/claude-home",
				CLIPath:      "/usr/bin/claude",
			},
			Storage: config.StorageConfig{DBPath: "/tmp/test.db"},
		},
		configPath: "/tmp/config.toml",
	}

	result := a.buildMCPConfig(42, 100)

	var parsed struct {
		MCPServers map[string]struct {
			Env map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	skills, ok := parsed.MCPServers["curlycatclaw-skills"]
	if !ok {
		t.Fatal("missing curlycatclaw-skills")
	}
	if skills.Env["CURLYCATCLAW_ISOLATED_HOME"] != "/tmp/claude-home" {
		t.Errorf("CURLYCATCLAW_ISOLATED_HOME = %q, want %q", skills.Env["CURLYCATCLAW_ISOLATED_HOME"], "/tmp/claude-home")
	}
	if skills.Env["CURLYCATCLAW_CLI_PATH"] != "/usr/bin/claude" {
		t.Errorf("CURLYCATCLAW_CLI_PATH = %q, want %q", skills.Env["CURLYCATCLAW_CLI_PATH"], "/usr/bin/claude")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStr(s, substr))
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
