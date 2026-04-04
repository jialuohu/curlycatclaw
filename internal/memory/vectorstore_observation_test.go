package memory

import (
	"context"
	"testing"
	"time"
)

// resetObservationsCollection drops and recreates the observations collection.
func resetObservationsCollection(t *testing.T, vs *VectorStore, ctx context.Context) {
	t.Helper()
	vs.DeleteCollection(ctx, observationsCollection) //nolint:errcheck
	if err := vs.ensureCollection(ctx, observationsCollection); err != nil {
		t.Fatalf("reset: create %s: %v", observationsCollection, err)
	}
}

func TestIndexObservation(t *testing.T) {
	skipIfNoQdrant(t)
	ctx := context.Background()

	vs, err := NewVectorStoreRaw(ctx, "localhost:6334")
	if err != nil {
		t.Fatalf("NewVectorStoreRaw failed: %v", err)
	}
	vs.embedder = FNVEmbedder{}
	resetObservationsCollection(t, vs, ctx)
	defer vs.Close()

	obs := Observation{
		ID:         "obs-test-001",
		Title:      "User prefers dark mode",
		Summary:    "The user explicitly asked for dark mode in all interfaces",
		UserID:     42001,
		ChatID:     1,
		ChatType:   "private",
		Type:       "preference",
		Importance: 7,
		CreatedAt:  time.Now().UTC(),
	}

	if err := vs.IndexObservation(ctx, obs); err != nil {
		t.Fatalf("IndexObservation failed: %v", err)
	}

	// Wait for indexing to complete.
	time.Sleep(500 * time.Millisecond)

	// Verify the point exists by searching for it.
	results, err := vs.SearchObservations(ctx, "dark mode preference", obs.UserID, obs.ChatID, obs.ChatType, 5, 0.0)
	if err != nil {
		t.Fatalf("SearchObservations failed: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected at least one result after indexing, got none")
	}

	found := false
	for _, r := range results {
		if r.ID == "obs-test-001" {
			found = true
			if r.Type != "preference" {
				t.Errorf("expected type 'preference', got %q", r.Type)
			}
			if r.Importance != 7 {
				t.Errorf("expected importance 7, got %d", r.Importance)
			}
			// ChatType is not on ObservationResult (only on Observation)
			if r.Score <= 0 {
				t.Error("expected positive rank score")
			}
			break
		}
	}
	if !found {
		t.Error("indexed observation not found in search results")
	}
}

func TestSearchObservations_ChatTypeFiltering(t *testing.T) {
	skipIfNoQdrant(t)
	ctx := context.Background()

	vs, err := NewVectorStoreRaw(ctx, "localhost:6334")
	if err != nil {
		t.Fatalf("NewVectorStoreRaw failed: %v", err)
	}
	vs.embedder = FNVEmbedder{}
	resetObservationsCollection(t, vs, ctx)
	defer vs.Close()

	userID := int64(42002)
	groupChatID := int64(9001)

	// Index a private observation.
	privateObs := Observation{
		ID:         "obs-private-001",
		Title:      "Likes morning meetings",
		Summary:    "User prefers scheduling meetings in the morning",
		UserID:     userID,
		ChatID:     1,
		ChatType:   "private",
		Type:       "preference",
		Importance: 5,
		CreatedAt:  time.Now().UTC(),
	}

	// Index a group observation.
	groupObs := Observation{
		ID:         "obs-group-001",
		Title:      "Team uses Go for backend",
		Summary:    "The team decided to use Go programming language for backend services",
		UserID:     userID,
		ChatID:     groupChatID,
		ChatType:   "group",
		Type:       "event",
		Importance: 6,
		CreatedAt:  time.Now().UTC(),
	}

	if err := vs.IndexObservation(ctx, privateObs); err != nil {
		t.Fatalf("IndexObservation (private) failed: %v", err)
	}
	if err := vs.IndexObservation(ctx, groupObs); err != nil {
		t.Fatalf("IndexObservation (group) failed: %v", err)
	}

	time.Sleep(500 * time.Millisecond)

	// Private search should find the private observation but not the group one.
	privateResults, err := vs.SearchObservations(ctx, "meetings morning schedule", userID, 1, "private", 10, 0.0)
	if err != nil {
		t.Fatalf("SearchObservations (private) failed: %v", err)
	}
	for _, r := range privateResults {
		if r.ID == "obs-group-001" {
			t.Error("private search returned group observation; chat type filtering broken")
		}
	}

	// Group search with the correct chatID should find the group observation.
	groupResults, err := vs.SearchObservations(ctx, "Go backend programming", userID, groupChatID, "group", 10, 0.0)
	if err != nil {
		t.Fatalf("SearchObservations (group) failed: %v", err)
	}
	foundGroup := false
	for _, r := range groupResults {
		if r.ID == "obs-group-001" {
			foundGroup = true
		}
		if r.ID == "obs-private-001" {
			t.Error("group search returned private observation; chat type filtering broken")
		}
	}
	if !foundGroup {
		t.Error("group search did not find the group observation")
	}

	// Group search with a different chatID should not find the group observation.
	otherGroupResults, err := vs.SearchObservations(ctx, "Go backend programming", userID, 9999, "group", 10, 0.0)
	if err != nil {
		t.Fatalf("SearchObservations (other group) failed: %v", err)
	}
	for _, r := range otherGroupResults {
		if r.ID == "obs-group-001" {
			t.Error("group search with wrong chatID returned observation; chat scoping broken")
		}
	}
}

