// Package mcp manages persistent connections to MCP (Model Context Protocol)
// servers via stdio and Streamable HTTP transports, providing tool discovery
// and invocation across all connected servers.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/jialuohu/curlycatclaw/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// sep is the delimiter used to namespace tool names: "server__tool".
const sep = "__"

// ToolDef bridges an MCP tool to a format suitable for the Claude API.
type ToolDef struct {
	ServerName  string          // MCP server that owns this tool
	Name        string          // fully qualified name: server__tool
	RawName     string          // original tool name from the MCP server
	Description string          // human-readable description
	InputSchema json.RawMessage // JSON Schema for tool input
}

// Manager manages persistent connections to one or more MCP servers.
// It provides aggregated tool discovery and routes tool calls to the
// correct server.
type Manager struct {
	servers map[string]*serverConn // name -> connection
	mu      sync.RWMutex
	version string
}

// serverConn holds the state for a single connected MCP server.
type serverConn struct {
	session *mcp.ClientSession
	config  config.MCPServerConfig
	tools   []*mcp.Tool
}

// NewManager creates a new Manager with no active connections.
// The version string is reported to MCP servers during handshake.
func NewManager(version string) *Manager {
	if version == "" {
		version = "dev"
	}
	return &Manager{
		servers: make(map[string]*serverConn),
		version: version,
	}
}

// Start launches and initialises all configured MCP servers. If a server
// fails to start, a warning is logged and the remaining servers are still
// started -- one broken server does not block the rest.
//
// envResolver is called for every environment variable value to support
// transparent credential decryption (e.g. "encrypted:ref:key_name" values).
// Pass nil to use values as-is.
func (m *Manager) Start(ctx context.Context, servers []config.MCPServerConfig, envResolver func(string) (string, error)) error {
	for _, srv := range servers {
		if err := m.startServer(ctx, srv, envResolver); err != nil {
			attrs := []any{"server", srv.Name, "error", err}
			if srv.Transport == "http" {
				attrs = append(attrs, "url", srv.URL)
			} else {
				attrs = append(attrs, "command", srv.Command)
			}
			slog.Warn("mcp: failed to start server", attrs...)
			continue
		}
		slog.Info("mcp: server started", "server", srv.Name)
	}
	return nil
}

// startServer launches a single MCP server and discovers its tools.
// It delegates to transport-specific helpers based on the config.
func (m *Manager) startServer(ctx context.Context, srv config.MCPServerConfig, envResolver func(string) (string, error)) error {
	var session *mcp.ClientSession
	var err error

	switch srv.Transport {
	case "http":
		session, err = m.startHTTPServer(ctx, srv, envResolver)
	default: // "" or "stdio"
		session, err = m.startStdioServer(ctx, srv, envResolver)
	}
	if err != nil {
		return err
	}

	// Discover all tools offered by this server using the paginated iterator.
	var tools []*mcp.Tool
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			// Non-fatal: some servers may not support tools at all.
			slog.Warn("mcp: error listing tools", "server", srv.Name, "error", err)
			break
		}
		tools = append(tools, tool)
	}

	sc := &serverConn{
		session: session,
		config:  srv,
		tools:   tools,
	}

	m.mu.Lock()
	m.servers[srv.Name] = sc
	m.mu.Unlock()

	slog.Info("mcp: discovered tools",
		"server", srv.Name,
		"count", len(tools),
	)
	return nil
}

// startStdioServer launches a local MCP server as a subprocess.
func (m *Manager) startStdioServer(ctx context.Context, srv config.MCPServerConfig, envResolver func(string) (string, error)) (*mcp.ClientSession, error) {
	env := filteredEnv(srv.EnvInherit)
	for k, v := range srv.Env {
		resolved := v
		if envResolver != nil {
			var err error
			resolved, err = envResolver(v)
			if err != nil {
				return nil, fmt.Errorf("resolve env %q: %w", k, err)
			}
		}
		env = append(env, k+"="+resolved)
	}

	cmd := exec.CommandContext(ctx, srv.Command, srv.Args...)
	cmd.Env = env

	transport := &mcp.CommandTransport{Command: cmd}
	client := mcp.NewClient(&mcp.Implementation{Name: "curlycatclaw", Version: m.version}, nil)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("connect stdio: %w", err)
	}
	return session, nil
}

