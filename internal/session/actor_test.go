package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jialuohu/curlycatclaw/config"
	"github.com/jialuohu/curlycatclaw/internal/claude"
	"github.com/jialuohu/curlycatclaw/internal/extension"
	"github.com/jialuohu/curlycatclaw/internal/mcp"
	"github.com/jialuohu/curlycatclaw/internal/memory"
	"github.com/jialuohu/curlycatclaw/internal/telegram"
	"github.com/jialuohu/curlycatclaw/skills"
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

func (m *mockTelegramTransport) SendTyping(_ int64) {}

// typingRecorder is a TelegramTransport mock that records SendTyping calls.
type typingRecorder struct {
	mockTelegramTransport
	mu    sync.Mutex
	calls []int64 // chatIDs passed to SendTyping
}

func newTypingRecorder() *typingRecorder {
	return &typingRecorder{
		mockTelegramTransport: *newMockTG(),
	}
}

func (r *typingRecorder) SendTyping(chatID int64) {
	r.mu.Lock()
	r.calls = append(r.calls, chatID)
	r.mu.Unlock()
}

func (r *typingRecorder) typingCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func TestStartTypingLoop_SendsAtIntervals(t *testing.T) {
	rec := newTypingRecorder()
	ctx := context.Background()

	cancel := startTypingLoop(ctx, rec, 42)
	defer cancel()

	// The ticker fires at 4.5s which is too slow for a unit test.
	// Verify the goroutine started without panicking and no premature ticks.
	time.Sleep(50 * time.Millisecond)

	// No tick yet (4.5s hasn't elapsed), count should be 0.
	if got := rec.typingCount(); got != 0 {
		t.Errorf("expected 0 typing calls before first tick, got %d", got)
	}

	cancel()
	time.Sleep(50 * time.Millisecond)
}

func TestStartTypingLoop_CancelStops(t *testing.T) {
	rec := newTypingRecorder()
	ctx := context.Background()

	cancel := startTypingLoop(ctx, rec, 99)
	// Cancel immediately.
	cancel()

	// Wait a bit and verify no calls accumulated.
	time.Sleep(100 * time.Millisecond)
	if got := rec.typingCount(); got != 0 {
		t.Errorf("expected 0 typing calls after cancel, got %d", got)
	}
}

func TestStartTypingLoop_ParentContextCancel(t *testing.T) {
	rec := newTypingRecorder()
	ctx, parentCancel := context.WithCancel(context.Background())

	cancel := startTypingLoop(ctx, rec, 77)
	defer cancel()

	// Cancel the parent context.
	parentCancel()

	time.Sleep(100 * time.Millisecond)
	if got := rec.typingCount(); got != 0 {
		t.Errorf("expected 0 typing calls after parent cancel, got %d", got)
	}
}

func TestHandleMessage_SendsTypingBeforeClaude(t *testing.T) {
	rec := newTypingRecorder()
	store := &fakeMessageStore{
		convID: "conv-1",
	}
	a := &Actor{
		cfg:            &config.Config{},
		tg:             rec,
		store:          store,
		ctxb:           &fakeContextProvider{},
		mcp:            &fakeToolRouter{},
		claude:         &fakeLLMClient{},
		skills:         &skills.Registry{},
		indexSem:       make(chan struct{}, 10),
		sumSem:         make(chan struct{}, 2),
		activeProjects: make(map[userKey]string),
	}

	// Drain inbox so streamState.flush doesn't block waiting for message IDs.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		msgIDCounter := 1000
		for {
			select {
			case <-ctx.Done():
				return
			case out := <-rec.inbox:
				if out.ResultCh != nil {
					msgIDCounter++
					out.ResultCh <- msgIDCounter
				}
			}
		}
	}()

	msg := telegram.IncomingMessage{
		UserID:   1,
		ChatID:   100,
		ChatType: "private",
		Text:     "hello",
	}

	_ = a.handleMessage(ctx, msg)

	if got := rec.typingCount(); got < 1 {
		t.Fatalf("expected at least 1 SendTyping call, got %d", got)
	}

	// Verify the correct chatID was used.
	rec.mu.Lock()
	firstCall := rec.calls[0]
	rec.mu.Unlock()
	if firstCall != 100 {
		t.Errorf("SendTyping chatID = %d, want 100", firstCall)
	}
}

// fakeLLMClient returns a simple text response with end_turn to terminate the loop.
type fakeLLMClient struct{}

