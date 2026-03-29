package session

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jialuohu/curlycatclaw/config"
	"github.com/jialuohu/curlycatclaw/internal/claude"
	"github.com/jialuohu/curlycatclaw/internal/mcp"
	"github.com/jialuohu/curlycatclaw/internal/memory"
	"github.com/jialuohu/curlycatclaw/internal/telegram"
	"github.com/jialuohu/curlycatclaw/skills"
)

// --- Mock implementations ---

// mockLLMEntry is a single queued mock response with optional streaming deltas
// and error injection.
type mockLLMEntry struct {
	resp   *claude.Response
	deltas []string // if non-empty, OnPartialText is called with each delta before returning
	err    error    // if non-nil, returned instead of resp (after deltas are fired)
}

type mockLLM struct {
	mu      sync.Mutex
	entries []mockLLMEntry        // rich entries (preferred)
	responses []*claude.Response  // legacy simple responses, used when entries is empty
	calls   []claude.SendParams
}

func (m *mockLLM) SendStreaming(_ context.Context, params claude.SendParams) (*claude.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, params)

	// Prefer rich entries if available.
	if len(m.entries) > 0 {
		entry := m.entries[0]
		m.entries = m.entries[1:]
		// Fire streaming deltas before returning, simulating the real client.
		if params.OnPartialText != nil {
			for _, d := range entry.deltas {
				params.OnPartialText(d)
			}
		}
		if entry.err != nil {
			return nil, entry.err
		}
		return entry.resp, nil
	}

	// Legacy path: simple responses without streaming.
	if len(m.responses) == 0 {
		return nil, fmt.Errorf("no more mock responses queued")
	}
	resp := m.responses[0]
	m.responses = m.responses[1:]
	return resp, nil
}

type mockStore struct {
	mu       sync.Mutex
	convID   string
	messages []storeMsg
	toolLogs []toolLog
}

type storeMsg struct {
	convID  string
	role    string
	content json.RawMessage
}

type toolLog struct {
	callID  string
	name    string
	isError bool
}

func (m *mockStore) GetActiveConversation(_, _ int64) (string, string, error) {
	return m.convID, "", nil
}

func (m *mockStore) GetConversationMessages(_ string) ([]memory.Message, error) {
	return nil, nil
}

func (m *mockStore) SaveSummary(_ string, _, _ int64, _ string, _ int, _, _ time.Time) error {
	return nil
}

func (m *mockStore) SetSummarizationStatus(_ string, _ string) error {
	return nil
}

func (m *mockStore) ConversationMeta(_ string) (int64, int64, int, time.Time, time.Time, error) {
	return 0, 0, 0, time.Time{}, time.Time{}, nil
}

func (m *mockStore) AppendMessage(convID, role string, content json.RawMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.messages = append(m.messages, storeMsg{convID, role, content})
	return nil
}

func (m *mockStore) LogToolCall(_, callID, name string, _ json.RawMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.toolLogs = append(m.toolLogs, toolLog{callID: callID, name: name})
	return nil
}

func (m *mockStore) CompleteToolCall(callID string, _ json.RawMessage, isError bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, tl := range m.toolLogs {
		if tl.callID == callID {
			m.toolLogs[i].isError = isError
			return nil
		}
	}
	return nil
}

type mockContextProvider struct {
	history []memory.Message
}

func (m *mockContextProvider) BuildContextWithBudget(_ context.Context, _, _ string) ([]memory.Message, error) {
	return m.history, nil
}

type mockToolRouter struct {
	tools     []mcp.ToolDef
	callFn    func(ctx context.Context, name string, args map[string]any) (string, error)
	callCount int
	mu        sync.Mutex
}

func (m *mockToolRouter) Tools() []mcp.ToolDef {
	return m.tools
}

func (m *mockToolRouter) CallTool(ctx context.Context, name string, args map[string]any, _, _ int64) (string, error) {
	m.mu.Lock()
	m.callCount++
	m.mu.Unlock()
	if m.callFn != nil {
		return m.callFn(ctx, name, args)
	}
	return "mock result", nil
}

type mockVectorIndexer struct {
	mu      sync.Mutex
	indexed []string
}

func (m *mockVectorIndexer) Index(_ context.Context, id, text string, _, _ int64, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.indexed = append(m.indexed, text)
	return nil
}

type mockTelegram struct {
	inbox   chan telegram.OutgoingMessage
	updates chan telegram.IncomingMessage
}

