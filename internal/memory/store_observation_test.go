package memory

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSaveObservation(t *testing.T) {
	s := newTestStore(t)

	convID, err := s.CreateConversation(42, 100)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	obs := Observation{
		ConversationID: convID,
		UserID:         42,
		ChatID:         100,
		ChatType:       "private",
		Type:           "decision",
		Title:          "Use SQLite for storage",
		Summary:        "Decided to use SQLite with WAL mode for all persistence.",
		Facts:          []string{"SQLite chosen over PostgreSQL", "WAL mode enabled"},
		Importance:     7,
		SourceMsgStart: 1,
		SourceMsgEnd:   5,
		ContentHash:    "abc123",
	}

	if err := s.SaveObservation(&obs); err != nil {
		t.Fatalf("SaveObservation: %v", err)
	}

	// Verify observation was inserted.
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM observations WHERE conversation_id = ?`, convID).Scan(&count); err != nil {
		t.Fatalf("count observations: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 observation, got %d", count)
	}

	// Verify facts were inserted.
	var factCount int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM observation_facts`).Scan(&factCount); err != nil {
		t.Fatalf("count facts: %v", err)
	}
	if factCount != 2 {
		t.Errorf("expected 2 facts, got %d", factCount)
	}

	// Verify UUID was generated (obs.ID was empty).
	var obsID string
	if err := s.db.QueryRow(`SELECT id FROM observations WHERE conversation_id = ?`, convID).Scan(&obsID); err != nil {
		t.Fatalf("select observation id: %v", err)
	}
	if obsID == "" {
		t.Error("expected non-empty observation ID")
	}
}

func TestSaveObservation_DuplicateHash(t *testing.T) {
	s := newTestStore(t)

	convID, err := s.CreateConversation(42, 100)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	obs := Observation{
		ConversationID: convID,
		UserID:         42,
		ChatID:         100,
		ChatType:       "private",
		Type:           "decision",
		Title:          "First observation",
		Summary:        "First summary.",
		Facts:          []string{"fact one"},
		Importance:     5,
		ContentHash:    "same_hash",
	}

	if err := s.SaveObservation(&obs); err != nil {
		t.Fatalf("SaveObservation first: %v", err)
	}

	// Verify dedup check detects the hash.
	exists, err := s.ObservationExistsByHash(42, "same_hash")
	if err != nil {
		t.Fatalf("ObservationExistsByHash: %v", err)
	}
	if !exists {
		t.Error("expected ObservationExistsByHash to return true for duplicate hash")
	}

	// Different user should not see the hash.
	exists, err = s.ObservationExistsByHash(99, "same_hash")
	if err != nil {
		t.Fatalf("ObservationExistsByHash other user: %v", err)
	}
	if exists {
		t.Error("expected ObservationExistsByHash to return false for different user")
	}
}

func TestDeleteObservation(t *testing.T) {
	s := newTestStore(t)

	convID, err := s.CreateConversation(42, 100)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	obs := Observation{
		ID:             "obs-delete-test",
		ConversationID: convID,
		UserID:         42,
		ChatID:         100,
		ChatType:       "private",
		Type:           "preference",
		Title:          "Prefers dark mode",
		Summary:        "User prefers dark mode in all editors.",
		Facts:          []string{"dark mode preferred", "applies to all editors"},
		Importance:     3,
		ContentHash:    "del_hash",
	}

	if err := s.SaveObservation(&obs); err != nil {
		t.Fatalf("SaveObservation: %v", err)
	}

	// Delete as the correct user should succeed.
	if err := s.DeleteObservation("obs-delete-test", 42); err != nil {
		t.Fatalf("DeleteObservation: %v", err)
	}

	// Verify observation is gone.
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM observations WHERE id = ?`, "obs-delete-test").Scan(&count); err != nil {
		t.Fatalf("count after delete: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 observations after delete, got %d", count)
	}

	// Verify facts are also gone.
	var factCount int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM observation_facts WHERE observation_id = ?`, "obs-delete-test").Scan(&factCount); err != nil {
		t.Fatalf("count facts after delete: %v", err)
	}
	if factCount != 0 {
		t.Errorf("expected 0 facts after delete, got %d", factCount)
	}
}

