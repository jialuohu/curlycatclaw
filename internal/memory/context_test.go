package memory

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func newTestContextBuilder(t *testing.T) (*ContextBuilder, *Store, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	convID, err := s.CreateConversation(1, 1)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	cb := NewContextBuilder(s)
	return cb, s, convID
}

func TestBuildContext_Empty(t *testing.T) {
	cb, _, convID := newTestContextBuilder(t)

	msgs, err := cb.BuildContext(convID)
	if err != nil {
		t.Fatalf("BuildContext: %v", err)
	}
	if msgs != nil {
		t.Errorf("expected nil for empty conversation, got %d messages", len(msgs))
	}
}

func TestBuildContext_SingleTurn(t *testing.T) {
	cb, s, convID := newTestContextBuilder(t)

	userContent, _ := json.Marshal("What is 2+2?")
	assistantContent, _ := json.Marshal("4")

	if err := s.AppendMessage(convID, "user", userContent); err != nil {
		t.Fatalf("AppendMessage(user): %v", err)
	}
	if err := s.AppendMessage(convID, "assistant", assistantContent); err != nil {
		t.Fatalf("AppendMessage(assistant): %v", err)
	}

	msgs, err := cb.BuildContext(convID)
	if err != nil {
		t.Fatalf("BuildContext: %v", err)
	}

	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
	if msgs[0].Role != "user" {
		t.Errorf("msgs[0].Role = %q, want %q", msgs[0].Role, "user")
	}
	if msgs[1].Role != "assistant" {
		t.Errorf("msgs[1].Role = %q, want %q", msgs[1].Role, "assistant")
	}
}

func TestBuildContext_TurnCounting(t *testing.T) {
	cb, s, convID := newTestContextBuilder(t)

	// Create 30 turns (user + assistant each).
	for i := 0; i < 30; i++ {
		userContent, _ := json.Marshal(fmt.Sprintf("question %d", i))
		assistantContent, _ := json.Marshal(fmt.Sprintf("answer %d", i))

		if err := s.AppendMessage(convID, "user", userContent); err != nil {
			t.Fatalf("AppendMessage(user %d): %v", i, err)
		}
		if err := s.AppendMessage(convID, "assistant", assistantContent); err != nil {
			t.Fatalf("AppendMessage(assistant %d): %v", i, err)
		}
	}

	msgs, err := cb.BuildContext(convID)
	if err != nil {
		t.Fatalf("BuildContext: %v", err)
	}

	// Count turns: each "user" message starts a new turn.
	turnCount := 0
	for _, m := range msgs {
		if m.Role == "user" {
			turnCount++
		}
	}

	if turnCount != 25 {
		t.Errorf("got %d turns, want 25", turnCount)
	}

	// The first retained turn should be turn 5 (0-indexed), since the first
	// 5 turns (0-4) are dropped.
	var firstQuestion string
	if err := json.Unmarshal(msgs[0].Content, &firstQuestion); err != nil {
		t.Fatalf("unmarshal first message: %v", err)
	}
	if firstQuestion != "question 5" {
		t.Errorf("first retained message = %q, want %q", firstQuestion, "question 5")
	}
}

func TestBuildContext_CharBudget(t *testing.T) {
	cb, s, convID := newTestContextBuilder(t)

	// Set a small char budget so we can trigger trimming without huge data.
	cb.SetMaxChars(1000)

	// Each turn has ~300 chars of content (user + assistant), so 5 turns = ~1500 chars.
	// With a 1000-char budget, the oldest turns should be dropped.
	for i := 0; i < 5; i++ {
		bigContent, _ := json.Marshal(strings.Repeat("x", 150))
		if err := s.AppendMessage(convID, "user", bigContent); err != nil {
			t.Fatalf("AppendMessage(user %d): %v", i, err)
		}
		if err := s.AppendMessage(convID, "assistant", bigContent); err != nil {
			t.Fatalf("AppendMessage(assistant %d): %v", i, err)
		}
	}

	msgs, err := cb.BuildContext(convID)
	if err != nil {
		t.Fatalf("BuildContext: %v", err)
	}

	// Count total characters to verify we're within budget.
	totalChars := 0
	for _, m := range msgs {
		totalChars += len(m.Content)
	}
	if totalChars > 1000 {
		t.Errorf("total chars = %d, want <= 1000", totalChars)
	}

	// We should have fewer than the original 10 messages.
	if len(msgs) >= 10 {
		t.Errorf("got %d messages, expected fewer than 10 after char budget trimming", len(msgs))
	}
}

func TestSplitTurns_ToolChainIsOneTurn(t *testing.T) {
	// Simulate: user -> assistant(tool_use) -> tool_result -> assistant(final)
	// This should be grouped as 1 turn since there is only 1 "user" message.
	msgs := []Message{
		{Role: "user", Content: json.RawMessage(`"Please search for cats"`)},
		{Role: "assistant", Content: json.RawMessage(`{"type":"tool_use","id":"call_1","name":"search","input":{"q":"cats"}}`)},
		{Role: "tool_result", Content: json.RawMessage(`{"result":"found 3 cats"}`)},
		{Role: "assistant", Content: json.RawMessage(`"I found 3 cats for you."`)},
	}

	turns := splitTurns(msgs)

	if len(turns) != 1 {
		t.Fatalf("got %d turns, want 1 (tool chain should be a single turn)", len(turns))
	}

	if len(turns[0].messages) != 4 {
		t.Errorf("turn has %d messages, want 4", len(turns[0].messages))
	}
}
