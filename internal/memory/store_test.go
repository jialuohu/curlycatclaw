package memory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestNewStore(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	s, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer s.Close()

	// Verify the database file was created on disk.
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("database file not found: %v", err)
	}

	// Verify WAL mode is enabled.
	var mode string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("query journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want %q", mode, "wal")
	}
}

func TestCreateConversation(t *testing.T) {
	s := newTestStore(t)

	id, err := s.CreateConversation(42, 100)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	// UUID v4 format: 8-4-4-4-12 hex chars.
	uuidRe := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if !uuidRe.MatchString(id) {
		t.Errorf("returned id %q does not match UUID v4 format", id)
	}
}

func TestAppendAndGetMessages(t *testing.T) {
	s := newTestStore(t)

	convID, err := s.CreateConversation(1, 1)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	userContent, _ := json.Marshal("Hello")
	assistantContent, _ := json.Marshal("Hi there")

	if err := s.AppendMessage(convID, "user", userContent); err != nil {
		t.Fatalf("AppendMessage(user): %v", err)
	}
	if err := s.AppendMessage(convID, "assistant", assistantContent); err != nil {
		t.Fatalf("AppendMessage(assistant): %v", err)
	}

	msgs, err := s.GetMessages(convID, 100)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}

	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}

	// Verify chronological order: user first, assistant second.
	if msgs[0].Role != "user" {
		t.Errorf("msgs[0].Role = %q, want %q", msgs[0].Role, "user")
	}
	if msgs[1].Role != "assistant" {
		t.Errorf("msgs[1].Role = %q, want %q", msgs[1].Role, "assistant")
	}

	// Verify content round-trips.
	if string(msgs[0].Content) != string(userContent) {
		t.Errorf("msgs[0].Content = %s, want %s", msgs[0].Content, userContent)
	}
	if string(msgs[1].Content) != string(assistantContent) {
		t.Errorf("msgs[1].Content = %s, want %s", msgs[1].Content, assistantContent)
	}
}

func TestGetMessages_Limit(t *testing.T) {
	s := newTestStore(t)

	convID, err := s.CreateConversation(1, 1)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	// Append 10 messages with sequential numbering.
	for i := 0; i < 10; i++ {
		content, _ := json.Marshal(i)
		if err := s.AppendMessage(convID, "user", content); err != nil {
			t.Fatalf("AppendMessage(%d): %v", i, err)
		}
	}

	// Get the last 3 messages.
	msgs, err := s.GetMessages(convID, 3)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}

	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3", len(msgs))
	}

	// The last 3 should be messages 7, 8, 9 in chronological order.
	for i, msg := range msgs {
		var got int
		if err := json.Unmarshal(msg.Content, &got); err != nil {
			t.Fatalf("unmarshal msgs[%d]: %v", i, err)
		}
		want := 7 + i
		if got != want {
			t.Errorf("msgs[%d] content = %d, want %d", i, got, want)
		}
	}
}

func TestLogAndCompleteToolCall(t *testing.T) {
	s := newTestStore(t)

	convID, err := s.CreateConversation(1, 1)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	callID := "call_001"
	input, _ := json.Marshal(map[string]string{"query": "test"})

	if err := s.LogToolCall(convID, callID, "search", input); err != nil {
		t.Fatalf("LogToolCall: %v", err)
	}

	output, _ := json.Marshal(map[string]string{"result": "found"})
	if err := s.CompleteToolCall(callID, output, false); err != nil {
		t.Fatalf("CompleteToolCall: %v", err)
	}
}

func TestCompleteToolCall_NotFound(t *testing.T) {
	s := newTestStore(t)

	output, _ := json.Marshal("done")
	err := s.CompleteToolCall("nonexistent_id", output, false)
	if err == nil {
		t.Fatal("CompleteToolCall with non-existent ID should return an error")
	}
}

func TestGetActiveConversation_CreatesNew(t *testing.T) {
	s := newTestStore(t)

	// No conversations exist yet, should create a new one.
	id, expiredID, err := s.GetActiveConversation(42, 100)
	if err != nil {
		t.Fatalf("GetActiveConversation: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty conversation ID")
	}
	if expiredID != "" {
		t.Errorf("expected empty expiredConvID for new conversation, got %q", expiredID)
	}
}

func TestGetActiveConversation_ReturnsExisting(t *testing.T) {
	s := newTestStore(t)

	// Create a conversation, then ask for active — should return the same one.
	created, err := s.CreateConversation(42, 100)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	active, expiredID, err := s.GetActiveConversation(42, 100)
	if err != nil {
		t.Fatalf("GetActiveConversation: %v", err)
	}

	if active != created {
		t.Errorf("GetActiveConversation returned %q, want %q", active, created)
	}
	if expiredID != "" {
		t.Errorf("expected empty expiredConvID for active conversation, got %q", expiredID)
	}
}

func TestGetActiveConversation_StaleCreatesNew(t *testing.T) {
	s := newTestStore(t)

	old, err := s.CreateConversation(42, 100)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	// Set updated_at to 5 hours ago via direct SQL.
	staleTime := time.Now().UTC().Add(-5 * time.Hour)
	if _, err := s.db.Exec(
		`UPDATE conversations SET updated_at = ? WHERE id = ?`,
		staleTime, old,
	); err != nil {
		t.Fatalf("update timestamp: %v", err)
	}

	active, expiredID, err := s.GetActiveConversation(42, 100)
	if err != nil {
		t.Fatalf("GetActiveConversation: %v", err)
	}

	if active == old {
		t.Error("expected a new conversation ID, got the stale one")
	}
	if expiredID != old {
		t.Errorf("expected expiredConvID = %q, got %q", old, expiredID)
	}
}

