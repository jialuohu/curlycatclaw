// Package claude provides a CLI subprocess manager for routing LLM calls
// through the claude CLI. This enables use of Claude Max subscription auth
// which is handled internally by the CLI binary.
package claude

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// CLIEvent is the interface for all events parsed from CLI stream-json output.
type CLIEvent interface {
	cliEvent()
}

// SystemInitEvent is emitted once when the CLI process starts.
type SystemInitEvent struct {
	SessionID string
	Model     string
	Tools     []string
	Version   string
}

func (SystemInitEvent) cliEvent() {}

// TextDeltaEvent carries a partial text chunk for streaming.
type TextDeltaEvent struct {
	Text string
}

func (TextDeltaEvent) cliEvent() {}

// ToolUseStartEvent fires when Claude begins a tool call (before execution).
// Parsed from content_block_start events in the stream.
type ToolUseStartEvent struct {
	Name string
}

func (ToolUseStartEvent) cliEvent() {}

// ToolUseEvent is emitted when the assistant invokes a tool.
type ToolUseEvent struct {
	ID    string
	Name  string
	Input json.RawMessage
}

func (ToolUseEvent) cliEvent() {}

// AssistantMessageEvent carries a complete assistant response.
type AssistantMessageEvent struct {
	TextContent string
	ToolCalls   []ToolUseEvent
}

func (AssistantMessageEvent) cliEvent() {}

// ResultEvent signals the CLI has finished processing the current turn.
type ResultEvent struct {
	Subtype    string  // "success", "error_max_turns", etc.
	Result     string  // final text result (success only)
	Cost       float64 // total_cost_usd
	Turns      int     // num_turns
	DurationMs int     // duration_ms
	IsError    bool
	Errors     []string // present on error subtypes
}

func (ResultEvent) cliEvent() {}

// ScanResult carries a single line from the persistent scan goroutine.
type ScanResult struct {
	Line []byte
	OK   bool
	Err  error
}

// userKey identifies a unique user conversation for process mapping.
type userKey struct {
	UserID int64
	ChatID int64
}

// CLIProcess wraps a single long-lived CLI subprocess.
type CLIProcess struct {
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	stdout io.ReadCloser
	scanCh chan ScanResult // persistent scan goroutine delivers lines here
	sessionID   string
	mu          sync.Mutex // serializes message sends
	lastUsed    time.Time
	done        chan struct{} // closed when process exits
	initMsgSent bool         // true if initial message was sent during spawn
}

// Send writes a user message to the CLI's stdin and reads streaming events
// until a ResultEvent is received. The onPartialText callback (if non-nil)
// fires for each text delta, enabling real-time streaming to Telegram.
// The onToolUse callback (if non-nil) fires when a tool call starts, before
// the tool executes, enabling real-time [tool] notifications.
func (p *CLIProcess) Send(ctx context.Context, userMsg json.RawMessage, onPartialText func(string), onToolUse func(string)) ([]CLIEvent, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastUsed = time.Now()

	slog.Info("cli: Send() called", "msg_len", len(userMsg), "skip_write", p.initMsgSent)

	// Skip the write if this message was already sent during spawn().
	if p.initMsgSent {
		p.initMsgSent = false
	} else {
		// Write message to stdin (append newline without mutating caller's slice).
		buf := make([]byte, len(userMsg)+1)
		copy(buf, userMsg)
		buf[len(userMsg)] = '\n'
		if _, err := p.stdin.Write(buf); err != nil {
			return nil, fmt.Errorf("cli: write stdin: %w", err)
		}
	}

	// Read events until we get a result.
	var events []CLIEvent
	for {
		select {
		case <-ctx.Done():
			return events, ctx.Err()
		case <-p.done:
			return events, fmt.Errorf("cli: process exited unexpectedly")
		case res := <-p.scanCh:
			if !res.OK {
				if res.Err != nil {
					return events, fmt.Errorf("cli: read stdout: %w", res.Err)
				}
				return events, fmt.Errorf("cli: stdout closed")
			}

			line := res.Line
			if len(line) == 0 {
				continue
			}

			event, err := parseStreamEvent(line)
			if err != nil {
				slog.Warn("cli: skip unparseable event", "err", err, "line_len", len(line))
				continue
			}

			// Log every event type for debugging.
			if event != nil {
				slog.Debug("cli: event", "type", fmt.Sprintf("%T", event))
			} else {
				// Peek at what we're skipping.
				var peek struct{ Type string `json:"type"` }
				if err := json.Unmarshal(line, &peek); err == nil {
					slog.Debug("cli: skip event", "type", peek.Type, "len", len(line))
				}
			}

			if event == nil {
				continue // unknown event type, tolerate gracefully
			}

			events = append(events, event)

			// Fire streaming callbacks.
			if td, ok := event.(TextDeltaEvent); ok && onPartialText != nil {
				onPartialText(td.Text)
			}
			if tu, ok := event.(ToolUseStartEvent); ok && onToolUse != nil {
				onToolUse(tu.Name)
			}

			// Result means this turn is done.
			if _, ok := event.(ResultEvent); ok {
				slog.Info("cli: result received, returning")
				return events, nil
			}
		}
	}
}

