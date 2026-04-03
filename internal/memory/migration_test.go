package memory

import (
	"encoding/json"
	"testing"
)

func TestEmbedderStateCRUD(t *testing.T) {
	s := newTestStore(t)

	// Initially nil (no row).
	st, err := s.GetEmbedderState()
	if err != nil {
		t.Fatalf("GetEmbedderState: %v", err)
	}
	if st != nil {
		t.Fatalf("expected nil state, got %+v", st)
	}

	// Init.
	if err := s.InitEmbedderState("fnv-384"); err != nil {
		t.Fatalf("InitEmbedderState: %v", err)
	}

	st, err = s.GetEmbedderState()
	if err != nil {
		t.Fatalf("GetEmbedderState: %v", err)
	}
	if st == nil {
		t.Fatal("expected non-nil state")
	}
	if st.ActiveEmbedder != "fnv-384" {
		t.Errorf("ActiveEmbedder = %q, want %q", st.ActiveEmbedder, "fnv-384")
	}
	if st.ActiveVersion != 0 {
		t.Errorf("ActiveVersion = %d, want 0", st.ActiveVersion)
	}
	if st.MigrationStatus != "" {
		t.Errorf("MigrationStatus = %q, want empty", st.MigrationStatus)
	}

	// Init is idempotent (OR IGNORE).
	if err := s.InitEmbedderState("ollama-bge-m3-1024"); err != nil {
		t.Fatalf("InitEmbedderState (second): %v", err)
	}
	st, _ = s.GetEmbedderState()
	if st.ActiveEmbedder != "fnv-384" {
		t.Errorf("InitEmbedderState should not overwrite: got %q", st.ActiveEmbedder)
	}
}

func TestEmbedderStateUpdateConfig(t *testing.T) {
	s := newTestStore(t)
	s.InitEmbedderState("fnv-384")

	// Update config at steady state (A6).
	if err := s.UpdateEmbedderConfig("fnv-384", "fnv", "", 384); err != nil {
		t.Fatalf("UpdateEmbedderConfig: %v", err)
	}

	st, _ := s.GetEmbedderState()
	if st.OldEmbedderType != "fnv" {
		t.Errorf("OldEmbedderType = %q, want %q", st.OldEmbedderType, "fnv")
	}
	if st.OldEmbedderDim != 384 {
		t.Errorf("OldEmbedderDim = %d, want 384", st.OldEmbedderDim)
	}
}

func TestEmbedderStateMigrationLifecycle(t *testing.T) {
	s := newTestStore(t)
	s.InitEmbedderState("fnv-384")
	s.UpdateEmbedderConfig("fnv-384", "fnv", "", 384)

	// Start migration.
	if err := s.StartMigration("ollama-bge-m3-1024", 1, "fnv", "", 384); err != nil {
		t.Fatalf("StartMigration: %v", err)
	}

	st, _ := s.GetEmbedderState()
	if st.MigrationStatus != "running" {
		t.Errorf("MigrationStatus = %q, want %q", st.MigrationStatus, "running")
	}
	if st.MigratingEmbedder != "ollama-bge-m3-1024" {
		t.Errorf("MigratingEmbedder = %q, want %q", st.MigratingEmbedder, "ollama-bge-m3-1024")
	}
	if st.MigratingVersion != 1 {
		t.Errorf("MigratingVersion = %d, want 1", st.MigratingVersion)
	}
	if st.StartedAt == nil {
		t.Error("StartedAt should be set")
	}

	// Update cursor.
	if err := s.UpdateMigrationCursor(100, 50, 10); err != nil {
		t.Fatalf("UpdateMigrationCursor: %v", err)
	}
	st, _ = s.GetEmbedderState()
	if st.LastMsgID != 100 || st.LastNoteID != 50 || st.LastSummaryID != 10 {
		t.Errorf("cursors = (%d, %d, %d), want (100, 50, 10)",
			st.LastMsgID, st.LastNoteID, st.LastSummaryID)
	}

	// Set completing.
	if err := s.SetMigrationStatus("completing"); err != nil {
		t.Fatalf("SetMigrationStatus: %v", err)
	}
	st, _ = s.GetEmbedderState()
	if st.MigrationStatus != "completing" {
		t.Errorf("MigrationStatus = %q, want %q", st.MigrationStatus, "completing")
	}

	// Complete.
	if err := s.CompleteMigration("ollama-bge-m3-1024", 1); err != nil {
		t.Fatalf("CompleteMigration: %v", err)
	}
	st, _ = s.GetEmbedderState()
	if st.ActiveEmbedder != "ollama-bge-m3-1024" {
		t.Errorf("ActiveEmbedder = %q, want %q", st.ActiveEmbedder, "ollama-bge-m3-1024")
	}
	if st.ActiveVersion != 1 {
		t.Errorf("ActiveVersion = %d, want 1", st.ActiveVersion)
	}
	if st.MigrationStatus != "" {
		t.Errorf("MigrationStatus = %q, want empty", st.MigrationStatus)
	}
	if st.MigratingEmbedder != "" {
		t.Errorf("MigratingEmbedder should be cleared, got %q", st.MigratingEmbedder)
	}
	if st.LastMsgID != 0 {
		t.Errorf("LastMsgID should be reset, got %d", st.LastMsgID)
	}
}

