package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

func TestParseStreamEvent_SystemInit(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"init","session_id":"sess-123","model":"claude-sonnet-4-6-20250514","tools":["Read","Write"],"claude_code_version":"2.1.87"}`)

	event, err := parseStreamEvent(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	init, ok := event.(SystemInitEvent)
	if !ok {
		t.Fatalf("expected SystemInitEvent, got %T", event)
	}
	if init.SessionID != "sess-123" {
		t.Errorf("session_id = %q, want %q", init.SessionID, "sess-123")
	}
	if init.Model != "claude-sonnet-4-6-20250514" {
		t.Errorf("model = %q, want %q", init.Model, "claude-sonnet-4-6-20250514")
	}
	if len(init.Tools) != 2 {
		t.Errorf("tools = %d, want 2", len(init.Tools))
	}
	if init.Version != "2.1.87" {
		t.Errorf("version = %q, want %q", init.Version, "2.1.87")
	}
}

func TestParseStreamEvent_TextDelta(t *testing.T) {
	line := []byte(`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello "}}}`)

	event, err := parseStreamEvent(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	td, ok := event.(TextDeltaEvent)
	if !ok {
		t.Fatalf("expected TextDeltaEvent, got %T", event)
	}
	if td.Text != "Hello " {
		t.Errorf("text = %q, want %q", td.Text, "Hello ")
	}
}

