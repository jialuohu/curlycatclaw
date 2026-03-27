package mcp

import (
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestSplitToolName_Valid(t *testing.T) {
	server, tool, ok := splitToolName("server__tool")
	if !ok {
		t.Fatal("expected ok=true, got false")
	}
	if server != "server" {
		t.Errorf("server: got %q, want %q", server, "server")
	}
	if tool != "tool" {
		t.Errorf("tool: got %q, want %q", tool, "tool")
	}
}

func TestSplitToolName_MultipleUnderscores(t *testing.T) {
	server, tool, ok := splitToolName("server__tool__extra")
	if !ok {
		t.Fatal("expected ok=true, got false")
	}
	if server != "server" {
		t.Errorf("server: got %q, want %q", server, "server")
	}
	if tool != "tool__extra" {
		t.Errorf("tool: got %q, want %q", tool, "tool__extra")
	}
}

func TestSplitToolName_NoSeparator(t *testing.T) {
	server, tool, ok := splitToolName("servertool")
	if ok {
		t.Fatal("expected ok=false, got true")
	}
	if server != "" {
		t.Errorf("server: got %q, want %q", server, "")
	}
	if tool != "" {
		t.Errorf("tool: got %q, want %q", tool, "")
	}
}

func TestSplitToolName_EmptyServer(t *testing.T) {
	server, tool, ok := splitToolName("__tool")
	if ok {
		t.Fatal("expected ok=false, got true")
	}
	if server != "" {
		t.Errorf("server: got %q, want %q", server, "")
	}
	if tool != "" {
		t.Errorf("tool: got %q, want %q", tool, "")
	}
}

func TestSplitToolName_EmptyTool(t *testing.T) {
	server, tool, ok := splitToolName("server__")
	if ok {
		t.Fatal("expected ok=false, got true")
	}
	if server != "" {
		t.Errorf("server: got %q, want %q", server, "")
	}
	if tool != "" {
		t.Errorf("tool: got %q, want %q", tool, "")
	}
}

func TestFormatResult_NilResult(t *testing.T) {
	got := formatResult(nil)
	if got != "" {
		t.Errorf("got %q, want %q", got, "")
	}
}

func TestFormatResult_TextContent(t *testing.T) {
	result := &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: "hello world"},
		},
	}
	got := formatResult(result)
	if got != "hello world" {
		t.Errorf("got %q, want %q", got, "hello world")
	}
}

func TestFormatResult_ErrorResult(t *testing.T) {
	result := &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: "something went wrong"},
		},
		IsError: true,
	}
	got := formatResult(result)
	// formatResult returns just the content; IsError is handled by CallTool.
	want := "something went wrong"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestNewManager(t *testing.T) {
	m := NewManager()
	if m == nil {
		t.Fatal("NewManager returned nil")
	}
	if m.servers == nil {
		t.Fatal("servers map is nil")
	}
	if len(m.servers) != 0 {
		t.Errorf("servers map length: got %d, want 0", len(m.servers))
	}
}

func TestTools_Empty(t *testing.T) {
	m := NewManager()
	tools := m.Tools()
	if len(tools) != 0 {
		t.Errorf("tools length: got %d, want 0", len(tools))
	}
}

func TestFilteredEnv_DefaultOnly(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")
	t.Setenv("CURLYCATCLAW_MASTER_KEY", "supersecret")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-secret")

	env := filteredEnv(nil)

	envMap := make(map[string]string)
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		envMap[k] = v
	}

	if envMap["PATH"] != "/usr/bin" {
		t.Errorf("PATH should pass through, got %q", envMap["PATH"])
	}
	if _, ok := envMap["CURLYCATCLAW_MASTER_KEY"]; ok {
		t.Error("CURLYCATCLAW_MASTER_KEY should NOT pass through")
	}
	if _, ok := envMap["ANTHROPIC_API_KEY"]; ok {
		t.Error("ANTHROPIC_API_KEY should NOT pass through")
	}
}

func TestFilteredEnv_WithExtra(t *testing.T) {
	t.Setenv("NODE_ENV", "production")
	t.Setenv("SECRET_TOKEN", "nope")

	env := filteredEnv([]string{"NODE_ENV"})

	envMap := make(map[string]string)
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		envMap[k] = v
	}

	if envMap["NODE_ENV"] != "production" {
		t.Errorf("NODE_ENV should pass through when in extra, got %q", envMap["NODE_ENV"])
	}
	if _, ok := envMap["SECRET_TOKEN"]; ok {
		t.Error("SECRET_TOKEN should NOT pass through")
	}
}

func TestFilteredEnv_ExplicitEnvAlwaysPresent(t *testing.T) {
	// filteredEnv only handles inheritance. Explicit srv.Env entries are
	// appended separately in startServer. This test verifies that
	// filteredEnv does not include unlisted vars, so explicit env wins.
	t.Setenv("BRAVE_API_KEY", "from-parent")

	env := filteredEnv(nil)

	for _, e := range env {
		k, _, _ := strings.Cut(e, "=")
		if k == "BRAVE_API_KEY" {
			t.Error("BRAVE_API_KEY should NOT be in filtered env (it's added via srv.Env)")
		}
	}
}