func TestSearchObservations_ImportanceFloor(t *testing.T) {
	skipIfNoQdrant(t)
	ctx := context.Background()

	vs, err := NewVectorStoreRaw(ctx, "localhost:6334")
	if err != nil {
		t.Fatalf("NewVectorStoreRaw failed: %v", err)
	}
	vs.embedder = FNVEmbedder{}
	resetObservationsCollection(t, vs, ctx)
	defer vs.Close()

	userID := int64(42003)

	// Index a low-importance observation (importance = 2, below threshold of 3).
	lowObs := Observation{
		ID:         "obs-low-001",
		Title:      "Mentioned weather casually",
		Summary:    "User made a passing remark about the weather being nice",
		UserID:     userID,
		ChatID:     1,
		ChatType:   "private",
		Type:       "event",
		Importance: 2,
		CreatedAt:  time.Now().UTC(),
	}

	// Index a high-importance observation (importance = 8).
	highObs := Observation{
		ID:         "obs-high-001",
		Title:      "Critical weather alert preference",
		Summary:    "User wants severe weather alerts sent immediately",
		UserID:     userID,
		ChatID:     1,
		ChatType:   "private",
		Type:       "preference",
		Importance: 8,
		CreatedAt:  time.Now().UTC(),
	}

	if err := vs.IndexObservation(ctx, lowObs); err != nil {
		t.Fatalf("IndexObservation (low) failed: %v", err)
	}
	if err := vs.IndexObservation(ctx, highObs); err != nil {
		t.Fatalf("IndexObservation (high) failed: %v", err)
	}

	time.Sleep(500 * time.Millisecond)

	// Search for weather-related observations.
	results, err := vs.SearchObservations(ctx, "weather alert preference", userID, 1, "private", 10, 0.0)
	if err != nil {
		t.Fatalf("SearchObservations failed: %v", err)
	}

	for _, r := range results {
		if r.ID == "obs-low-001" {
			t.Errorf("observation with importance %d should have been filtered (< 3)", r.Importance)
		}
		if r.Importance < 3 {
			t.Errorf("result with importance %d should have been filtered out", r.Importance)
		}
	}

	// Verify the high-importance observation is returned.
	foundHigh := false
	for _, r := range results {
		if r.ID == "obs-high-001" {
			foundHigh = true
			break
		}
	}
	if !foundHigh {
		t.Error("high-importance observation not found in results")
	}
}

func TestDeleteObservationVector(t *testing.T) {
	skipIfNoQdrant(t)
	ctx := context.Background()

	vs, err := NewVectorStoreRaw(ctx, "localhost:6334")
	if err != nil {
		t.Fatalf("NewVectorStoreRaw failed: %v", err)
	}
	vs.embedder = FNVEmbedder{}
	resetObservationsCollection(t, vs, ctx)
	defer vs.Close()

	userID := int64(42004)
	obs := Observation{
		ID:         "obs-delete-001",
		Title:      "Unique deletable observation",
		Summary:    "This observation will be deleted from the vector store",
		UserID:     userID,
		ChatID:     1,
		ChatType:   "private",
		Type:       "event",
		Importance: 5,
		CreatedAt:  time.Now().UTC(),
	}

	if err := vs.IndexObservation(ctx, obs); err != nil {
		t.Fatalf("IndexObservation failed: %v", err)
	}

	time.Sleep(500 * time.Millisecond)

	// Verify it exists.
	results, err := vs.SearchObservations(ctx, "deletable observation vector store", userID, 1, "private", 5, 0.0)
	if err != nil {
		t.Fatalf("SearchObservations (pre-delete) failed: %v", err)
	}
	found := false
	for _, r := range results {
		if r.ID == "obs-delete-001" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("observation not found before delete")
	}

	// Delete the observation vector.
	if err := vs.DeleteObservationVector(ctx, "obs-delete-001"); err != nil {
		t.Fatalf("DeleteObservationVector failed: %v", err)
	}

	time.Sleep(500 * time.Millisecond)

	// Verify it's gone.
	results, err = vs.SearchObservations(ctx, "deletable observation vector store", userID, 1, "private", 5, 0.0)
	if err != nil {
		t.Fatalf("SearchObservations (post-delete) failed: %v", err)
	}
	for _, r := range results {
		if r.ID == "obs-delete-001" {
			t.Error("observation still found after delete")
		}
	}
}
