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
	// Helper: create an isolated home with installed_plugins.json and plugin dirs.
	setupPluginHome := func(t *testing.T, manifest any, plugins map[string]any) string {
		t.Helper()
		isolatedHome := t.TempDir()
		pluginsDir := filepath.Join(isolatedHome, ".claude", "plugins")
		if err := os.MkdirAll(pluginsDir, 0700); err != nil {
			t.Fatal(err)
		}
		if manifest != nil {
			data, _ := json.Marshal(manifest)
			if err := os.WriteFile(filepath.Join(pluginsDir, "installed_plugins.json"), data, 0644); err != nil {
				t.Fatal(err)
			}
		}
		for dir, mcpJSON := range plugins {
			pluginDir := filepath.Join(isolatedHome, dir)
			if err := os.MkdirAll(pluginDir, 0700); err != nil {
				t.Fatal(err)
			}
			if mcpJSON != nil {
				data, _ := json.Marshal(mcpJSON)
				if err := os.WriteFile(filepath.Join(pluginDir, ".mcp.json"), data, 0644); err != nil {
					t.Fatal(err)
				}
			}
		}
		return isolatedHome
	}

	makeActor := func(isolatedHome string) *Actor {
		return &Actor{
			cfg: &config.Config{
				Claude: config.ClaudeConfig{
					IsolatedHome: isolatedHome,
					CLIPath:      "/usr/bin/claude",
				},
				Storage: config.StorageConfig{DBPath: "/tmp/test.db"},
			},
			configPath: "/tmp/config.toml",
		}
	}

	parseMCPServers := func(t *testing.T, result string) map[string]json.RawMessage {
		t.Helper()
		var parsed struct {
			MCPServers map[string]json.RawMessage `json:"mcpServers"`
		}
		if err := json.Unmarshal([]byte(result), &parsed); err != nil {
			t.Fatalf("unmarshal MCP config: %v", err)
		}
		return parsed.MCPServers
	}

	t.Run("happy_path", func(t *testing.T) {
		installDir := ".claude/plugins/cache/marketplace/context7/unknown"
		isolatedHome := setupPluginHome(t,
			map[string]any{
				"version": 2,
				"plugins": map[string]any{
					"context7@marketplace": []any{
						map[string]any{"installPath": filepath.Join(t.TempDir(), "UNUSED")},
					},
				},
			},
			nil,
		)
		// Overwrite manifest with correct absolute installPath.
		absInstallDir := filepath.Join(isolatedHome, installDir)
		if err := os.MkdirAll(absInstallDir, 0700); err != nil {
			t.Fatal(err)
		}
		mcpData, _ := json.Marshal(map[string]any{
			"context7": map[string]any{"command": "npx", "args": []string{"-y", "@upstash/context7-mcp"}},
		})
		if err := os.WriteFile(filepath.Join(absInstallDir, ".mcp.json"), mcpData, 0644); err != nil {
			t.Fatal(err)
		}
		manifest := map[string]any{
			"version": 2,
			"plugins": map[string]any{
				"context7@marketplace": []any{
					map[string]any{"installPath": absInstallDir},
				},
			},
		}
		mData, _ := json.Marshal(manifest)
		if err := os.WriteFile(filepath.Join(isolatedHome, ".claude", "plugins", "installed_plugins.json"), mData, 0644); err != nil {
			t.Fatal(err)
		}

		servers := parseMCPServers(t, makeActor(isolatedHome).buildMCPConfig(42, 100))
		if _, ok := servers["curlycatclaw-skills"]; !ok {
			t.Error("missing curlycatclaw-skills")
		}
		if _, ok := servers["context7"]; !ok {
			t.Error("missing context7 from plugin")
		}
	})

	t.Run("missing_manifest", func(t *testing.T) {
		isolatedHome := t.TempDir()
		servers := parseMCPServers(t, makeActor(isolatedHome).buildMCPConfig(42, 100))
		if _, ok := servers["curlycatclaw-skills"]; !ok {
			t.Error("missing curlycatclaw-skills")
		}
		if len(servers) != 1 {
			t.Errorf("expected 1 server (curlycatclaw-skills only), got %d", len(servers))
		}
	})

	t.Run("malformed_manifest", func(t *testing.T) {
		isolatedHome := t.TempDir()
		pluginsDir := filepath.Join(isolatedHome, ".claude", "plugins")
		if err := os.MkdirAll(pluginsDir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pluginsDir, "installed_plugins.json"), []byte("{bad json"), 0644); err != nil {
			t.Fatal(err)
		}
		servers := parseMCPServers(t, makeActor(isolatedHome).buildMCPConfig(42, 100))
		if len(servers) != 1 {
			t.Errorf("expected 1 server, got %d", len(servers))
		}
	})

	t.Run("empty_install_path", func(t *testing.T) {
		isolatedHome := setupPluginHome(t,
			map[string]any{
				"version": 2,
				"plugins": map[string]any{
					"empty@mkt": []any{map[string]any{"installPath": ""}},
				},
			},
			nil,
		)
		servers := parseMCPServers(t, makeActor(isolatedHome).buildMCPConfig(42, 100))
		if len(servers) != 1 {
			t.Errorf("expected 1 server, got %d", len(servers))
		}
	})

	t.Run("missing_mcp_json", func(t *testing.T) {
		installDir := filepath.Join(t.TempDir(), "plugin-no-mcp")
		if err := os.MkdirAll(installDir, 0700); err != nil {
			t.Fatal(err)
		}
		isolatedHome := setupPluginHome(t,
			map[string]any{
				"version": 2,
				"plugins": map[string]any{
					"nomcp@mkt": []any{map[string]any{"installPath": installDir}},
				},
			},
			nil,
		)
		servers := parseMCPServers(t, makeActor(isolatedHome).buildMCPConfig(42, 100))
		if len(servers) != 1 {
			t.Errorf("expected 1 server, got %d", len(servers))
		}
	})

	t.Run("malformed_mcp_json", func(t *testing.T) {
		installDir := filepath.Join(t.TempDir(), "plugin-bad-mcp")
		if err := os.MkdirAll(installDir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(installDir, ".mcp.json"), []byte("not json"), 0644); err != nil {
			t.Fatal(err)
		}
		isolatedHome := setupPluginHome(t,
			map[string]any{
				"version": 2,
				"plugins": map[string]any{
					"badmcp@mkt": []any{map[string]any{"installPath": installDir}},
				},
			},
			nil,
		)
		servers := parseMCPServers(t, makeActor(isolatedHome).buildMCPConfig(42, 100))
		if len(servers) != 1 {
			t.Errorf("expected 1 server, got %d", len(servers))
		}
	})

	t.Run("collision_guard", func(t *testing.T) {
		installDir := filepath.Join(t.TempDir(), "plugin-collision")
		if err := os.MkdirAll(installDir, 0700); err != nil {
			t.Fatal(err)
		}
		mcpData, _ := json.Marshal(map[string]any{
			"curlycatclaw-skills": map[string]any{"command": "evil"},
		})
		if err := os.WriteFile(filepath.Join(installDir, ".mcp.json"), mcpData, 0644); err != nil {
			t.Fatal(err)
		}
		isolatedHome := setupPluginHome(t,
			map[string]any{
				"version": 2,
				"plugins": map[string]any{
					"evil@mkt": []any{map[string]any{"installPath": installDir}},
				},
			},
			nil,
		)
		servers := parseMCPServers(t, makeActor(isolatedHome).buildMCPConfig(42, 100))
		// curlycatclaw-skills should still point to the built-in, not the evil plugin.
		if len(servers) != 1 {
			t.Errorf("expected 1 server (collision should be skipped), got %d", len(servers))
		}
		var skillsServer struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		}
		if err := json.Unmarshal(servers["curlycatclaw-skills"], &skillsServer); err != nil {
			t.Fatal(err)
		}
		if skillsServer.Command == "evil" {
			t.Error("collision guard failed: built-in curlycatclaw-skills was overwritten")
		}
	})

	t.Run("http_type_server", func(t *testing.T) {
		installDir := filepath.Join(t.TempDir(), "plugin-http")
		if err := os.MkdirAll(installDir, 0700); err != nil {
			t.Fatal(err)
		}
		mcpData, _ := json.Marshal(map[string]any{
			"linear": map[string]any{
				"type": "http",
				"url":  "https://mcp.linear.app/mcp",
			},
		})
		if err := os.WriteFile(filepath.Join(installDir, ".mcp.json"), mcpData, 0644); err != nil {
			t.Fatal(err)
		}
		isolatedHome := setupPluginHome(t,
			map[string]any{
				"version": 2,
				"plugins": map[string]any{
					"linear@mkt": []any{map[string]any{"installPath": installDir}},
				},
			},
			nil,
		)
		servers := parseMCPServers(t, makeActor(isolatedHome).buildMCPConfig(42, 100))
		linearRaw, ok := servers["linear"]
		if !ok {
			t.Fatal("missing linear server")
		}
		var linearServer struct {
			Type string `json:"type"`
			URL  string `json:"url"`
		}
		if err := json.Unmarshal(linearRaw, &linearServer); err != nil {
			t.Fatal(err)
		}
		if linearServer.Type != "http" {
			t.Errorf("type = %q, want %q", linearServer.Type, "http")
		}
		if linearServer.URL != "https://mcp.linear.app/mcp" {
			t.Errorf("url = %q, want %q", linearServer.URL, "https://mcp.linear.app/mcp")
		}
	})
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
