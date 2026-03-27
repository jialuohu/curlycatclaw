package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// ---------- test helpers ----------

// sseEvent formats a single SSE frame.
func sseEvent(eventType, data string) string {
	return fmt.Sprintf("event: %s\ndata: %s\n\n", eventType, data)
}

// newTestServer creates an httptest.Server that responds with the given SSE
// event sequence on any POST to /v1/messages. The server returns
// Content-Type: text/event-stream.
func newTestServer(t *testing.T, events []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, ev := range events {
			fmt.Fprint(w, ev)
		}
	}))
}

// newErrorServer returns a server that replies with the given HTTP status code
// and a JSON error body.
func newErrorServer(t *testing.T, statusCode int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		fmt.Fprint(w, body)
	}))
}

// testClient builds a *Client pointing at the given base URL.
func testClient(baseURL string) *Client {
	return NewClient("test-key", "claude-sonnet-4-6-20250514",
		option.WithBaseURL(baseURL),
	)
}

// ---------- canned SSE payloads ----------

// simpleTextEvents returns an SSE sequence for a plain text response.
func simpleTextEvents(text string) []string {
	parts := splitTextForStream(text)
	events := []string{
		sseEvent("message_start", `{"type":"message_start","message":{"id":"msg_test","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-6-20250514","stop_reason":null,"usage":{"input_tokens":10,"output_tokens":0}}}`),
		sseEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
	}
	for _, p := range parts {
		escaped, _ := json.Marshal(p)
		events = append(events, sseEvent("content_block_delta",
			fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%s}}`, string(escaped))))
	}
	events = append(events,
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":0}`),
		sseEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":5}}`),
		sseEvent("message_stop", `{"type":"message_stop"}`),
	)
	return events
}

// splitTextForStream splits text roughly in half for streaming simulation.
func splitTextForStream(text string) []string {
	if len(text) <= 1 {
		return []string{text}
	}
	mid := len(text) / 2
	return []string{text[:mid], text[mid:]}
}

// singleToolUseEvents returns SSE events with one tool call and optional
// preceding text.
func singleToolUseEvents(toolID, toolName string, input json.RawMessage) []string {
	events := []string{
		sseEvent("message_start", `{"type":"message_start","message":{"id":"msg_test","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-6-20250514","stop_reason":null,"usage":{"input_tokens":10,"output_tokens":0}}}`),
		sseEvent("content_block_start", fmt.Sprintf(
			`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"%s","name":"%s","input":{}}}`,
			toolID, toolName)),
	}
	// Stream the input JSON in one chunk via input_json_delta.
	inputStr := string(input)
	escaped, _ := json.Marshal(inputStr)
	events = append(events,
		sseEvent("content_block_delta", fmt.Sprintf(
			`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":%s}}`, string(escaped))),
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":0}`),
		sseEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":20}}`),
		sseEvent("message_stop", `{"type":"message_stop"}`),
	)
	return events
}

// parallelToolUseEvents returns SSE events with multiple tool calls.
func parallelToolUseEvents(tools []struct {
	ID    string
	Name  string
	Input json.RawMessage
}) []string {
	events := []string{
		sseEvent("message_start", `{"type":"message_start","message":{"id":"msg_test","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-6-20250514","stop_reason":null,"usage":{"input_tokens":10,"output_tokens":0}}}`),
	}
	for i, tool := range tools {
		events = append(events,
			sseEvent("content_block_start", fmt.Sprintf(
				`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":"%s","name":"%s","input":{}}}`,
				i, tool.ID, tool.Name)),
		)
		inputStr := string(tool.Input)
		escaped, _ := json.Marshal(inputStr)
		events = append(events,
			sseEvent("content_block_delta", fmt.Sprintf(
				`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%s}}`,
				i, string(escaped))),
			sseEvent("content_block_stop", fmt.Sprintf(
				`{"type":"content_block_stop","index":%d}`, i)),
		)
	}
	events = append(events,
		sseEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":30}}`),
		sseEvent("message_stop", `{"type":"message_stop"}`),
	)
	return events
}

// emptyResponseEvents returns SSE events for a response with no content blocks.
func emptyResponseEvents() []string {
	return []string{
		sseEvent("message_start", `{"type":"message_start","message":{"id":"msg_test","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-6-20250514","stop_reason":null,"usage":{"input_tokens":5,"output_tokens":0}}}`),
		sseEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":0}}`),
		sseEvent("message_stop", `{"type":"message_stop"}`),
	}
}

// ---------- tests ----------

func TestSendStreaming_SimpleText(t *testing.T) {
	srv := newTestServer(t, simpleTextEvents("Hello, world!"))
	defer srv.Close()

	c := testClient(srv.URL)
	resp, err := c.SendStreaming(context.Background(), SendParams{
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock("Hi")),
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.TextContent != "Hello, world!" {
		t.Errorf("text = %q, want %q", resp.TextContent, "Hello, world!")
	}
	if resp.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q, want %q", resp.StopReason, "end_turn")
	}
	if len(resp.ToolCalls) != 0 {
		t.Errorf("tool_calls = %d, want 0", len(resp.ToolCalls))
	}
}