// Alive returns true if the CLI process is still running.
func (p *CLIProcess) Alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// Kill terminates the CLI process.
func (p *CLIProcess) Kill() {
	p.stdin.Close()
	if p.stdout != nil {
		p.stdout.Close()
	}
	if p.cmd.Process != nil {
		p.cmd.Process.Kill() //nolint:errcheck
	}
}

// CLIManager manages long-lived CLI processes keyed by (userID, chatID).
type CLIManager struct {
	cliPath    string
	model      string
	effort     string // default effort level for all spawns
	oauthToken string // long-lived token from `claude setup-token`

	mu        sync.Mutex
	processes map[userKey]*CLIProcess
	spawning  map[userKey]chan struct{} // in-flight spawns (singleflight)
}

// NewCLIManager creates a new manager. If oauthToken is non-empty, it is
// injected as CLAUDE_CODE_OAUTH_TOKEN on each subprocess.
func NewCLIManager(cliPath, model, effort, oauthToken string) *CLIManager {
	return &CLIManager{
		cliPath:    cliPath,
		model:      model,
		effort:     effort,
		oauthToken: oauthToken,
		processes:  make(map[userKey]*CLIProcess),
	}
}

// SpawnParams configures a new CLI process.
type SpawnParams struct {
	SystemPrompt string
	MCPConfig    string          // JSON string for --mcp-config
	InitialMsg   json.RawMessage // first message to send (required for spawn, CLI waits for it before init)
	WorkDir      string          // if set, cmd.Dir is set to this path
	HomeDir      string          // if set, HOME env var is replaced with this path
	Model        string          // if set, overrides CLIManager.model for this spawn
	Effort       string          // if set, overrides CLIManager.effort for this spawn
	SafeMode     bool            // if true, omit --dangerously-skip-permissions (for untrusted content)
}

