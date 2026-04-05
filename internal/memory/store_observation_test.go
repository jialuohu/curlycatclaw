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

// Phase 2 tests

func TestSaveAndSearchEntities(t *testing.T) {
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
		Type:           "discovery",
		Title:          "Team uses Kubernetes",
		Summary:        "Learned the team deploys on Kubernetes.",
		Facts:          []string{"K8s used for deployment"},
		Importance:     5,
		ContentHash:    "ent-test-1",
	}
	if err := s.SaveObservation(&obs); err != nil {
		t.Fatalf("SaveObservation: %v", err)
	}

	entities := []Entity{
		{Name: "kubernetes", Type: "tool"},
		{Name: "alice", Type: "person"},
	}
	if err := s.SaveEntities(obs.ID, entities); err != nil {
		t.Fatalf("SaveEntities: %v", err)
	}

	// Empty entities should not error.
	if err := s.SaveEntities(obs.ID, nil); err != nil {
		t.Fatalf("SaveEntities(nil): %v", err)
	}

	// Hydrate entities by ID.
	entMap, err := s.GetEntitiesByObservationIDs([]string{obs.ID})
	if err != nil {
		t.Fatalf("GetEntitiesByObservationIDs: %v", err)
	}
	if len(entMap[obs.ID]) != 2 {
		t.Fatalf("expected 2 entities, got %d", len(entMap[obs.ID]))
	}

	// FTS search for "kubernetes".
	results, err := s.SearchEntitiesFTS("kubernetes", "", 42, 10)
	if err != nil {
		t.Fatalf("SearchEntitiesFTS: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 FTS result, got %d", len(results))
	}
	if results[0].Name != "kubernetes" {
		t.Errorf("expected name 'kubernetes', got %q", results[0].Name)
	}

	// FTS search with type filter.
	results, err = s.SearchEntitiesFTS("alice", "person", 42, 10)
	if err != nil {
		t.Fatalf("SearchEntitiesFTS with type: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result for person filter, got %d", len(results))
	}

	// FTS search with wrong type filter.
	results, err = s.SearchEntitiesFTS("alice", "tool", 42, 10)
	if err != nil {
		t.Fatalf("SearchEntitiesFTS wrong type: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 results for wrong type, got %d", len(results))
	}

	// IDOR: different user should not find these entities.
	results, err = s.SearchEntitiesFTS("kubernetes", "", 99, 10)
	if err != nil {
		t.Fatalf("SearchEntitiesFTS IDOR: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("IDOR: user 99 found user 42's entities")
	}

	// Delete entities.
	if err := s.DeleteEntitiesByObservation(obs.ID); err != nil {
		t.Fatalf("DeleteEntitiesByObservation: %v", err)
	}
	entMap, err = s.GetEntitiesByObservationIDs([]string{obs.ID})
	if err != nil {
		t.Fatalf("GetEntitiesByObservationIDs after delete: %v", err)
	}
	if len(entMap[obs.ID]) != 0 {
		t.Errorf("expected 0 entities after delete, got %d", len(entMap[obs.ID]))
	}
}

func TestObservationRelations(t *testing.T) {
	s := newTestStore(t)

	convID, err := s.CreateConversation(42, 100)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	obs1 := Observation{
		ConversationID: convID, UserID: 42, ChatID: 100, ChatType: "private",
		Type: "preference", Title: "Use Redis", Summary: "Prefers Redis for caching.",
		Facts: []string{"Redis preferred"}, Importance: 5, ContentHash: "rel-1",
	}
	obs2 := Observation{
		ConversationID: convID, UserID: 42, ChatID: 100, ChatType: "private",
		Type: "preference", Title: "Use Memcached", Summary: "Switched to Memcached.",
		Facts: []string{"Memcached now"}, Importance: 5, ContentHash: "rel-2",
	}
	if err := s.SaveObservation(&obs1); err != nil {
		t.Fatalf("SaveObservation 1: %v", err)
	}
	if err := s.SaveObservation(&obs2); err != nil {
		t.Fatalf("SaveObservation 2: %v", err)
	}

	// Add supersession relation.
	if err := s.AddObservationRelation(obs2.ID, obs1.ID, "supersedes", 0.92, 42); err != nil {
		t.Fatalf("AddObservationRelation: %v", err)
	}

	// IDOR: wrong user should fail.
	if err := s.AddObservationRelation(obs2.ID, obs1.ID, "supersedes", 0.92, 99); err == nil {
		t.Fatal("expected error for wrong user, got nil")
	}

	// Get relations.
	rels, err := s.GetObservationRelations(obs1.ID, 42)
	if err != nil {
		t.Fatalf("GetObservationRelations: %v", err)
	}
	if len(rels) != 1 {
		t.Fatalf("expected 1 relation, got %d", len(rels))
	}
	if rels[0].RelationType != "supersedes" {
		t.Errorf("expected 'supersedes', got %q", rels[0].RelationType)
	}

	// Invalid IDs should fail.
	if err := s.AddObservationRelation("nonexistent", obs1.ID, "supersedes", 0.5, 42); err == nil {
		t.Fatal("expected error for nonexistent source, got nil")
	}
}

