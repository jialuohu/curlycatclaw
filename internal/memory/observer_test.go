package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// mockObserverStore implements ObserverStore for testing.
type mockObserverStore struct {
	messages     []Message
	observations []Observation
	titles       []string
	hashes       map[string]bool // userID:hash -> exists

	saveErr      error
	titlesErr    error
	hashErr      error
	countErr     error
	messagesErr  error
}

func newMockObserverStore() *mockObserverStore {
	return &mockObserverStore{
		hashes: make(map[string]bool),
	}
}

func (m *mockObserverStore) GetMessagesSinceRowid(convID string, afterRowid, upToRowid int64) ([]Message, error) {
	if m.messagesErr != nil {
		return nil, m.messagesErr
	}
	return m.messages, nil
}

func (m *mockObserverStore) SaveObservation(obs Observation) error {
	if m.saveErr != nil {
		return m.saveErr
	}
	m.observations = append(m.observations, obs)
	hashKey := fmt.Sprintf("%d:%s", obs.UserID, obs.ContentHash)
	m.hashes[hashKey] = true
	return nil
}

func (m *mockObserverStore) GetRecentObservationTitles(convID string, limit int) ([]string, error) {
	if m.titlesErr != nil {
		return nil, m.titlesErr
	}
	return m.titles, nil
}

func (m *mockObserverStore) ObservationExistsByHash(userID int64, hash string) (bool, error) {
	if m.hashErr != nil {
		return false, m.hashErr
	}
	key := fmt.Sprintf("%d:%s", userID, hash)
	return m.hashes[key], nil
}

func (m *mockObserverStore) CountObservations(convID string) (int, error) {
	if m.countErr != nil {
		return 0, m.countErr
	}
	return len(m.observations), nil
}

// makeMessages creates a list of user/assistant message pairs long enough to
// exceed the minTranscriptChars threshold.
func makeMessages(pairs int) []Message {
	msgs := make([]Message, 0, pairs*2)
	for i := 0; i < pairs; i++ {
		userContent, _ := json.Marshal(fmt.Sprintf("User message number %d with enough text to build a transcript", i))
		asstContent, _ := json.Marshal(fmt.Sprintf("Assistant response number %d with additional details about the project", i))
		msgs = append(msgs,
			Message{Role: "user", Content: userContent},
			Message{Role: "assistant", Content: asstContent},
		)
	}
	return msgs
}

func TestExtract_Success(t *testing.T) {
	store := newMockObserverStore()
	store.messages = makeMessages(5)

	claudeResp := `[
		{
			"type": "decision",
			"title": "Decided to use SQLite for storage",
			"summary": "The user chose SQLite with WAL mode for the persistence layer.",
			"facts": ["SQLite selected", "WAL mode enabled"],
			"importance": 6
		}
	]`

	sendFn := func(ctx context.Context, system, user string) (string, error) {
		if !strings.Contains(system, "memory observer") {
			t.Errorf("system prompt should contain 'memory observer', got %q", system)
		}
		if !strings.Contains(user, "Recent conversation segment") {
			t.Errorf("user prompt should contain transcript header, got %q", user)
		}
		return claudeResp, nil
	}

	ext := NewObservationExtractor(sendFn, store)
	obs, err := ext.Extract(context.Background(), "conv-1", 42, 100, "private", 0, 100, 20, 10000)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(obs) != 1 {
		t.Fatalf("got %d observations, want 1", len(obs))
	}

	o := obs[0]
	if o.Type != "decision" {
		t.Errorf("Type = %q, want %q", o.Type, "decision")
	}
	if o.Title != "Decided to use SQLite for storage" {
		t.Errorf("Title = %q, want %q", o.Title, "Decided to use SQLite for storage")
	}
	if o.Importance != 6 {
		t.Errorf("Importance = %d, want 6", o.Importance)
	}
	if len(o.Facts) != 2 {
		t.Errorf("got %d facts, want 2", len(o.Facts))
	}
	if o.ConversationID != "conv-1" {
		t.Errorf("ConversationID = %q, want %q", o.ConversationID, "conv-1")
	}
	if o.UserID != 42 {
		t.Errorf("UserID = %d, want 42", o.UserID)
	}
	if o.ChatID != 100 {
		t.Errorf("ChatID = %d, want 100", o.ChatID)
	}
	if o.ContentHash == "" {
		t.Error("ContentHash should not be empty")
	}

	// Verify it was persisted to the store.
	if len(store.observations) != 1 {
		t.Errorf("store has %d observations, want 1", len(store.observations))
	}
}