// GetOrCreate returns the existing CLI process for a user or spawns a new one.
// The isNew return value is true when a fresh subprocess was spawned (not
// reused from cache). Callers can use this to inject conversation history
// into the first message sent to a newly spawned process.
// Uses per-key singleflight to prevent double-spawn races when concurrent
// messages arrive for the same user before the first spawn completes.
func (m *CLIManager) GetOrCreate(ctx context.Context, userID, chatID int64, params SpawnParams) (proc *CLIProcess, isNew bool, err error) {
	key := userKey{UserID: userID, ChatID: chatID}

	m.mu.Lock()
	if p, ok := m.processes[key]; ok && p.Alive() {
		p.lastUsed = time.Now()
		m.mu.Unlock()
		return p, false, nil
	}
	// Remove dead process if present.
	delete(m.processes, key)

	// Check if another goroutine is already spawning for this key.
	if ch, ok := m.spawning[key]; ok {
		m.mu.Unlock()
		// Wait for the in-flight spawn to finish.
		select {
		case <-ch:
			// Spawn completed; retry to get the process.
			m.mu.Lock()
			if p, ok := m.processes[key]; ok && p.Alive() {
				p.lastUsed = time.Now()
				m.mu.Unlock()
				return p, false, nil
			}
			m.mu.Unlock()
			return nil, false, fmt.Errorf("cli: concurrent spawn failed for user %d", userID)
		case <-ctx.Done():
			return nil, false, ctx.Err()
		}
	}

	// Mark this key as spawning.
	if m.spawning == nil {
		m.spawning = make(map[userKey]chan struct{})
	}
	spawnDone := make(chan struct{})
	m.spawning[key] = spawnDone
	m.mu.Unlock()

	// Ensure spawning entry is cleaned up even if spawn() panics,
	// so concurrent waiters aren't stuck forever.
	cleaned := false
	defer func() {
		if !cleaned {
			m.mu.Lock()
			delete(m.spawning, key)
			close(spawnDone)
			m.mu.Unlock()
		}
	}()

	proc, err = m.spawn(ctx, params)

	m.mu.Lock()
	delete(m.spawning, key)
	close(spawnDone) // unblock any waiters
	cleaned = true
	if err == nil {
		m.processes[key] = proc
	}
	m.mu.Unlock()

	return proc, err == nil, err
}

// SpawnOneShot creates a temporary CLI process not tracked in the manager's
// process map. The caller is responsible for calling Kill() on the returned
// process. Used for cron tasks that need isolated execution.
func (m *CLIManager) SpawnOneShot(ctx context.Context, params SpawnParams) (*CLIProcess, error) {
	return m.spawn(ctx, params)
}

// Remove kills and removes the process for a user.
func (m *CLIManager) Remove(userID, chatID int64) {
	key := userKey{UserID: userID, ChatID: chatID}
	m.mu.Lock()
	if proc, ok := m.processes[key]; ok {
		proc.Kill()
		delete(m.processes, key)
	}
	m.mu.Unlock()
}

// Cleanup kills all processes idle longer than maxIdle.
func (m *CLIManager) Cleanup(maxIdle time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	for key, proc := range m.processes {
		if !proc.Alive() || now.Sub(proc.lastUsed) > maxIdle {
			proc.Kill()
			delete(m.processes, key)
		}
	}
}

// Shutdown kills all managed CLI processes. Blocks until all exit or timeout.
func (m *CLIManager) Shutdown(timeout time.Duration) {
	m.mu.Lock()
	procs := make([]*CLIProcess, 0, len(m.processes))
	for _, p := range m.processes {
		procs = append(procs, p)
	}
	m.processes = make(map[userKey]*CLIProcess)
	m.mu.Unlock()

	for _, p := range procs {
		p.Kill()
	}

	deadline := time.After(timeout)
	for _, p := range procs {
		select {
		case <-p.done:
		case <-deadline:
			slog.Warn("cli: shutdown timeout, some processes may be orphaned")
			return
		}
	}
}

