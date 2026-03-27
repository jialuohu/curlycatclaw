package mcp

import (
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