func TestExtract_EmptyTranscript(t *testing.T) {
	store := newMockObserverStore()
	// Short messages that produce a transcript under 200 chars.
	userContent, _ := json.Marshal("Hi")
	asstContent, _ := json.Marshal("Hello")
	store.messages = []Message{
		{Role: "user", Content: userContent},
		{Role: "assistant", Content: asstContent},
	}

	sendCalled := false
	sendFn := func(ctx context.Context, system, user string) (string, error) {
		sendCalled = true
		return "[]", nil
	}

	ext := NewObservationExtractor(sendFn, store)
	obs, err := ext.Extract(context.Background(), "conv-1", 42, 100, "private", 0, 10, 20, 10000)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if obs != nil {
		t.Errorf("expected nil observations for short transcript, got %d", len(obs))
	}
	if sendCalled {
		t.Error("send should not be called for short transcript")
	}
}

func TestExtract_InvalidJSON(t *testing.T) {
	store := newMockObserverStore()
	store.messages = makeMessages(5)

	sendFn := func(ctx context.Context, system, user string) (string, error) {
		return "this is not json at all!!!", nil
	}

	ext := NewObservationExtractor(sendFn, store)
	obs, err := ext.Extract(context.Background(), "conv-1", 42, 100, "private", 0, 100, 20, 10000)
	if err != nil {
		t.Fatalf("Extract should not return error for bad JSON, got: %v", err)
	}
	if obs != nil {
		t.Errorf("expected nil observations for invalid JSON, got %d", len(obs))
	}
}

func TestExtract_MarkdownWrappedJSON(t *testing.T) {
	store := newMockObserverStore()
	store.messages = makeMessages(5)

	claudeResp := "```json\n" + `[
		{
			"type": "preference",
			"title": "Prefers dark mode",
			"summary": "The user expressed a strong preference for dark mode in all editors.",
			"facts": ["dark mode preferred"],
			"importance": 3
		}
	]` + "\n```"

	sendFn := func(ctx context.Context, system, user string) (string, error) {
		return claudeResp, nil
	}

	ext := NewObservationExtractor(sendFn, store)
	obs, err := ext.Extract(context.Background(), "conv-1", 42, 100, "private", 0, 100, 20, 10000)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(obs) != 1 {
		t.Fatalf("got %d observations, want 1", len(obs))
	}
	if obs[0].Type != "preference" {
		t.Errorf("Type = %q, want %q", obs[0].Type, "preference")
	}
}

func TestExtract_ValidationClamping(t *testing.T) {
	store := newMockObserverStore()
	store.messages = makeMessages(5)

	longTitle := strings.Repeat("a", 300)
	claudeResp := fmt.Sprintf(`[
		{
			"type": "decision",
			"title": %q,
			"summary": "Normal summary.",
			"facts": ["f1","f2","f3","f4","f5","f6","f7","f8","f9","f10","f11","f12"],
			"importance": 15
		},
		{
			"type": "decision",
			"title": "Zero importance",
			"summary": "Another observation.",
			"facts": [],
			"importance": 0
		}
	]`, longTitle)

	sendFn := func(ctx context.Context, system, user string) (string, error) {
		return claudeResp, nil
	}

	ext := NewObservationExtractor(sendFn, store)
	obs, err := ext.Extract(context.Background(), "conv-1", 42, 100, "private", 0, 100, 20, 10000)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(obs) != 2 {
		t.Fatalf("got %d observations, want 2", len(obs))
	}

	// First: importance clamped from 15 to 10, title truncated to 200 runes, facts capped at 10.
	o1 := obs[0]
	if o1.Importance != 10 {
		t.Errorf("Importance = %d, want 10 (clamped from 15)", o1.Importance)
	}
	if len([]rune(o1.Title)) != 200 {
		t.Errorf("Title rune length = %d, want 200", len([]rune(o1.Title)))
	}
	if len(o1.Facts) != 10 {
		t.Errorf("got %d facts, want 10 (capped)", len(o1.Facts))
	}

	// Second: importance clamped from 0 to 1.
	o2 := obs[1]
	if o2.Importance != 1 {
		t.Errorf("Importance = %d, want 1 (clamped from 0)", o2.Importance)
	}
}