// spawn creates and starts a new CLI subprocess.
func (m *CLIManager) spawn(ctx context.Context, params SpawnParams) (_ *CLIProcess, retErr error) {
	args := []string{
		"--print",
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--include-partial-messages",
		"--verbose",
		"--no-session-persistence",
		"--replay-user-messages",
	}
	if !params.SafeMode {
		args = append(args, "--dangerously-skip-permissions")
	}

	if params.SystemPrompt != "" {
		args = append(args, "--system-prompt", params.SystemPrompt)
	}
	if params.MCPConfig != "" {
		args = append(args, "--strict-mcp-config", "--mcp-config", params.MCPConfig)
		// Block built-in scheduling tools that compete with curlycatclaw's
		// set_reminder/list_reminders/cancel_reminder MCP skills.
		args = append(args, "--disallowedTools", "CronCreate,CronDelete,CronList")
	}
	model := m.model
	if params.Model != "" {
		model = params.Model
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	effort := m.effort
	if params.Effort != "" {
		effort = params.Effort
	}
	if effort != "" {
		args = append(args, "--effort", effort)
	}

	cmd := exec.CommandContext(ctx, m.cliPath, args...)

	if params.WorkDir != "" {
		cmd.Dir = params.WorkDir
	}

	// Build environment: allowlist-filtered to prevent leaking secrets
	// (CURLYCATCLAW_MASTER_KEY, Telegram token, etc.) to the CLI subprocess
	// and any MCP servers it spawns. Matches the filteredEnv pattern used
	// by internal/mcp/manager.go for MCP child processes.
	env := filteredSpawnEnv()
	if params.HomeDir != "" {
		env = replaceEnv(env, "HOME", params.HomeDir)
	}

	// Inject long-lived OAuth token so the CLI handles auth via
	// token exchange at /api/oauth/claude_cli/create_api_key.
	if m.oauthToken != "" {
		env = replaceEnv(env, "CLAUDE_CODE_OAUTH_TOKEN", m.oauthToken)
		slog.Info("cli: OAuth token injected via CLAUDE_CODE_OAUTH_TOKEN")
	}
	cmd.Env = env

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("cli: stdin pipe: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, fmt.Errorf("cli: stdout pipe: %w", err)
	}

	// Capture stderr for diagnostics (bounded buffer).
	var stderrBuf bytes.Buffer
	cmd.Stderr = &limitWriter{w: &stderrBuf, max: 4096}

	if err := cmd.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		return nil, fmt.Errorf("cli: start process: %w", err)
	}

	// Cleanup on failure after Start.
	defer func() {
		if retErr != nil {
			stdin.Close()
			stdout.Close()
			if cmd.Process != nil {
				cmd.Process.Kill() //nolint:errcheck
			}
		}
	}()

	done := make(chan struct{})
	go func() {
		cmd.Wait() //nolint:errcheck
		close(done)
	}()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 256*1024), 16*1024*1024) // up to 16MB lines (base64 PDFs)

	scanCh := make(chan ScanResult, 1)
	go func() {
		for scanner.Scan() {
			line := append([]byte(nil), scanner.Bytes()...)
			select {
			case scanCh <- ScanResult{Line: line, OK: true}:
			case <-done:
				return
			}
		}
		select {
		case scanCh <- ScanResult{OK: false, Err: scanner.Err()}:
		case <-done:
		}
	}()

	proc := &CLIProcess{
		cmd:      cmd,
		stdin:    stdin,
		stdout:   stdout,
		scanCh:   scanCh,
		lastUsed: time.Now(),
		done:     done,
	}

	// The CLI with --input-format stream-json waits for the first message
	// on stdin before emitting the init event. Write it now.
	if len(params.InitialMsg) > 0 {
		initBuf := make([]byte, len(params.InitialMsg)+1)
		copy(initBuf, params.InitialMsg)
		initBuf[len(params.InitialMsg)] = '\n'
		if _, err := stdin.Write(initBuf); err != nil {
			return nil, fmt.Errorf("cli: write initial message: %w", err)
		}
	}

	// Read the init event to capture session_id.
	initCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	for {
		select {
		case <-initCtx.Done():
			return nil, fmt.Errorf("cli: timeout waiting for init event")
		case <-done:
			return nil, fmt.Errorf("cli: process exited before init: stderr=%s", stderrBuf.String())
		case res := <-scanCh:
			if !res.OK {
				return nil, fmt.Errorf("cli: stdout closed before init: stderr=%s", stderrBuf.String())
			}

			line := res.Line
			if len(line) == 0 {
				continue
			}

			event, err := parseStreamEvent(line)
			if err != nil {
				continue
			}
			if init, ok := event.(SystemInitEvent); ok {
				proc.sessionID = init.SessionID
				proc.initMsgSent = len(params.InitialMsg) > 0
				slog.Info("cli: process started",
					"session_id", init.SessionID,
					"model", init.Model,
					"pid", cmd.Process.Pid)
				return proc, nil
			}
		}
	}
}