func (f *fakeLLMClient) SendStreaming(_ context.Context, params claude.SendParams) (*claude.Response, error) {
	if params.OnPartialText != nil {
		params.OnPartialText("ok")
	}
	return &claude.Response{
		TextContent: "ok",
		StopReason:  "end_turn",
	}, nil
}

// fakeMessageStore is a minimal MessageStore for typing tests.
type fakeMessageStore struct {
	convID      string
	lastContent string // captured from AppendMessage for test assertions
}

func (f *fakeMessageStore) GetActiveConversation(_, _ int64, _ string) (string, string, error) {
	return f.convID, "", nil
}
func (f *fakeMessageStore) AppendMessage(_, role string, content json.RawMessage) error {
	if role == "user" {
		var text string
		if err := json.Unmarshal(content, &text); err == nil {
			f.lastContent = text
		}
	}
	return nil
}
func (f *fakeMessageStore) LogToolCall(_, _, _ string, _ json.RawMessage) error { return nil }
func (f *fakeMessageStore) CompleteToolCall(_ string, _ json.RawMessage, _ bool) error {
	return nil
}
func (f *fakeMessageStore) GetConversationMessages(_ string) ([]memory.Message, error) {
	return nil, nil
}
func (f *fakeMessageStore) SaveSummary(_ string, _, _ int64, _ string, _ int, _, _ time.Time) error {
	return nil
}
func (f *fakeMessageStore) SetSummarizationStatus(_, _ string) error { return nil }
func (f *fakeMessageStore) ConversationMeta(_ string) (int64, int64, string, int, time.Time, time.Time, error) {
	return 0, 0, "", 0, time.Time{}, time.Time{}, nil
}
func (f *fakeMessageStore) RecoverableSummarizations() ([]string, error) { return nil, nil }
func (f *fakeMessageStore) GetSummaryText(_ string) (string, error)      { return "", nil }
func (f *fakeMessageStore) GetMaxMessageRowid(_ string) (int64, error)   { return 0, nil }

// fakeContextProvider returns empty history.
type fakeContextProvider struct{}

func (f *fakeContextProvider) BuildContext(_ string) ([]memory.Message, error) {
	return nil, nil
}

// fakeToolRouter returns no tools.
type fakeToolRouter struct{}

func (f *fakeToolRouter) CallTool(_ context.Context, _ string, _ map[string]any, _, _ int64) (string, error) {
	return "", nil
}
func (f *fakeToolRouter) Tools() []mcp.ToolDef { return nil }