func TestExtract_InvalidTypeSkipped(t *testing.T) {
	store := newMockObserverStore()
	store.messages = makeMessages(5)

	claudeResp := `[
		{
			"type": "emotion",
			"title": "Felt happy",
			"summary": "The user was happy.",
			"facts": [],
			"importance": 3
		},
		{
			"type": "decision",
			"title": "Valid observation",
			"summary": "This one should be kept.",
			"facts": [],
			"importance": 5
		}
	]`

	sendFn := func(ctx context.Context, system, user string) (string, error) {
		return claudeResp, nil
	}

	ext := NewObservationExtractor(sendFn, store)
	obs, err := ext.Extract(context.Background(), "conv-1", 42, 100, "private", 0, 100, 20, 10000)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(obs) != 1 {
		t.Fatalf("got %d observations, want 1 (invalid type skipped)", len(obs))
	}
	if obs[0].Title != "Valid observation" {
		t.Errorf("Title = %q, want %q", obs[0].Title, "Valid observation")
	}
}

func TestExtract_DuplicateHash(t *testing.T) {
	store := newMockObserverStore()
	store.messages = makeMessages(5)

	claudeResp := `[
		{
			"type": "decision",
			"title": "Use Go for the backend",
			"summary": "Decided Go is the right language for the server component.",
			"facts": ["Go chosen"],
			"importance": 5
		}
	]`

	sendFn := func(ctx context.Context, system, user string) (string, error) {
		return claudeResp, nil
	}

	ext := NewObservationExtractor(sendFn, store)

	// First extraction should succeed.
	obs1, err := ext.Extract(context.Background(), "conv-1", 42, 100, "private", 0, 100, 20, 10000)
	if err != nil {
		t.Fatalf("Extract 1: %v", err)
	}
	if len(obs1) != 1 {
		t.Fatalf("first extraction got %d observations, want 1", len(obs1))
	}

	// Second extraction with the same content should skip the duplicate.
	obs2, err := ext.Extract(context.Background(), "conv-1", 42, 100, "private", 0, 100, 20, 10000)
	if err != nil {
		t.Fatalf("Extract 2: %v", err)
	}
	if len(obs2) != 0 {
		t.Errorf("second extraction got %d observations, want 0 (duplicate skipped)", len(obs2))
	}

	// Store should only have 1 observation total.
	if len(store.observations) != 1 {
		t.Errorf("store has %d observations, want 1", len(store.observations))
	}
}

func TestExtract_MaxPerConversation(t *testing.T) {
	store := newMockObserverStore()
	store.messages = makeMessages(5)

	// Pre-fill store with observations so it's at the limit.
	for i := 0; i < 3; i++ {
		store.observations = append(store.observations, Observation{
			ConversationID: "conv-1",
			Title:          fmt.Sprintf("existing-%d", i),
		})
	}

	claudeResp := `[
		{
			"type": "decision",
			"title": "Should not be saved",
			"summary": "This observation exceeds the per-conversation limit.",
			"facts": [],
			"importance": 5
		}
	]`

	sendFn := func(ctx context.Context, system, user string) (string, error) {
		return claudeResp, nil
	}

	ext := NewObservationExtractor(sendFn, store)
	obs, err := ext.Extract(context.Background(), "conv-1", 42, 100, "private", 0, 100, 3, 10000)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(obs) != 0 {
		t.Errorf("got %d observations, want 0 (at limit)", len(obs))
	}

	// Store count should remain at 3.
	if len(store.observations) != 3 {
		t.Errorf("store has %d observations, want 3", len(store.observations))
	}
}

func TestContentHash(t *testing.T) {
	// Deterministic: same input produces same hash.
	h1 := observationContentHash("Use Go", "Go was chosen")
	h2 := observationContentHash("Use Go", "Go was chosen")
	if h1 != h2 {
		t.Errorf("hash should be deterministic: %q != %q", h1, h2)
	}

	// Case-insensitive: different casing produces same hash.
	h3 := observationContentHash("USE GO", "GO WAS CHOSEN")
	if h1 != h3 {
		t.Errorf("hash should be case-insensitive: %q != %q", h1, h3)
	}

	// Whitespace-trimmed: outer whitespace is trimmed but inner spaces remain.
	// "  Use Go  |  Go was chosen  " trims to "Use Go  |  Go was chosen"
	// which differs from "Use Go|Go was chosen", so different hash is expected.
	h4 := observationContentHash("  Use Go  ", "  Go was chosen  ")
	// The inputs differ (internal spaces around pipe), so hashes must differ.
	if h1 == h4 {
		t.Errorf("hash with internal whitespace should differ from trimmed: both = %q", h1)
	}

	// Pure leading/trailing whitespace on the combined string is trimmed.
	h6 := observationContentHash("Use Go", "Go was chosen")
	if h1 != h6 {
		t.Errorf("same content should produce same hash: %q != %q", h1, h6)
	}

	// Different content produces different hash.
	h5 := observationContentHash("Use Rust", "Rust was chosen")
	if h1 == h5 {
		t.Errorf("different content should produce different hash: %q == %q", h1, h5)
	}

	// Hash is a hex-encoded SHA-256 (64 chars).
	if len(h1) != 64 {
		t.Errorf("hash length = %d, want 64 (SHA-256 hex)", len(h1))
	}
}