// parseStreamEvent parses a single line of stream-json output.
// Returns nil, nil for unknown event types (tolerant parsing per Codex review).
func parseStreamEvent(line []byte) (CLIEvent, error) {
	// Peek at the type field.
	var envelope struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
	}
	if err := json.Unmarshal(line, &envelope); err != nil {
		return nil, fmt.Errorf("unmarshal envelope: %w", err)
	}

	switch envelope.Type {
	case "system":
		if envelope.Subtype == "init" {
			return parseInitEvent(line)
		}
		return nil, nil // unknown system subtype

	case "stream_event":
		return parseStreamDelta(line)

	case "assistant":
		return parseAssistantMessage(line)

	case "result":
		return parseResultEvent(line)

	default:
		return nil, nil // unknown type, tolerate gracefully
	}
}

func parseInitEvent(line []byte) (CLIEvent, error) {
	var raw struct {
		SessionID string   `json:"session_id"`
		Model     string   `json:"model"`
		Tools     []string `json:"tools"`
		Version   string   `json:"claude_code_version"`
	}
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil, fmt.Errorf("parse init: %w", err)
	}
	return SystemInitEvent{
		SessionID: raw.SessionID,
		Model:     raw.Model,
		Tools:     raw.Tools,
		Version:   raw.Version,
	}, nil
}

func parseStreamDelta(line []byte) (CLIEvent, error) {
	var raw struct {
		Event json.RawMessage `json:"event"`
	}
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil, fmt.Errorf("parse stream_event: %w", err)
	}

	var inner struct {
		Type         string `json:"type"`
		ContentBlock struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"content_block"`
		Delta struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
	}
	if err := json.Unmarshal(raw.Event, &inner); err != nil {
		return nil, nil // unparseable inner event
	}

	// Tool use start: fires before tool execution begins.
	if inner.Type == "content_block_start" && inner.ContentBlock.Type == "tool_use" && inner.ContentBlock.Name != "" {
		return ToolUseStartEvent{Name: inner.ContentBlock.Name}, nil
	}

	if inner.Type == "content_block_delta" && inner.Delta.Type == "text_delta" && inner.Delta.Text != "" {
		return TextDeltaEvent{Text: inner.Delta.Text}, nil
	}

	return nil, nil // non-text delta, skip
}

func parseAssistantMessage(line []byte) (CLIEvent, error) {
	var raw struct {
		Message struct {
			Content []struct {
				Type  string          `json:"type"`
				Text  string          `json:"text"`
				ID    string          `json:"id"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil, fmt.Errorf("parse assistant: %w", err)
	}

	var text string
	var tools []ToolUseEvent
	for _, block := range raw.Message.Content {
		switch block.Type {
		case "text":
			if text != "" {
				text += "\n"
			}
			text += block.Text
		case "tool_use":
			tools = append(tools, ToolUseEvent{
				ID:    block.ID,
				Name:  block.Name,
				Input: block.Input,
			})
		}
	}

	return AssistantMessageEvent{
		TextContent: text,
		ToolCalls:   tools,
	}, nil
}

func parseResultEvent(line []byte) (CLIEvent, error) {
	var raw struct {
		Subtype    string   `json:"subtype"`
		Result     string   `json:"result"`
		TotalCost  float64  `json:"total_cost_usd"`
		NumTurns   int      `json:"num_turns"`
		DurationMs int      `json:"duration_ms"`
		IsError    bool     `json:"is_error"`
		Errors     []string `json:"errors"`
	}
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil, fmt.Errorf("parse result: %w", err)
	}

	return ResultEvent{
		Subtype:    raw.Subtype,
		Result:     raw.Result,
		Cost:       raw.TotalCost,
		Turns:      raw.NumTurns,
		DurationMs: raw.DurationMs,
		IsError:    raw.IsError,
		Errors:     raw.Errors,
	}, nil
}

