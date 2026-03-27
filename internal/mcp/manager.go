// Package mcp manages persistent connections to MCP (Model Context Protocol)
// servers via stdio transport, providing tool discovery and invocation across
// all connected servers.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"

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
}

// serverConn holds the state for a single connected MCP server.
type serverConn struct {
	session *mcp.ClientSession
	config  config.MCPServerConfig
	tools   []*mcp.Tool
}

// NewManager creates a new Manager with no active connections.
func NewManager() *Manager {
	return &Manager{
		servers: make(map[string]*serverConn),
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
			slog.Warn("mcp: failed to start server",
				"server", srv.Name,
				"command", srv.Command,
				"error", err,
			)
			continue
		}
		slog.Info("mcp: server started", "server", srv.Name)
	}
	return nil
}

// startServer launches a single MCP server and discovers its tools.
func (m *Manager) startServer(ctx context.Context, srv config.MCPServerConfig, envResolver func(string) (string, error)) error {
	// Build environment for the subprocess. Start with the current process
	// environment so the child inherits PATH, HOME, etc.
	env := os.Environ()
	for k, v := range srv.Env {
		resolved := v
		if envResolver != nil {
			var err error
			resolved, err = envResolver(v)
			if err != nil {
				return fmt.Errorf("resolve env %q: %w", k, err)
			}
		}
		env = append(env, k+"="+resolved)
	}

	cmd := exec.CommandContext(ctx, srv.Command, srv.Args...)
	cmd.Env = env

	transport := &mcp.CommandTransport{Command: cmd}

	client := mcp.NewClient(
		&mcp.Implementation{
			Name:    "curlycatclaw",
			Version: "0.1.0",
		},
		nil,
	)

	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
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
// produced by Tools (e.g. "search__web_search").
func (m *Manager) CallTool(ctx context.Context, serverTool string, arguments map[string]any) (string, error) {
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

// Shutdown gracefully closes all MCP server connections.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for name, sc := range m.servers {
		if err := sc.session.Close(); err != nil {
			slog.Warn("mcp: error closing server", "server", name, "error", err)
		} else {
			slog.Info("mcp: server stopped", "server", name)
		}
	}
	// Clear the map so a subsequent Shutdown is a no-op.
	m.servers = make(map[string]*serverConn)
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
