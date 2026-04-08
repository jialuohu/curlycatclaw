package mcp

import (
	"net/http"
	"net/http/httptest"
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
	m := NewManager("test")
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
	m := NewManager("test")
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

func TestHeaderRoundTripper_InjectsHeaders(t *testing.T) {
	var gotHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rt := &headerRoundTripper{
		base:    http.DefaultTransport,
		headers: map[string]string{"X-Api-Key": "test-key-123", "X-Custom": "val"},
	}
	client := &http.Client{Transport: rt}

	req, _ := http.NewRequest("GET", srv.URL, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if got := gotHeaders.Get("X-Api-Key"); got != "test-key-123" {
		t.Errorf("X-Api-Key = %q, want %q", got, "test-key-123")
	}
	if got := gotHeaders.Get("X-Custom"); got != "val" {
		t.Errorf("X-Custom = %q, want %q", got, "val")
	}
}

func TestHeaderRoundTripper_DoesNotMutateOriginal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rt := &headerRoundTripper{
		base:    http.DefaultTransport,
		headers: map[string]string{"X-Injected": "yes"},
	}
	client := &http.Client{Transport: rt}

	req, _ := http.NewRequest("GET", srv.URL, nil)
	originalHeader := req.Header.Clone()

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// Original request header must not be modified.
	if got := req.Header.Get("X-Injected"); got != "" {
		t.Errorf("original request was mutated: X-Injected = %q, want empty", got)
	}
	if len(req.Header) != len(originalHeader) {
		t.Errorf("original header count changed: %d -> %d", len(originalHeader), len(req.Header))
	}
}

func TestHeaderRoundTripper_SkipsReservedHeaders(t *testing.T) {
	var gotHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rt := &headerRoundTripper{
		base: http.DefaultTransport,
		headers: map[string]string{
			"Content-Type":   "text/plain",  // reserved, should be skipped
			"Mcp-Session-Id": "injected-id", // reserved, should be skipped
			"X-Api-Key":      "allowed",     // not reserved, should be set
		},
	}
	client := &http.Client{Transport: rt}

	req, _ := http.NewRequest("GET", srv.URL, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if got := gotHeaders.Get("X-Api-Key"); got != "allowed" {
		t.Errorf("X-Api-Key = %q, want %q", got, "allowed")
	}
	// Content-Type should NOT be the injected value (Go sets its own default or none).
	if got := gotHeaders.Get("Content-Type"); got == "text/plain" {
		t.Error("Content-Type should not be overwritten by headerRoundTripper")
	}
	if got := gotHeaders.Get("Mcp-Session-Id"); got == "injected-id" {
		t.Error("Mcp-Session-Id should not be overwritten by headerRoundTripper")
	}
}

func TestHeaderRoundTripper_BlocksRedirects(t *testing.T) {
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Api-Key"); got != "" {
			t.Errorf("API key leaked to redirect target: %q", got)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer redirectTarget.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectTarget.URL, http.StatusFound)
	}))
	defer redirector.Close()

	rt := &headerRoundTripper{
		base:    http.DefaultTransport,
		headers: map[string]string{"X-Api-Key": "secret"},
	}
	client := &http.Client{
		Transport: rt,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	req, _ := http.NewRequest("GET", redirector.URL, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// Should get the redirect response, not follow it.
	if resp.StatusCode != http.StatusFound {
		t.Errorf("expected 302, got %d", resp.StatusCode)
	}
}

func TestServerNames_ReturnsAllNames(t *testing.T) {
	m := NewManager("test")
	m.servers["bravo"] = &serverConn{}
	m.servers["alpha"] = &serverConn{}
	m.servers["charlie"] = &serverConn{}

	names := m.ServerNames()
	if len(names) != 3 {
		t.Fatalf("got %d names, want 3", len(names))
	}
	// Should be sorted alphabetically.
	want := []string{"alpha", "bravo", "charlie"}
	for i, name := range names {
		if name != want[i] {
			t.Errorf("names[%d] = %q, want %q", i, name, want[i])
		}
	}
}

func TestServerNames_Empty(t *testing.T) {
	m := NewManager("test")

	names := m.ServerNames()
	if len(names) != 0 {
		t.Fatalf("got %d names, want 0", len(names))
	}
}

func TestIsRegistered_True(t *testing.T) {
	m := NewManager("test")
	m.servers["search"] = &serverConn{}

	if !m.IsRegistered("search") {
		t.Error("IsRegistered(search) = false, want true")
	}
}

func TestIsRegistered_False(t *testing.T) {
	m := NewManager("test")

	if m.IsRegistered("nonexistent") {
		t.Error("IsRegistered(nonexistent) = true, want false")
	}
}