func TestParseObservationJSON_EmptyArray(t *testing.T) {
	raw, err := parseObservationJSON("[]")
	if err != nil {
		t.Fatalf("parseObservationJSON: %v", err)
	}
	if len(raw) != 0 {
		t.Errorf("got %d observations, want 0", len(raw))
	}
}

func TestParseObservationJSON_MarkdownFences(t *testing.T) {
	resp := "```json\n[{\"type\":\"decision\",\"title\":\"t\",\"summary\":\"s\",\"facts\":[],\"importance\":5}]\n```"
	raw, err := parseObservationJSON(resp)
	if err != nil {
		t.Fatalf("parseObservationJSON: %v", err)
	}
	if len(raw) != 1 {
		t.Fatalf("got %d observations, want 1", len(raw))
	}
	if raw[0].Type != "decision" {
		t.Errorf("Type = %q, want %q", raw[0].Type, "decision")
	}
}

func TestParseObservationJSON_PlainFences(t *testing.T) {
	resp := "```\n[{\"type\":\"preference\",\"title\":\"t\",\"summary\":\"s\",\"facts\":[],\"importance\":2}]\n```"
	raw, err := parseObservationJSON(resp)
	if err != nil {
		t.Fatalf("parseObservationJSON: %v", err)
	}
	if len(raw) != 1 {
		t.Fatalf("got %d observations, want 1", len(raw))
	}
}

func TestValidateRawObservation_ControlChars(t *testing.T) {
	r := rawObservation{
		Type:       "decision",
		Title:      "hello\x00world",
		Summary:    "test\x07summary",
		Facts:      []string{"fact\x01one"},
		Importance: 5,
	}
	obs, ok := validateRawObservation(r)
	if !ok {
		t.Fatal("expected valid observation")
	}
	if strings.ContainsAny(obs.Title, "\x00") {
		t.Error("title should have control chars stripped")
	}
	if obs.Title != "helloworld" {
		t.Errorf("Title = %q, want %q", obs.Title, "helloworld")
	}
	if obs.Summary != "testsummary" {
		t.Errorf("Summary = %q, want %q", obs.Summary, "testsummary")
	}
	if len(obs.Facts) != 1 || obs.Facts[0] != "factone" {
		t.Errorf("Facts = %v, want [factone]", obs.Facts)
	}
}

func TestValidateRawObservation_EmptyTitleOrSummary(t *testing.T) {
	// Empty title should be rejected.
	r1 := rawObservation{Type: "decision", Title: "", Summary: "ok", Importance: 5}
	if _, ok := validateRawObservation(r1); ok {
		t.Error("empty title should be rejected")
	}

	// Empty summary should be rejected.
	r2 := rawObservation{Type: "decision", Title: "ok", Summary: "", Importance: 5}
	if _, ok := validateRawObservation(r2); ok {
		t.Error("empty summary should be rejected")
	}

	// Only control chars (which get stripped) should be rejected.
	r3 := rawObservation{Type: "decision", Title: "\x00\x01", Summary: "ok", Importance: 5}
	if _, ok := validateRawObservation(r3); ok {
		t.Error("title with only control chars should be rejected")
	}
}

func TestSanitizeObservationString(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"normal text", "normal text"},
		{"  spaces  ", "spaces"},
		{"ctrl\x00chars\x07here", "ctrlcharshere"},
		{"tabs\tare\tcontrol", "tabsarecontrol"},
		{"newlines\nare\ncontrol", "newlinesarecontrol"},
		{"keep spaces between words", "keep spaces between words"},
		{"", ""},
	}
	for _, tt := range tests {
		got := sanitizeObservationString(tt.input)
		if got != tt.want {
			t.Errorf("sanitizeObservationString(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestTruncateRunes(t *testing.T) {
	// ASCII
	if got := truncateRunes("hello", 3); got != "hel" {
		t.Errorf("truncateRunes(hello, 3) = %q, want %q", got, "hel")
	}
	// No truncation needed.
	if got := truncateRunes("hi", 10); got != "hi" {
		t.Errorf("truncateRunes(hi, 10) = %q, want %q", got, "hi")
	}
	// Multi-byte: each emoji is 1 rune.
	emojis := strings.Repeat("\U0001f680", 5)
	if got := truncateRunes(emojis, 3); got != "\U0001f680\U0001f680\U0001f680" {
		t.Errorf("truncateRunes(emojis, 3) = %q, want 3 rockets", got)
	}
}