func TestUpdateConfigNotDuringMigration(t *testing.T) {
	s := newTestStore(t)
	s.InitEmbedderState("fnv-384")
	s.UpdateEmbedderConfig("fnv-384", "fnv", "", 384)

	// Start migration.
	s.StartMigration("ollama-bge-m3-1024", 1, "fnv", "", 384)

	// UpdateEmbedderConfig should NOT overwrite during migration
	// (WHERE clause filters on null/empty migration_status).
	s.UpdateEmbedderConfig("voyage-voyage-3-lite-512", "voyage", "voyage-3-lite", 512)

	st, _ := s.GetEmbedderState()
	if st.OldEmbedderType != "fnv" {
		t.Errorf("UpdateEmbedderConfig should not run during migration, got type=%q", st.OldEmbedderType)
	}
}

func TestKeysetPagination(t *testing.T) {
	s := newTestStore(t)

	// Insert some messages.
	convID := createTestConversation(t, s, 1, 0)
	for i := 0; i < 5; i++ {
		s.AppendMessage(convID, "user", mustJSON(map[string]any{
			"type": "text", "text": "message " + string(rune('A'+i)),
		}))
	}

	// Fetch first 2.
	texts, maxID, err := s.MessageTextsAfter(0, 2)
	if err != nil {
		t.Fatalf("MessageTextsAfter(0, 2): %v", err)
	}
	if len(texts) != 2 {
		t.Fatalf("expected 2 texts, got %d", len(texts))
	}
	if maxID == 0 {
		t.Fatal("maxID should be non-zero")
	}

	// Fetch next 2.
	texts2, maxID2, err := s.MessageTextsAfter(maxID, 2)
	if err != nil {
		t.Fatalf("MessageTextsAfter(%d, 2): %v", maxID, err)
	}
	if len(texts2) != 2 {
		t.Fatalf("expected 2 texts, got %d", len(texts2))
	}
	if maxID2 <= maxID {
		t.Errorf("maxID2 (%d) should be > maxID (%d)", maxID2, maxID)
	}

	// Fetch remaining.
	texts3, _, err := s.MessageTextsAfter(maxID2, 2)
	if err != nil {
		t.Fatalf("MessageTextsAfter(%d, 2): %v", maxID2, err)
	}
	if len(texts3) != 1 {
		t.Fatalf("expected 1 text, got %d", len(texts3))
	}

	// Exhausted.
	texts4, _, err := s.MessageTextsAfter(maxID2+100, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(texts4) != 0 {
		t.Fatalf("expected 0 texts, got %d", len(texts4))
	}
}

func TestNoteTextsAfter_NoTable(t *testing.T) {
	s := newTestStore(t)

	// Notes table doesn't exist yet — should return nil, not error.
	texts, maxID, err := s.NoteTextsAfter(0, 10)
	if err != nil {
		t.Fatalf("NoteTextsAfter: %v", err)
	}
	if texts != nil {
		t.Fatalf("expected nil, got %v", texts)
	}
	if maxID != 0 {
		t.Fatalf("expected maxID=0, got %d", maxID)
	}
}

func TestVersionedNames(t *testing.T) {
	names := VersionedNames(3)
	if names[0] != "curlycatclaw_messages_v3" {
		t.Errorf("messages = %q", names[0])
	}
	if names[1] != "curlycatclaw_notes_v3" {
		t.Errorf("notes = %q", names[1])
	}
	if names[2] != "curlycatclaw_summaries_v3" {
		t.Errorf("summaries = %q", names[2])
	}
}

func TestCollectionIndex(t *testing.T) {
	if collectionIndex("message") != 0 {
		t.Error("message should be 0")
	}
	if collectionIndex("note") != 1 {
		t.Error("note should be 1")
	}
	if collectionIndex("summary") != 2 {
		t.Error("summary should be 2")
	}
}

func TestOllamaDefaults(t *testing.T) {
	e := NewOllamaEmbedder("", "", 0)
	if e.model != "bge-m3" {
		t.Errorf("default model = %q, want %q", e.model, "bge-m3")
	}
	if e.dim != 1024 {
		t.Errorf("default dim = %d, want 1024", e.dim)
	}
	if e.Name() != "ollama-bge-m3-1024" {
		t.Errorf("Name() = %q, want %q", e.Name(), "ollama-bge-m3-1024")
	}
}

// helpers

func createTestConversation(t *testing.T, s *Store, userID, chatID int64) string {
	t.Helper()
	convID, _, err := s.GetActiveConversation(userID, chatID, "private")
	if err != nil {
		t.Fatalf("GetActiveConversation: %v", err)
	}
	return convID
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
