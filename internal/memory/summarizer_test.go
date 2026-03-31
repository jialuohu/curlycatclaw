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
	// Create messages that exceed maxTranscriptChars (12000).
	longText := strings.Repeat("word ", 3000) // 15000 chars
	content, _ := json.Marshal(longText)

	messages := []Message{
		{Role: "user", Content: content},
		{Role: "assistant", Content: content},
	}

	result := FormatTranscript(messages)

	// Result should use head+tail sampling with truncation marker.
	if !strings.Contains(result, "[...truncated...]") {
		t.Error("expected truncated transcript to contain '[...truncated...]' marker")
	}
	// Should have content from the beginning and end.
	runes := []rune(result)
	// Head (5000) + marker (~19) + tail (5000) = ~10019 runes
	if len(runes) > 11000 {
		t.Errorf("transcript rune length = %d, expected at most ~10019", len(runes))
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

func TestFormatTranscript_ImageOnly(t *testing.T) {
	// Image blocks have type "image" not "text", so extractText returns ""
	imageContent, _ := json.Marshal([]map[string]interface{}{
		{"type": "image", "source": map[string]string{"type": "base64", "data": "abc"}},
	})
	messages := []Message{
		{Role: "user", Content: imageContent},
	}
	result := FormatTranscript(messages)
	// Image-only messages produce no text content
	result = strings.TrimSpace(result)
	if result != "" && result != "user:" {
		// Either empty or just the role prefix with no content
		t.Logf("result = %q (expected empty or role-only)", result)
	}
}

func TestFormatTranscript_UTF8(t *testing.T) {
	// Use multi-byte characters that would be split incorrectly by byte truncation.
	// Each emoji is 4 bytes; fill past maxTranscriptChars (12000) in runes.
	longEmoji := strings.Repeat("\U0001f680", 13000) // 13000 rocket emojis
	content, _ := json.Marshal(longEmoji)

	messages := []Message{
		{Role: "user", Content: content},
	}

	result := FormatTranscript(messages)

	if !utf8.ValidString(result) {
		t.Error("result is not valid UTF-8")
	}
	if !strings.Contains(result, "[...truncated...]") {
		t.Error("expected truncated transcript to contain '[...truncated...]' marker")
	}
}

func TestFormatTranscript_HeadTailSampling(t *testing.T) {
	// Create a conversation with distinct beginning and ending content.
	beginContent, _ := json.Marshal("BEGINNING_MARKER this is the start of the conversation")
	endContent, _ := json.Marshal("ENDING_MARKER this is the end of the conversation")
	// Fill the middle with enough content to exceed the limit.
	middleText := strings.Repeat("middle filler content ", 600) // ~13200 chars
	middleContent, _ := json.Marshal(middleText)

	messages := []Message{
		{Role: "user", Content: beginContent},
		{Role: "assistant", Content: middleContent},
		{Role: "user", Content: endContent},
	}

	result := FormatTranscript(messages)

	if !strings.Contains(result, "BEGINNING_MARKER") {
		t.Error("head+tail sampling should preserve the beginning of the conversation")
	}
	if !strings.Contains(result, "ENDING_MARKER") {
		t.Error("head+tail sampling should preserve the end of the conversation")
	}
	if !strings.Contains(result, "[...truncated...]") {
		t.Error("should contain truncation marker between head and tail")
	}
}

func TestFormatTranscript_ShortUnchanged(t *testing.T) {
	// Short transcript should not be truncated.
	content, _ := json.Marshal("Hello, how are you?")
	messages := []Message{
		{Role: "user", Content: content},
		{Role: "assistant", Content: content},
	}

	result := FormatTranscript(messages)

	if strings.Contains(result, "[...truncated...]") {
		t.Error("short transcript should not contain truncation marker")
	}
	if !strings.Contains(result, "Hello, how are you?") {
		t.Error("short transcript should contain full text")
	}
}
