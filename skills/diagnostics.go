package skills

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	diagMaxSectionBytes = 2048 // 2KB per section
	diagHealthTimeout   = 3 * time.Second
	diagToolCallLimit   = 5
)

// DiagSafeConfig holds only non-sensitive config fields for diagnostics output.
// Computed once at init from *config.Config. The full config is never stored.
type DiagSafeConfig struct {
	EvalEnabled   bool
	IngestEnabled bool
	VoiceEnabled  bool
	WasmEnabled   bool
	MCPServers    []string
	EmbedModel    string
	LogLevel      string
}

// MCPStatusProvider reports registered MCP server status.
type MCPStatusProvider interface {
	ServerNames() []string
	IsRegistered(name string) bool
}

// InitDiagnosticsSkills returns the capture_diagnostics skill.
func InitDiagnosticsSkills(version string, db *sql.DB, mcpStatus MCPStatusProvider, safeCfg DiagSafeConfig, qdrantURL, ollamaURL string) []*Skill {
	diagSkill := &Skill{
		Name:        "capture_diagnostics",
		Description: "Capture environment diagnostics for bug reports. Returns structured markdown with version, MCP status, recent errors, recent tool calls, and health status. Never exposes API keys or credentials.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Execute:     makeDiagnosticsExecute(version, db, mcpStatus, safeCfg, qdrantURL, ollamaURL),
	}

	return []*Skill{diagSkill}
}

func makeDiagnosticsExecute(version string, db *sql.DB, mcpStatus MCPStatusProvider, safeCfg DiagSafeConfig, qdrantURL, ollamaURL string) func(ctx context.Context, input json.RawMessage) (string, error) {
	httpClient := &http.Client{Timeout: diagHealthTimeout}

	return func(ctx context.Context, _ json.RawMessage) (string, error) {
		user := GetUser(ctx)
		if user.UserID == 0 {
			return "", fmt.Errorf("capture_diagnostics requires user context (user ID must not be zero)")
		}

		var sb strings.Builder
		sb.WriteString("## Environment\n")
		fmt.Fprintf(&sb, "- **Version:** %s\n", version)

		// MCP Servers (ServerNames returns only registered servers)
		if mcpStatus != nil {
			names := mcpStatus.ServerNames()
			if len(names) > 0 {
				fmt.Fprintf(&sb, "- **MCP Servers:** %s\n", strings.Join(names, ", "))
			} else {
				sb.WriteString("- **MCP Servers:** none configured\n")
			}
		}

		// Health checks (best effort, never block)
		qdrantStatus := checkHealth(httpClient, qdrantURL)
		ollamaStatus := checkHealth(httpClient, ollamaURL)
		fmt.Fprintf(&sb, "- **Health:** Qdrant: %s, Ollama: %s\n", qdrantStatus, ollamaStatus)

		// Config summary (safe fields only)
		features := []string{}
		if safeCfg.EvalEnabled {
			features = append(features, "eval=on")
		} else {
			features = append(features, "eval=off")
		}
		if safeCfg.IngestEnabled {
			features = append(features, "ingest=on")
		} else {
			features = append(features, "ingest=off")
		}
		if safeCfg.VoiceEnabled {
			features = append(features, "voice=on")
		} else {
			features = append(features, "voice=off")
		}
		if safeCfg.WasmEnabled {
			features = append(features, "wasm=on")
		} else {
			features = append(features, "wasm=off")
		}
		fmt.Fprintf(&sb, "- **Features:** %s\n", strings.Join(features, ", "))
		if safeCfg.EmbedModel != "" {
			fmt.Fprintf(&sb, "- **Embed Model:** %s\n", safeCfg.EmbedModel)
		}

		// Recent errors (best effort)
		sb.WriteString("\n## Recent Errors (last 5)\n")
		errors, err := recentToolErrors(db, user.UserID, diagToolCallLimit)
		if err != nil {
			sb.WriteString("_unavailable_\n")
		} else if len(errors) == 0 {
			sb.WriteString("No recent errors.\n")
		} else {
			for i, tc := range errors {
				output := truncate(tc.Output, diagMaxSectionBytes/diagToolCallLimit)
				fmt.Fprintf(&sb, "%d. [%s] `%s` — %s\n", i+1, tc.Timestamp.Format("2006-01-02 15:04:05"), tc.ToolName, output)
			}
		}

		// Recent tool calls (best effort)
		sb.WriteString("\n## Recent Tool Calls (last 5)\n")
		calls, err := recentToolCalls(db, user.UserID, diagToolCallLimit)
		if err != nil {
			sb.WriteString("_unavailable_\n")
		} else if len(calls) == 0 {
			sb.WriteString("No recent tool calls.\n")
		} else {
			for i, tc := range calls {
				status := "OK"
				if tc.IsError {
					status = "ERROR"
				}
				fmt.Fprintf(&sb, "%d. [%s] `%s` — %s\n", i+1, tc.Timestamp.Format("2006-01-02 15:04:05"), tc.ToolName, status)
			}
		}

		result := sb.String()
		if len(result) > diagMaxSectionBytes*3 {
			result = result[:diagMaxSectionBytes*3] + "\n\n_...truncated_\n"
		}
		return result, nil
	}
}