// startHTTPServer connects to a remote MCP server via Streamable HTTP.
func (m *Manager) startHTTPServer(ctx context.Context, srv config.MCPServerConfig, envResolver func(string) (string, error)) (*mcp.ClientSession, error) {
	resolvedHeaders := make(map[string]string, len(srv.Headers))
	for k, v := range srv.Headers {
		resolved := v
		if envResolver != nil {
			var err error
			resolved, err = envResolver(v)
			if err != nil {
				return nil, fmt.Errorf("resolve header %q: %w", k, err)
			}
		}
		resolvedHeaders[k] = resolved
	}

	httpClient := &http.Client{
		Transport: &headerRoundTripper{
			base:    http.DefaultTransport,
			headers: resolvedHeaders,
		},
		// Prevent redirects from leaking API keys to unexpected hosts.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Timeout: 60 * time.Second,
	}

	transport := &mcp.StreamableClientTransport{
		Endpoint:             srv.URL,
		HTTPClient:           httpClient,
		DisableStandaloneSSE: true,
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "curlycatclaw", Version: m.version}, nil)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("connect http %q: %w", srv.URL, err)
	}

	return session, nil
}

// reservedHTTPHeaders must not be overwritten by user-configured headers
// because the SDK manages them internally.
var reservedHTTPHeaders = map[string]struct{}{
	"content-type":   {},
	"accept":         {},
	"mcp-session-id": {},
}

// headerRoundTripper injects static headers into every HTTP request.
// It clones the request before mutating to satisfy the http.RoundTripper contract.
type headerRoundTripper struct {
	base    http.RoundTripper
	headers map[string]string
}

func (h *headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	for k, v := range h.headers {
		if _, reserved := reservedHTTPHeaders[strings.ToLower(k)]; reserved {
			continue
		}
		clone.Header.Set(k, v)
	}
	return h.base.RoundTrip(clone)
}

// Tools returns all available tools across every connected server. Each tool
// name is prefixed with its server name and the separator ("__") to avoid
// collisions. For example, server "search" with tool "web_search" becomes
// "search__web_search".
func (m *Manager) Tools() []ToolDef {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var defs []ToolDef
	for serverName, sc := range m.servers {
		for _, t := range sc.tools {
			schema, err := json.Marshal(t.InputSchema)
			if err != nil {
				slog.Warn("mcp: failed to marshal input schema",
					"server", serverName,
					"tool", t.Name,
					"error", err,
				)
				schema = []byte(`{"type":"object"}`)
			}

			defs = append(defs, ToolDef{
				ServerName:  serverName,
				Name:        serverName + sep + t.Name,
				RawName:     t.Name,
				Description: t.Description,
				InputSchema: schema,
			})
		}
	}
	return defs
}

// CallTool routes a tool call to the correct MCP server and returns the
// result as a string. The serverTool argument must be a fully qualified name
// produced by Tools (e.g. "search__web_search"). When userID is non-zero,
// a _user_context map is injected into the arguments so MCP servers can
// implement per-user access control.
func (m *Manager) CallTool(ctx context.Context, serverTool string, arguments map[string]any, userID, chatID int64) (string, error) {
	// Clone arguments to avoid mutating the caller's map.
	args := make(map[string]any, len(arguments)+1)
	for k, v := range arguments {
		args[k] = v
	}
	if userID != 0 {
		args["_user_context"] = map[string]any{
			"user_id": userID,
			"chat_id": chatID,
		}
	}
	arguments = args
	serverName, rawTool, ok := splitToolName(serverTool)
	if !ok {
		return "", fmt.Errorf("mcp: invalid tool name %q (expected server%stool)", serverTool, sep)
	}

	m.mu.RLock()
	sc, exists := m.servers[serverName]
	m.mu.RUnlock()

	if !exists {
		return "", fmt.Errorf("mcp: unknown server %q", serverName)
	}

	result, err := sc.session.CallTool(ctx, &mcp.CallToolParams{
		Name:      rawTool,
		Arguments: arguments,
	})
	if err != nil {
		return "", fmt.Errorf("mcp: call %q on server %q: %w", rawTool, serverName, err)
	}

	formatted := formatResult(result)
	if result.IsError {
		return "", fmt.Errorf("%s", formatted)
	}
	return formatted, nil
}