func TestParseStreamEvent_TextDelta_NonTextSkipped(t *testing.T) {
	// input_json_delta events should be silently skipped.
	line := []byte(`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"key\":"}}}`)

	event, err := parseStreamEvent(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event != nil {
		t.Errorf("expected nil for non-text delta, got %T", event)
	}
}

func TestParseStreamEvent_ToolUseStart(t *testing.T) {
	line := []byte(`{"type":"stream_event","event":{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_abc","name":"install_plugin","input":{}}}}`)

	event, err := parseStreamEvent(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tu, ok := event.(ToolUseStartEvent)
	if !ok {
		t.Fatalf("expected ToolUseStartEvent, got %T", event)
	}
	if tu.Name != "install_plugin" {
		t.Errorf("name = %q, want %q", tu.Name, "install_plugin")
	}
}

func TestParseStreamEvent_ToolUseStart_TextBlockIgnored(t *testing.T) {
	// content_block_start with type "text" should NOT produce a ToolUseStartEvent.
	line := []byte(`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}`)

	event, err := parseStreamEvent(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event != nil {
		t.Errorf("expected nil for text content_block_start, got %T", event)
	}
}

func TestParseStreamEvent_ToolUseStart_EmptyName(t *testing.T) {
	// content_block_start with tool_use but empty name should be skipped.
	line := []byte(`{"type":"stream_event","event":{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_abc","name":"","input":{}}}}`)

	event, err := parseStreamEvent(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event != nil {
		t.Errorf("expected nil for empty tool name, got %T", event)
	}
}

func TestParseStreamEvent_AssistantMessage_TextOnly(t *testing.T) {
	line := []byte(`{"type":"assistant","uuid":"msg-1","session_id":"sess-1","message":{"id":"msg-1","type":"message","role":"assistant","content":[{"type":"text","text":"Hello, world!"}],"model":"claude-sonnet-4-6-20250514","stop_reason":"end_turn"}}`)

	event, err := parseStreamEvent(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	am, ok := event.(AssistantMessageEvent)
	if !ok {
		t.Fatalf("expected AssistantMessageEvent, got %T", event)
	}
	if am.TextContent != "Hello, world!" {
		t.Errorf("text = %q, want %q", am.TextContent, "Hello, world!")
	}
	if len(am.ToolCalls) != 0 {
		t.Errorf("tool_calls = %d, want 0", len(am.ToolCalls))
	}
}

func TestParseStreamEvent_AssistantMessage_WithToolUse(t *testing.T) {
	line := []byte(`{"type":"assistant","uuid":"msg-2","session_id":"sess-1","message":{"id":"msg-2","type":"message","role":"assistant","content":[{"type":"text","text":"Let me search for that."},{"type":"tool_use","id":"toolu_1","name":"search","input":{"query":"golang testing"}}],"model":"claude-sonnet-4-6-20250514","stop_reason":"tool_use"}}`)

	event, err := parseStreamEvent(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	am, ok := event.(AssistantMessageEvent)
	if !ok {
		t.Fatalf("expected AssistantMessageEvent, got %T", event)
	}
	if am.TextContent != "Let me search for that." {
		t.Errorf("text = %q, want %q", am.TextContent, "Let me search for that.")
	}
	if len(am.ToolCalls) != 1 {
		t.Fatalf("tool_calls = %d, want 1", len(am.ToolCalls))
	}
	tc := am.ToolCalls[0]
	if tc.ID != "toolu_1" {
		t.Errorf("tool ID = %q, want %q", tc.ID, "toolu_1")
	}
	if tc.Name != "search" {
		t.Errorf("tool name = %q, want %q", tc.Name, "search")
	}

	var input map[string]string
	if err := json.Unmarshal(tc.Input, &input); err != nil {
		t.Fatalf("unmarshal input: %v", err)
	}
	if input["query"] != "golang testing" {
		t.Errorf("input query = %q, want %q", input["query"], "golang testing")
	}
}

func TestParseStreamEvent_Result_Success(t *testing.T) {
	line := []byte(`{"type":"result","subtype":"success","uuid":"r-1","session_id":"sess-1","duration_ms":2500,"duration_api_ms":2000,"is_error":false,"num_turns":3,"result":"Done!","total_cost_usd":0.015,"usage":{"input_tokens":100,"output_tokens":50}}`)

	event, err := parseStreamEvent(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	r, ok := event.(ResultEvent)
	if !ok {
		t.Fatalf("expected ResultEvent, got %T", event)
	}
	if r.Subtype != "success" {
		t.Errorf("subtype = %q, want %q", r.Subtype, "success")
	}
	if r.Result != "Done!" {
		t.Errorf("result = %q, want %q", r.Result, "Done!")
	}
	if r.Cost != 0.015 {
		t.Errorf("cost = %f, want %f", r.Cost, 0.015)
	}
	if r.Turns != 3 {
		t.Errorf("turns = %d, want %d", r.Turns, 3)
	}
	if r.IsError {
		t.Error("is_error should be false")
	}
}

func TestParseStreamEvent_Result_Error(t *testing.T) {
	line := []byte(`{"type":"result","subtype":"error_max_turns","uuid":"r-2","session_id":"sess-1","duration_ms":5000,"is_error":true,"num_turns":10,"total_cost_usd":0.1,"errors":["exceeded max turns"]}`)

	event, err := parseStreamEvent(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	r, ok := event.(ResultEvent)
	if !ok {
		t.Fatalf("expected ResultEvent, got %T", event)
	}
	if r.Subtype != "error_max_turns" {
		t.Errorf("subtype = %q, want %q", r.Subtype, "error_max_turns")
	}
	if !r.IsError {
		t.Error("is_error should be true")
	}
	if len(r.Errors) != 1 || r.Errors[0] != "exceeded max turns" {
		t.Errorf("errors = %v, want [exceeded max turns]", r.Errors)
	}
}

func TestParseStreamEvent_UnknownType(t *testing.T) {
	line := []byte(`{"type":"some_future_event","data":"whatever"}`)

	event, err := parseStreamEvent(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event != nil {
		t.Errorf("expected nil for unknown type, got %T", event)
	}
}

func TestParseStreamEvent_MalformedJSON(t *testing.T) {
	line := []byte(`not valid json at all`)

	_, err := parseStreamEvent(line)
	if err == nil {
		t.Error("expected error for malformed JSON")
	}
}

func TestParseStreamEvent_EmptyLine(t *testing.T) {
	// Empty lines should not cause errors at the caller level.
	line := []byte(`{}`)

	event, err := parseStreamEvent(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event != nil {
		t.Errorf("expected nil for empty envelope, got %T", event)
	}
}

func TestParseStreamEvent_SystemNonInit(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"compact_boundary","uuid":"c-1","session_id":"sess-1"}`)

	event, err := parseStreamEvent(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event != nil {
		t.Errorf("expected nil for non-init system event, got %T", event)
	}
}

func TestBuildUserMessage(t *testing.T) {
	msg := BuildUserMessage("Hello!")

	var parsed map[string]any
	if err := json.Unmarshal(msg, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if parsed["type"] != "user" {
		t.Errorf("type = %q, want %q", parsed["type"], "user")
	}

	message, ok := parsed["message"].(map[string]any)
	if !ok {
		t.Fatal("message field missing or wrong type")
	}
	if message["role"] != "user" {
		t.Errorf("role = %q, want %q", message["role"], "user")
	}

	content, ok := message["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("content = %v, want 1 element", message["content"])
	}

	block, ok := content[0].(map[string]any)
	if !ok {
		t.Fatal("content block wrong type")
	}
	if block["type"] != "text" {
		t.Errorf("block type = %q, want %q", block["type"], "text")
	}
	if block["text"] != "Hello!" {
		t.Errorf("block text = %q, want %q", block["text"], "Hello!")
	}
}

func TestScanResult_Fields(t *testing.T) {
	r := ScanResult{Line: []byte("hello"), OK: true, Err: nil}
	if !r.OK {
		t.Error("expected OK=true")
	}
	if string(r.Line) != "hello" {
		t.Errorf("Line = %q, want hello", r.Line)
	}
	if r.Err != nil {
		t.Errorf("Err = %v, want nil", r.Err)
	}

	// Verify error-carrying variant.
	errResult := ScanResult{OK: false, Err: fmt.Errorf("read failed")}
	if errResult.OK {
		t.Error("expected OK=false for error result")
	}
	if errResult.Err == nil || errResult.Err.Error() != "read failed" {
		t.Errorf("Err = %v, want 'read failed'", errResult.Err)
	}
}

func TestReplaceEnv_ExistingKey(t *testing.T) {
	env := []string{"HOME=/old", "PATH=/usr/bin", "USER=test"}
	result := replaceEnv(env, "HOME", "/new")

	found := false
	for _, e := range result {
		if e == "HOME=/new" {
			found = true
		}
		if e == "HOME=/old" {
			t.Error("old HOME value should be replaced")
		}
	}
	if !found {
		t.Error("HOME=/new not found in result")
	}
	if len(result) != 3 {
		t.Errorf("len = %d, want 3 (replace in place)", len(result))
	}
}

func TestReplaceEnv_NewKey(t *testing.T) {
	env := []string{"PATH=/usr/bin", "USER=test"}
	result := replaceEnv(env, "HOME", "/home/user")

	if len(result) != 3 {
		t.Errorf("len = %d, want 3 (appended)", len(result))
	}
	found := false
	for _, e := range result {
		if e == "HOME=/home/user" {
			found = true
		}
	}
	if !found {
		t.Error("HOME=/home/user not found in result")
	}
}

func TestReplaceEnv_EmptyEnv(t *testing.T) {
	result := replaceEnv(nil, "KEY", "value")
	if len(result) != 1 || result[0] != "KEY=value" {
		t.Errorf("result = %v, want [KEY=value]", result)
	}
}

func TestReplaceEnv_SimilarPrefix(t *testing.T) {
	// Ensure HOME_DIR is not matched when replacing HOME.
	env := []string{"HOME_DIR=/data", "HOME=/old"}
	result := replaceEnv(env, "HOME", "/new")

	if len(result) != 2 {
		t.Errorf("len = %d, want 2", len(result))
	}
	for _, e := range result {
		if e == "HOME_DIR=/data" {
			continue // should be untouched
		}
		if e == "HOME=/new" {
			continue // correctly replaced
		}
		t.Errorf("unexpected entry: %q", e)
	}
}

func TestBuildImageMessage(t *testing.T) {
	msg := BuildImageMessage("What's this?", []ImageBlock{
		{MediaType: "image/jpeg", Data: "base64data"},
	})

	var parsed map[string]any
	if err := json.Unmarshal(msg, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	message := parsed["message"].(map[string]any)
	content := message["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("content = %d elements, want 2", len(content))
	}

	textBlock := content[0].(map[string]any)
	if textBlock["type"] != "text" {
		t.Errorf("first block type = %q, want %q", textBlock["type"], "text")
	}

	imgBlock := content[1].(map[string]any)
	if imgBlock["type"] != "image" {
		t.Errorf("second block type = %q, want %q", imgBlock["type"], "image")
	}

	source := imgBlock["source"].(map[string]any)
	if source["type"] != "base64" {
		t.Errorf("source type = %q, want %q", source["type"], "base64")
	}
	if source["media_type"] != "image/jpeg" {
		t.Errorf("media_type = %q, want %q", source["media_type"], "image/jpeg")
	}
}

func TestFilteredSpawnEnv_ExcludesSecrets(t *testing.T) {
	// Set a secret that should be filtered out.
	t.Setenv("CURLYCATCLAW_MASTER_KEY", "deadbeef")
	t.Setenv("TELEGRAM_TOKEN", "123456:ABC")
	// Set an allowed var.
	t.Setenv("TZ", "America/Los_Angeles")

	env := filteredSpawnEnv()

	envMap := make(map[string]string)
	for _, e := range env {
		if k, v, ok := strings.Cut(e, "="); ok {
			envMap[k] = v
		}
	}

	// Secrets must NOT be present.
	if _, ok := envMap["CURLYCATCLAW_MASTER_KEY"]; ok {
		t.Error("CURLYCATCLAW_MASTER_KEY should be filtered from CLI subprocess env")
	}
	if _, ok := envMap["TELEGRAM_TOKEN"]; ok {
		t.Error("TELEGRAM_TOKEN should be filtered from CLI subprocess env")
	}

	// Allowed vars must be present.
	if envMap["TZ"] != "America/Los_Angeles" {
		t.Errorf("TZ should be in filtered env, got %q", envMap["TZ"])
	}

	// PATH must always be present (baseline).
	if _, ok := envMap["PATH"]; !ok {
		// PATH is almost always set, but on some CI it might not be.
		// Only fail if it was set in the original env.
		if os.Getenv("PATH") != "" {
			t.Error("PATH should be in filtered env")
		}
	}
}

func TestNewCLIManager_EffortField(t *testing.T) {
	mgr := NewCLIManager("/usr/bin/claude", "claude-sonnet-4-6-20250514", "high", "tok")
	if mgr.effort != "high" {
		t.Errorf("effort = %q, want %q", mgr.effort, "high")
	}
}

func TestNewCLIManager_EmptyEffort(t *testing.T) {
	mgr := NewCLIManager("/usr/bin/claude", "claude-sonnet-4-6-20250514", "", "tok")
	if mgr.effort != "" {
		t.Errorf("effort = %q, want empty", mgr.effort)
	}
}

// --- extractUserText tests ---

func TestExtractUserText_SingleMessage(t *testing.T) {
	params := SendParams{
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock("hello world")),
		},
	}
	got := extractUserText(params)
	if got != "hello world" {
		t.Errorf("extractUserText = %q, want %q", got, "hello world")
	}
}

func TestExtractUserText_Empty(t *testing.T) {
	got := extractUserText(SendParams{})
	if got != "" {
		t.Errorf("extractUserText = %q, want empty", got)
	}
}

func TestExtractUserText_SkipsAssistant(t *testing.T) {
	params := SendParams{
		Messages: []anthropic.MessageParam{
			anthropic.NewAssistantMessage(anthropic.NewTextBlock("I am assistant")),
			anthropic.NewUserMessage(anthropic.NewTextBlock("user text")),
		},
	}
	got := extractUserText(params)
	if got != "user text" {
		t.Errorf("extractUserText = %q, want %q", got, "user text")
	}
}

// --- responseFromEvents tests ---

func TestResponseFromEvents_AssistantMessage(t *testing.T) {
	events := []CLIEvent{
		AssistantMessageEvent{TextContent: "extracted result"},
		ResultEvent{Subtype: "success"},
	}
	resp, err := responseFromEvents(events)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.TextContent != "extracted result" {
		t.Errorf("TextContent = %q, want %q", resp.TextContent, "extracted result")
	}
}

func TestResponseFromEvents_FallbackToResult(t *testing.T) {
	events := []CLIEvent{
		ResultEvent{Subtype: "success", Result: "fallback text"},
	}
	resp, err := responseFromEvents(events)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.TextContent != "fallback text" {
		t.Errorf("TextContent = %q, want %q", resp.TextContent, "fallback text")
	}
}

func TestResponseFromEvents_Error(t *testing.T) {
	events := []CLIEvent{
		ResultEvent{Subtype: "error_max_turns", IsError: true, Errors: []string{"max turns exceeded"}},
	}
	_, err := responseFromEvents(events)
	if err == nil {
		t.Fatal("expected error for IsError result")
	}
	if !strings.Contains(err.Error(), "max turns exceeded") {
		t.Errorf("error = %q, want to contain 'max turns exceeded'", err.Error())
	}
}

func TestResponseFromEvents_MultipleAssistant(t *testing.T) {
	events := []CLIEvent{
		AssistantMessageEvent{TextContent: "part1"},
		AssistantMessageEvent{TextContent: "part2"},
		ResultEvent{Subtype: "success"},
	}
	resp, err := responseFromEvents(events)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.TextContent != "part1\npart2" {
		t.Errorf("TextContent = %q, want %q", resp.TextContent, "part1\npart2")
	}
}

func TestResponseFromEvents_Empty(t *testing.T) {
	resp, err := responseFromEvents(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.TextContent != "" {
		t.Errorf("TextContent = %q, want empty", resp.TextContent)
	}
}

// --- PersistentCLISender tests ---

// mockIngestCLI tracks GetOrCreate/Remove calls for testing.
type mockIngestCLI struct {
	mu          sync.Mutex
	getOrCreate func(ctx context.Context, userID, chatID int64, params SpawnParams) (*CLIProcess, bool, error)
	removes     []int64 // chatIDs passed to Remove
}

func (m *mockIngestCLI) GetOrCreate(ctx context.Context, userID, chatID int64, params SpawnParams) (*CLIProcess, bool, error) {
	return m.getOrCreate(ctx, userID, chatID, params)
}

func (m *mockIngestCLI) Remove(userID, chatID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removes = append(m.removes, chatID)
}

// newTestCLIProcess creates a CLIProcess with a pre-loaded scanCh for testing.
// Set isNew=true to simulate a freshly spawned process (initMsgSent=true,
// so Send() skips the stdin write).
func newTestCLIProcess(isNew bool, events ...[]byte) *CLIProcess {
	scanCh := make(chan ScanResult, len(events))
	for _, ev := range events {
		scanCh <- ScanResult{Line: ev, OK: true}
	}
	done := make(chan struct{})
	// Provide a writable stdin so Send() doesn't panic on reused processes.
	r, w, _ := os.Pipe()
	return &CLIProcess{
		scanCh:      scanCh,
		done:        done,
		stdin:       w,
		stdout:      r,
		initMsgSent: isNew,
	}
}

func TestPersistentCLISender_Send_Success(t *testing.T) {
	assistantLine := []byte(`{"type":"assistant","message":{"id":"m1","type":"message","role":"assistant","content":[{"type":"text","text":"{\"valuable\":true}"}],"stop_reason":"end_turn"}}`)
	resultLine := []byte(`{"type":"result","subtype":"success","is_error":false,"result":""}`)

	proc := newTestCLIProcess(true, assistantLine, resultLine)

	mock := &mockIngestCLI{
		getOrCreate: func(_ context.Context, _, _ int64, _ SpawnParams) (*CLIProcess, bool, error) {
			return proc, true, nil
		},
	}

	sender := &PersistentCLISender{
		mgr:      mock,
		model:    "test-model",
		maxTurns: 20,
		turns:    make(map[int64]int),
	}

	resp, err := sender.Send(context.Background(), SendParams{
		SystemPrompt: "You are an email analyst",
		Messages:     []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("test email"))},
	})
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}
	if resp.TextContent != `{"valuable":true}` {
		t.Errorf("TextContent = %q, want %q", resp.TextContent, `{"valuable":true}`)
	}
}

func TestPersistentCLISender_Send_TrustSeparation(t *testing.T) {
	resultLine := []byte(`{"type":"result","subtype":"success","is_error":false,"result":"ok"}`)

	var gotChatIDs []int64
	mock := &mockIngestCLI{
		getOrCreate: func(_ context.Context, _, chatID int64, _ SpawnParams) (*CLIProcess, bool, error) {
			gotChatIDs = append(gotChatIDs, chatID)
			proc := newTestCLIProcess(true, resultLine)
			return proc, true, nil
		},
	}

	sender := &PersistentCLISender{
		mgr:      mock,
		model:    "test-model",
		maxTurns: 20,
		turns:    make(map[int64]int),
	}

	// Untrusted (email) prompt → chatID=-1
	_, err := sender.Send(context.Background(), SendParams{
		SystemPrompt: "You are an email analyst for a personal AI assistant.",
		Messages:     []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("email"))},
	})
	if err != nil {
		t.Fatalf("untrusted Send failed: %v", err)
	}

	// Trusted (notes) prompt → chatID=-2
	_, err = sender.Send(context.Background(), SendParams{
		SystemPrompt: "You are a knowledge extraction agent for a personal AI assistant.",
		Messages:     []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("note"))},
	})
	if err != nil {
		t.Fatalf("trusted Send failed: %v", err)
	}

	if len(gotChatIDs) != 2 {
		t.Fatalf("expected 2 GetOrCreate calls, got %d", len(gotChatIDs))
	}
	if gotChatIDs[0] != ingestUntrustedChatID {
		t.Errorf("first call chatID = %d, want %d", gotChatIDs[0], ingestUntrustedChatID)
	}
	if gotChatIDs[1] != ingestTrustedChatID {
		t.Errorf("second call chatID = %d, want %d", gotChatIDs[1], ingestTrustedChatID)
	}
}

func TestPersistentCLISender_Send_Recycle(t *testing.T) {
	resultLine := []byte(`{"type":"result","subtype":"success","is_error":false,"result":"ok"}`)

	callCount := 0
	mock := &mockIngestCLI{
		getOrCreate: func(_ context.Context, _, _ int64, _ SpawnParams) (*CLIProcess, bool, error) {
			callCount++
			// First call is a fresh spawn (isNew=true). All subsequent are
			// reuse (isNew=false) until Remove is called, after which the
			// next call is another fresh spawn.
			isNew := callCount == 1 || callCount == 4 // call 4 = after Remove + re-create
			proc := newTestCLIProcess(isNew, resultLine)
			return proc, isNew, nil
		},
	}

	sender := &PersistentCLISender{
		mgr:      mock,
		model:    "test-model",
		maxTurns: 2,
		turns:    make(map[int64]int),
	}

	params := SendParams{
		SystemPrompt: "You are an email analyst for a personal AI assistant.",
		Messages:     []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("email"))},
	}

	// Call 1: isNew=true, turns 0→1 (isNew doesn't reset because turns was 0)
	if _, err := sender.Send(context.Background(), params); err != nil {
		t.Fatalf("Send 0 failed: %v", err)
	}
	// Call 2: isNew=false, turns 1→2
	if _, err := sender.Send(context.Background(), params); err != nil {
		t.Fatalf("Send 1 failed: %v", err)
	}
	// Call 3: turns=2 >= maxTurns=2 → Remove called, turns reset to 0.
	// Then GetOrCreate is call 3 (isNew=false), turns 0→1.
	if _, err := sender.Send(context.Background(), params); err != nil {
		t.Fatalf("Send 2 failed: %v", err)
	}

	mock.mu.Lock()
	removeCount := len(mock.removes)
	mock.mu.Unlock()

	if removeCount != 1 {
		t.Errorf("Remove called %d times, want 1", removeCount)
	}
}

func TestPersistentCLISender_Send_ErrorRecovery(t *testing.T) {
	callCount := 0
	mock := &mockIngestCLI{
		getOrCreate: func(_ context.Context, _, _ int64, _ SpawnParams) (*CLIProcess, bool, error) {
			callCount++
			if callCount == 1 {
				// First call: return a process whose scanCh is closed (simulates death).
				proc := newTestCLIProcess(true)
				close(proc.done)
				return proc, true, nil
			}
			// Second call: return a working process.
			resultLine := []byte(`{"type":"result","subtype":"success","is_error":false,"result":"recovered"}`)
			return newTestCLIProcess(true, resultLine), true, nil
		},
	}

	sender := &PersistentCLISender{
		mgr:      mock,
		model:    "test-model",
		maxTurns: 20,
		turns:    make(map[int64]int),
	}

	params := SendParams{
		SystemPrompt: "You are an email analyst for a personal AI assistant.",
		Messages:     []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("test"))},
	}

	// First call should fail (process is dead).
	_, err := sender.Send(context.Background(), params)
	if err == nil {
		t.Fatal("expected error from dead process")
	}

	// Remove should have been called.
	mock.mu.Lock()
	if len(mock.removes) != 1 {
		t.Errorf("Remove called %d times, want 1", len(mock.removes))
	}
	mock.mu.Unlock()

	// Second call should succeed with fresh process.
	resp, err := sender.Send(context.Background(), params)
	if err != nil {
		t.Fatalf("recovery Send failed: %v", err)
	}
	if resp.TextContent != "recovered" {
		t.Errorf("TextContent = %q, want %q", resp.TextContent, "recovered")
	}
}

func TestPersistentCLISender_Send_ExternalKill(t *testing.T) {
	resultLine := []byte(`{"type":"result","subtype":"success","is_error":false,"result":"ok"}`)

	isNewSequence := []bool{false, true} // second call: process was killed externally
	callIdx := 0
	mock := &mockIngestCLI{
		getOrCreate: func(_ context.Context, _, _ int64, _ SpawnParams) (*CLIProcess, bool, error) {
			isNew := isNewSequence[callIdx]
			callIdx++
			proc := newTestCLIProcess(isNew, resultLine)
			return proc, isNew, nil
		},
	}

	sender := &PersistentCLISender{
		mgr:      mock,
		model:    "test-model",
		maxTurns: 20,
		turns:    make(map[int64]int),
	}

	chatID := ingestUntrustedChatID
	// Simulate existing turns from before the external kill.
	sender.turns[chatID] = 15

	params := SendParams{
		SystemPrompt: "You are an email analyst for a personal AI assistant.",
		Messages:     []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("test"))},
	}

	// First call: isNew=false, turns stay at 15, then incremented to 16.
	if _, err := sender.Send(context.Background(), params); err != nil {
		t.Fatalf("Send 0 failed: %v", err)
	}
	if sender.turns[chatID] != 16 {
		t.Errorf("turns after first send = %d, want 16", sender.turns[chatID])
	}

	// Second call: isNew=true (externally killed), turns should reset then increment to 1.
	if _, err := sender.Send(context.Background(), params); err != nil {
		t.Fatalf("Send 1 failed: %v", err)
	}
	if sender.turns[chatID] != 1 {
		t.Errorf("turns after external kill = %d, want 1", sender.turns[chatID])
	}
}

func TestPersistentCLISender_Send_NoUserText(t *testing.T) {
	sender := &PersistentCLISender{
		mgr:      &mockIngestCLI{},
		maxTurns: 20,
		turns:    make(map[int64]int),
	}

	_, err := sender.Send(context.Background(), SendParams{})
	if err == nil {
		t.Fatal("expected error for empty messages")
	}
	if !strings.Contains(err.Error(), "no user text") {
		t.Errorf("error = %q, want to contain 'no user text'", err.Error())
	}
}

func TestNewPersistentCLISender_DefaultMaxTurns(t *testing.T) {
	mgr := NewCLIManager("/usr/bin/claude", "model", "", "tok")
	sender := NewPersistentCLISender(mgr, "model", 0)
	if sender.maxTurns != defaultMaxTurns {
		t.Errorf("maxTurns = %d, want %d", sender.maxTurns, defaultMaxTurns)
	}
}

func TestSpawn_SafeMode(t *testing.T) {
	// We can't easily test spawn() directly (requires a real CLI binary),
	// but we can verify SpawnParams.SafeMode is plumbed through by checking
	// that PersistentCLISender passes SafeMode: true in SpawnParams.
	var gotParams SpawnParams
	mock := &mockIngestCLI{
		getOrCreate: func(_ context.Context, _, _ int64, params SpawnParams) (*CLIProcess, bool, error) {
			gotParams = params
			resultLine := []byte(`{"type":"result","subtype":"success","is_error":false,"result":"ok"}`)
			return newTestCLIProcess(true, resultLine), true, nil
		},
	}

	sender := &PersistentCLISender{
		mgr:      mock,
		model:    "test-model",
		maxTurns: 20,
		turns:    make(map[int64]int),
	}

	_, err := sender.Send(context.Background(), SendParams{
		SystemPrompt: "test prompt",
		Messages:     []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("test"))},
	})
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	if !gotParams.SafeMode {
		t.Error("SpawnParams.SafeMode = false, want true")
	}
	if gotParams.SystemPrompt != "test prompt" {
		t.Errorf("SpawnParams.SystemPrompt = %q, want %q", gotParams.SystemPrompt, "test prompt")
	}
	if gotParams.Model != "test-model" {
		t.Errorf("SpawnParams.Model = %q, want %q", gotParams.Model, "test-model")
	}
}
