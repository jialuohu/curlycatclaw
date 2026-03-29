package memory

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestFormatTranscript_PlainText(t *testing.T) {
	userContent, _ := json.Marshal("How do I build this?")
	assistantContent, _ := json.Marshal("Run go build ./...")

	messages := []Message{
		{Role: "user", Content: userContent},
		{Role: "assistant", Content: assistantContent},
	}

	result := FormatTranscript(messages)

	if !strings.Contains(result, "User: How do I build this?") {
		t.Errorf("expected user message in transcript, got %q", result)
	}
	if !strings.Contains(result, "Assistant: Run go build ./...") {
		t.Errorf("expected assistant message in transcript, got %q", result)
	}
}

func TestFormatTranscript_StripToolResult(t *testing.T) {
	userContent, _ := json.Marshal("Search for X")
	toolContent, _ := json.Marshal("tool output here")
	assistantContent, _ := json.Marshal("Here is what I found")

	messages := []Message{
		{Role: "user", Content: userContent},
		{Role: "tool_result", Content: toolContent},
		{Role: "assistant", Content: assistantContent},
	}

	result := FormatTranscript(messages)

	if strings.Contains(result, "tool output here") {
		t.Errorf("tool_result content should be stripped, got %q", result)
	}
	if !strings.Contains(result, "User: Search for X") {
		t.Errorf("expected user message in transcript, got %q", result)
	}
	if !strings.Contains(result, "Assistant: Here is what I found") {
		t.Errorf("expected assistant message in transcript, got %q", result)
	}
}

func TestFormatTranscript_JSONString(t *testing.T) {
	// Content stored as a JSON-quoted string (e.g., "\"hello\"").
	content := json.RawMessage(`"This is a quoted string"`)

	messages := []Message{
		{Role: "user", Content: content},
	}

	result := FormatTranscript(messages)

	if !strings.Contains(result, "User: This is a quoted string") {
		t.Errorf("expected decoded JSON string in transcript, got %q", result)
	}
}

func TestFormatTranscript_Empty(t *testing.T) {
	result := FormatTranscript(nil)
	if result != "" {
		t.Errorf("expected empty string for nil messages, got %q", result)
	}

	result = FormatTranscript([]Message{})
	if result != "" {
		t.Errorf("expected empty string for empty messages, got %q", result)
	}
}

func TestFormatTranscript_Truncation(t *testing.T) {
	// Create messages that exceed maxTranscriptChars (4000).
	longText := strings.Repeat("word ", 1000) // 5000 chars
	content, _ := json.Marshal(longText)

	messages := []Message{
		{Role: "user", Content: content},
		{Role: "assistant", Content: content},
	}

	result := FormatTranscript(messages)

	// Result should be truncated to around 4000 chars plus "...".
	if len(result) > 4010 {
		t.Errorf("transcript length = %d, expected at most ~4003 (4000 + ...)", len(result))
	}
	if !strings.HasSuffix(result, "...") {
		t.Errorf("expected truncated transcript to end with '...', got suffix %q", result[len(result)-10:])
	}
}

func TestSummarizer_Generate(t *testing.T) {
	mockSend := func(ctx context.Context, system, user string) (string, error) {
		// Verify the system prompt is passed through.
		if !strings.Contains(system, "summarize") {
			t.Errorf("system prompt should contain 'summarize', got %q", system)
		}
		// Verify the user prompt contains the transcript.
		if !strings.Contains(user, "Hello") {
			t.Errorf("user prompt should contain transcript text, got %q", user)
		}
		return "The user asked about building the project.", nil
	}

	summarizer := NewSummarizer(mockSend)

	userContent, _ := json.Marshal("Hello")
	assistantContent, _ := json.Marshal("Hi there")
	messages := []Message{
		{Role: "user", Content: userContent},
		{Role: "assistant", Content: assistantContent},
	}

	result, err := summarizer.Summarize(context.Background(), messages)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if result != "The user asked about building the project." {
		t.Errorf("result = %q, want %q", result, "The user asked about building the project.")
	}
}

func TestSummarizer_EmptyTranscript(t *testing.T) {
	sendCalled := false
	mockSend := func(ctx context.Context, system, user string) (string, error) {
		sendCalled = true
		return "should not be called", nil
	}

	summarizer := NewSummarizer(mockSend)

	result, err := summarizer.Summarize(context.Background(), nil)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if result != "" {
		t.Errorf("expected empty string for empty messages, got %q", result)
	}
	if sendCalled {
		t.Error("send function should not be called for empty transcript")
	}
}

func TestFormatTranscript_UTF8(t *testing.T) {
	// Use multi-byte characters that would be split incorrectly by byte truncation.
	// Each emoji is 4 bytes; fill past maxTranscriptChars (4000) in runes.
	longEmoji := strings.Repeat("\U0001f680", 4100) // 4100 rocket emojis
	content, _ := json.Marshal(longEmoji)

	messages := []Message{
		{Role: "user", Content: content},
	}

	result := FormatTranscript(messages)

	if !utf8.ValidString(result) {
		t.Error("result is not valid UTF-8")
	}
	if !strings.HasSuffix(result, "...") {
		t.Errorf("expected truncated transcript to end with '...', got suffix %q", result[len(result)-10:])
	}
}
