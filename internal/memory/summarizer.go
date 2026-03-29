package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

)

const (
	summarySystemPrompt = "You summarize conversations. Be specific with names, files, numbers. No greetings or filler."
	summaryUserPrompt   = "Summarize this conversation in 2-3 sentences. Focus on: what the user asked about, key decisions made, any action items or follow-ups mentioned.\n\nConversation:\n%s"
	maxTranscriptChars  = 4000
)

// ConversationSummarizer generates summaries of conversations via a Claude client.
type ConversationSummarizer struct {
	send func(ctx context.Context, system, user string) (string, error)
}

// NewSummarizer creates a summarizer that calls the provided send function.
// The send function should perform a non-streaming Claude API call.
func NewSummarizer(sendFn func(ctx context.Context, system, user string) (string, error)) *ConversationSummarizer {
	return &ConversationSummarizer{send: sendFn}
}

// Summarize generates a summary from a list of messages.
func (s *ConversationSummarizer) Summarize(ctx context.Context, messages []Message) (string, error) {
	transcript := FormatTranscript(messages)
	if transcript == "" {
		return "", nil
	}

	userMsg := fmt.Sprintf(summaryUserPrompt, transcript)
	return s.send(ctx, summarySystemPrompt, userMsg)
}

// FormatTranscript converts stored messages into a plain-text transcript
// suitable for summarization. Strips tool_use/tool_result blocks and
// extracts readable text. Truncates to maxTranscriptChars.
func FormatTranscript(messages []Message) string {
	var sb strings.Builder

	for _, m := range messages {
		text := extractText(m)
		if text == "" {
			continue
		}

		switch m.Role {
		case "user":
			sb.WriteString("User: ")
		case "assistant":
			sb.WriteString("Assistant: ")
		default:
			continue // skip tool_result in transcript
		}
		sb.WriteString(text)
		sb.WriteString("\n")

		if sb.Len() > maxTranscriptChars {
			break
		}
	}

	result := sb.String()
	runes := []rune(result)
	if len(runes) > maxTranscriptChars {
		result = string(runes[:maxTranscriptChars]) + "..."
	}
	return strings.TrimSpace(result)
}

// extractText pulls readable text from a message's JSON content.
func extractText(m Message) string {
	if len(m.Content) == 0 {
		return ""
	}

	// Try as a simple JSON string first (common for user messages).
	var simple string
	if err := json.Unmarshal(m.Content, &simple); err == nil {
		return simple
	}

	// Try as an Anthropic message content block.
	var block struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.Content, &block); err == nil && block.Text != "" {
		return block.Text
	}

	// Try as an array of content blocks (assistant with text + tool_use).
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.Content, &blocks); err == nil {
		var texts []string
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				texts = append(texts, b.Text)
			}
		}
		return strings.Join(texts, "\n")
	}

	// Try as a claude.Response (assistant messages stored this way).
	var resp struct {
		TextContent string          `json:"TextContent"`
		ToolCalls   json.RawMessage `json:"ToolCalls"`
	}
	if err := json.Unmarshal(m.Content, &resp); err == nil && resp.TextContent != "" {
		return resp.TextContent
	}

	return ""
}

