package claude

import (
	"encoding/json"
	"fmt"
	"testing"
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
	r := scanResult{line: []byte("hello"), ok: true, err: nil}
	if !r.ok {
		t.Error("expected ok=true")
	}
	if string(r.line) != "hello" {
		t.Errorf("line = %q, want hello", r.line)
	}
	if r.err != nil {
		t.Errorf("err = %v, want nil", r.err)
	}

	// Verify error-carrying variant.
	errResult := scanResult{ok: false, err: fmt.Errorf("read failed")}
	if errResult.ok {
		t.Error("expected ok=false for error result")
	}
	if errResult.err == nil || errResult.err.Error() != "read failed" {
		t.Errorf("err = %v, want 'read failed'", errResult.err)
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