// BuildUserMessage creates the stream-json input for a text-only user message.
func BuildUserMessage(text string) json.RawMessage {
	msg := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role": "user",
			"content": []map[string]any{
				{"type": "text", "text": text},
			},
		},
	}
	data, _ := json.Marshal(msg) //nolint:errcheck // marshal of map[string]any with string values cannot fail
	return data
}

// BuildImageMessage creates stream-json input for a message with text and images.
func BuildImageMessage(text string, images []ImageBlock) json.RawMessage {
	content := []map[string]any{
		{"type": "text", "text": text},
	}
	for _, img := range images {
		content = append(content, map[string]any{
			"type": "image",
			"source": map[string]any{
				"type":       "base64",
				"media_type": img.MediaType,
				"data":       img.Data,
			},
		})
	}
	msg := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": content,
		},
	}
	data, _ := json.Marshal(msg) //nolint:errcheck // marshal of map[string]any with string values cannot fail
	return data
}


// ImageBlock holds base64-encoded image data for multimodal messages.
type ImageBlock struct {
	MediaType string // e.g. "image/jpeg"
	Data      string // base64 encoded
}

// DocumentBlock holds base64-encoded document data (e.g., PDF) for multimodal messages.
type DocumentBlock struct {
	MediaType string // e.g. "application/pdf"
	Data      string // base64 encoded
	FileName  string // original filename
}

// BuildMultimodalMessage creates stream-json input for a message with text, images, and documents.
func BuildMultimodalMessage(text string, images []ImageBlock, documents []DocumentBlock) json.RawMessage {
	content := []map[string]any{
		{"type": "text", "text": text},
	}
	for _, img := range images {
		content = append(content, map[string]any{
			"type": "image",
			"source": map[string]any{
				"type":       "base64",
				"media_type": img.MediaType,
				"data":       img.Data,
			},
		})
	}
	for _, doc := range documents {
		content = append(content, map[string]any{
			"type": "document",
			"source": map[string]any{
				"type":       "base64",
				"media_type": doc.MediaType,
				"data":       doc.Data,
			},
		})
	}
	msg := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": content,
		},
	}
	data, _ := json.Marshal(msg) //nolint:errcheck
	return data
}

// limitWriter wraps a writer and stops writing after max bytes.
type limitWriter struct {
	w   io.Writer
	max int
	n   int
}

func (lw *limitWriter) Write(p []byte) (int, error) {
	if lw.n >= lw.max {
		return len(p), nil // silently discard
	}
	remaining := lw.max - lw.n
	if len(p) > remaining {
		p = p[:remaining]
	}
	n, err := lw.w.Write(p)
	lw.n += n
	return len(p), err // report full write to avoid broken pipe
}

// spawnEnvAllowlist is the set of environment variables passed to CLI
// subprocesses. Prevents secrets like CURLYCATCLAW_MASTER_KEY, Telegram
// tokens, and API keys from leaking to the CLI and any MCP servers it spawns.
var spawnEnvAllowlist = map[string]struct{}{
	// Baseline (matches internal/mcp/manager.go defaultEnvAllowlist).
	"PATH": {}, "HOME": {}, "USER": {}, "LANG": {}, "LC_ALL": {},
	"SHELL": {}, "TMPDIR": {}, "TZ": {}, "XDG_RUNTIME_DIR": {},
	// Node.js runtime (Claude CLI is a Node.js app).
	"NODE_PATH": {}, "NODE_OPTIONS": {}, "NODE_EXTRA_CA_CERTS": {},
	// Terminal (needed for Claude CLI output formatting).
	"TERM": {}, "COLORTERM": {},
	// Playwright (needed by scrapling-mcp browser tools).
	"PLAYWRIGHT_BROWSERS_PATH": {},
}