func newMockTelegram() *mockTelegram {
	return &mockTelegram{
		inbox:   make(chan telegram.OutgoingMessage, 64),
		updates: make(chan telegram.IncomingMessage, 64),
	}
}

func (m *mockTelegram) Inbox() chan<- telegram.OutgoingMessage  { return m.inbox }
func (m *mockTelegram) Updates() <-chan telegram.IncomingMessage { return m.updates }

// drainInbox runs a goroutine that reads from inbox and responds to ResultCh
// with a fake message ID, simulating the real Channel actor behavior.
// It also forwards messages to the returned channel for test assertions.
func (m *mockTelegram) drainInbox(ctx context.Context) <-chan telegram.OutgoingMessage {
	out := make(chan telegram.OutgoingMessage, 64)
	go func() {
		msgIDCounter := 1000
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-m.inbox:
				if !ok {
					return
				}
				if msg.ResultCh != nil {
					msgIDCounter++
					msg.ResultCh <- msgIDCounter
				}
				out <- msg
			}
		}
	}()
	return out
}

func defaultCfg() *config.Config {
	return &config.Config{
		Timezone: "UTC",
	}
}

func newTestActor(llm LLMClient, store MessageStore, ctxb ContextProvider, router ToolRouter, vector VectorIndexer, tg TelegramTransport) *Actor {
	return &Actor{
		cfg:      defaultCfg(),
		claude:   llm,
		tg:       tg,
		mcp:      router,
		store:    store,
		ctxb:     ctxb,
		skills:   skills.NewRegistry(),
		vector:   vector,
		indexSem: make(chan struct{}, 10),
	}
}

// --- Tests ---

func TestHandleMessage_BasicFlow(t *testing.T) {
	llm := &mockLLM{
		responses: []*claude.Response{
			{TextContent: "Hello! How can I help?"},
		},
	}
	store := &mockStore{convID: "conv-1"}
	ctxb := &mockContextProvider{}
	router := &mockToolRouter{}
	tg := newMockTelegram()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	outbox := tg.drainInbox(ctx)

	actor := newTestActor(llm, store, ctxb, router, nil, tg)

	msg := telegram.IncomingMessage{
		ChatID: 100, UserID: 42, Text: "Hi there",
	}

	if err := actor.handleMessage(context.Background(), msg); err != nil {
		t.Fatalf("handleMessage: %v", err)
	}

	// Verify user message was stored.
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.messages) < 1 {
		t.Fatal("expected at least 1 stored message")
	}
	if store.messages[0].role != "user" {
		t.Errorf("first stored message role = %q, want %q", store.messages[0].role, "user")
	}

	// Verify Claude was called.
	llm.mu.Lock()
	defer llm.mu.Unlock()
	if len(llm.calls) != 1 {
		t.Fatalf("expected 1 Claude call, got %d", len(llm.calls))
	}

	// Verify response sent to Telegram.
	select {
	case out := <-outbox:
		if out.Text != "Hello! How can I help?" {
			t.Errorf("telegram text = %q, want %q", out.Text, "Hello! How can I help?")
		}
		if out.ChatID != 100 {
			t.Errorf("telegram chatID = %d, want 100", out.ChatID)
		}
	case <-time.After(time.Second):
		t.Fatal("expected telegram message within 1s")
	}
}

func TestHandleMessage_ToolUseLoop(t *testing.T) {
	llm := &mockLLM{
		responses: []*claude.Response{
			{
				TextContent: "Let me search that.",
				ToolCalls: []claude.ToolCall{
					{ID: "tc-1", Name: "search__web", Input: json.RawMessage(`{"q":"test"}`)},
				},
			},
			{TextContent: "Here are the results!"},
		},
	}
	store := &mockStore{convID: "conv-2"}
	ctxb := &mockContextProvider{}
	router := &mockToolRouter{
		callFn: func(_ context.Context, name string, _ map[string]any) (string, error) {
			if name != "search__web" {
				return "", fmt.Errorf("unexpected tool: %s", name)
			}
			return "search results here", nil
		},
	}
	tg := newMockTelegram()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	outbox := tg.drainInbox(ctx)

	actor := newTestActor(llm, store, ctxb, router, nil, tg)

	msg := telegram.IncomingMessage{ChatID: 100, UserID: 42, Text: "search something"}
	if err := actor.handleMessage(context.Background(), msg); err != nil {
		t.Fatalf("handleMessage: %v", err)
	}

	// Should have called Claude twice (once with tool_use, once after tool result).
	llm.mu.Lock()
	if len(llm.calls) != 2 {
		t.Fatalf("expected 2 Claude calls, got %d", len(llm.calls))
	}
	llm.mu.Unlock()

	// Tool should have been called once.
	router.mu.Lock()
	if router.callCount != 1 {
		t.Errorf("expected 1 tool call, got %d", router.callCount)
	}
	router.mu.Unlock()

	// Final response should be sent to Telegram.
	select {
	case out := <-outbox:
		if out.Text != "Here are the results!" {
			t.Errorf("telegram text = %q, want %q", out.Text, "Here are the results!")
		}
	case <-time.After(time.Second):
		t.Fatal("expected telegram message within 1s")
	}
}