// AddServer dynamically starts a single MCP server and adds it to the
// manager. This enables runtime addition of MCP servers without restart.
// Pass nil envResolver when env values are already resolved (e.g. runtime
// extensions with plaintext env vars).
func (m *Manager) AddServer(ctx context.Context, cfg config.MCPServerConfig, envResolver func(string) (string, error)) error {
	m.mu.Lock()
	_, exists := m.servers[cfg.Name]
	if exists {
		m.mu.Unlock()
		return fmt.Errorf("mcp: server %q already exists", cfg.Name)
	}
	// Reserve the name to prevent concurrent AddServer races.
	m.servers[cfg.Name] = nil
	m.mu.Unlock()

	if err := m.startServer(ctx, cfg, envResolver); err != nil {
		// Remove the reservation on failure.
		m.mu.Lock()
		if m.servers[cfg.Name] == nil {
			delete(m.servers, cfg.Name)
		}
		m.mu.Unlock()
		return err
	}
	slog.Info("mcp: server added dynamically", "server", cfg.Name)
	return nil
}

// RemoveServer stops and removes a single MCP server by name.
func (m *Manager) RemoveServer(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	sc, exists := m.servers[name]
	if !exists {
		return fmt.Errorf("mcp: unknown server %q", name)
	}
	if err := sc.session.Close(); err != nil {
		slog.Warn("mcp: error closing server during removal", "server", name, "error", err)
	}
	delete(m.servers, name)
	slog.Info("mcp: server removed", "server", name)
	return nil
}

// Shutdown gracefully closes all MCP server connections concurrently.
// Each server gets a per-server timeout to prevent one hung connection
// (e.g. an HTTP DELETE that never completes) from blocking the rest.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()

	var wg sync.WaitGroup
	for name, sc := range m.servers {
		if sc == nil {
			continue // AddServer reservation in progress
		}
		wg.Add(1)
		go func(name string, sc *serverConn) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			done := make(chan error, 1)
			go func() { done <- sc.session.Close() }()

			select {
			case err := <-done:
				if err != nil {
					slog.Warn("mcp: error closing server", "server", name, "error", err)
				} else {
					slog.Info("mcp: server stopped", "server", name)
				}
			case <-ctx.Done():
				slog.Warn("mcp: timeout closing server", "server", name)
			}
		}(name, sc)
	}
	wg.Wait()
	// Clear the map so a subsequent Shutdown is a no-op.
	m.servers = make(map[string]*serverConn)
}

// defaultEnvAllowlist is the minimum set of environment variables that MCP
// child processes need to function (find binaries, set locale, etc.).
var defaultEnvAllowlist = map[string]struct{}{
	"PATH": {}, "HOME": {}, "USER": {}, "LANG": {}, "LC_ALL": {},
	"SHELL": {}, "TMPDIR": {}, "TZ": {}, "XDG_RUNTIME_DIR": {},
}

// filteredEnv returns a copy of the current environment containing only
// variables on the default allowlist plus any extras. This prevents secrets
// like CURLYCATCLAW_MASTER_KEY from leaking to MCP child processes.
func filteredEnv(extra []string) []string {
	allow := make(map[string]struct{}, len(defaultEnvAllowlist)+len(extra))
	for k, v := range defaultEnvAllowlist {
		allow[k] = v
	}
	for _, k := range extra {
		allow[k] = struct{}{}
	}
	var out []string
	for _, entry := range os.Environ() {
		if k, _, ok := strings.Cut(entry, "="); ok {
			if _, pass := allow[k]; pass {
				out = append(out, entry)
			}
		}
	}
	return out
}

// splitToolName splits "server__tool" into ("server", "tool", true).
// Returns ("", "", false) when the separator is absent or in an invalid
// position.
func splitToolName(qualified string) (server, tool string, ok bool) {
	idx := strings.Index(qualified, sep)
	if idx <= 0 || idx+len(sep) >= len(qualified) {
		return "", "", false
	}
	return qualified[:idx], qualified[idx+len(sep):], true
}

// formatResult converts a CallToolResult into a single string suitable for
// inclusion in an LLM conversation turn.
func formatResult(result *mcp.CallToolResult) string {
	if result == nil {
		return ""
	}

	var parts []string
	for _, c := range result.Content {
		switch v := c.(type) {
		case *mcp.TextContent:
			parts = append(parts, v.Text)
		default:
			// For non-text content (images, audio, etc.), marshal to JSON so the
			// caller at least sees something useful.
			data, err := json.Marshal(v)
			if err != nil {
				parts = append(parts, fmt.Sprintf("[unserializable content: %T]", v))
			} else {
				parts = append(parts, string(data))
			}
		}
	}

	return strings.Join(parts, "\n")
}