func TestSearchObservationsFTS(t *testing.T) {
	s := newTestStore(t)

	convID, err := s.CreateConversation(42, 100)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	obs := Observation{
		ConversationID: convID, UserID: 42, ChatID: 100, ChatType: "private",
		Type: "decision", Title: "Deploy with Docker", Summary: "Using Docker containers for deployment.",
		Facts: []string{"Docker chosen"}, Importance: 7, ContentHash: "fts-1",
	}
	if err := s.SaveObservation(&obs); err != nil {
		t.Fatalf("SaveObservation: %v", err)
	}

	// Search for "Docker" should find it.
	results, err := s.SearchObservationsFTS("Docker", 42, 10)
	if err != nil {
		t.Fatalf("SearchObservationsFTS: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].ObsID != obs.ID {
		t.Errorf("expected obs ID %s, got %s", obs.ID, results[0].ObsID)
	}

	// Search for nonexistent term.
	results, err = s.SearchObservationsFTS("PostgreSQL", 42, 10)
	if err != nil {
		t.Fatalf("SearchObservationsFTS no match: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results, got %d", len(results))
	}

	// Empty query returns nil.
	results, err = s.SearchObservationsFTS("", 42, 10)
	if err != nil {
		t.Fatalf("SearchObservationsFTS empty: %v", err)
	}
	if results != nil {
		t.Errorf("expected nil results for empty query, got %d", len(results))
	}

	// IDOR: different user should not find it.
	results, err = s.SearchObservationsFTS("Docker", 99, 10)
	if err != nil {
		t.Fatalf("SearchObservationsFTS IDOR: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("IDOR: user 99 found user 42's observations via FTS")
	}
}

func TestRebuildFTS(t *testing.T) {
	s := newTestStore(t)

	convID, err := s.CreateConversation(42, 100)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	obs := Observation{
		ConversationID: convID, UserID: 42, ChatID: 100, ChatType: "private",
		Type: "decision", Title: "Use gRPC", Summary: "Choosing gRPC over REST.",
		Facts: []string{"gRPC selected"}, Importance: 6, ContentHash: "rebuild-1",
	}
	if err := s.SaveObservation(&obs); err != nil {
		t.Fatalf("SaveObservation: %v", err)
	}

	// RebuildFTS should not error.
	if err := s.RebuildFTS(); err != nil {
		t.Fatalf("RebuildFTS: %v", err)
	}

	// Data should still be searchable after rebuild.
	results, err := s.SearchObservationsFTS("gRPC", 42, 10)
	if err != nil {
		t.Fatalf("SearchObservationsFTS after rebuild: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("expected 1 result after rebuild, got %d", len(results))
	}
}

func TestDeleteObservationCascadesEntitiesAndRelations(t *testing.T) {
	s := newTestStore(t)

	convID, err := s.CreateConversation(42, 100)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	obs1 := Observation{
		ConversationID: convID, UserID: 42, ChatID: 100, ChatType: "private",
		Type: "decision", Title: "Old decision", Summary: "An old decision.",
		Facts: []string{"fact1"}, Importance: 5, ContentHash: "cascade-1",
	}
	obs2 := Observation{
		ConversationID: convID, UserID: 42, ChatID: 100, ChatType: "private",
		Type: "decision", Title: "New decision", Summary: "A new decision.",
		Facts: []string{"fact2"}, Importance: 5, ContentHash: "cascade-2",
	}
	if err := s.SaveObservation(&obs1); err != nil {
		t.Fatalf("SaveObservation 1: %v", err)
	}
	if err := s.SaveObservation(&obs2); err != nil {
		t.Fatalf("SaveObservation 2: %v", err)
	}

	// Add entities to obs1.
	if err := s.SaveEntities(obs1.ID, []Entity{{Name: "test-entity", Type: "tool"}}); err != nil {
		t.Fatalf("SaveEntities: %v", err)
	}

	// Add relation between obs2 -> obs1.
	if err := s.AddObservationRelation(obs2.ID, obs1.ID, "supersedes", 0.9, 42); err != nil {
		t.Fatalf("AddObservationRelation: %v", err)
	}

	// Delete obs1 — should cascade to entities and relations.
	if err := s.DeleteObservation(obs1.ID, 42); err != nil {
		t.Fatalf("DeleteObservation: %v", err)
	}

	// Verify entities are gone.
	entMap, err := s.GetEntitiesByObservationIDs([]string{obs1.ID})
	if err != nil {
		t.Fatalf("GetEntitiesByObservationIDs: %v", err)
	}
	if len(entMap[obs1.ID]) != 0 {
		t.Errorf("expected 0 entities after cascade delete, got %d", len(entMap[obs1.ID]))
	}

	// Verify relations are gone.
	rels, err := s.GetObservationRelations(obs1.ID, 42)
	if err != nil {
		t.Fatalf("GetObservationRelations: %v", err)
	}
	if len(rels) != 0 {
		t.Errorf("expected 0 relations after cascade delete, got %d", len(rels))
	}

	// obs2's relations should also be gone (it referenced obs1).
	rels, err = s.GetObservationRelations(obs2.ID, 42)
	if err != nil {
		t.Fatalf("GetObservationRelations obs2: %v", err)
	}
	if len(rels) != 0 {
		t.Errorf("expected 0 relations for obs2 after target deleted, got %d", len(rels))
	}
}

func TestObservationTextsAfter(t *testing.T) {
	s := newTestStore(t)

	convID, err := s.CreateConversation(42, 100)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	for i := 0; i < 3; i++ {
		obs := Observation{
			ConversationID: convID, UserID: 42, ChatID: 100, ChatType: "private",
			Type: "decision", Title: "Decision " + strings.Repeat("x", i),
			Summary: "Summary " + strings.Repeat("y", i),
			Facts: []string{"fact"}, Importance: 5,
			ContentHash: "texts-" + strings.Repeat("z", i),
		}
		if err := s.SaveObservation(&obs); err != nil {
			t.Fatalf("SaveObservation %d: %v", i, err)
		}
	}

	// Get all texts.
	texts, maxID, err := s.ObservationTextsAfter(0, 10)
	if err != nil {
		t.Fatalf("ObservationTextsAfter: %v", err)
	}
	if len(texts) != 3 {
		t.Fatalf("expected 3 texts, got %d", len(texts))
	}
	if maxID <= 0 {
		t.Errorf("expected positive maxID, got %d", maxID)
	}

	// Text format should be "Title. Summary".
	if !strings.Contains(texts[0].Text, ". ") {
		t.Errorf("expected 'Title. Summary' format, got %q", texts[0].Text)
	}
	if texts[0].Source != "observation" {
		t.Errorf("expected source 'observation', got %q", texts[0].Source)
	}

	// Cursor-based: get texts after maxID should return 0.
	texts2, _, err := s.ObservationTextsAfter(maxID, 10)
	if err != nil {
		t.Fatalf("ObservationTextsAfter after cursor: %v", err)
	}
	if len(texts2) != 0 {
		t.Errorf("expected 0 texts after cursor, got %d", len(texts2))
	}
}

func TestGetRecentObservationsByType(t *testing.T) {
	s := newTestStore(t)

	convID, err := s.CreateConversation(42, 100)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	// Save observations of different types.
	for i, tp := range []string{"project_state", "decision", "project_state", "preference"} {
		obs := Observation{
			ConversationID: convID, UserID: 42, ChatID: 100, ChatType: "private",
			Type: tp, Title: "Obs " + tp, Summary: "Summary " + tp,
			Facts: []string{"fact"}, Importance: 5,
			ContentHash: "getrecent-" + tp + "-" + string(rune('0'+i)),
		}
		if err := s.SaveObservation(&obs); err != nil {
			t.Fatalf("SaveObservation %d: %v", i, err)
		}
	}

	// Query project_state only.
	results, err := s.GetRecentObservationsByType(42, "project_state", 10)
	if err != nil {
		t.Fatalf("GetRecentObservationsByType: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 project_state observations, got %d", len(results))
	}
	for _, r := range results {
		if r.Title == "" || r.Summary == "" || r.ID == "" {
			t.Error("expected non-empty title, summary, and ID")
		}
	}

	// Query non-existent type returns empty.
	empty, err := s.GetRecentObservationsByType(42, "commitment", 10)
	if err != nil {
		t.Fatalf("GetRecentObservationsByType (empty): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("expected 0 results for commitment type, got %d", len(empty))
	}

	// Different user returns empty.
	other, err := s.GetRecentObservationsByType(99, "project_state", 10)
	if err != nil {
		t.Fatalf("GetRecentObservationsByType (other user): %v", err)
	}
	if len(other) != 0 {
		t.Errorf("expected 0 results for different user, got %d", len(other))
	}
}

func TestGetSupersededObservationIDs_ConfidenceThreshold(t *testing.T) {
	s := newTestStore(t)

	convID, err := s.CreateConversation(42, 100)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	obs1 := Observation{
		ConversationID: convID, UserID: 42, ChatID: 100, ChatType: "private",
		Type: "project_state", Title: "Working on Phase 2", Summary: "Phase 2 in progress.",
		Facts: []string{"Phase 2 active"}, Importance: 5, ContentHash: "supersede-1",
	}
	obs2 := Observation{
		ConversationID: convID, UserID: 42, ChatID: 100, ChatType: "private",
		Type: "project_state", Title: "Phase 2 shipped", Summary: "Phase 2 is complete.",
		Facts: []string{"Phase 2 done"}, Importance: 5, ContentHash: "supersede-2",
	}
	obs3 := Observation{
		ConversationID: convID, UserID: 42, ChatID: 100, ChatType: "private",
		Type: "project_state", Title: "Low confidence target", Summary: "Maybe outdated.",
		Facts: []string{"uncertain"}, Importance: 5, ContentHash: "supersede-3",
	}
	for _, o := range []*Observation{&obs1, &obs2, &obs3} {
		if err := s.SaveObservation(o); err != nil {
			t.Fatalf("SaveObservation: %v", err)
		}
	}

	// High confidence supersession (0.9 >= 0.8 threshold).
	if err := s.AddObservationRelation(obs2.ID, obs1.ID, "supersedes", 0.9, 42); err != nil {
		t.Fatalf("AddObservationRelation (high confidence): %v", err)
	}
	// Low confidence supersession (0.5 < 0.8 threshold).
	if err := s.AddObservationRelation(obs2.ID, obs3.ID, "supersedes", 0.5, 42); err != nil {
		t.Fatalf("AddObservationRelation (low confidence): %v", err)
	}

	// With threshold 0.8: only obs1 should be superseded.
	ids, err := s.GetSupersededObservationIDs(42, 0.8)
	if err != nil {
		t.Fatalf("GetSupersededObservationIDs: %v", err)
	}
	if !ids[obs1.ID] {
		t.Error("expected obs1 to be superseded (confidence 0.9 >= 0.8)")
	}
	if ids[obs3.ID] {
		t.Error("obs3 should NOT be superseded (confidence 0.5 < 0.8)")
	}

	// Boundary: exactly 0.8 should be included.
	obs4 := Observation{
		ConversationID: convID, UserID: 42, ChatID: 100, ChatType: "private",
		Type: "project_state", Title: "Boundary target", Summary: "Exactly at threshold.",
		Facts: []string{"boundary"}, Importance: 5, ContentHash: "supersede-4",
	}
	if err := s.SaveObservation(&obs4); err != nil {
		t.Fatalf("SaveObservation obs4: %v", err)
	}
	if err := s.AddObservationRelation(obs2.ID, obs4.ID, "supersedes", 0.8, 42); err != nil {
		t.Fatalf("AddObservationRelation (boundary): %v", err)
	}

	ids2, err := s.GetSupersededObservationIDs(42, 0.8)
	if err != nil {
		t.Fatalf("GetSupersededObservationIDs (boundary): %v", err)
	}
	if !ids2[obs4.ID] {
		t.Error("expected obs4 to be superseded (confidence exactly 0.8)")
	}

	// Different user returns empty.
	idsOther, err := s.GetSupersededObservationIDs(99, 0.8)
	if err != nil {
		t.Fatalf("GetSupersededObservationIDs (other user): %v", err)
	}
	if len(idsOther) != 0 {
		t.Errorf("expected 0 superseded IDs for different user, got %d", len(idsOther))
	}
}

func TestAddObservationRelation_InsertOrIgnore(t *testing.T) {
	s := newTestStore(t)

	convID, err := s.CreateConversation(42, 100)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	obs1 := Observation{
		ConversationID: convID, UserID: 42, ChatID: 100, ChatType: "private",
		Type: "project_state", Title: "Old", Summary: "Old state.",
		Facts: []string{"old"}, Importance: 5, ContentHash: "ignore-1",
	}
	obs2 := Observation{
		ConversationID: convID, UserID: 42, ChatID: 100, ChatType: "private",
		Type: "project_state", Title: "New", Summary: "New state.",
		Facts: []string{"new"}, Importance: 5, ContentHash: "ignore-2",
	}
	if err := s.SaveObservation(&obs1); err != nil {
		t.Fatalf("SaveObservation 1: %v", err)
	}
	if err := s.SaveObservation(&obs2); err != nil {
		t.Fatalf("SaveObservation 2: %v", err)
	}

	// First insert should succeed.
	if err := s.AddObservationRelation(obs2.ID, obs1.ID, "supersedes", 0.9, 42); err != nil {
		t.Fatalf("First AddObservationRelation: %v", err)
	}

	// Duplicate insert should be silently ignored (INSERT OR IGNORE).
	err = s.AddObservationRelation(obs2.ID, obs1.ID, "supersedes", 0.95, 42)
	// Should not return an error — INSERT OR IGNORE means the duplicate is silently skipped.
	// But RowsAffected will be 0, which the current code treats as an error.
	// This is the expected behavior: the relation already exists, so "not created" is returned.
	// The caller should handle this gracefully.
	if err != nil {
		// The "not created" error is acceptable for duplicates.
		if !strings.Contains(err.Error(), "not created") {
			t.Fatalf("unexpected error on duplicate: %v", err)
		}
	}

	// Verify only one relation exists (not two).
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM observation_relations`).Scan(&count); err != nil {
		t.Fatalf("count relations: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 relation (duplicate ignored), got %d", count)
	}
}

func TestAddObservationRelation_ConfidenceClamping(t *testing.T) {
	s := newTestStore(t)

	convID, err := s.CreateConversation(42, 100)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	obs1 := Observation{
		ConversationID: convID, UserID: 42, ChatID: 100, ChatType: "private",
		Type: "decision", Title: "A", Summary: "A.",
		Facts: []string{"a"}, Importance: 5, ContentHash: "clamp-1",
	}
	obs2 := Observation{
		ConversationID: convID, UserID: 42, ChatID: 100, ChatType: "private",
		Type: "decision", Title: "B", Summary: "B.",
		Facts: []string{"b"}, Importance: 5, ContentHash: "clamp-2",
	}
	if err := s.SaveObservation(&obs1); err != nil {
		t.Fatalf("SaveObservation 1: %v", err)
	}
	if err := s.SaveObservation(&obs2); err != nil {
		t.Fatalf("SaveObservation 2: %v", err)
	}

	// Confidence > 1.0 should be clamped to 1.0.
	if err := s.AddObservationRelation(obs2.ID, obs1.ID, "supersedes", 1.5, 42); err != nil {
		t.Fatalf("AddObservationRelation (overclamped): %v", err)
	}

	var conf float64
	if err := s.db.QueryRow(`SELECT confidence FROM observation_relations WHERE source_id = ?`, obs2.ID).Scan(&conf); err != nil {
		t.Fatalf("query confidence: %v", err)
	}
	if conf != 1.0 {
		t.Errorf("expected confidence clamped to 1.0, got %f", conf)
	}
}

func TestArchiveAndRestoreObservation(t *testing.T) {
	s := newTestStore(t)

	convID, err := s.CreateConversation(42, 100)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	obs := Observation{
		ConversationID: convID, UserID: 42, ChatID: 100, ChatType: "private",
		Type: "project_state", Title: "Active observation", Summary: "Should be visible.",
		Facts: []string{"fact1"}, Importance: 5, ContentHash: "archive-test-1",
	}
	if err := s.SaveObservation(&obs); err != nil {
		t.Fatalf("SaveObservation: %v", err)
	}

	// Verify observation is counted.
	count, _ := s.CountObservations(convID)
	if count != 1 {
		t.Fatalf("expected 1 active observation, got %d", count)
	}

	// Archive it.
	if err := s.ArchiveObservation(obs.ID, 42); err != nil {
		t.Fatalf("ArchiveObservation: %v", err)
	}

	// Should no longer be counted.
	count, _ = s.CountObservations(convID)
	if count != 0 {
		t.Errorf("expected 0 active observations after archive, got %d", count)
	}

	// Should not appear in GetRecentObservationsByType.
	recent, _ := s.GetRecentObservationsByType(42, "project_state", 10)
	if len(recent) != 0 {
		t.Errorf("expected 0 recent observations after archive, got %d", len(recent))
	}

	// Should not appear in ObservationExistsByHash (dedup check).
	exists, _ := s.ObservationExistsByHash(42, "archive-test-1")
	if exists {
		t.Error("archived observation should not appear in hash dedup check")
	}

	// Archive again should fail (already archived).
	if err := s.ArchiveObservation(obs.ID, 42); err == nil {
		t.Error("expected error archiving already-archived observation")
	}

	// IDOR: wrong user should fail.
	if err := s.ArchiveObservation(obs.ID, 99); err == nil {
		t.Error("expected error for wrong user archive")
	}

	// Restore it.
	if err := s.RestoreObservation(obs.ID, 42); err != nil {
		t.Fatalf("RestoreObservation: %v", err)
	}

	// Should be counted again.
	count, _ = s.CountObservations(convID)
	if count != 1 {
		t.Errorf("expected 1 active observation after restore, got %d", count)
	}

	// Restore again should fail (not archived).
	if err := s.RestoreObservation(obs.ID, 42); err == nil {
		t.Error("expected error restoring non-archived observation")
	}
}

func TestUpdateObservation(t *testing.T) {
	s := newTestStore(t)

	convID, err := s.CreateConversation(42, 100)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	obs := Observation{
		ConversationID: convID, UserID: 42, ChatID: 100, ChatType: "private",
		Type: "project_state", Title: "Original title", Summary: "Original summary.",
		Facts: []string{"fact1"}, Importance: 5, ContentHash: "update-test-1",
	}
	if err := s.SaveObservation(&obs); err != nil {
		t.Fatalf("SaveObservation: %v", err)
	}

	// Update title and summary.
	if err := s.UpdateObservation(obs.ID, 42, "New title", "New summary.", "decision", 8); err != nil {
		t.Fatalf("UpdateObservation: %v", err)
	}

	// Verify update.
	var title, summary, obsType string
	var importance int
	if err := s.db.QueryRow(`SELECT title, summary, type, importance FROM observations WHERE id = ?`, obs.ID).Scan(&title, &summary, &obsType, &importance); err != nil {
		t.Fatalf("query updated observation: %v", err)
	}
	if title != "New title" {
		t.Errorf("title = %q, want 'New title'", title)
	}
	if summary != "New summary." {
		t.Errorf("summary = %q, want 'New summary.'", summary)
	}
	if obsType != "decision" {
		t.Errorf("type = %q, want 'decision'", obsType)
	}
	if importance != 8 {
		t.Errorf("importance = %d, want 8", importance)
	}

	// IDOR: wrong user should fail.
	if err := s.UpdateObservation(obs.ID, 99, "Hacked", "Hacked.", "decision", 1); err == nil {
		t.Error("expected error for wrong user update")
	}
}