func TestHandleMessage_MaxToolRounds(t *testing.T) {
	// Every response returns a tool call, forcing the loop to hit maxToolRounds.
	responses := make([]*claude.Response, maxToolRounds+1)
	for i := range responses {
		responses[i] = &claude.Response{
			ToolCalls: []claude.ToolCall{
				{ID: fmt.Sprintf("tc-%d", i), Name: "search__web", Input: json.RawMessage(`{}`)},
			},
		}
	}

	llm := &mockLLM{responses: responses}
	store := &mockStore{convID: "conv-3"}
	ctxb := &mockContextProvider{}
	router := &mockToolRouter{}
	tg := newMockTelegram()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	outbox := tg.drainInbox(ctx)

	actor := newTestActor(llm, store, ctxb, router, nil, tg)

	msg := telegram.IncomingMessage{ChatID: 100, UserID: 42, Text: "loop test"}
	if err := actor.handleMessage(context.Background(), msg); err != nil {
		t.Fatalf("handleMessage: %v", err)
	}

	// Should have called Claude exactly maxToolRounds times.
	llm.mu.Lock()
	if len(llm.calls) != maxToolRounds {
		t.Errorf("expected %d Claude calls, got %d", maxToolRounds, len(llm.calls))
	}
	llm.mu.Unlock()

	// Should send the "stuck in loop" message.
	select {
	case out := <-outbox:
		if out.Text == "" {
			t.Error("expected non-empty loop-limit message")
		}
	case <-time.After(time.Second):
		t.Fatal("expected telegram message within 1s")
	}
}

func TestHandleMessage_ToolConfirmation(t *testing.T) {
	llm := &mockLLM{
		responses: []*claude.Response{
			{
				ToolCalls: []claude.ToolCall{
					{ID: "tc-1", Name: "cancel_reminder_123", Input: json.RawMessage(`{}`)},
				},
			},
			{TextContent: "Waiting for confirmation."},
		},
	}
	store := &mockStore{convID: "conv-4"}
	ctxb := &mockContextProvider{}
	router := &mockToolRouter{}
	tg := newMockTelegram()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	outbox := tg.drainInbox(ctx)

	actor := newTestActor(llm, store, ctxb, router, nil, tg)
	actor.cfg.ConfirmTools = []string{"cancel_reminder"}

	msg := telegram.IncomingMessage{ChatID: 100, UserID: 42, Text: "cancel my reminder"}
	if err := actor.handleMessage(context.Background(), msg); err != nil {
		t.Fatalf("handleMessage: %v", err)
	}

	// Tool should NOT have been called via MCP (it requires confirmation).
	router.mu.Lock()
	if router.callCount != 0 {
		t.Errorf("expected 0 tool calls (confirmation required), got %d", router.callCount)
	}
	router.mu.Unlock()

	// Should have sent confirmation preview to Telegram.
	select {
	case out := <-outbox:
		if len(out.Text) == 0 {
			t.Error("expected confirmation preview message")
		}
	case <-time.After(time.Second):
		t.Fatal("expected telegram message within 1s")
	}
}