func TestGetActiveConversation_DifferentChatIDs(t *testing.T) {
	s := newTestStore(t)

	const userID int64 = 42

	id1, _, err := s.GetActiveConversation(userID, 100)
	if err != nil {
		t.Fatalf("GetActiveConversation(chatID=100): %v", err)
	}

	id2, _, err := s.GetActiveConversation(userID, 200)
	if err != nil {
		t.Fatalf("GetActiveConversation(chatID=200): %v", err)
	}

	if id1 == id2 {
		t.Errorf("same user with different chatIDs got the same conversation %q", id1)
	}
}

func TestGetActiveConversation_TransactionConsistency(t *testing.T) {
	s := newTestStore(t)
	const userID int64 = 42
	const chatID int64 = 1

	// First call creates a conversation.
	id1, _, err := s.GetActiveConversation(userID, chatID)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if id1 == "" {
		t.Fatal("first call returned empty ID")
	}

	// Second call within 4h should return the same conversation.
	id2, _, err := s.GetActiveConversation(userID, chatID)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if id2 != id1 {
		t.Errorf("expected same conversation %q, got %q", id1, id2)
	}

	// Verify only one conversation exists for this user/chat.
	var count int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM conversations WHERE user_id = ? AND chat_id = ?`,
		userID, chatID,
	).Scan(&count); err != nil {
		t.Fatalf("count conversations: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 conversation, got %d", count)
	}
}

func TestGetConversationMessages(t *testing.T) {
	s := newTestStore(t)

	convID, err := s.CreateConversation(1, 1)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	c1, _ := json.Marshal("Hello")
	c2, _ := json.Marshal("Hi there")
	if err := s.AppendMessage(convID, "user", c1); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if err := s.AppendMessage(convID, "assistant", c2); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	msgs, err := s.GetConversationMessages(convID)
	if err != nil {
		t.Fatalf("GetConversationMessages: %v", err)
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

func TestGetConversationMessages_Empty(t *testing.T) {
	s := newTestStore(t)

	convID, err := s.CreateConversation(1, 1)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	msgs, err := s.GetConversationMessages(convID)
	if err != nil {
		t.Fatalf("GetConversationMessages: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("got %d messages, want 0", len(msgs))
	}
}

func TestSaveSummary(t *testing.T) {
	s := newTestStore(t)

	convID, err := s.CreateConversation(42, 100)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	now := time.Now().UTC()
	err = s.SaveSummary(convID, 42, 100, "test summary", 5, now.Add(-time.Hour), now)
	if err != nil {
		t.Fatalf("SaveSummary: %v", err)
	}

	// Insert again with same convID should not error (INSERT OR IGNORE).
	err = s.SaveSummary(convID, 42, 100, "duplicate summary", 5, now.Add(-time.Hour), now)
	if err != nil {
		t.Fatalf("SaveSummary duplicate: %v", err)
	}
}

func TestSetSummarizationStatus(t *testing.T) {
	s := newTestStore(t)

	convID, err := s.CreateConversation(42, 100)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	if err := s.SetSummarizationStatus(convID, "pending"); err != nil {
		t.Fatalf("SetSummarizationStatus: %v", err)
	}

	// Verify via PendingSummarizations.
	ids, err := s.PendingSummarizations()
	if err != nil {
		t.Fatalf("PendingSummarizations: %v", err)
	}
	if len(ids) != 1 || ids[0] != convID {
		t.Errorf("PendingSummarizations = %v, want [%s]", ids, convID)
	}

	// Mark as done and verify it's no longer pending.
	if err := s.SetSummarizationStatus(convID, "done"); err != nil {
		t.Fatalf("SetSummarizationStatus(done): %v", err)
	}
	ids, err = s.PendingSummarizations()
	if err != nil {
		t.Fatalf("PendingSummarizations after done: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("expected 0 pending after done, got %d", len(ids))
	}
}

func TestPendingSummarizations_Empty(t *testing.T) {
	s := newTestStore(t)

	ids, err := s.PendingSummarizations()
	if err != nil {
		t.Fatalf("PendingSummarizations: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("expected 0 pending, got %d", len(ids))
	}
}

func TestConversationMeta(t *testing.T) {
	s := newTestStore(t)

	convID, err := s.CreateConversation(42, 100)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	c1, _ := json.Marshal("Hello")
	c2, _ := json.Marshal("Hi there")
	if err := s.AppendMessage(convID, "user", c1); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if err := s.AppendMessage(convID, "assistant", c2); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	userID, chatID, msgCount, firstAt, lastAt, err := s.ConversationMeta(convID)
	if err != nil {
		t.Fatalf("ConversationMeta: %v", err)
	}
	if userID != 42 {
		t.Errorf("userID = %d, want 42", userID)
	}
	if chatID != 100 {
		t.Errorf("chatID = %d, want 100", chatID)
	}
	if msgCount != 2 {
		t.Errorf("msgCount = %d, want 2", msgCount)
	}
	if firstAt.IsZero() {
		t.Error("firstAt should not be zero")
	}
	if lastAt.IsZero() {
		t.Error("lastAt should not be zero")
	}
	if lastAt.Before(firstAt) {
		t.Errorf("lastAt (%v) should not be before firstAt (%v)", lastAt, firstAt)
	}
}
