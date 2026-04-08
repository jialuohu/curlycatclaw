package memory

import (
	"encoding/json"
	"fmt"
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
	id, expiredID, err := s.GetActiveConversation(42, 100, "private")
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

	active, expiredID, err := s.GetActiveConversation(42, 100, "private")
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

	active, expiredID, err := s.GetActiveConversation(42, 100, "private")
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

	id1, _, err := s.GetActiveConversation(userID, 100, "private")
	if err != nil {
		t.Fatalf("GetActiveConversation(chatID=100): %v", err)
	}

	id2, _, err := s.GetActiveConversation(userID, 200, "private")
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
	id1, _, err := s.GetActiveConversation(userID, chatID, "private")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if id1 == "" {
		t.Fatal("first call returned empty ID")
	}

	// Second call within 4h should return the same conversation.
	id2, _, err := s.GetActiveConversation(userID, chatID, "private")
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

	userID, chatID, _, msgCount, firstAt, lastAt, err := s.ConversationMeta(convID)
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

func TestRecoverableSummarizations(t *testing.T) {
	s := newTestStore(t)

	// Create conversations with various statuses.
	c1, _ := s.CreateConversation(1, 100)
	c2, _ := s.CreateConversation(2, 200)
	c3, _ := s.CreateConversation(3, 300)
	c4, _ := s.CreateConversation(4, 400)

	s.SetSummarizationStatus(c1, "pending")
	s.SetSummarizationStatus(c2, "failed")
	s.SetSummarizationStatus(c3, "indexed_failed")
	s.SetSummarizationStatus(c4, "done")

	ids, err := s.RecoverableSummarizations()
	if err != nil {
		t.Fatalf("RecoverableSummarizations: %v", err)
	}
	if len(ids) != 3 {
		t.Fatalf("expected 3 recoverable, got %d: %v", len(ids), ids)
	}
	// Should include pending, failed, indexed_failed but not done.
	found := map[string]bool{}
	for _, id := range ids {
		found[id] = true
	}
	for _, want := range []string{c1, c2, c3} {
		if !found[want] {
			t.Errorf("expected %s in recoverable list", want)
		}
	}
	if found[c4] {
		t.Error("'done' conversation should not be in recoverable list")
	}
}

func TestRecoverableSummarizations_Empty(t *testing.T) {
	s := newTestStore(t)

	ids, err := s.RecoverableSummarizations()
	if err != nil {
		t.Fatalf("RecoverableSummarizations: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("expected 0 recoverable, got %d", len(ids))
	}
}

func TestGetSummaryText(t *testing.T) {
	s := newTestStore(t)

	convID, _ := s.CreateConversation(42, 100)
	now := time.Now().UTC()
	s.SaveSummary(convID, 42, 100, "User discussed Go testing patterns.", 5, now.Add(-time.Hour), now)

	text, err := s.GetSummaryText(convID)
	if err != nil {
		t.Fatalf("GetSummaryText: %v", err)
	}
	if text != "User discussed Go testing patterns." {
		t.Errorf("GetSummaryText = %q, want %q", text, "User discussed Go testing patterns.")
	}
}

func TestGetSummaryText_NotFound(t *testing.T) {
	s := newTestStore(t)

	text, err := s.GetSummaryText("nonexistent")
	if err != nil {
		t.Fatalf("GetSummaryText: %v", err)
	}
	if text != "" {
		t.Errorf("expected empty string for missing summary, got %q", text)
	}
}

func TestAllMessageTexts(t *testing.T) {
	s := newTestStore(t)

	convID, err := s.CreateConversation(42, 100)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	// Add messages with different content formats.
	simple, _ := json.Marshal("Hello, world!")
	if err := s.AppendMessage(convID, "user", simple); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	blocks, _ := json.Marshal([]map[string]string{
		{"type": "text", "text": "Response text"},
	})
	if err := s.AppendMessage(convID, "assistant", blocks); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	// Tool result message (should be skipped by extractText since it has no readable text).
	toolContent, _ := json.Marshal(map[string]string{"type": "tool_use", "id": "call_1"})
	if err := s.AppendMessage(convID, "assistant", toolContent); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	texts, err := s.AllMessageTexts()
	if err != nil {
		t.Fatalf("AllMessageTexts: %v", err)
	}

	// Should have 2 texts (tool_use block has no extractable text).
	if len(texts) != 2 {
		t.Fatalf("expected 2 texts, got %d", len(texts))
	}

	if texts[0].Text != "Hello, world!" {
		t.Errorf("texts[0].Text = %q, want %q", texts[0].Text, "Hello, world!")
	}
	if texts[0].Source != "message" {
		t.Errorf("texts[0].Source = %q, want %q", texts[0].Source, "message")
	}
	if texts[0].UserID != 42 {
		t.Errorf("texts[0].UserID = %d, want 42", texts[0].UserID)
	}
	if texts[0].ChatID != 100 {
		t.Errorf("texts[0].ChatID = %d, want 100", texts[0].ChatID)
	}

	if texts[1].Text != "Response text" {
		t.Errorf("texts[1].Text = %q, want %q", texts[1].Text, "Response text")
	}
}

func TestAllMessageTexts_Empty(t *testing.T) {
	s := newTestStore(t)

	texts, err := s.AllMessageTexts()
	if err != nil {
		t.Fatalf("AllMessageTexts: %v", err)
	}
	if len(texts) != 0 {
		t.Fatalf("expected 0 texts, got %d", len(texts))
	}
}

func TestAllNoteTexts(t *testing.T) {
	s := newTestStore(t)

	// Create notes table (normally done by skills.InitNoteSkills).
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS notes (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id    INTEGER NOT NULL DEFAULT 0,
		chat_id    INTEGER NOT NULL DEFAULT 0,
		title      TEXT NOT NULL,
		content    TEXT NOT NULL,
		created_at DATETIME NOT NULL
	)`)
	if err != nil {
		t.Fatalf("create notes table: %v", err)
	}

	now := time.Now().UTC()
	_, err = s.db.Exec(
		`INSERT INTO notes (user_id, chat_id, title, content, created_at) VALUES (?, ?, ?, ?, ?)`,
		42, 100, "Test Note", "Some content here", now,
	)
	if err != nil {
		t.Fatalf("insert note: %v", err)
	}

	texts, err := s.AllNoteTexts()
	if err != nil {
		t.Fatalf("AllNoteTexts: %v", err)
	}
	if len(texts) != 1 {
		t.Fatalf("expected 1 text, got %d", len(texts))
	}
	if texts[0].Text != "Test Note\nSome content here" {
		t.Errorf("texts[0].Text = %q, want %q", texts[0].Text, "Test Note\nSome content here")
	}
	if texts[0].Source != "note" {
		t.Errorf("texts[0].Source = %q, want %q", texts[0].Source, "note")
	}
	if texts[0].UserID != 42 {
		t.Errorf("texts[0].UserID = %d, want 42", texts[0].UserID)
	}
}

func TestAllNoteTexts_NoTable(t *testing.T) {
	s := newTestStore(t)

	// notes table does not exist.
	texts, err := s.AllNoteTexts()
	if err != nil {
		t.Fatalf("AllNoteTexts: %v", err)
	}
	if len(texts) != 0 {
		t.Fatalf("expected 0 texts when table missing, got %d", len(texts))
	}
}

func TestAllSummaryTexts(t *testing.T) {
	s := newTestStore(t)

	convID, err := s.CreateConversation(42, 100)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	// Set chat_type on the conversation.
	_, err = s.db.Exec(`UPDATE conversations SET chat_type = 'private' WHERE id = ?`, convID)
	if err != nil {
		t.Fatalf("set chat_type: %v", err)
	}

	now := time.Now().UTC()
	if err := s.SaveSummary(convID, 42, 100, "User discussed testing patterns.", 5, now.Add(-time.Hour), now); err != nil {
		t.Fatalf("SaveSummary: %v", err)
	}

	// Set chat_type on the summary.
	_, err = s.db.Exec(`UPDATE conversation_summaries SET chat_type = 'private' WHERE conversation_id = ?`, convID)
	if err != nil {
		t.Fatalf("set summary chat_type: %v", err)
	}

	texts, err := s.AllSummaryTexts()
	if err != nil {
		t.Fatalf("AllSummaryTexts: %v", err)
	}
	if len(texts) != 1 {
		t.Fatalf("expected 1 text, got %d", len(texts))
	}
	if texts[0].Text != "User discussed testing patterns." {
		t.Errorf("texts[0].Text = %q, want %q", texts[0].Text, "User discussed testing patterns.")
	}
	if texts[0].Source != "summary" {
		t.Errorf("texts[0].Source = %q, want %q", texts[0].Source, "summary")
	}
	if texts[0].ChatType != "private" {
		t.Errorf("texts[0].ChatType = %q, want %q", texts[0].ChatType, "private")
	}
	if texts[0].ID != convID {
		t.Errorf("texts[0].ID = %q, want %q", texts[0].ID, convID)
	}
}

func TestAllSummaryTexts_Empty(t *testing.T) {
	s := newTestStore(t)

	texts, err := s.AllSummaryTexts()
	if err != nil {
		t.Fatalf("AllSummaryTexts: %v", err)
	}
	if len(texts) != 0 {
		t.Fatalf("expected 0 texts, got %d", len(texts))
	}
}

// --- RecentToolErrors / RecentToolCallsByUser tests ---

// insertToolCall is a test helper that inserts a tool call with optional error status and timestamp override.
func insertToolCall(t *testing.T, s *Store, convID, callID, name string, isError bool, output string, createdAt time.Time) {
	t.Helper()
	input, _ := json.Marshal(map[string]string{"q": "test"})
	if err := s.LogToolCall(convID, callID, name, input); err != nil {
		t.Fatalf("LogToolCall(%s): %v", callID, err)
	}
	out, _ := json.Marshal(output)
	if err := s.CompleteToolCall(callID, out, isError); err != nil {
		t.Fatalf("CompleteToolCall(%s): %v", callID, err)
	}
	if !createdAt.IsZero() {
		if _, err := s.db.Exec(`UPDATE tool_calls SET created_at = ? WHERE id = ?`, createdAt, callID); err != nil {
			t.Fatalf("update created_at for %s: %v", callID, err)
		}
	}
}

func TestRecentToolErrors_ReturnsErrorsOnly(t *testing.T) {
	s := newTestStore(t)
	convID, err := s.CreateConversation(1, 100)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	now := time.Now().UTC()
	insertToolCall(t, s, convID, "ok1", "search", false, "found", now)
	insertToolCall(t, s, convID, "err1", "search", true, "timeout", now)
	insertToolCall(t, s, convID, "ok2", "read", false, "data", now)
	insertToolCall(t, s, convID, "err2", "write", true, "permission denied", now)

	records, err := s.RecentToolErrors(1, 10)
	if err != nil {
		t.Fatalf("RecentToolErrors: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("got %d records, want 2", len(records))
	}
	for _, r := range records {
		if !r.IsError {
			t.Errorf("expected IsError=true for %q", r.ToolName)
		}
	}
}

func TestRecentToolErrors_UserScoped(t *testing.T) {
	s := newTestStore(t)
	conv1, err := s.CreateConversation(1, 100)
	if err != nil {
		t.Fatalf("CreateConversation(user1): %v", err)
	}
	conv2, err := s.CreateConversation(2, 200)
	if err != nil {
		t.Fatalf("CreateConversation(user2): %v", err)
	}

	now := time.Now().UTC()
	insertToolCall(t, s, conv1, "u1_err1", "search", true, "err1", now)
	insertToolCall(t, s, conv1, "u1_err2", "read", true, "err2", now)
	insertToolCall(t, s, conv2, "u2_err1", "write", true, "err3", now)

	records, err := s.RecentToolErrors(1, 10)
	if err != nil {
		t.Fatalf("RecentToolErrors(user1): %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("user1: got %d records, want 2", len(records))
	}

	records, err = s.RecentToolErrors(2, 10)
	if err != nil {
		t.Fatalf("RecentToolErrors(user2): %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("user2: got %d records, want 1", len(records))
	}
	if records[0].ToolName != "write" {
		t.Errorf("user2 record tool = %q, want %q", records[0].ToolName, "write")
	}
}

func TestRecentToolErrors_ZeroUserID(t *testing.T) {
	s := newTestStore(t)

	_, err := s.RecentToolErrors(0, 10)
	if err == nil {
		t.Fatal("expected error for zero userID")
	}
}

func TestRecentToolErrors_EmptyTable(t *testing.T) {
	s := newTestStore(t)

	records, err := s.RecentToolErrors(1, 10)
	if err != nil {
		t.Fatalf("RecentToolErrors: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("got %d records, want 0", len(records))
	}
}

func TestRecentToolCallsByUser_Limit(t *testing.T) {
	s := newTestStore(t)
	convID, err := s.CreateConversation(1, 100)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	now := time.Now().UTC()
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("call_%d", i)
		insertToolCall(t, s, convID, id, "search", false, "ok", now)
	}

	records, err := s.RecentToolCallsByUser(1, 3)
	if err != nil {
		t.Fatalf("RecentToolCallsByUser: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("got %d records, want 3", len(records))
	}
}

func TestRecentToolCallsByUser_24HourScope(t *testing.T) {
	s := newTestStore(t)
	convID, err := s.CreateConversation(1, 100)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	now := time.Now().UTC()
	old := now.Add(-25 * time.Hour) // 25 hours ago, outside 24h window

	insertToolCall(t, s, convID, "recent1", "search", false, "ok", now)
	insertToolCall(t, s, convID, "recent2", "read", true, "err", now)
	insertToolCall(t, s, convID, "old1", "write", false, "ok", old)
	insertToolCall(t, s, convID, "old2", "delete", true, "err", old)

	records, err := s.RecentToolCallsByUser(1, 10)
	if err != nil {
		t.Fatalf("RecentToolCallsByUser: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("got %d records, want 2 (only recent)", len(records))
	}
	// Verify the old calls are excluded.
	for _, r := range records {
		if r.ToolName == "write" || r.ToolName == "delete" {
			t.Errorf("unexpected old tool call %q in results", r.ToolName)
		}
	}
}