func TestHandleMessage_VectorIndexing(t *testing.T) {
	llm := &mockLLM{
		responses: []*claude.Response{
			{TextContent: "Got it."},
		},
	}
	store := &mockStore{convID: "conv-5"}
	ctxb := &mockContextProvider{}
	router := &mockToolRouter{}
	vector := &mockVectorIndexer{}
	tg := newMockTelegram()

	actor := newTestActor(llm, store, ctxb, router, vector, tg)

	msg := telegram.IncomingMessage{ChatID: 100, UserID: 42, Text: "remember this"}
	if err := actor.handleMessage(context.Background(), msg); err != nil {
		t.Fatalf("handleMessage: %v", err)
	}

	// Wait for the async goroutine to complete.
	actor.indexWg.Wait()

	vector.mu.Lock()
	defer vector.mu.Unlock()
	if len(vector.indexed) != 1 {
		t.Fatalf("expected 1 indexed message, got %d", len(vector.indexed))
	}
	if vector.indexed[0] != "remember this" {
		t.Errorf("indexed text = %q, want %q", vector.indexed[0], "remember this")
	}
}

func TestHandleMessage_ClaudeError(t *testing.T) {
	llm := &mockLLM{} // No responses queued -> returns error.
	store := &mockStore{convID: "conv-6"}
	ctxb := &mockContextProvider{}
	router := &mockToolRouter{}
	tg := newMockTelegram()

	actor := newTestActor(llm, store, ctxb, router, nil, tg)

	msg := telegram.IncomingMessage{ChatID: 100, UserID: 42, Text: "hello"}
	err := actor.handleMessage(context.Background(), msg)

	if err == nil {
		t.Fatal("expected error from handleMessage when Claude fails")
	}
}

func TestHandleMessage_ShutdownCleanup(t *testing.T) {
	// Use a slow vector indexer to verify shutdown waits for it.
	slowVector := &slowVectorIndexer{delay: 100 * time.Millisecond}

	llm := &mockLLM{
		responses: []*claude.Response{
			{TextContent: "Done."},
		},
	}
	store := &mockStore{convID: "conv-7"}
	ctxb := &mockContextProvider{}
	router := &mockToolRouter{}
	tg := newMockTelegram()

	actor := newTestActor(llm, store, ctxb, router, slowVector, tg)

	msg := telegram.IncomingMessage{ChatID: 100, UserID: 42, Text: "index me"}
	if err := actor.handleMessage(context.Background(), msg); err != nil {
		t.Fatalf("handleMessage: %v", err)
	}

	// Goroutine should still be running (100ms delay).
	// Wait for it via the WaitGroup.
	done := make(chan struct{})
	go func() {
		actor.indexWg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Good, goroutine finished.
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for index goroutine")
	}

	slowVector.mu.Lock()
	if len(slowVector.indexed) != 1 {
		t.Errorf("expected 1 indexed message, got %d", len(slowVector.indexed))
	}
	slowVector.mu.Unlock()
}

type slowVectorIndexer struct {
	delay   time.Duration
	mu      sync.Mutex
	indexed []string
}

func (m *slowVectorIndexer) Index(_ context.Context, _, text string, _, _ int64, _ string) error {
	time.Sleep(m.delay)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.indexed = append(m.indexed, text)
	return nil
}

// --- Streaming & Image Tests ---

// collectOutbox drains messages from the outbox channel until timeout,
// returning all collected messages.
func collectOutbox(outbox <-chan telegram.OutgoingMessage, timeout time.Duration) []telegram.OutgoingMessage {
	var msgs []telegram.OutgoingMessage
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case msg := <-outbox:
			msgs = append(msgs, msg)
			// Reset timer after each message to catch any trailing edits.
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(timeout)
		case <-timer.C:
			return msgs
		}
	}
}

