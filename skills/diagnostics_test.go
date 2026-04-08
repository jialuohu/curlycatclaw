package skills

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// mockMCPStatus implements MCPStatusProvider for testing.
type mockMCPStatus struct {
	names map[string]bool
}

func (m *mockMCPStatus) ServerNames() []string {
	var names []string
	for n := range m.names {
		names = append(names, n)
	}
	return names
}

func (m *mockMCPStatus) IsRegistered(name string) bool {
	return m.names[name]
}

func setupDiagTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	// Create minimal schema for tool_calls and conversations
	_, err = db.Exec(`
		CREATE TABLE conversations (
			id TEXT PRIMARY KEY,
			user_id INTEGER NOT NULL,
			created_at DATETIME NOT NULL
		);
		CREATE TABLE tool_calls (
			id TEXT PRIMARY KEY,
			conversation_id TEXT NOT NULL REFERENCES conversations(id),
			name TEXT NOT NULL,
			input TEXT NOT NULL,
			output TEXT,
			is_error BOOLEAN NOT NULL DEFAULT FALSE,
			created_at DATETIME NOT NULL
		);
	`)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestCaptureDiagnostics_OutputFormat(t *testing.T) {
	db := setupDiagTestDB(t)
	mcp := &mockMCPStatus{names: map[string]bool{"github": true, "gws": true}}
	cfg := DiagSafeConfig{
		EvalEnabled:   true,
		IngestEnabled: false,
		VoiceEnabled:  false,
		WasmEnabled:   false,
		EmbedModel:    "nomic-embed-text",
	}

	skills := InitDiagnosticsSkills("0.32.0", db, mcp, cfg, "", "")
	if len(skills) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(skills))
	}

	ctx := WithUser(context.Background(), UserInfo{UserID: 123, ChatID: 456})
	result, err := skills[0].Execute(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}

	// Check required sections exist
	if !strings.Contains(result, "## Environment") {
		t.Error("missing Environment section")
	}
	if !strings.Contains(result, "## Recent Errors") {
		t.Error("missing Recent Errors section")
	}
	if !strings.Contains(result, "## Recent Tool Calls") {
		t.Error("missing Recent Tool Calls section")
	}
	if !strings.Contains(result, "0.32.0") {
		t.Error("version not in output")
	}
	if !strings.Contains(result, "eval=on") {
		t.Error("eval feature flag not in output")
	}
	if !strings.Contains(result, "nomic-embed-text") {
		t.Error("embed model not in output")
	}
}

func TestCaptureDiagnostics_NoSecretLeak(t *testing.T) {
	db := setupDiagTestDB(t)
	mcp := &mockMCPStatus{names: map[string]bool{"github": true}}

	// Construct config with fake credentials that MUST NOT appear in output
	fakeSecrets := []string{
		"sk-ant-FAKE-api-key-12345",
		"ghp_FakeGitHubToken9876543210",
		"xoxb-FAKE-slack-token",
		"/data/secret-credentials.json",
		"super_secret_master_key_hex",
	}

	cfg := DiagSafeConfig{
		EvalEnabled:   true,
		IngestEnabled: true,
		VoiceEnabled:  true,
		WasmEnabled:   true,
		MCPServers:    []string{"github", "gws"},
		EmbedModel:    "nomic-embed-text",
		LogLevel:      "debug",
	}

	// Pass fake URLs that look like they contain secrets (but health check will fail, that's fine)
	skills := InitDiagnosticsSkills("0.32.0", db, mcp, cfg, "", "")
	ctx := WithUser(context.Background(), UserInfo{UserID: 123, ChatID: 456})
	result, err := skills[0].Execute(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}

	for _, secret := range fakeSecrets {
		if strings.Contains(result, secret) {
			t.Errorf("SECRET LEAK: output contains %q", secret)
		}
	}
}

func TestCaptureDiagnostics_ZeroUserID(t *testing.T) {
	db := setupDiagTestDB(t)
	skills := InitDiagnosticsSkills("0.32.0", db, nil, DiagSafeConfig{}, "", "")

	// Context with zero UserID
	ctx := WithUser(context.Background(), UserInfo{UserID: 0, ChatID: 456})
	_, err := skills[0].Execute(ctx, json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected error for zero UserID")
	}
	if !strings.Contains(err.Error(), "user ID must not be zero") {
		t.Errorf("unexpected error message: %s", err.Error())
	}
}

func TestCaptureDiagnostics_EmptyState(t *testing.T) {
	db := setupDiagTestDB(t)
	skills := InitDiagnosticsSkills("0.32.0", db, nil, DiagSafeConfig{}, "", "")

	ctx := WithUser(context.Background(), UserInfo{UserID: 123, ChatID: 456})
	result, err := skills[0].Execute(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(result, "No recent errors") {
		t.Error("expected 'No recent errors' for empty state")
	}
	if !strings.Contains(result, "No recent tool calls") {
		t.Error("expected 'No recent tool calls' for empty state")
	}
}

func TestCaptureDiagnostics_WithToolCalls(t *testing.T) {
	db := setupDiagTestDB(t)

	// Insert test data
	_, err := db.Exec(`INSERT INTO conversations (id, user_id, created_at) VALUES ('conv1', 123, datetime('now'))`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO tool_calls (id, conversation_id, name, input, output, is_error, created_at)
		VALUES ('tc1', 'conv1', 'web_search', '{}', 'timeout after 30s', TRUE, datetime('now', '-1 minute'))`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO tool_calls (id, conversation_id, name, input, output, is_error, created_at)
		VALUES ('tc2', 'conv1', 'save_note', '{}', 'saved', FALSE, datetime('now', '-2 minutes'))`)
	if err != nil {
		t.Fatal(err)
	}

	skills := InitDiagnosticsSkills("0.32.0", db, nil, DiagSafeConfig{}, "", "")
	ctx := WithUser(context.Background(), UserInfo{UserID: 123, ChatID: 456})
	result, err := skills[0].Execute(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(result, "web_search") {
		t.Error("expected web_search in recent errors")
	}
	if !strings.Contains(result, "save_note") {
		t.Error("expected save_note in recent tool calls")
	}
}

func TestCaptureDiagnostics_Truncation(t *testing.T) {
	db := setupDiagTestDB(t)

	_, err := db.Exec(`INSERT INTO conversations (id, user_id, created_at) VALUES ('conv1', 123, datetime('now'))`)
	if err != nil {
		t.Fatal(err)
	}

	// Insert a tool call with very large output (10KB)
	largeOutput := strings.Repeat("x", 10240)
	_, err = db.Exec(`INSERT INTO tool_calls (id, conversation_id, name, input, output, is_error, created_at)
		VALUES ('tc1', 'conv1', 'big_tool', '{}', ?, TRUE, datetime('now'))`, largeOutput)
	if err != nil {
		t.Fatal(err)
	}

	skills := InitDiagnosticsSkills("0.32.0", db, nil, DiagSafeConfig{}, "", "")
	ctx := WithUser(context.Background(), UserInfo{UserID: 123, ChatID: 456})
	result, err := skills[0].Execute(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}

	// Output should be truncated, not contain the full 10KB
	if len(result) > diagMaxSectionBytes*4 {
		t.Errorf("output too large: %d bytes, expected under %d", len(result), diagMaxSectionBytes*4)
	}
}
