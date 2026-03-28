package session

import (
	"context"
	"encoding/json"
	"fmt"
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

type mockLLM struct {
	mu        sync.Mutex
	responses []*claude.Response // queued responses, shifted on each call
	calls     []claude.SendParams
}

func (m *mockLLM) SendStreaming(_ context.Context, params claude.SendParams) (*claude.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, params)
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

func defaultCfg() *config.Config {
	return &config.Config{
		Timezone: "UTC",
	}
}

func newTestActor(llm LLMClient, store MessageStore, ctxb ContextProvider, router ToolRouter, vector VectorIndexer, tg TelegramTransport) *Actor {
	return &Actor{
		cfg:    defaultCfg(),
		claude: llm,
		tg:     tg,
		mcp:    router,
		store:  store,
		ctxb:   ctxb,
		skills: skills.NewRegistry(),
		vector: vector,
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
	case out := <-tg.inbox:
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
	case out := <-tg.inbox:
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
	case out := <-tg.inbox:
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
	case out := <-tg.inbox:
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
