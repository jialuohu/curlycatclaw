package memory

import (
	"path/filepath"
	"testing"
	"time"
)

func newTestStoreForEval(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := NewStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestLogInteractionEvent(t *testing.T) {
	s := newTestStoreForEval(t)
	convID := createTestConversation(t, s, 42, 100)

	if err := s.LogInteractionEvent(convID, 42, 100, "effort_override", "max"); err != nil {
		t.Fatalf("LogInteractionEvent: %v", err)
	}
	if err := s.LogInteractionEvent(convID, 42, 100, "retry", "high"); err != nil {
		t.Fatalf("LogInteractionEvent: %v", err)
	}
	if err := s.LogInteractionEvent("", 42, 100, "effort_override", "low"); err != nil {
		t.Fatalf("LogInteractionEvent (no conv): %v", err)
	}

	events, err := s.GetInteractionEvents(convID)
	if err != nil {
		t.Fatalf("GetInteractionEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events for convID, got %d", len(events))
	}
	if events[0].EventType != "effort_override" || events[0].Payload != "max" {
		t.Errorf("event[0] = %s/%s, want effort_override/max", events[0].EventType, events[0].Payload)
	}
	if events[1].EventType != "retry" || events[1].Payload != "high" {
		t.Errorf("event[1] = %s/%s, want retry/high", events[1].EventType, events[1].Payload)
	}
	if events[0].UserID != 42 || events[0].ChatID != 100 {
		t.Errorf("event[0] user/chat = %d/%d, want 42/100", events[0].UserID, events[0].ChatID)
	}
}

func TestMapTelegramMessage(t *testing.T) {
	s := newTestStoreForEval(t)
	convID := createTestConversation(t, s, 42, 100)

	if err := s.MapTelegramMessage(100, 555, convID); err != nil {
		t.Fatalf("MapTelegramMessage: %v", err)
	}

	got, err := s.LookupConversationByTelegramMessage(100, 555)
	if err != nil {
		t.Fatalf("LookupConversationByTelegramMessage: %v", err)
	}
	if got != convID {
		t.Errorf("got convID %q, want %q", got, convID)
	}

	// Duplicate insert should be ignored (INSERT OR IGNORE).
	if err := s.MapTelegramMessage(100, 555, convID); err != nil {
		t.Fatalf("MapTelegramMessage duplicate: %v", err)
	}

	// Lookup for non-existent message should return error.
	_, err = s.LookupConversationByTelegramMessage(100, 999)
	if err == nil {
		t.Error("expected error for non-existent telegram message, got nil")
	}
}

func TestLogEvalReaction(t *testing.T) {
	s := newTestStoreForEval(t)
	convID := createTestConversation(t, s, 42, 100)

	if err := s.LogEvalReaction(convID, 42, 100, 555, "👍"); err != nil {
		t.Fatalf("LogEvalReaction: %v", err)
	}
	if err := s.LogEvalReaction(convID, 42, 100, 556, "👎"); err != nil {
		t.Fatalf("LogEvalReaction: %v", err)
	}

	// Verify reactions were stored by querying directly.
	rows, err := s.db.Query(
		`SELECT reaction, telegram_message_id FROM eval_reactions WHERE conversation_id = ? ORDER BY created_at ASC`,
		convID,
	)
	if err != nil {
		t.Fatalf("query eval_reactions: %v", err)
	}
	defer rows.Close()

	var reactions []struct {
		reaction string
		tgMsgID  int
	}
	for rows.Next() {
		var r struct {
			reaction string
			tgMsgID  int
		}
		if err := rows.Scan(&r.reaction, &r.tgMsgID); err != nil {
			t.Fatalf("scan: %v", err)
		}
		reactions = append(reactions, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	if len(reactions) != 2 {
		t.Fatalf("expected 2 reactions, got %d", len(reactions))
	}
	if reactions[0].reaction != "👍" || reactions[0].tgMsgID != 555 {
		t.Errorf("reaction[0] = %s/%d, want 👍/555", reactions[0].reaction, reactions[0].tgMsgID)
	}
	if reactions[1].reaction != "👎" || reactions[1].tgMsgID != 556 {
		t.Errorf("reaction[1] = %s/%d, want 👎/556", reactions[1].reaction, reactions[1].tgMsgID)
	}
}

func TestGetConversationsSince(t *testing.T) {
	s := newTestStoreForEval(t)

	// Create two conversations.
	convID1, err := s.CreateConversation(42, 100)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	convID2, err := s.CreateConversation(42, 200)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	before := time.Now().UTC().Add(-1 * time.Second)

	// Add messages to both.
	if err := s.AppendMessage(convID1, "user", mustJSON("hello")); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if err := s.AppendMessage(convID2, "user", mustJSON("world")); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	convs, err := s.GetConversationsSince(before)
	if err != nil {
		t.Fatalf("GetConversationsSince: %v", err)
	}
	if len(convs) != 2 {
		t.Fatalf("expected 2 conversations, got %d", len(convs))
	}

	// Query with future time should return 0.
	convs, err = s.GetConversationsSince(time.Now().UTC().Add(1 * time.Hour))
	if err != nil {
		t.Fatalf("GetConversationsSince (future): %v", err)
	}
	if len(convs) != 0 {
		t.Errorf("expected 0 conversations for future time, got %d", len(convs))
	}
}

func TestTimeBasedIndexesExist(t *testing.T) {
	s := newTestStoreForEval(t)

	// Verify indexes exist by checking sqlite_master.
	for _, idx := range []string{"idx_messages_created_at", "idx_tool_calls_created_at"} {
		var name string
		err := s.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='index' AND name=?`, idx).Scan(&name)
		if err != nil {
			t.Errorf("index %s not found: %v", idx, err)
		}
	}
}

