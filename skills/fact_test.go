package skills

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jialuohu/curlycatclaw/internal/memory"
)

// newTestFactStore creates a FactStore backed by a temp SQLite DB with
// the full schema (via memory.NewStore which runs migrations).
func newTestFactStore(t *testing.T, maxFacts int) *memory.FactStore {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	store, err := memory.NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return memory.NewFactStore(store.DB(), maxFacts)
}

// initFactSkillsForTest creates fact skills and returns them keyed by name.
func initFactSkillsForTest(t *testing.T, fs *memory.FactStore) map[string]*Skill {
	t.Helper()
	skills := InitFactSkills(fs)
	m := make(map[string]*Skill, len(skills))
	for _, s := range skills {
		m[s.Name] = s
	}
	return m
}

func TestRememberFact_Basic(t *testing.T) {
	fs := newTestFactStore(t, 50)
	skills := initFactSkillsForTest(t, fs)

	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})
	input, _ := json.Marshal(rememberFactInput{Fact: "Likes Go", Category: "preference"})

	result, err := skills["remember_fact"].Execute(ctx, input)
	if err != nil {
		t.Fatalf("remember_fact: %v", err)
	}
	if !strings.Contains(result, "Remembered") {
		t.Errorf("result = %q, want it to contain %q", result, "Remembered")
	}
	if !strings.Contains(result, "Likes Go") {
		t.Errorf("result = %q, want it to contain the fact text", result)
	}
	if !strings.Contains(result, "preference") {
		t.Errorf("result = %q, want it to contain the category", result)
	}

	// Verify the fact is stored.
	facts, err := fs.GetFacts(1)
	if err != nil {
		t.Fatalf("GetFacts: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("got %d facts, want 1", len(facts))
	}
	if facts[0].Fact != "Likes Go" {
		t.Errorf("stored fact = %q, want %q", facts[0].Fact, "Likes Go")
	}
}

func TestForgetFact_Basic(t *testing.T) {
	fs := newTestFactStore(t, 50)
	skills := initFactSkillsForTest(t, fs)

	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})

	// Add a fact first.
	addInput, _ := json.Marshal(rememberFactInput{Fact: "To be forgotten", Category: "general"})
	addResult, err := skills["remember_fact"].Execute(ctx, addInput)
	if err != nil {
		t.Fatalf("remember_fact: %v", err)
	}

	// Extract the ID from the result.
	facts, err := fs.GetFacts(1)
	if err != nil {
		t.Fatalf("GetFacts: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("got %d facts, want 1", len(facts))
	}
	factID := facts[0].ID
	_ = addResult

	// Delete it.
	delInput, _ := json.Marshal(forgetFactInput{FactID: factID})
	result, err := skills["forget_fact"].Execute(ctx, delInput)
	if err != nil {
		t.Fatalf("forget_fact: %v", err)
	}
	if !strings.Contains(result, "Forgot") {
		t.Errorf("result = %q, want it to contain %q", result, "Forgot")
	}

	// Verify removal.
	facts, err = fs.GetFacts(1)
	if err != nil {
		t.Fatalf("GetFacts after delete: %v", err)
	}
	if len(facts) != 0 {
		t.Errorf("got %d facts after delete, want 0", len(facts))
	}
}

func TestListFacts_Empty(t *testing.T) {
	fs := newTestFactStore(t, 50)
	skills := initFactSkillsForTest(t, fs)

	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})
	result, err := skills["list_facts"].Execute(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("list_facts: %v", err)
	}
	if result != "No facts saved yet." {
		t.Errorf("result = %q, want %q", result, "No facts saved yet.")
	}
}

func TestListFacts_WithFacts(t *testing.T) {
	fs := newTestFactStore(t, 50)
	skills := initFactSkillsForTest(t, fs)

	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})

	// Add facts in different categories.
	factsToAdd := []rememberFactInput{
		{Fact: "Name is Jerry", Category: "identity"},
		{Fact: "Prefers dark mode", Category: "preference"},
		{Fact: "Building curlycatclaw", Category: "project"},
	}
	for _, f := range factsToAdd {
		input, _ := json.Marshal(f)
		if _, err := skills["remember_fact"].Execute(ctx, input); err != nil {
			t.Fatalf("remember_fact(%q): %v", f.Fact, err)
		}
	}

	result, err := skills["list_facts"].Execute(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("list_facts: %v", err)
	}

	// Verify output contains total count.
	if !strings.Contains(result, "3 total") {
		t.Errorf("result should contain '3 total', got %q", result)
	}

	// Verify output contains category headers.
	for _, cat := range []string{"identity", "preference", "project"} {
		if !strings.Contains(result, cat) {
			t.Errorf("result should contain category %q, got %q", cat, result)
		}
	}

	// Verify output contains fact text with IDs.
	for _, f := range factsToAdd {
		if !strings.Contains(result, f.Fact) {
			t.Errorf("result should contain fact %q, got %q", f.Fact, result)
		}
	}
	if !strings.Contains(result, "[id=") {
		t.Errorf("result should contain fact IDs like '[id=', got %q", result)
	}
}