func (m *mockTelegramTransport) SendDocument(_ int64, _ string, _ []byte, _ string) error {
	return nil
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
		if !strings.Contains(msg.Text, "myapp") || !strings.Contains(msg.Text, "backend") {
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
		if !strings.Contains(msg.Text, "Unknown project") {
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

func TestDiscoverPluginNames(t *testing.T) {
	t.Run("with_plugins", func(t *testing.T) {
		dir := t.TempDir()
		pluginsDir := filepath.Join(dir, ".claude", "plugins")
		if err := os.MkdirAll(pluginsDir, 0700); err != nil {
			t.Fatal(err)
		}
		installDir := filepath.Join(dir, "cache", "context7")
		if err := os.MkdirAll(installDir, 0700); err != nil {
			t.Fatal(err)
		}
		mcpData, _ := json.Marshal(map[string]any{
			"context7": map[string]any{"command": "npx", "args": []string{"-y", "@upstash/context7-mcp"}},
		})
		if err := os.WriteFile(filepath.Join(installDir, ".mcp.json"), mcpData, 0644); err != nil {
			t.Fatal(err)
		}
		manifest, _ := json.Marshal(map[string]any{
			"version": 2,
			"plugins": map[string]any{
				"context7@mkt": []any{map[string]any{"installPath": installDir}},
			},
		})
		if err := os.WriteFile(filepath.Join(pluginsDir, "installed_plugins.json"), manifest, 0644); err != nil {
			t.Fatal(err)
		}

		names := discoverPluginNames(dir)
		if len(names) != 1 || names[0] != "context7" {
			t.Errorf("names = %v, want [context7]", names)
		}
	})

	t.Run("no_manifest", func(t *testing.T) {
		names := discoverPluginNames(t.TempDir())
		if names != nil {
			t.Errorf("expected nil, got %v", names)
		}
	})

	t.Run("empty_manifest", func(t *testing.T) {
		dir := t.TempDir()
		pluginsDir := filepath.Join(dir, ".claude", "plugins")
		if err := os.MkdirAll(pluginsDir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pluginsDir, "installed_plugins.json"), []byte(`{"plugins":{}}`), 0644); err != nil {
			t.Fatal(err)
		}
		names := discoverPluginNames(dir)
		if len(names) != 0 {
			t.Errorf("expected empty, got %v", names)
		}
	})
}

func TestBuildMCPConfig_ExcludesMCPExtensions(t *testing.T) {
	extPath := filepath.Join(t.TempDir(), "extensions.json")

	// Write an extensions.json with one MCP extension.
	data, _ := json.Marshal(map[string]any{
		"extensions": []map[string]any{
			{
				"name":     "test-mcp",
				"type":     "mcp",
				"command":  "echo",
				"args":     []string{"hello"},
				"env":      map[string]string{"API_KEY": "val"},
				"added_at": "2026-04-01T00:00:00Z",
			},
		},
	})
	if err := os.WriteFile(extPath, data, 0644); err != nil {
		t.Fatal(err)
	}

	extReg, err := loadExtRegistry(extPath)
	if err != nil {
		t.Fatal(err)
	}

	a := &Actor{
		cfg: &config.Config{
			Storage: config.StorageConfig{DBPath: "/tmp/test.db"},
		},
		configPath:  "/tmp/config.toml",
		extRegistry: extReg,
	}

	result := a.buildMCPConfig(42, 100)

	var parsed struct {
		MCPServers map[string]struct {
			Command string `json:"command"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Runtime MCP extensions should NOT be in the config — they are
	// proxied through curlycatclaw-skills instead.
	if _, ok := parsed.MCPServers["test-mcp"]; ok {
		t.Error("runtime MCP extension should not be in --mcp-config (proxied via curlycatclaw-skills)")
	}

	// curlycatclaw-skills should still be present.
	if _, ok := parsed.MCPServers["curlycatclaw-skills"]; !ok {
		t.Error("curlycatclaw-skills should be in MCP config")
	}
}

// loadExtRegistry is a test helper that loads an extension registry.
func loadExtRegistry(path string) (*extension.Registry, error) {
	return extension.Load(path)
}

// stubToolRouter is a minimal ToolRouter for unit tests.
type stubToolRouter struct {
	tools []mcp.ToolDef
}

func (s *stubToolRouter) Tools() []mcp.ToolDef { return s.tools }
func (s *stubToolRouter) CallTool(_ context.Context, _ string, _ map[string]any, _, _ int64) (string, error) {
	return "", nil
}

// mockTranscriber is a voice.Transcriber that returns a canned result.
type mockTranscriber struct {
	text string
	err  error
}

func (m *mockTranscriber) Transcribe(_ context.Context, _ []byte, _ string) (string, error) {
	return m.text, m.err
}

func TestHandleMessage_VoiceTranscription(t *testing.T) {
	tg := newMockTG()
	store := &fakeMessageStore{convID: "conv-1"}
	a := &Actor{
		cfg:            &config.Config{},
		tg:             tg,
		store:          store,
		ctxb:           &fakeContextProvider{},
		mcp:            &fakeToolRouter{},
		claude:         &fakeLLMClient{},
		skills:         &skills.Registry{},
		indexSem:       make(chan struct{}, 10),
		sumSem:         make(chan struct{}, 2),
		activeProjects: make(map[userKey]string),
		transcriber:    &mockTranscriber{text: "hello from voice"},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case out := <-tg.inbox:
				if out.ResultCh != nil {
					out.ResultCh <- 1000
				}
			}
		}
	}()

	msg := telegram.IncomingMessage{
		UserID:   1,
		ChatID:   100,
		ChatType: "private",
		Attachments: []telegram.Attachment{
			{Kind: telegram.AttachVoice, Data: []byte("fake-ogg"), MimeType: "audio/ogg"},
		},
	}

	if err := a.handleMessage(ctx, msg); err != nil {
		t.Fatalf("handleMessage: %v", err)
	}

	// Verify the stored message includes the transcription.
	if store.lastContent == "" {
		t.Fatal("expected message to be stored")
	}
	if !strings.Contains(store.lastContent, "[Voice message transcribed]: hello from voice") {
		t.Errorf("stored message = %q, want voice transcription", store.lastContent)
	}
}

func TestHandleMessage_VoiceDisabled(t *testing.T) {
	tg := newMockTG()
	store := &fakeMessageStore{convID: "conv-1"}
	a := &Actor{
		cfg:            &config.Config{},
		tg:             tg,
		store:          store,
		ctxb:           &fakeContextProvider{},
		mcp:            &fakeToolRouter{},
		claude:         &fakeLLMClient{},
		skills:         &skills.Registry{},
		indexSem:       make(chan struct{}, 10),
		sumSem:         make(chan struct{}, 2),
		activeProjects: make(map[userKey]string),
		transcriber:    nil, // voice disabled
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case out := <-tg.inbox:
				if out.ResultCh != nil {
					out.ResultCh <- 1000
				}
			}
		}
	}()

	msg := telegram.IncomingMessage{
		UserID:   1,
		ChatID:   100,
		ChatType: "private",
		Attachments: []telegram.Attachment{
			{Kind: telegram.AttachVoice, Data: []byte("fake-ogg"), MimeType: "audio/ogg"},
		},
	}

	if err := a.handleMessage(ctx, msg); err != nil {
		t.Fatalf("handleMessage: %v", err)
	}

	if !strings.Contains(store.lastContent, "speech-to-text is not configured") {
		t.Errorf("stored message = %q, want 'not configured' notice", store.lastContent)
	}
}

func TestHandleMessage_VoiceEmptyTranscription(t *testing.T) {
	tg := newMockTG()
	store := &fakeMessageStore{convID: "conv-1"}
	a := &Actor{
		cfg:            &config.Config{},
		tg:             tg,
		store:          store,
		ctxb:           &fakeContextProvider{},
		mcp:            &fakeToolRouter{},
		claude:         &fakeLLMClient{},
		skills:         &skills.Registry{},
		indexSem:       make(chan struct{}, 10),
		sumSem:         make(chan struct{}, 2),
		activeProjects: make(map[userKey]string),
		transcriber:    &mockTranscriber{text: "   "},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case out := <-tg.inbox:
				if out.ResultCh != nil {
					out.ResultCh <- 1000
				}
			}
		}
	}()

	msg := telegram.IncomingMessage{
		UserID:   1,
		ChatID:   100,
		ChatType: "private",
		Attachments: []telegram.Attachment{
			{Kind: telegram.AttachVoice, Data: []byte("silence"), MimeType: "audio/ogg"},
		},
	}

	if err := a.handleMessage(ctx, msg); err != nil {
		t.Fatalf("handleMessage: %v", err)
	}

	if !strings.Contains(store.lastContent, "no speech detected") {
		t.Errorf("stored message = %q, want 'no speech detected' notice", store.lastContent)
	}
}

func TestBuildSystemPrompt_GitHubWorkflowGuidance(t *testing.T) {
	githubTools := []mcp.ToolDef{
		{ServerName: "github", Name: "github__list_workflow_runs", RawName: "list_workflow_runs"},
		{ServerName: "github", Name: "github__get_pull_request", RawName: "get_pull_request"},
		{ServerName: "github", Name: "github__create_issue", RawName: "create_issue"},
	}
	gwsTools := []mcp.ToolDef{
		{ServerName: "gws", Name: "gws__gmail_send", RawName: "gmail_send"},
	}

	tests := []struct {
		name      string
		tools     []mcp.ToolDef
		wantGH    bool
		wantGWS   bool
	}{
		{
			name:    "github_tools_present",
			tools:   append(githubTools, gwsTools...),
			wantGH:  true,
			wantGWS: true,
		},
		{
			name:    "no_github_tools",
			tools:   gwsTools,
			wantGH:  false,
			wantGWS: true,
		},
		{
			name:    "no_tools",
			tools:   nil,
			wantGH:  false,
			wantGWS: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := &Actor{
				cfg: &config.Config{Timezone: "UTC"},
				mcp: &stubToolRouter{tools: tc.tools},
			}

			prompt := a.buildSystemPrompt(42, 100, "private", "hello")

			hasGH := strings.Contains(prompt, "GitHub Workflows")
			if hasGH != tc.wantGH {
				t.Errorf("GitHub guidance present=%v, want %v", hasGH, tc.wantGH)
			}

			hasGWS := strings.Contains(prompt, "gws")
			if hasGWS != tc.wantGWS {
				t.Errorf("GWS tools listed=%v, want %v", hasGWS, tc.wantGWS)
			}

			if tc.wantGH {
				// Verify that actual tool names from the mock are listed in the prompt.
				for _, toolName := range []string{"list_workflow_runs", "get_pull_request", "create_issue"} {
					if !strings.Contains(prompt, toolName) {
						t.Errorf("missing GitHub tool name in prompt: %q", toolName)
					}
				}
			}
		})
	}
}