// filteredSpawnEnv returns a copy of the current process environment filtered
// through spawnEnvAllowlist. CLAUDE_CODE_OAUTH_TOKEN is injected separately
// by spawn() after this call.
func filteredSpawnEnv() []string {
	var out []string
	for _, entry := range os.Environ() {
		if k, _, ok := strings.Cut(entry, "="); ok {
			if _, pass := spawnEnvAllowlist[k]; pass {
				out = append(out, entry)
			}
		}
	}
	return out
}

// replaceEnv replaces or appends a KEY=VALUE pair in an environment slice.
// If the key already exists, its value is replaced in-place. Otherwise, the
// new entry is appended.
func replaceEnv(env []string, key, value string) []string {
	prefix := key + "="
	result := make([]string, len(env))
	copy(result, env)
	for i, e := range result {
		if len(e) >= len(prefix) && e[:len(prefix)] == prefix {
			result[i] = prefix + value
			return result
		}
	}
	return append(result, prefix+value)
}

// NewTestProcess creates a CLIProcess from pre-wired pipes for testing.
// The caller is responsible for feeding ScanResult values into scanCh
// and closing done when the "process" exits.
func NewTestProcess(stdin io.WriteCloser, stdout io.ReadCloser, scanCh chan ScanResult, done chan struct{}) *CLIProcess {
	return &CLIProcess{
		stdin:    stdin,
		stdout:   stdout,
		scanCh:   scanCh,
		lastUsed: time.Now(),
		done:     done,
	}
}

// CLISender implements ingest.LLMClient by running the claude CLI in one-shot
// print mode. Used when no API key is available (CLI subscription auth).
type CLISender struct {
	CLIPath    string // path to claude binary
	OAuthToken string // CLAUDE_CODE_OAUTH_TOKEN
	Model      string // e.g. "claude-sonnet-4-6"
}

// Send runs a one-shot claude CLI call and returns the text response.
func (c *CLISender) Send(ctx context.Context, params SendParams) (*Response, error) {
	userText := extractUserText(params)
	if userText == "" {
		return nil, fmt.Errorf("cli sender: no user text in messages")
	}

	args := []string{
		"--print",
		"--no-session-persistence",
		"--output-format", "text",
	}
	if params.SystemPrompt != "" {
		args = append(args, "--system-prompt", params.SystemPrompt)
	}
	if c.Model != "" {
		args = append(args, "--model", c.Model)
	}
	args = append(args, "--", userText)

	cmd := exec.CommandContext(ctx, c.CLIPath, args...)
	cmd.Env = append(filteredSpawnEnv(), "CLAUDE_CODE_OAUTH_TOKEN="+c.OAuthToken)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("cli sender: %w (stderr: %s)", err, stderr.String())
	}

	return &Response{TextContent: strings.TrimSpace(stdout.String())}, nil
}

// extractUserText returns the text from the first user message in params.
func extractUserText(params SendParams) string {
	for _, msg := range params.Messages {
		if msg.Role == "user" {
			for _, block := range msg.Content {
				if t := block.GetText(); t != nil {
					return *t
				}
			}
		}
	}
	return ""
}

// Dedicated process keys for ingest extraction. Negative chatIDs never
// collide with real Telegram chat IDs (which are positive).
const (
	ingestUserID          int64 = 0
	ingestUntrustedChatID int64 = -1
	ingestTrustedChatID   int64 = -2
	defaultMaxTurns             = 20
)

// ingestCLI is the subset of CLIManager used by PersistentCLISender.
// Defined as an interface for testability.
type ingestCLI interface {
	GetOrCreate(ctx context.Context, userID, chatID int64, params SpawnParams) (*CLIProcess, bool, error)
	Remove(userID, chatID int64)
}