func TestSendStreaming_SingleToolCall(t *testing.T) {
	input := json.RawMessage(`{"city":"San Francisco"}`)
	srv := newTestServer(t, singleToolUseEvents("toolu_123", "get_weather", input))
	defer srv.Close()

	c := testClient(srv.URL)
	resp, err := c.SendStreaming(context.Background(), SendParams{
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock("What is the weather?")),
		},
		Tools: []anthropic.ToolUnionParam{{
			OfTool: &anthropic.ToolParam{
				Name:        "get_weather",
				Description: anthropic.String("Get weather for a city"),
				InputSchema: anthropic.ToolInputSchemaParam{
					Properties: map[string]any{
						"city": map[string]any{"type": "string"},
					},
					Required: []string{"city"},
				},
			},
		}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.StopReason != "tool_use" {
		t.Errorf("stop_reason = %q, want %q", resp.StopReason, "tool_use")
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("tool_calls = %d, want 1", len(resp.ToolCalls))
	}
	tc := resp.ToolCalls[0]
	if tc.ID != "toolu_123" {
		t.Errorf("tool_call.ID = %q, want %q", tc.ID, "toolu_123")
	}
	if tc.Name != "get_weather" {
		t.Errorf("tool_call.Name = %q, want %q", tc.Name, "get_weather")
	}

	var parsed map[string]string
	if err := json.Unmarshal(tc.Input, &parsed); err != nil {
		t.Fatalf("unmarshal tool input: %v", err)
	}
	if parsed["city"] != "San Francisco" {
		t.Errorf("tool input city = %q, want %q", parsed["city"], "San Francisco")
	}
}

func TestSendStreaming_ParallelToolCalls(t *testing.T) {
	tools := []struct {
		ID    string
		Name  string
		Input json.RawMessage
	}{
		{ID: "toolu_a", Name: "get_weather", Input: json.RawMessage(`{"city":"NYC"}`)},
		{ID: "toolu_b", Name: "get_time", Input: json.RawMessage(`{"timezone":"EST"}`)},
		{ID: "toolu_c", Name: "get_news", Input: json.RawMessage(`{"topic":"tech"}`)},
	}
	srv := newTestServer(t, parallelToolUseEvents(tools))
	defer srv.Close()

	c := testClient(srv.URL)
	resp, err := c.SendStreaming(context.Background(), SendParams{
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock("NYC weather, time, and tech news")),
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.StopReason != "tool_use" {
		t.Errorf("stop_reason = %q, want %q", resp.StopReason, "tool_use")
	}
	if len(resp.ToolCalls) != 3 {
		t.Fatalf("tool_calls = %d, want 3", len(resp.ToolCalls))
	}

	wantNames := []string{"get_weather", "get_time", "get_news"}
	for i, tc := range resp.ToolCalls {
		if tc.Name != wantNames[i] {
			t.Errorf("tool_calls[%d].Name = %q, want %q", i, tc.Name, wantNames[i])
		}
	}
}

func TestSendStreaming_OnPartialText(t *testing.T) {
	srv := newTestServer(t, simpleTextEvents("streaming works"))
	defer srv.Close()

	var mu sync.Mutex
	var deltas []string
	c := testClient(srv.URL)
	resp, err := c.SendStreaming(context.Background(), SendParams{
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock("stream test")),
		},
		OnPartialText: func(delta string) {
			mu.Lock()
			deltas = append(deltas, delta)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The callback should have been invoked with the deltas.
	mu.Lock()
	joined := strings.Join(deltas, "")
	mu.Unlock()

	if joined != resp.TextContent {
		t.Errorf("streamed deltas = %q, final text = %q — expected them to match", joined, resp.TextContent)
	}
	if len(deltas) == 0 {
		t.Error("OnPartialText was never called")
	}
}

func TestSendStreaming_APIError(t *testing.T) {
	t.Run("rate limit 429", func(t *testing.T) {
		srv := newErrorServer(t, 429, `{"type":"error","error":{"type":"rate_limit_error","message":"rate limited"}}`)
		defer srv.Close()

		c := testClient(srv.URL)
		_, err := c.SendStreaming(context.Background(), SendParams{
			Messages: []anthropic.MessageParam{
				anthropic.NewUserMessage(anthropic.NewTextBlock("hi")),
			},
		})
		if err == nil {
			t.Fatal("expected error, got nil")
		}

		var rlErr *RateLimitError
		if !errors.As(err, &rlErr) {
			t.Errorf("expected RateLimitError, got %T: %v", err, err)
		}
	})

	t.Run("server error 500", func(t *testing.T) {
		srv := newErrorServer(t, 500, `{"type":"error","error":{"type":"api_error","message":"internal error"}}`)
		defer srv.Close()

		c := testClient(srv.URL)
		_, err := c.SendStreaming(context.Background(), SendParams{
			Messages: []anthropic.MessageParam{
				anthropic.NewUserMessage(anthropic.NewTextBlock("hi")),
			},
		})
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "claude:") {
			t.Errorf("error should be wrapped with claude prefix: %v", err)
		}

		// Should NOT be a RateLimitError.
		var rlErr *RateLimitError
		if errors.As(err, &rlErr) {
			t.Error("500 error should not be RateLimitError")
		}
	})
}

func TestSendStreaming_EmptyResponse(t *testing.T) {
	srv := newTestServer(t, emptyResponseEvents())
	defer srv.Close()

	c := testClient(srv.URL)
	resp, err := c.SendStreaming(context.Background(), SendParams{
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock("say nothing")),
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.TextContent != "" {
		t.Errorf("text = %q, want empty", resp.TextContent)
	}
	if len(resp.ToolCalls) != 0 {
		t.Errorf("tool_calls = %d, want 0", len(resp.ToolCalls))
	}
	if resp.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q, want %q", resp.StopReason, "end_turn")
	}
}