func TestHandleMessage_StreamingText(t *testing.T) {
	// Mock LLM fires OnPartialText with multiple deltas, then returns the
	// full text. The session's streamState should produce an initial send
	// (getting a message ID back via ResultCh) followed by edits to that
	// same message ID.
	llm := &mockLLM{
		entries: []mockLLMEntry{
			{
				deltas: []string{"Hello", ", ", "world", "!"},
				resp:   &claude.Response{TextContent: "Hello, world!"},
			},
		},
	}
	store := &mockStore{convID: "conv-stream-1"}
	ctxb := &mockContextProvider{}
	router := &mockToolRouter{}
	tg := newMockTelegram()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	outbox := tg.drainInbox(ctx)

	actor := newTestActor(llm, store, ctxb, router, nil, tg)

	msg := telegram.IncomingMessage{ChatID: 200, UserID: 42, Text: "stream test"}
	if err := actor.handleMessage(context.Background(), msg); err != nil {
		t.Fatalf("handleMessage: %v", err)
	}

	// Collect all outgoing messages (initial send + edits).
	msgs := collectOutbox(outbox, 500*time.Millisecond)
	if len(msgs) == 0 {
		t.Fatal("expected at least 1 outgoing message from streaming")
	}

	// The first message should be a fresh send (MessageID == 0) with ResultCh.
	first := msgs[0]
	if first.MessageID != 0 {
		t.Errorf("first message should be a new send (MessageID=0), got %d", first.MessageID)
	}
	if first.ChatID != 200 {
		t.Errorf("first message ChatID = %d, want 200", first.ChatID)
	}

	// If there are subsequent messages, they should be edits to the same ID.
	// The drainInbox helper assigned incrementing IDs starting at 1001.
	// The first ResultCh gets 1001, so edits should reference 1001.
	for i := 1; i < len(msgs); i++ {
		if msgs[i].MessageID == 0 {
			t.Errorf("message[%d] should be an edit (MessageID != 0), got 0", i)
		}
	}

	// The final message text should contain the full accumulated content.
	last := msgs[len(msgs)-1]
	if last.Text != "Hello, world!" {
		t.Errorf("final message text = %q, want %q", last.Text, "Hello, world!")
	}

	// Verify no fallback trySend happened (streaming covered the output).
	// The outbox should be drained at this point.
	select {
	case extra := <-outbox:
		t.Errorf("unexpected extra message: %q", extra.Text)
	case <-time.After(100 * time.Millisecond):
		// Good, no extra messages.
	}
}

func TestHandleMessage_StreamingWithToolUse(t *testing.T) {
	// First Claude call: streams text deltas + returns tool call.
	// Second Claude call: returns final text (new message).
	llm := &mockLLM{
		entries: []mockLLMEntry{
			{
				deltas: []string{"Let me ", "search..."},
				resp: &claude.Response{
					TextContent: "Let me search...",
					ToolCalls: []claude.ToolCall{
						{ID: "tc-stream-1", Name: "search__web", Input: json.RawMessage(`{"q":"test"}`)},
					},
				},
			},
			{
				deltas: []string{"Here ", "are results."},
				resp:   &claude.Response{TextContent: "Here are results."},
			},
		},
	}
	store := &mockStore{convID: "conv-stream-2"}
	ctxb := &mockContextProvider{}
	router := &mockToolRouter{
		callFn: func(_ context.Context, name string, _ map[string]any) (string, error) {
			return "some results", nil
		},
	}
	tg := newMockTelegram()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	outbox := tg.drainInbox(ctx)

	actor := newTestActor(llm, store, ctxb, router, nil, tg)

	msg := telegram.IncomingMessage{ChatID: 300, UserID: 42, Text: "search something"}
	if err := actor.handleMessage(context.Background(), msg); err != nil {
		t.Fatalf("handleMessage: %v", err)
	}

	// Collect all outgoing messages.
	msgs := collectOutbox(outbox, 500*time.Millisecond)
	if len(msgs) < 2 {
		t.Fatalf("expected at least 2 outgoing messages (first round + second round), got %d", len(msgs))
	}

	// Claude should have been called twice.
	llm.mu.Lock()
	if len(llm.calls) != 2 {
		t.Fatalf("expected 2 Claude calls, got %d", len(llm.calls))
	}
	llm.mu.Unlock()

	// Tool should have been called once.
	router.mu.Lock()
	if router.callCount != 1 {
		t.Errorf("expected 1 tool call, got %d", router.callCount)
	}
	router.mu.Unlock()

	// Find messages from the second round (after tool execution).
	// The stream resets between rounds, so the second round produces
	// a new send (MessageID == 0) with fresh text.
	var secondRoundMsgs []telegram.OutgoingMessage
	seenFirstSend := false
	for _, m := range msgs {
		if m.MessageID == 0 && m.ResultCh != nil {
			if seenFirstSend {
				secondRoundMsgs = append(secondRoundMsgs, m)
			}
			seenFirstSend = true
		} else if seenFirstSend && len(secondRoundMsgs) > 0 {
			secondRoundMsgs = append(secondRoundMsgs, m)
		}
	}

	// The final message from the second round should contain the final answer.
	lastMsg := msgs[len(msgs)-1]
	if lastMsg.Text != "Here are results." {
		t.Errorf("final text = %q, want %q", lastMsg.Text, "Here are results.")
	}
}

