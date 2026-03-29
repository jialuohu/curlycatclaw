package memory

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestFactStore_AddAndGet(t *testing.T) {
	s := newTestStore(t)
	fs := NewFactStore(s.DB(), 50)

	id, err := fs.AddFact(42, "Prefers dark mode", "preference", "explicit")
	if err != nil {
		t.Fatalf("AddFact: %v", err)
	}
	if id <= 0 {
		t.Fatalf("expected positive ID, got %d", id)
	}

	facts, err := fs.GetFacts(42)
	if err != nil {
		t.Fatalf("GetFacts: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("got %d facts, want 1", len(facts))
	}

	f := facts[0]
	if f.ID != id {
		t.Errorf("ID = %d, want %d", f.ID, id)
	}
	if f.UserID != 42 {
		t.Errorf("UserID = %d, want 42", f.UserID)
	}
	if f.Fact != "Prefers dark mode" {
		t.Errorf("Fact = %q, want %q", f.Fact, "Prefers dark mode")
	}
	if f.Category != "preference" {
		t.Errorf("Category = %q, want %q", f.Category, "preference")
	}
	if f.Source != "explicit" {
		t.Errorf("Source = %q, want %q", f.Source, "explicit")
	}
	if f.CreatedAt.IsZero() {
		t.Error("CreatedAt should not be zero")
	}
	if f.UpdatedAt.IsZero() {
		t.Error("UpdatedAt should not be zero")
	}
}

func TestFactStore_AddFact_Sanitization(t *testing.T) {
	s := newTestStore(t)
	fs := NewFactStore(s.DB(), 50)

	// Control characters should be stripped.
	_, err := fs.AddFact(1, "hello\x00world\x07test", "general", "")
	if err != nil {
		t.Fatalf("AddFact with control chars: %v", err)
	}

	facts, err := fs.GetFacts(1)
	if err != nil {
		t.Fatalf("GetFacts: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("got %d facts, want 1", len(facts))
	}
	if facts[0].Fact != "helloworldtest" {
		t.Errorf("Fact = %q, want %q (control chars stripped)", facts[0].Fact, "helloworldtest")
	}

	// Long fact should be truncated to 200 characters.
	longFact := strings.Repeat("a", 300)
	_, err = fs.AddFact(2, longFact, "general", "")
	if err != nil {
		t.Fatalf("AddFact long: %v", err)
	}

	facts2, err := fs.GetFacts(2)
	if err != nil {
		t.Fatalf("GetFacts: %v", err)
	}
	if len(facts2) != 1 {
		t.Fatalf("got %d facts, want 1", len(facts2))
	}
	if len(facts2[0].Fact) != 200 {
		t.Errorf("fact length = %d, want 200", len(facts2[0].Fact))
	}
}

func TestFactStore_AddFact_MaxLimit(t *testing.T) {
	s := newTestStore(t)
	fs := NewFactStore(s.DB(), 2)

	if _, err := fs.AddFact(1, "fact one", "general", ""); err != nil {
		t.Fatalf("AddFact 1: %v", err)
	}
	if _, err := fs.AddFact(1, "fact two", "general", ""); err != nil {
		t.Fatalf("AddFact 2: %v", err)
	}

	// Third fact should fail with limit error.
	_, err := fs.AddFact(1, "fact three", "general", "")
	if err == nil {
		t.Fatal("expected error when exceeding maxFacts, got nil")
	}
	if !strings.Contains(err.Error(), "limit reached") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "limit reached")
	}

	// Different user should still be able to add.
	if _, err := fs.AddFact(2, "other user fact", "general", ""); err != nil {
		t.Fatalf("AddFact for different user should succeed: %v", err)
	}
}

func TestFactStore_DeleteFact(t *testing.T) {
	s := newTestStore(t)
	fs := NewFactStore(s.DB(), 50)

	id, err := fs.AddFact(1, "to be deleted", "general", "")
	if err != nil {
		t.Fatalf("AddFact: %v", err)
	}

	if err := fs.DeleteFact(id, 1); err != nil {
		t.Fatalf("DeleteFact: %v", err)
	}

	facts, err := fs.GetFacts(1)
	if err != nil {
		t.Fatalf("GetFacts: %v", err)
	}
	if len(facts) != 0 {
		t.Errorf("got %d facts after delete, want 0", len(facts))
	}
}