func TestDeleteObservation_IDORProtection(t *testing.T) {
	s := newTestStore(t)

	convID, err := s.CreateConversation(42, 100)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	obs := Observation{
		ID:             "obs-idor-test",
		ConversationID: convID,
		UserID:         42,
		ChatID:         100,
		ChatType:       "private",
		Type:           "decision",
		Title:          "Secret decision",
		Summary:        "Should not be deletable by other users.",
		Importance:     5,
		ContentHash:    "idor_hash",
	}

	if err := s.SaveObservation(&obs); err != nil {
		t.Fatalf("SaveObservation: %v", err)
	}

	// Attempt to delete as a different user should fail.
	err = s.DeleteObservation("obs-idor-test", 99)
	if err == nil {
		t.Fatal("expected error when deleting observation owned by another user")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' error, got: %v", err)
	}

	// Verify observation still exists.
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM observations WHERE id = ?`, "obs-idor-test").Scan(&count); err != nil {
		t.Fatalf("count after failed delete: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 observation after failed delete, got %d", count)
	}
}

func TestDeleteObservation_NotFound(t *testing.T) {
	s := newTestStore(t)

	err := s.DeleteObservation("nonexistent", 42)
	if err == nil {
		t.Fatal("expected error when deleting non-existent observation")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' error, got: %v", err)
	}
}

func TestExtractionState(t *testing.T) {
	s := newTestStore(t)

	convID, err := s.CreateConversation(42, 100)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	// Initially no state exists.
	st, err := s.GetExtractionState(convID)
	if err != nil {
		t.Fatalf("GetExtractionState: %v", err)
	}
	if st != nil {
		t.Fatalf("expected nil state for new conversation, got %+v", st)
	}

	// Increment turn count (upserts).
	if err := s.IncrementExtractionTurnCount(convID); err != nil {
		t.Fatalf("IncrementExtractionTurnCount: %v", err)
	}

	st, err = s.GetExtractionState(convID)
	if err != nil {
		t.Fatalf("GetExtractionState after increment: %v", err)
	}
	if st == nil {
		t.Fatal("expected non-nil state after increment")
	}
	if st.TurnCount != 1 {
		t.Errorf("TurnCount = %d, want 1", st.TurnCount)
	}
	if st.Status != "idle" {
		t.Errorf("Status = %q, want %q", st.Status, "idle")
	}
	if st.LastExtractedMsgRowid != 0 {
		t.Errorf("LastExtractedMsgRowid = %d, want 0", st.LastExtractedMsgRowid)
	}

	// Increment again.
	if err := s.IncrementExtractionTurnCount(convID); err != nil {
		t.Fatalf("IncrementExtractionTurnCount second: %v", err)
	}

	st, err = s.GetExtractionState(convID)
	if err != nil {
		t.Fatalf("GetExtractionState after second increment: %v", err)
	}
	if st.TurnCount != 2 {
		t.Errorf("TurnCount = %d, want 2", st.TurnCount)
	}

	// Update extraction state with specific values.
	if err := s.UpdateExtractionState(convID, 50, 0, "idle"); err != nil {
		t.Fatalf("UpdateExtractionState: %v", err)
	}

	st, err = s.GetExtractionState(convID)
	if err != nil {
		t.Fatalf("GetExtractionState after update: %v", err)
	}
	if st.LastExtractedMsgRowid != 50 {
		t.Errorf("LastExtractedMsgRowid = %d, want 50", st.LastExtractedMsgRowid)
	}
	if st.TurnCount != 0 {
		t.Errorf("TurnCount = %d, want 0 after update", st.TurnCount)
	}
	if st.LastExtractionAt == nil {
		t.Error("LastExtractionAt should be set after UpdateExtractionState")
	}
}

func TestGetObservationFactsByIDs(t *testing.T) {
	s := newTestStore(t)

	convID, err := s.CreateConversation(42, 100)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	obs1 := Observation{
		ID:             "obs-facts-1",
		ConversationID: convID,
		UserID:         42,
		ChatID:         100,
		ChatType:       "private",
		Type:           "decision",
		Title:          "First obs",
		Summary:        "First.",
		Facts:          []string{"fact-a", "fact-b"},
		Importance:     5,
		ContentHash:    "hash1",
	}
	obs2 := Observation{
		ID:             "obs-facts-2",
		ConversationID: convID,
		UserID:         42,
		ChatID:         100,
		ChatType:       "private",
		Type:           "preference",
		Title:          "Second obs",
		Summary:        "Second.",
		Facts:          []string{"fact-c"},
		Importance:     3,
		ContentHash:    "hash2",
	}

	if err := s.SaveObservation(&obs1); err != nil {
		t.Fatalf("SaveObservation obs1: %v", err)
	}
	if err := s.SaveObservation(&obs2); err != nil {
		t.Fatalf("SaveObservation obs2: %v", err)
	}

	// Batch load facts for both observations.
	factsMap, err := s.GetObservationFactsByIDs([]string{"obs-facts-1", "obs-facts-2"})
	if err != nil {
		t.Fatalf("GetObservationFactsByIDs: %v", err)
	}

	if len(factsMap) != 2 {
		t.Fatalf("expected 2 entries in factsMap, got %d", len(factsMap))
	}

	facts1 := factsMap["obs-facts-1"]
	if len(facts1) != 2 {
		t.Fatalf("expected 2 facts for obs-facts-1, got %d", len(facts1))
	}
	if facts1[0] != "fact-a" || facts1[1] != "fact-b" {
		t.Errorf("facts for obs-facts-1 = %v, want [fact-a, fact-b]", facts1)
	}

	facts2 := factsMap["obs-facts-2"]
	if len(facts2) != 1 {
		t.Fatalf("expected 1 fact for obs-facts-2, got %d", len(facts2))
	}
	if facts2[0] != "fact-c" {
		t.Errorf("facts for obs-facts-2 = %v, want [fact-c]", facts2)
	}

	// Empty IDs should return empty map.
	empty, err := s.GetObservationFactsByIDs([]string{})
	if err != nil {
		t.Fatalf("GetObservationFactsByIDs empty: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("expected empty map for empty IDs, got %d entries", len(empty))
	}

	// Non-existent IDs should return empty map.
	missing, err := s.GetObservationFactsByIDs([]string{"nonexistent"})
	if err != nil {
		t.Fatalf("GetObservationFactsByIDs nonexistent: %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("expected empty map for nonexistent IDs, got %d entries", len(missing))
	}
}

func TestCountObservations(t *testing.T) {
	s := newTestStore(t)

	convID, err := s.CreateConversation(42, 100)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	// No observations yet.
	count, err := s.CountObservations(convID)
	if err != nil {
		t.Fatalf("CountObservations: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 observations, got %d", count)
	}

	// Add two observations.
	for i, title := range []string{"First", "Second"} {
		obs := Observation{
			ConversationID: convID,
			UserID:         42,
			ChatID:         100,
			ChatType:       "private",
			Type:           "decision",
			Title:          title,
			Summary:        title + " summary.",
			Importance:     5,
			ContentHash:    title + "_hash",
		}
		if err := s.SaveObservation(&obs); err != nil {
			t.Fatalf("SaveObservation %d: %v", i, err)
		}
	}

	count, err = s.CountObservations(convID)
	if err != nil {
		t.Fatalf("CountObservations after insert: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 observations, got %d", count)
	}

	// Different conversation should have 0.
	otherConvID, err := s.CreateConversation(42, 200)
	if err != nil {
		t.Fatalf("CreateConversation other: %v", err)
	}
	count, err = s.CountObservations(otherConvID)
	if err != nil {
		t.Fatalf("CountObservations other: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 observations for other conversation, got %d", count)
	}
}

func TestRecoverableExtractions(t *testing.T) {
	s := newTestStore(t)

	// No extraction states yet.
	ids, err := s.RecoverableExtractions()
	if err != nil {
		t.Fatalf("RecoverableExtractions empty: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("expected 0 recoverable, got %d", len(ids))
	}

	convA, err := s.CreateConversation(1, 100)
	if err != nil {
		t.Fatalf("CreateConversation A: %v", err)
	}
	convB, err := s.CreateConversation(2, 200)
	if err != nil {
		t.Fatalf("CreateConversation B: %v", err)
	}
	convC, err := s.CreateConversation(3, 300)
	if err != nil {
		t.Fatalf("CreateConversation C: %v", err)
	}

	// Set different statuses.
	if err := s.UpdateExtractionState(convA, 10, 0, "failed"); err != nil {
		t.Fatalf("UpdateExtractionState A: %v", err)
	}
	if err := s.UpdateExtractionState(convB, 20, 0, "pending"); err != nil {
		t.Fatalf("UpdateExtractionState B: %v", err)
	}
	if err := s.UpdateExtractionState(convC, 30, 0, "idle"); err != nil {
		t.Fatalf("UpdateExtractionState C: %v", err)
	}

	ids, err = s.RecoverableExtractions()
	if err != nil {
		t.Fatalf("RecoverableExtractions: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("expected 2 recoverable, got %d: %v", len(ids), ids)
	}

	found := map[string]bool{}
	for _, id := range ids {
		found[id] = true
	}
	if !found[convA] {
		t.Errorf("expected convA (%s) in recoverable list", convA)
	}
	if !found[convB] {
		t.Errorf("expected convB (%s) in recoverable list", convB)
	}
	if found[convC] {
		t.Errorf("convC (%s) with 'idle' status should not be in recoverable list", convC)
	}
}

func TestGetRecentObservationTitles(t *testing.T) {
	s := newTestStore(t)

	convID, err := s.CreateConversation(42, 100)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	// No observations yet.
	titles, err := s.GetRecentObservationTitles(convID, 10)
	if err != nil {
		t.Fatalf("GetRecentObservationTitles: %v", err)
	}
	if len(titles) != 0 {
		t.Errorf("expected 0 titles, got %d", len(titles))
	}

	// Add observations.
	for _, title := range []string{"Alpha", "Beta", "Gamma"} {
		obs := Observation{
			ConversationID: convID,
			UserID:         42,
			ChatID:         100,
			ChatType:       "private",
			Type:           "decision",
			Title:          title,
			Summary:        title + " summary.",
			Importance:     5,
			ContentHash:    title + "_hash",
		}
		if err := s.SaveObservation(&obs); err != nil {
			t.Fatalf("SaveObservation %s: %v", title, err)
		}
	}

	titles, err = s.GetRecentObservationTitles(convID, 2)
	if err != nil {
		t.Fatalf("GetRecentObservationTitles with limit: %v", err)
	}
	if len(titles) != 2 {
		t.Fatalf("expected 2 titles with limit=2, got %d", len(titles))
	}
	// Most recent first.
	if titles[0] != "Gamma" {
		t.Errorf("titles[0] = %q, want %q", titles[0], "Gamma")
	}
	if titles[1] != "Beta" {
		t.Errorf("titles[1] = %q, want %q", titles[1], "Beta")
	}
}

func TestGetMaxMessageRowid(t *testing.T) {
	s := newTestStore(t)

	convID, err := s.CreateConversation(42, 100)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	// No messages yet.
	maxRowid, err := s.GetMaxMessageRowid(convID)
	if err != nil {
		t.Fatalf("GetMaxMessageRowid empty: %v", err)
	}
	if maxRowid != 0 {
		t.Errorf("expected 0 for empty conversation, got %d", maxRowid)
	}

	// Add messages.
	c1, _ := json.Marshal("Hello")
	c2, _ := json.Marshal("World")
	if err := s.AppendMessage(convID, "user", c1); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if err := s.AppendMessage(convID, "assistant", c2); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	maxRowid, err = s.GetMaxMessageRowid(convID)
	if err != nil {
		t.Fatalf("GetMaxMessageRowid: %v", err)
	}
	if maxRowid <= 0 {
		t.Errorf("expected positive max rowid, got %d", maxRowid)
	}
}

func TestGetMessagesSinceRowid(t *testing.T) {
	s := newTestStore(t)

	convID, err := s.CreateConversation(42, 100)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	// Add 5 messages.
	for i := 0; i < 5; i++ {
		content, _ := json.Marshal(i)
		if err := s.AppendMessage(convID, "user", content); err != nil {
			t.Fatalf("AppendMessage(%d): %v", i, err)
		}
	}

	maxRowid, err := s.GetMaxMessageRowid(convID)
	if err != nil {
		t.Fatalf("GetMaxMessageRowid: %v", err)
	}

	// Get messages after rowid 0 up to max (all of them).
	msgs, err := s.GetMessagesSinceRowid(convID, 0, maxRowid)
	if err != nil {
		t.Fatalf("GetMessagesSinceRowid: %v", err)
	}
	if len(msgs) != 5 {
		t.Fatalf("expected 5 messages, got %d", len(msgs))
	}

	// Get only the last 2 messages.
	msgs, err = s.GetMessagesSinceRowid(convID, maxRowid-2, maxRowid)
	if err != nil {
		t.Fatalf("GetMessagesSinceRowid partial: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}

	// Verify content of the last 2 messages.
	var val0, val1 int
	if err := json.Unmarshal(msgs[0].Content, &val0); err != nil {
		t.Fatalf("unmarshal msgs[0]: %v", err)
	}
	if err := json.Unmarshal(msgs[1].Content, &val1); err != nil {
		t.Fatalf("unmarshal msgs[1]: %v", err)
	}
	if val0 != 3 || val1 != 4 {
		t.Errorf("expected messages 3 and 4, got %d and %d", val0, val1)
	}
}