// PersistentCLISender implements ingest.LLMClient by reusing long-lived CLI
// subprocesses managed by CLIManager. Amortizes Node.js startup and OAuth auth
// overhead across many extraction calls. Maintains separate processes for
// trusted and untrusted content to prevent cross-contamination.
type PersistentCLISender struct {
	mgr      ingestCLI
	model    string
	maxTurns int
	mu       sync.Mutex
	turns    map[int64]int // per-chatID turn counter
}

// NewPersistentCLISender creates a PersistentCLISender backed by the given
// CLIManager. maxTurns controls how many extractions per process before
// recycling (0 defaults to 20).
func NewPersistentCLISender(mgr *CLIManager, model string, maxTurns int) *PersistentCLISender {
	if maxTurns <= 0 {
		maxTurns = defaultMaxTurns
	}
	return &PersistentCLISender{
		mgr:      mgr,
		model:    model,
		maxTurns: maxTurns,
		turns:    make(map[int64]int),
	}
}

// Send implements ingest.LLMClient. Routes trusted and untrusted extraction to
// separate persistent processes with proper system prompts. Spawns processes in
// SafeMode (no --dangerously-skip-permissions) to prevent tool execution with
// untrusted content.
func (p *PersistentCLISender) Send(ctx context.Context, params SendParams) (*Response, error) {
	userText := extractUserText(params)
	if userText == "" {
		return nil, fmt.Errorf("persistent cli sender: no user text in messages")
	}

	// Route to separate processes based on system prompt. Default is
	// untrusted (safe default). Only known trusted prompts get the
	// trusted process, so new source types fail safe to untrusted.
	chatID := ingestUntrustedChatID
	if strings.HasPrefix(params.SystemPrompt, "You are a knowledge extraction") {
		chatID = ingestTrustedChatID
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Recycle process if turn limit reached.
	if p.turns[chatID] >= p.maxTurns {
		p.mgr.Remove(ingestUserID, chatID)
		p.turns[chatID] = 0
	}

	msg := BuildUserMessage(userText)

	proc, isNew, err := p.mgr.GetOrCreate(ctx, ingestUserID, chatID, SpawnParams{
		SystemPrompt: params.SystemPrompt,
		InitialMsg:   msg,
		Model:        p.model,
		SafeMode:     true,
	})
	if err != nil {
		return nil, fmt.Errorf("persistent cli sender: spawn: %w", err)
	}

	// If the process was externally killed (by Cleanup) and re-spawned,
	// reset the turn counter to match actual process state.
	if isNew && p.turns[chatID] > 0 {
		p.turns[chatID] = 0
	}

	events, err := proc.Send(ctx, msg, nil, nil)
	if err != nil {
		p.mgr.Remove(ingestUserID, chatID)
		p.turns[chatID] = 0
		return nil, fmt.Errorf("persistent cli sender: send: %w", err)
	}

	p.turns[chatID]++

	return responseFromEvents(events)
}

// responseFromEvents builds a Response from CLI stream events, following the
// same pattern as the session actor: accumulate AssistantMessageEvent text,
// fall back to ResultEvent.Result.
func responseFromEvents(events []CLIEvent) (*Response, error) {
	var text strings.Builder
	for _, ev := range events {
		switch e := ev.(type) {
		case AssistantMessageEvent:
			if e.TextContent != "" {
				if text.Len() > 0 {
					text.WriteString("\n")
				}
				text.WriteString(e.TextContent)
			}
		case ResultEvent:
			if e.IsError {
				errMsg := strings.Join(e.Errors, "; ")
				if errMsg == "" {
					errMsg = e.Subtype
				}
				return nil, fmt.Errorf("persistent cli sender: %s", errMsg)
			}
			if text.Len() == 0 && e.Result != "" {
				text.WriteString(e.Result)
			}
		}
	}
	return &Response{TextContent: strings.TrimSpace(text.String())}, nil
}
