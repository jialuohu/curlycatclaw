package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jialuohu/curlycatclaw/internal/memory"
)

// mockObservationStore implements ObservationStore for testing.
type mockObservationStore struct {
	searchResults []ObservationSearchResult
	searchErr     error
	deletedID     string
	deletedUserID int64
	deleteErr     error
	vectorDeleted string
	vectorDelErr  error
}

func (m *mockObservationStore) SearchObservations(_ context.Context, _ string, _ int64, _ string, _ int) ([]ObservationSearchResult, error) {
	return m.searchResults, m.searchErr
}

func (m *mockObservationStore) DeleteObservation(id string, userID int64) error {
	m.deletedID = id
	m.deletedUserID = userID
	return m.deleteErr
}

func (m *mockObservationStore) DeleteObservationVector(_ context.Context, id string) error {
	m.vectorDeleted = id
	return m.vectorDelErr
}

// newTestObservationDB creates a temp SQLite DB with the observations table.
func newTestObservationDB(t *testing.T) *memory.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	store, err := memory.NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	// Create the observations table.
	_, err = store.DB().Exec(`CREATE TABLE IF NOT EXISTS observations (
		rowid INTEGER PRIMARY KEY AUTOINCREMENT,
		id TEXT UNIQUE NOT NULL,
		conversation_id TEXT NOT NULL,
		user_id INTEGER NOT NULL,
		chat_id INTEGER NOT NULL,
		chat_type TEXT NOT NULL DEFAULT 'private',
		type TEXT NOT NULL,
		title TEXT NOT NULL,
		summary TEXT NOT NULL,
		importance INTEGER NOT NULL DEFAULT 5,
		source_msg_start INTEGER,
		source_msg_end INTEGER,
		content_hash TEXT NOT NULL,
		created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`)
	if err != nil {
		t.Fatalf("create observations table: %v", err)
	}
	_, err = store.DB().Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_observations_user_hash ON observations(user_id, content_hash)`)
	if err != nil {
		t.Fatalf("create unique index: %v", err)
	}
	return store
}

func TestSearchObservations_InvalidType(t *testing.T) {
	store := &mockObservationStore{}
	db := newTestObservationDB(t)
	skills, err := InitObservationSkills(db.DB(), store)
	if err != nil {
		t.Fatalf("InitObservationSkills: %v", err)
	}

	var searchSkill *Skill
	for _, s := range skills {
		if s.Name == "search_observations" {
			searchSkill = s
			break
		}
	}
	if searchSkill == nil {
		t.Fatal("search_observations skill not found")
	}

	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})

	// Valid query, invalid type.
	input, _ := json.Marshal(searchObservationsInput{Query: "test", Type: "invalid_type"})
	_, err = searchSkill.Execute(ctx, input)
	if err == nil {
		t.Fatal("expected error for invalid observation type")
	}
	if !strings.Contains(err.Error(), "invalid observation type") {
		t.Errorf("error = %q, want it to contain 'invalid observation type'", err.Error())
	}
}

func TestSearchObservations_ValidType(t *testing.T) {
	store := &mockObservationStore{
		searchResults: []ObservationSearchResult{
			{ID: "abc-123", Title: "User prefers dark mode", Type: "preference", Score: 0.95, CreatedAt: "2026-04-01"},
		},
	}
	db := newTestObservationDB(t)
	skills, err := InitObservationSkills(db.DB(), store)
	if err != nil {
		t.Fatalf("InitObservationSkills: %v", err)
	}

	var searchSkill *Skill
	for _, s := range skills {
		if s.Name == "search_observations" {
			searchSkill = s
			break
		}
	}

	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})
	input, _ := json.Marshal(searchObservationsInput{Query: "dark mode", Type: "preference"})
	result, err := searchSkill.Execute(ctx, input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "1 observations") {
		t.Errorf("result = %q, want it to contain '1 observations'", result)
	}
	if !strings.Contains(result, "dark mode") {
		t.Errorf("result = %q, want it to contain observation content", result)
	}
}

func TestForgetObservation_IDOR(t *testing.T) {
	// The store's DeleteObservation should receive the caller's userID,
	// not any other user's. When it returns "not found", that prevents IDOR.
	store := &mockObservationStore{
		deleteErr: fmt.Errorf("observation not found"),
	}
	db := newTestObservationDB(t)
	skills, err := InitObservationSkills(db.DB(), store)
	if err != nil {
		t.Fatalf("InitObservationSkills: %v", err)
	}

	var forgetSkill *Skill
	for _, s := range skills {
		if s.Name == "forget_observation" {
			forgetSkill = s
			break
		}
	}
	if forgetSkill == nil {
		t.Fatal("forget_observation skill not found")
	}

	// User 99 tries to delete an observation that belongs to user 1.
	ctx := WithUser(context.Background(), UserInfo{UserID: 99, ChatID: 10})
	validUUID := "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d"
	input, _ := json.Marshal(forgetObservationInput{ID: validUUID})
	_, err = forgetSkill.Execute(ctx, input)
	if err == nil {
		t.Fatal("expected error when deleting another user's observation")
	}
	// Verify the store received the caller's userID for IDOR enforcement.
	if store.deletedUserID != 99 {
		t.Errorf("DeleteObservation called with userID=%d, want 99", store.deletedUserID)
	}
	if store.deletedID != validUUID {
		t.Errorf("DeleteObservation called with id=%q, want %q", store.deletedID, validUUID)
	}
}

func TestForgetObservation_InvalidUUID(t *testing.T) {
	store := &mockObservationStore{}
	db := newTestObservationDB(t)
	skills, err := InitObservationSkills(db.DB(), store)
	if err != nil {
		t.Fatalf("InitObservationSkills: %v", err)
	}

	var forgetSkill *Skill
	for _, s := range skills {
		if s.Name == "forget_observation" {
			forgetSkill = s
			break
		}
	}

	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})
	input, _ := json.Marshal(forgetObservationInput{ID: "not-a-uuid"})
	_, err = forgetSkill.Execute(ctx, input)
	if err == nil {
		t.Fatal("expected error for invalid UUID format")
	}
	if !strings.Contains(err.Error(), "invalid observation ID format") {
		t.Errorf("error = %q, want 'invalid observation ID format'", err.Error())
	}
}

func TestListObservations_LimitClamping(t *testing.T) {
	db := newTestObservationDB(t)
	store := &mockObservationStore{}
	skills, err := InitObservationSkills(db.DB(), store)
	if err != nil {
		t.Fatalf("InitObservationSkills: %v", err)
	}

	var listSkill *Skill
	for _, s := range skills {
		if s.Name == "list_observations" {
			listSkill = s
			break
		}
	}
	if listSkill == nil {
		t.Fatal("list_observations skill not found")
	}

	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})

	// Insert some observations.
	now := time.Now().UTC()
	for i := range 5 {
		_, err := db.DB().Exec(
			`INSERT INTO observations (id, conversation_id, user_id, chat_id, type, title, summary, content_hash, created_at) VALUES (?, 'conv1', ?, 1, ?, ?, '', ?, ?)`,
			fmt.Sprintf("a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b%04x", i), int64(1), "decision",
			fmt.Sprintf("Observation %d", i), fmt.Sprintf("hash-%d", i), now.Add(-time.Duration(i)*time.Hour),
		)
		if err != nil {
			t.Fatalf("insert observation: %v", err)
		}
	}

	// Test limit=0 defaults to 10 (returns all 5).
	input, _ := json.Marshal(listObservationsInput{Limit: 0})
	result, err := listSkill.Execute(ctx, input)
	if err != nil {
		t.Fatalf("Execute(limit=0): %v", err)
	}
	if !strings.Contains(result, "5 observations") {
		t.Errorf("limit=0 should default to 10, showing all 5, got %q", result)
	}

	// Test limit=2 returns exactly 2.
	input, _ = json.Marshal(listObservationsInput{Limit: 2})
	result, err = listSkill.Execute(ctx, input)
	if err != nil {
		t.Fatalf("Execute(limit=2): %v", err)
	}
	if !strings.Contains(result, "2 observations") {
		t.Errorf("limit=2 should return 2 observations, got %q", result)
	}

	// Test limit=100 is clamped to 50 (returns all 5).
	input, _ = json.Marshal(listObservationsInput{Limit: 100})
	result, err = listSkill.Execute(ctx, input)
	if err != nil {
		t.Fatalf("Execute(limit=100): %v", err)
	}
	if !strings.Contains(result, "5 observations") {
		t.Errorf("limit=100 clamped to 50 should still return all 5, got %q", result)
	}
}

func TestListObservations_TypeFilter(t *testing.T) {
	db := newTestObservationDB(t)
	store := &mockObservationStore{}
	skills, err := InitObservationSkills(db.DB(), store)
	if err != nil {
		t.Fatalf("InitObservationSkills: %v", err)
	}

	var listSkill *Skill
	for _, s := range skills {
		if s.Name == "list_observations" {
			listSkill = s
			break
		}
	}

	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})
	now := time.Now().UTC()

	// Insert observations of different types.
	_, _ = db.DB().Exec(
		`INSERT INTO observations (id, conversation_id, user_id, chat_id, type, title, summary, content_hash, created_at) VALUES (?, 'conv1', ?, 1, ?, ?, '', ?, ?)`,
		"a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b0001", int64(1), "decision", "Decision obs", "hash-decision", now,
	)
	_, _ = db.DB().Exec(
		`INSERT INTO observations (id, conversation_id, user_id, chat_id, type, title, summary, content_hash, created_at) VALUES (?, 'conv1', ?, 1, ?, ?, '', ?, ?)`,
		"a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b0002", int64(1), "preference", "Pref obs", "hash-pref", now,
	)

	// Filter by "decision" type.
	input, _ := json.Marshal(listObservationsInput{Type: "decision"})
	result, err := listSkill.Execute(ctx, input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "1 observations") {
		t.Errorf("expected 1 decision observation, got %q", result)
	}
	if !strings.Contains(result, "Decision obs") {
		t.Errorf("expected decision observation content, got %q", result)
	}

	// Invalid type.
	input, _ = json.Marshal(listObservationsInput{Type: "invalid_type"})
	_, err = listSkill.Execute(ctx, input)
	if err == nil {
		t.Fatal("expected error for invalid type")
	}
}

func TestGetObservation_IDOR(t *testing.T) {
	db := newTestObservationDB(t)
	store := &mockObservationStore{}
	skills, err := InitObservationSkills(db.DB(), store)
	if err != nil {
		t.Fatalf("InitObservationSkills: %v", err)
	}

	var getSkill *Skill
	for _, s := range skills {
		if s.Name == "get_observation" {
			getSkill = s
			break
		}
	}

	// Insert an observation belonging to user 1.
	now := time.Now().UTC()
	obsID := "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b0099"
	_, err = db.DB().Exec(
		`INSERT INTO observations (id, conversation_id, user_id, chat_id, type, title, summary, content_hash, created_at) VALUES (?, 'conv1', ?, 1, ?, ?, '', ?, ?)`,
		obsID, int64(1), "preference", "Secret observation", "hash-secret", now,
	)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// User 1 can see it.
	ctx1 := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})
	input, _ := json.Marshal(getObservationInput{ID: obsID})
	result, err := getSkill.Execute(ctx1, input)
	if err != nil {
		t.Fatalf("user 1 get: %v", err)
	}
	if !strings.Contains(result, "Secret observation") {
		t.Errorf("user 1 should see observation content, got %q", result)
	}

	// User 99 cannot see it (IDOR protection).
	ctx99 := WithUser(context.Background(), UserInfo{UserID: 99, ChatID: 10})
	_, err = getSkill.Execute(ctx99, input)
	if err == nil {
		t.Fatal("user 99 should not be able to see user 1's observation")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q, want 'not found'", err.Error())
	}
}

func TestSearchObservations_EmptyQuery(t *testing.T) {
	store := &mockObservationStore{}
	db := newTestObservationDB(t)
	skills, err := InitObservationSkills(db.DB(), store)
	if err != nil {
		t.Fatalf("InitObservationSkills: %v", err)
	}

	var searchSkill *Skill
	for _, s := range skills {
		if s.Name == "search_observations" {
			searchSkill = s
			break
		}
	}

	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})
	input, _ := json.Marshal(searchObservationsInput{Query: ""})
	_, err = searchSkill.Execute(ctx, input)
	if err == nil {
		t.Fatal("expected error for empty query")
	}
	if !strings.Contains(err.Error(), "query is required") {
		t.Errorf("error = %q, want 'query is required'", err.Error())
	}
}

func TestSearchObservations_StoreError(t *testing.T) {
	store := &mockObservationStore{
		searchErr: fmt.Errorf("connection refused"),
	}
	db := newTestObservationDB(t)
	skills, err := InitObservationSkills(db.DB(), store)
	if err != nil {
		t.Fatalf("InitObservationSkills: %v", err)
	}

	var searchSkill *Skill
	for _, s := range skills {
		if s.Name == "search_observations" {
			searchSkill = s
			break
		}
	}

	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})
	input, _ := json.Marshal(searchObservationsInput{Query: "test"})
	_, err = searchSkill.Execute(ctx, input)
	if err == nil {
		t.Fatal("expected error when store fails")
	}
	if !strings.Contains(err.Error(), "search observations") {
		t.Errorf("error = %q, want it to contain 'search observations'", err.Error())
	}
}

func TestSearchObservations_NoResults(t *testing.T) {
	store := &mockObservationStore{
		searchResults: nil,
	}
	db := newTestObservationDB(t)
	skills, err := InitObservationSkills(db.DB(), store)
	if err != nil {
		t.Fatalf("InitObservationSkills: %v", err)
	}

	var searchSkill *Skill
	for _, s := range skills {
		if s.Name == "search_observations" {
			searchSkill = s
			break
		}
	}

	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})
	input, _ := json.Marshal(searchObservationsInput{Query: "nonexistent"})
	result, err := searchSkill.Execute(ctx, input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "No observations found") {
		t.Errorf("result = %q, want 'No observations found'", result)
	}
}

func TestSearchObservations_LimitClamping(t *testing.T) {
	store := &mockObservationStore{
		searchResults: []ObservationSearchResult{
			{ID: "1", Title: "obs1", Type: "decision", Score: 0.9},
		},
	}
	db := newTestObservationDB(t)
	skills, err := InitObservationSkills(db.DB(), store)
	if err != nil {
		t.Fatalf("InitObservationSkills: %v", err)
	}

	var searchSkill *Skill
	for _, s := range skills {
		if s.Name == "search_observations" {
			searchSkill = s
			break
		}
	}

	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})

	// Negative limit should default to 10.
	input, _ := json.Marshal(searchObservationsInput{Query: "test", Limit: -5})
	_, err = searchSkill.Execute(ctx, input)
	if err != nil {
		t.Fatalf("Execute(limit=-5): %v", err)
	}

	// Limit > 50 should be clamped.
	input, _ = json.Marshal(searchObservationsInput{Query: "test", Limit: 100})
	_, err = searchSkill.Execute(ctx, input)
	if err != nil {
		t.Fatalf("Execute(limit=100): %v", err)
	}
}

func TestGetObservation_EmptyID(t *testing.T) {
	db := newTestObservationDB(t)
	store := &mockObservationStore{}
	skills, err := InitObservationSkills(db.DB(), store)
	if err != nil {
		t.Fatalf("InitObservationSkills: %v", err)
	}

	var getSkill *Skill
	for _, s := range skills {
		if s.Name == "get_observation" {
			getSkill = s
			break
		}
	}

	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})
	input, _ := json.Marshal(getObservationInput{ID: ""})
	_, err = getSkill.Execute(ctx, input)
	if err == nil {
		t.Fatal("expected error for empty ID")
	}
	if !strings.Contains(err.Error(), "id is required") {
		t.Errorf("error = %q, want 'id is required'", err.Error())
	}
}

func TestGetObservation_InvalidUUID(t *testing.T) {
	db := newTestObservationDB(t)
	store := &mockObservationStore{}
	skills, err := InitObservationSkills(db.DB(), store)
	if err != nil {
		t.Fatalf("InitObservationSkills: %v", err)
	}

	var getSkill *Skill
	for _, s := range skills {
		if s.Name == "get_observation" {
			getSkill = s
			break
		}
	}

	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})
	input, _ := json.Marshal(getObservationInput{ID: "not-valid"})
	_, err = getSkill.Execute(ctx, input)
	if err == nil {
		t.Fatal("expected error for invalid UUID")
	}
	if !strings.Contains(err.Error(), "invalid observation ID format") {
		t.Errorf("error = %q, want 'invalid observation ID format'", err.Error())
	}
}

func TestForgetObservation_EmptyID(t *testing.T) {
	store := &mockObservationStore{}
	db := newTestObservationDB(t)
	skills, err := InitObservationSkills(db.DB(), store)
	if err != nil {
		t.Fatalf("InitObservationSkills: %v", err)
	}

	var forgetSkill *Skill
	for _, s := range skills {
		if s.Name == "forget_observation" {
			forgetSkill = s
			break
		}
	}

	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})
	input, _ := json.Marshal(forgetObservationInput{ID: ""})
	_, err = forgetSkill.Execute(ctx, input)
	if err == nil {
		t.Fatal("expected error for empty ID")
	}
	if !strings.Contains(err.Error(), "id is required") {
		t.Errorf("error = %q, want 'id is required'", err.Error())
	}
}

func TestForgetObservation_VectorCleanupFailure(t *testing.T) {
	store := &mockObservationStore{
		vectorDelErr: fmt.Errorf("qdrant unreachable"),
	}
	db := newTestObservationDB(t)
	skills, err := InitObservationSkills(db.DB(), store)
	if err != nil {
		t.Fatalf("InitObservationSkills: %v", err)
	}

	var forgetSkill *Skill
	for _, s := range skills {
		if s.Name == "forget_observation" {
			forgetSkill = s
			break
		}
	}

	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})
	validUUID := "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d"
	input, _ := json.Marshal(forgetObservationInput{ID: validUUID})
	result, err := forgetSkill.Execute(ctx, input)
	if err != nil {
		t.Fatalf("expected success (vector cleanup is best-effort), got error: %v", err)
	}
	if !strings.Contains(result, "vector cleanup failed") {
		t.Errorf("result = %q, want it to mention vector cleanup failure", result)
	}
	if !strings.Contains(result, "Deleted") {
		t.Errorf("result = %q, want it to confirm deletion", result)
	}
}

func TestForgetObservation_Success(t *testing.T) {
	store := &mockObservationStore{}
	db := newTestObservationDB(t)
	skills, err := InitObservationSkills(db.DB(), store)
	if err != nil {
		t.Fatalf("InitObservationSkills: %v", err)
	}

	var forgetSkill *Skill
	for _, s := range skills {
		if s.Name == "forget_observation" {
			forgetSkill = s
			break
		}
	}

	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})
	validUUID := "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d"
	input, _ := json.Marshal(forgetObservationInput{ID: validUUID})
	result, err := forgetSkill.Execute(ctx, input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "Deleted observation") {
		t.Errorf("result = %q, want 'Deleted observation'", result)
	}
	if store.deletedID != validUUID {
		t.Errorf("deletedID = %q, want %q", store.deletedID, validUUID)
	}
	if store.vectorDeleted != validUUID {
		t.Errorf("vectorDeleted = %q, want %q", store.vectorDeleted, validUUID)
	}
}

func TestListObservations_NegativeLimit(t *testing.T) {
	db := newTestObservationDB(t)
	store := &mockObservationStore{}
	skills, err := InitObservationSkills(db.DB(), store)
	if err != nil {
		t.Fatalf("InitObservationSkills: %v", err)
	}

	var listSkill *Skill
	for _, s := range skills {
		if s.Name == "list_observations" {
			listSkill = s
			break
		}
	}

	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})

	// Insert one observation.
	_, err = db.DB().Exec(
		`INSERT INTO observations (id, conversation_id, user_id, chat_id, type, title, summary, content_hash, created_at) VALUES (?, 'conv1', ?, 1, ?, ?, '', ?, ?)`,
		"a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b0001", int64(1), "decision", "Test obs", "hash-neg", time.Now().UTC(),
	)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Negative limit defaults to 10, should return the 1 observation.
	input, _ := json.Marshal(listObservationsInput{Limit: -5})
	result, err := listSkill.Execute(ctx, input)
	if err != nil {
		t.Fatalf("Execute(limit=-5): %v", err)
	}
	if !strings.Contains(result, "1 observations") {
		t.Errorf("expected 1 observation with default limit, got %q", result)
	}
}

func TestListObservations_Empty(t *testing.T) {
	db := newTestObservationDB(t)
	store := &mockObservationStore{}
	skills, err := InitObservationSkills(db.DB(), store)
	if err != nil {
		t.Fatalf("InitObservationSkills: %v", err)
	}

	var listSkill *Skill
	for _, s := range skills {
		if s.Name == "list_observations" {
			listSkill = s
			break
		}
	}

	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})
	input, _ := json.Marshal(listObservationsInput{})
	result, err := listSkill.Execute(ctx, input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result != "No observations found." {
		t.Errorf("result = %q, want %q", result, "No observations found.")
	}
}