func TestFactStore_DeleteFact_WrongUser(t *testing.T) {
	s := newTestStore(t)
	fs := NewFactStore(s.DB(), 50)

	id, err := fs.AddFact(1, "user 1 fact", "general", "")
	if err != nil {
		t.Fatalf("AddFact: %v", err)
	}

	// Attempt to delete with a different userID (IDOR protection).
	err = fs.DeleteFact(id, 999)
	if err == nil {
		t.Fatal("expected error when deleting with wrong userID, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "not found")
	}

	// Original fact should still exist.
	facts, err := fs.GetFacts(1)
	if err != nil {
		t.Fatalf("GetFacts: %v", err)
	}
	if len(facts) != 1 {
		t.Errorf("got %d facts, want 1 (fact should not be deleted)", len(facts))
	}
}

func TestFactStore_UpdateLastReferenced(t *testing.T) {
	s := newTestStore(t)
	fs := NewFactStore(s.DB(), 50)

	id, err := fs.AddFact(1, "referenced fact", "general", "")
	if err != nil {
		t.Fatalf("AddFact: %v", err)
	}

	// Before update, last_referenced_at should be nil.
	facts, err := fs.GetFacts(1)
	if err != nil {
		t.Fatalf("GetFacts: %v", err)
	}
	if facts[0].LastReferencedAt != nil {
		t.Error("LastReferencedAt should be nil before update")
	}

	if err := fs.UpdateLastReferenced([]int64{id}); err != nil {
		t.Fatalf("UpdateLastReferenced: %v", err)
	}

	facts, err = fs.GetFacts(1)
	if err != nil {
		t.Fatalf("GetFacts after update: %v", err)
	}
	if facts[0].LastReferencedAt == nil {
		t.Fatal("LastReferencedAt should not be nil after update")
	}
	if facts[0].LastReferencedAt.IsZero() {
		t.Error("LastReferencedAt should not be zero after update")
	}
}

func TestFactStore_Categories(t *testing.T) {
	s := newTestStore(t)
	fs := NewFactStore(s.DB(), 50)

	categories := []struct {
		fact     string
		category string
	}{
		{"Uses Vim", "preference"},
		{"Name is Jerry", "identity"},
		{"Building curlycatclaw", "project"},
		{"General info", "general"},
	}

	for _, c := range categories {
		if _, err := fs.AddFact(1, c.fact, c.category, ""); err != nil {
			t.Fatalf("AddFact(%q, %q): %v", c.fact, c.category, err)
		}
	}

	facts, err := fs.GetFacts(1)
	if err != nil {
		t.Fatalf("GetFacts: %v", err)
	}
	if len(facts) != 4 {
		t.Fatalf("got %d facts, want 4", len(facts))
	}

	// Facts are ordered by category then ID, so verify each has the right category.
	factMap := make(map[string]string)
	for _, f := range facts {
		factMap[f.Fact] = f.Category
	}
	for _, c := range categories {
		got, ok := factMap[c.fact]
		if !ok {
			t.Errorf("fact %q not found", c.fact)
			continue
		}
		if got != c.category {
			t.Errorf("fact %q: category = %q, want %q", c.fact, got, c.category)
		}
	}
}

func TestSanitizeFact_UTF8Truncation(t *testing.T) {
	// 201 party popper emojis (each is 4 bytes in UTF-8).
	input := strings.Repeat("\U0001f389", 201)

	result := sanitizeFact(input)

	runes := []rune(result)
	if len(runes) != 200 {
		t.Errorf("rune count = %d, want 200", len(runes))
	}
	if !utf8.ValidString(result) {
		t.Error("result is not valid UTF-8")
	}
	// Each rune is 4 bytes, so 200 runes = 800 bytes.
	if len(result) != 800 {
		t.Errorf("byte length = %d, want 800 (200 x 4-byte runes)", len(result))
	}
}
