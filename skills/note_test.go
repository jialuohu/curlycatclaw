package skills

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// newTestDB opens an in-memory SQLite database for testing.
func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// initNoteSkillsForTest creates skills and returns them keyed by name.
func initNoteSkillsForTest(t *testing.T, db *sql.DB) map[string]*Skill {
	t.Helper()
	skills, err := InitNoteSkills(db)
	if err != nil {
		t.Fatalf("InitNoteSkills: %v", err)
	}
	m := make(map[string]*Skill, len(skills))
	for _, s := range skills {
		m[s.Name] = s
	}
	return m
}

func TestInitNoteSkills_CreatesTable(t *testing.T) {
	db := newTestDB(t)

	skills, err := InitNoteSkills(db)
	if err != nil {
		t.Fatalf("InitNoteSkills: %v", err)
	}

	if len(skills) != 2 {
		t.Fatalf("expected 2 skills, got %d", len(skills))
	}

	// Verify the notes table exists by querying sqlite_master.
	var name string
	err = db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='notes'`).Scan(&name)
	if err != nil {
		t.Fatalf("notes table not found: %v", err)
	}
	if name != "notes" {
		t.Errorf("table name = %q, want %q", name, "notes")
	}

	// Verify idempotent: calling again should not error.
	if _, err := InitNoteSkills(db); err != nil {
		t.Fatalf("second InitNoteSkills should be idempotent: %v", err)
	}
}

func TestSaveNote_ValidInput(t *testing.T) {
	db := newTestDB(t)
	skills := initNoteSkillsForTest(t, db)

	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})
	input, _ := json.Marshal(saveNoteInput{Title: "Grocery List", Content: "Eggs, Milk, Bread"})

	result, err := skills["save_note"].Execute(ctx, input)
	if err != nil {
		t.Fatalf("save_note: %v", err)
	}
	if result != "Note saved: Grocery List" {
		t.Errorf("result = %q, want %q", result, "Note saved: Grocery List")
	}

	// Verify the note is in the database.
	var title, content string
	var userID int64
	err = db.QueryRow(`SELECT user_id, title, content FROM notes WHERE title = ?`, "Grocery List").Scan(&userID, &title, &content)
	if err != nil {
		t.Fatalf("query saved note: %v", err)
	}
	if userID != 1 {
		t.Errorf("user_id = %d, want 1", userID)
	}
	if content != "Eggs, Milk, Bread" {
		t.Errorf("content = %q, want %q", content, "Eggs, Milk, Bread")
	}
}

func TestSaveNote_EmptyTitle(t *testing.T) {
	db := newTestDB(t)
	skills := initNoteSkillsForTest(t, db)

	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})
	input, _ := json.Marshal(saveNoteInput{Title: "", Content: "some content"})

	_, err := skills["save_note"].Execute(ctx, input)
	if err == nil {
		t.Fatal("expected error for empty title, got nil")
	}
	if !strings.Contains(err.Error(), "title is required") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "title is required")
	}
}

func TestSaveNote_EmptyContent(t *testing.T) {
	db := newTestDB(t)
	skills := initNoteSkillsForTest(t, db)

	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})
	input, _ := json.Marshal(saveNoteInput{Title: "A Title", Content: ""})

	_, err := skills["save_note"].Execute(ctx, input)
	if err == nil {
		t.Fatal("expected error for empty content, got nil")
	}
	if !strings.Contains(err.Error(), "content is required") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "content is required")
	}
}

func TestSearchNotes_MatchingResults(t *testing.T) {
	db := newTestDB(t)
	skills := initNoteSkillsForTest(t, db)

	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})

	// Save two notes; one should match the search.
	notes := []saveNoteInput{
		{Title: "Meeting Notes", Content: "Discuss project timeline"},
		{Title: "Grocery List", Content: "Eggs, Milk, Bread"},
	}
	for _, n := range notes {
		input, _ := json.Marshal(n)
		if _, err := skills["save_note"].Execute(ctx, input); err != nil {
			t.Fatalf("save_note(%s): %v", n.Title, err)
		}
	}

	searchInput, _ := json.Marshal(searchNotesInput{Query: "project"})
	result, err := skills["search_notes"].Execute(ctx, searchInput)
	if err != nil {
		t.Fatalf("search_notes: %v", err)
	}

	if !strings.Contains(result, "Meeting Notes") {
		t.Errorf("expected result to contain %q, got %q", "Meeting Notes", result)
	}
	if !strings.Contains(result, "Discuss project timeline") {
		t.Errorf("expected result to contain note content, got %q", result)
	}
	// The non-matching note should not appear.
	if strings.Contains(result, "Grocery List") {
		t.Errorf("result should not contain non-matching note %q", "Grocery List")
	}
}

func TestSearchNotes_NoMatches(t *testing.T) {
	db := newTestDB(t)
	skills := initNoteSkillsForTest(t, db)

	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})

	// Save a note, then search for something unrelated.
	input, _ := json.Marshal(saveNoteInput{Title: "Recipe", Content: "Chocolate cake"})
	if _, err := skills["save_note"].Execute(ctx, input); err != nil {
		t.Fatalf("save_note: %v", err)
	}

	searchInput, _ := json.Marshal(searchNotesInput{Query: "quantum"})
	result, err := skills["search_notes"].Execute(ctx, searchInput)
	if err != nil {
		t.Fatalf("search_notes: %v", err)
	}

	want := "No notes found matching 'quantum'"
	if result != want {
		t.Errorf("result = %q, want %q", result, want)
	}
}

func TestSearchNotes_EmptyQuery(t *testing.T) {
	db := newTestDB(t)
	skills := initNoteSkillsForTest(t, db)

	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})
	searchInput, _ := json.Marshal(searchNotesInput{Query: ""})

	_, err := skills["search_notes"].Execute(ctx, searchInput)
	if err == nil {
		t.Fatal("expected error for empty query, got nil")
	}
	if !strings.Contains(err.Error(), "query is required") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "query is required")
	}
}

func TestSaveNote_TitleTooLong(t *testing.T) {
	db := newTestDB(t)
	skills := initNoteSkillsForTest(t, db)

	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})
	longTitle := strings.Repeat("a", maxNoteTitleLen+1)
	input, _ := json.Marshal(saveNoteInput{Title: longTitle, Content: "some content"})

	_, err := skills["save_note"].Execute(ctx, input)
	if err == nil {
		t.Fatal("expected error for title too long, got nil")
	}
	if !strings.Contains(err.Error(), "title too long") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "title too long")
	}
}

func TestSaveNote_ContentTooLarge(t *testing.T) {
	db := newTestDB(t)
	skills := initNoteSkillsForTest(t, db)

	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})
	largeContent := strings.Repeat("x", maxNoteContentBytes+1)
	input, _ := json.Marshal(saveNoteInput{Title: "A Title", Content: largeContent})

	_, err := skills["save_note"].Execute(ctx, input)
	if err == nil {
		t.Fatal("expected error for content too large, got nil")
	}
	if !strings.Contains(err.Error(), "content too large") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "content too large")
	}
}

func TestSaveNote_TitleAtMaxLen(t *testing.T) {
	db := newTestDB(t)
	skills := initNoteSkillsForTest(t, db)

	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})
	exactTitle := strings.Repeat("a", maxNoteTitleLen)
	input, _ := json.Marshal(saveNoteInput{Title: exactTitle, Content: "some content"})

	_, err := skills["save_note"].Execute(ctx, input)
	if err != nil {
		t.Fatalf("title at exactly max length should succeed, got: %v", err)
	}
}

func TestNotes_UserScoped(t *testing.T) {
	db := newTestDB(t)
	skills := initNoteSkillsForTest(t, db)

	// User A saves a note.
	ctxA := WithUser(context.Background(), UserInfo{UserID: 100, ChatID: 1})
	inputA, _ := json.Marshal(saveNoteInput{Title: "User A Secret", Content: "Only for user A"})
	if _, err := skills["save_note"].Execute(ctxA, inputA); err != nil {
		t.Fatalf("save_note(userA): %v", err)
	}

	// User B saves a different note.
	ctxB := WithUser(context.Background(), UserInfo{UserID: 200, ChatID: 1})
	inputB, _ := json.Marshal(saveNoteInput{Title: "User B Secret", Content: "Only for user B"})
	if _, err := skills["save_note"].Execute(ctxB, inputB); err != nil {
		t.Fatalf("save_note(userB): %v", err)
	}

	// User A searches — should only see their own note.
	searchInput, _ := json.Marshal(searchNotesInput{Query: "Secret"})

	resultA, err := skills["search_notes"].Execute(ctxA, searchInput)
	if err != nil {
		t.Fatalf("search_notes(userA): %v", err)
	}
	if !strings.Contains(resultA, "User A Secret") {
		t.Errorf("userA search should contain their note, got %q", resultA)
	}
	if strings.Contains(resultA, "User B Secret") {
		t.Errorf("userA search should NOT contain userB's note, got %q", resultA)
	}

	// User B searches — should only see their own note.
	resultB, err := skills["search_notes"].Execute(ctxB, searchInput)
	if err != nil {
		t.Fatalf("search_notes(userB): %v", err)
	}
	if !strings.Contains(resultB, "User B Secret") {
		t.Errorf("userB search should contain their note, got %q", resultB)
	}
	if strings.Contains(resultB, "User A Secret") {
		t.Errorf("userB search should NOT contain userA's note, got %q", resultB)
	}
}