func TestHandleMessage_StreamingError(t *testing.T) {
	// Mock LLM fires some deltas, then returns an error.
	// The partial text should get "[error: response incomplete]" appended.
	llm := &mockLLM{
		entries: []mockLLMEntry{
			{
				deltas: []string{"Partial ", "response"},
				err:    fmt.Errorf("connection reset"),
			},
		},
	}
	store := &mockStore{convID: "conv-stream-err"}
	ctxb := &mockContextProvider{}
	router := &mockToolRouter{}
	tg := newMockTelegram()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	outbox := tg.drainInbox(ctx)

	actor := newTestActor(llm, store, ctxb, router, nil, tg)

	msg := telegram.IncomingMessage{ChatID: 400, UserID: 42, Text: "error test"}
	err := actor.handleMessage(context.Background(), msg)
	if err == nil {
		t.Fatal("expected error from handleMessage when Claude fails mid-stream")
	}

	// Collect outgoing messages. Should include the partial text + error notice.
	msgs := collectOutbox(outbox, 500*time.Millisecond)
	if len(msgs) == 0 {
		t.Fatal("expected at least 1 outgoing message with error notice")
	}

	// The last message should contain the error marker appended to partial text.
	last := msgs[len(msgs)-1]
	if !strings.Contains(last.Text, "[error: response incomplete]") {
		t.Errorf("expected error notice in text, got %q", last.Text)
	}
	if !strings.Contains(last.Text, "Partial response") {
		t.Errorf("expected partial text preserved, got %q", last.Text)
	}
}

func TestHandleMessage_ImageMessage(t *testing.T) {
	// Send a message with Photos populated. Verify Claude receives content
	// blocks containing both text and image.
	llm := &mockLLM{
		responses: []*claude.Response{
			{TextContent: "Nice photo!"},
		},
	}
	store := &mockStore{convID: "conv-img-1"}
	ctxb := &mockContextProvider{
		// Provide a history message so toAnthropicMessages produces a
		// user message that handleMessage can replace with multimodal blocks.
		history: []memory.Message{
			{Role: "user", Content: json.RawMessage(`"describe this image"`)},
		},
	}
	router := &mockToolRouter{}
	tg := newMockTelegram()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	outbox := tg.drainInbox(ctx)

	actor := newTestActor(llm, store, ctxb, router, nil, tg)

	// Construct a message with a photo.
	photoData := []byte("fake-jpeg-data-for-test")
	msg := telegram.IncomingMessage{
		ChatID: 500,
		UserID: 42,
		Text:   "describe this image",
		Photos: []telegram.Photo{
			{
				Data:     photoData,
				MimeType: "image/jpeg",
				FileID:   "file-123",
				Width:    800,
				Height:   600,
			},
		},
	}
	if err := actor.handleMessage(context.Background(), msg); err != nil {
		t.Fatalf("handleMessage: %v", err)
	}

	// Verify Claude was called with multimodal content blocks.
	llm.mu.Lock()
	defer llm.mu.Unlock()
	if len(llm.calls) != 1 {
		t.Fatalf("expected 1 Claude call, got %d", len(llm.calls))
	}

	call := llm.calls[0]
	if len(call.Messages) == 0 {
		t.Fatal("expected at least 1 message in Claude call")
	}

	// The last message should be the user message with multimodal blocks.
	lastMsg := call.Messages[len(call.Messages)-1]

	// Serialize to JSON for inspection since the SDK types use union wrappers.
	msgJSON, err := json.Marshal(lastMsg)
	if err != nil {
		t.Fatalf("failed to marshal message: %v", err)
	}
	msgStr := string(msgJSON)

	// Verify the message contains a text block with the user's text.
	if !strings.Contains(msgStr, "describe this image") {
		t.Errorf("expected text block in message, got %s", msgStr)
	}

	// Verify the message contains base64-encoded image data.
	expectedB64 := base64.StdEncoding.EncodeToString(photoData)
	if !strings.Contains(msgStr, expectedB64) {
		t.Errorf("expected base64 image data in message, got %s", msgStr)
	}

	// Verify the message contains the mime type.
	if !strings.Contains(msgStr, "image/jpeg") {
		t.Errorf("expected image/jpeg mime type in message, got %s", msgStr)
	}

	// Verify response was sent to Telegram.
	select {
	case out := <-outbox:
		if out.Text != "Nice photo!" {
			t.Errorf("telegram text = %q, want %q", out.Text, "Nice photo!")
		}
	case <-time.After(time.Second):
		t.Fatal("expected telegram message within 1s")
	}
}