// checkHealth does a GET to the URL with a short timeout. Returns "OK" or "unreachable".
func checkHealth(client *http.Client, rawURL string) string {
	if rawURL == "" {
		return "not configured"
	}
	// Ensure the URL has a scheme; bare host:port is common in config.
	u := rawURL
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		u = "http://" + u
	}
	resp, err := client.Get(u)
	if err != nil {
		return "unreachable"
	}
	resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return "OK"
	}
	return fmt.Sprintf("HTTP %d", resp.StatusCode)
}

// truncate cuts a string to approximately maxBytes, respecting UTF-8 rune boundaries.
func truncate(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	// Walk runes to find the last complete rune within maxBytes.
	end := 0
	for i := range s {
		if i > maxBytes {
			break
		}
		end = i
	}
	return s[:end] + "..."
}

// toolCallRecord matches memory.ToolCallRecord for internal use.
type toolCallRecord struct {
	ToolName  string
	Timestamp time.Time
	IsError   bool
	Output    string
}

// recentToolErrors queries tool_calls with errors for a user within the last 24h.
func recentToolErrors(db *sql.DB, userID int64, limit int) ([]toolCallRecord, error) {
	if userID == 0 {
		return nil, fmt.Errorf("userID must not be zero")
	}
	rows, err := db.Query(`
		SELECT t.name, t.created_at, t.is_error, COALESCE(t.output, '')
		FROM tool_calls t
		JOIN conversations c ON t.conversation_id = c.id
		WHERE c.user_id = ? AND t.is_error = TRUE
		  AND t.created_at > datetime('now', '-24 hours')
		ORDER BY t.created_at DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanToolCalls(rows)
}

// recentToolCalls queries recent tool_calls for a user within the last 24h.
func recentToolCalls(db *sql.DB, userID int64, limit int) ([]toolCallRecord, error) {
	if userID == 0 {
		return nil, fmt.Errorf("userID must not be zero")
	}
	rows, err := db.Query(`
		SELECT t.name, t.created_at, t.is_error, COALESCE(t.output, '')
		FROM tool_calls t
		JOIN conversations c ON t.conversation_id = c.id
		WHERE c.user_id = ?
		  AND t.created_at > datetime('now', '-24 hours')
		ORDER BY t.created_at DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanToolCalls(rows)
}

func scanToolCalls(rows *sql.Rows) ([]toolCallRecord, error) {
	var result []toolCallRecord
	for rows.Next() {
		var tc toolCallRecord
		var ts string
		if err := rows.Scan(&tc.ToolName, &ts, &tc.IsError, &tc.Output); err != nil {
			return nil, err
		}
		// SQLite may return timestamps with or without fractional seconds.
		tc.Timestamp, _ = time.Parse("2006-01-02 15:04:05", ts)
		if tc.Timestamp.IsZero() {
			tc.Timestamp, _ = time.Parse("2006-01-02T15:04:05Z", ts)
		}
		result = append(result, tc)
	}
	return result, rows.Err()
}
