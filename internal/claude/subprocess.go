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
	"path/filepath"
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

// scanResult carries a single line from the persistent scan goroutine.
type scanResult struct {
	line []byte
	ok   bool
	err  error
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
	scanCh chan scanResult // persistent scan goroutine delivers lines here
	sessionID   string
	mu          sync.Mutex // serializes message sends
	lastUsed    time.Time
	done        chan struct{} // closed when process exits
	initMsgSent bool         // true if initial message was sent during spawn
}

// Send writes a user message to the CLI's stdin and reads streaming events
// until a ResultEvent is received. The onPartialText callback (if non-nil)
// fires for each text delta, enabling real-time streaming to Telegram.
func (p *CLIProcess) Send(ctx context.Context, userMsg json.RawMessage, onPartialText func(string)) ([]CLIEvent, error) {
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
			if !res.ok {
				if res.err != nil {
					return events, fmt.Errorf("cli: read stdout: %w", res.err)
				}
				return events, fmt.Errorf("cli: stdout closed")
			}

			line := res.line
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

			// Fire streaming callback for text deltas.
			if td, ok := event.(TextDeltaEvent); ok && onPartialText != nil {
				onPartialText(td.Text)
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
	cliPath string
	model   string

	mu        sync.Mutex
	processes map[userKey]*CLIProcess
	spawning  map[userKey]chan struct{} // in-flight spawns (singleflight)
}

// NewCLIManager creates a new manager.
func NewCLIManager(cliPath, model string) *CLIManager {
	return &CLIManager{
		cliPath:   cliPath,
		model:     model,
		processes: make(map[userKey]*CLIProcess),
	}
}

// SpawnParams configures a new CLI process.
type SpawnParams struct {
	SystemPrompt string
	MCPConfig    string          // JSON string for --mcp-config
	InitialMsg   json.RawMessage // first message to send (required for spawn, CLI waits for it before init)
}

// GetOrCreate returns the existing CLI process for a user or spawns a new one.
// Uses per-key singleflight to prevent double-spawn races when concurrent
// messages arrive for the same user before the first spawn completes.
func (m *CLIManager) GetOrCreate(ctx context.Context, userID, chatID int64, params SpawnParams) (*CLIProcess, error) {
	key := userKey{UserID: userID, ChatID: chatID}

	m.mu.Lock()
	if proc, ok := m.processes[key]; ok && proc.Alive() {
		proc.lastUsed = time.Now()
		m.mu.Unlock()
		return proc, nil
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
			if proc, ok := m.processes[key]; ok && proc.Alive() {
				proc.lastUsed = time.Now()
				m.mu.Unlock()
				return proc, nil
			}
			m.mu.Unlock()
			return nil, fmt.Errorf("cli: concurrent spawn failed for user %d", userID)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	// Mark this key as spawning.
	if m.spawning == nil {
		m.spawning = make(map[userKey]chan struct{})
	}
	done := make(chan struct{})
	m.spawning[key] = done
	m.mu.Unlock()

	proc, err := m.spawn(ctx, params)

	m.mu.Lock()
	delete(m.spawning, key)
	close(done) // unblock any waiters
	if err == nil {
		m.processes[key] = proc
	}
	m.mu.Unlock()

	return proc, err
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
		"--bare",
		"--no-session-persistence",
		"--replay-user-messages",
	}

	if params.SystemPrompt != "" {
		args = append(args, "--system-prompt", params.SystemPrompt)
	}
	if params.MCPConfig != "" {
		args = append(args, "--strict-mcp-config", "--mcp-config", params.MCPConfig)
	}
	if m.model != "" {
		args = append(args, "--model", m.model)
	}

	cmd := exec.CommandContext(ctx, m.cliPath, args...)

	// In --bare mode, the CLI only reads ANTHROPIC_API_KEY for auth.
	// Read the OAuth access token from ~/.claude/.credentials.json and
	// inject it as ANTHROPIC_API_KEY so Max subscription auth works.
	if token := readOAuthToken(); token != "" {
		cmd.Env = append(os.Environ(), "ANTHROPIC_API_KEY="+token)
		slog.Info("cli: injecting OAuth token", "len", len(token))
	} else {
		slog.Warn("cli: no OAuth token found, CLI auth may fail")
	}

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
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024) // up to 1MB lines

	scanCh := make(chan scanResult, 1)
	go func() {
		for scanner.Scan() {
			line := append([]byte(nil), scanner.Bytes()...)
			scanCh <- scanResult{line: line, ok: true}
		}
		scanCh <- scanResult{ok: false, err: scanner.Err()}
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
			if !res.ok {
				return nil, fmt.Errorf("cli: stdout closed before init: stderr=%s", stderrBuf.String())
			}

			line := res.line
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
		Type  string `json:"type"`
		Delta struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
	}
	if err := json.Unmarshal(raw.Event, &inner); err != nil {
		return nil, nil // unparseable inner event
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

// readOAuthToken reads the Claude OAuth access token from ~/.claude/.credentials.json.
// Returns empty string if the file doesn't exist or can't be parsed.
func readOAuthToken() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, ".claude", ".credentials.json"))
	if err != nil {
		return ""
	}
	var creds struct {
		ClaudeAiOauth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(data, &creds); err != nil {
		return ""
	}
	return creds.ClaudeAiOauth.AccessToken
}

// ImageBlock holds base64-encoded image data for multimodal messages.
type ImageBlock struct {
	MediaType string // e.g. "image/jpeg"
	Data      string // base64 encoded
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
