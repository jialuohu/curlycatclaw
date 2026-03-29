package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/jialuohu/curlycatclaw/internal/claude"
)

// ---------- test helpers ----------

// sseEvent formats a single SSE frame.
func sseEvent(eventType, data string) string {
	return fmt.Sprintf("event: %s\ndata: %s\n\n", eventType, data)
}

// simpleTextSSE returns an SSE sequence for a plain text response.
func simpleTextSSE(text string) []string {
	escaped, _ := json.Marshal(text)
	return []string{
		sseEvent("message_start", `{"type":"message_start","message":{"id":"msg_test","type":"message","role":"assistant","content":[],"model":"claude-haiku-4-5-20251001","stop_reason":null,"usage":{"input_tokens":10,"output_tokens":0}}}`),
		sseEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		sseEvent("content_block_delta",
			fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%s}}`, string(escaped))),
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":0}`),
		sseEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":5}}`),
		sseEvent("message_stop", `{"type":"message_stop"}`),
	}
}

// newMockSSEServer creates a test server that responds with canned SSE events.
func newMockSSEServer(t *testing.T, events []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, ev := range events {
			fmt.Fprint(w, ev)
		}
	}))
}

// newErrorSSEServer returns a server that replies with an HTTP error.
func newErrorSSEServer(t *testing.T, statusCode int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		fmt.Fprint(w, `{"type":"error","error":{"type":"api_error","message":"test error"}}`)
	}))
}

// newTestHaikuClient creates a Claude client pointing at the test server.
func newTestHaikuClient(baseURL string) *claude.Client {
	return claude.NewClient(option.WithAPIKey("test-key"), "claude-haiku-4-5-20251001",
		option.WithBaseURL(baseURL),
	)
}

// newTestDB creates an in-memory SQLite database with the required tables.
func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "budget_test.db")
	s, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s.DB()
}

// newTestBudgetManager creates a BudgetManager for testing with a mock server.
func newTestBudgetManager(t *testing.T, srv *httptest.Server) (*BudgetManager, *sql.DB) {
	t.Helper()
	db := newTestDB(t)
	client := newTestHaikuClient(srv.URL)
	bm, err := NewBudgetManager(db, client, true)
	if err != nil {
		t.Fatalf("NewBudgetManager: %v", err)
	}
	return bm, db
}

// makeTurn creates a turn with user+assistant messages.
func makeTurn(userText, assistantText string) turn {
	uc, _ := json.Marshal(userText)
	ac, _ := json.Marshal(assistantText)
	return turn{
		messages: []Message{
			{Role: "user", Content: uc},
			{Role: "assistant", Content: ac},
		},
		chars: len(uc) + len(ac),
	}
}

// ---------- tests ----------

func TestKeywordFastPath(t *testing.T) {
	// All turns contain the keyword "meeting", so the LLM should never be called.
	// We provide an error server to verify it's not hit.
	srv := newErrorSSEServer(t, 500)
	defer srv.Close()

	bm, _ := newTestBudgetManager(t, srv)

	turns := []turn{
		makeTurn("Let's discuss the meeting agenda", "Sure, here are the topics"),
		makeTurn("Schedule the next meeting", "I've added it to the calendar"),
		makeTurn("Book a meeting room", "Done, room 5 is reserved"),
	}

	classified, err := bm.ClassifyTurns(context.Background(),"tell me about the meeting", turns)
	if err != nil {
		t.Fatalf("ClassifyTurns: %v", err)
	}

	if len(classified) != 3 {
		t.Fatalf("got %d classified turns, want 3", len(classified))
	}

	// All turns mention "meeting", so they should all be "full" via keyword fast-path.
	for i, ct := range classified {
		if ct.Classification != "full" {
			t.Errorf("turn %d: classification = %q, want %q", i, ct.Classification, "full")
		}
	}
}

func TestKeywordFastPath_MatchesSubstring(t *testing.T) {
	// Provide a working LLM for non-keyword turns.
	sseResp := simpleTextSSE("1|NONE|")
	srv := newMockSSEServer(t, sseResp)
	defer srv.Close()

	bm, _ := newTestBudgetManager(t, srv)

	turns := []turn{
		makeTurn("The deployment succeeded", "Great news!"),
		makeTurn("What's for lunch?", "Pizza sounds good"),
	}

	classified, err := bm.ClassifyTurns(context.Background(),"check deployment status", turns)
	if err != nil {
		t.Fatalf("ClassifyTurns: %v", err)
	}

	// "deployment" appears in turn 0 content -> full via keyword.
	if classified[0].Classification != "full" {
		t.Errorf("turn 0: classification = %q, want %q (keyword fast-path)", classified[0].Classification, "full")
	}
}

func TestCacheHit(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, ev := range simpleTextSSE("1|SUMMARY|A summary of the turn") {
			fmt.Fprint(w, ev)
		}
	}))
	defer srv.Close()

	bm, _ := newTestBudgetManager(t, srv)

	turns := []turn{
		makeTurn("Some unrelated past topic xyz", "An equally unrelated response abc"),
	}

	// First call: should hit the LLM.
	_, err := bm.ClassifyTurns(context.Background(),"current question about def", turns)
	if err != nil {
		t.Fatalf("first ClassifyTurns: %v", err)
	}
	if callCount != 1 {
		t.Fatalf("expected 1 LLM call, got %d", callCount)
	}

	// Second call with same inputs: should use cache.
	classified, err := bm.ClassifyTurns(context.Background(),"current question about def", turns)
	if err != nil {
		t.Fatalf("second ClassifyTurns: %v", err)
	}
	if callCount != 1 {
		t.Errorf("expected no additional LLM call (cache hit), got %d total calls", callCount)
	}

	if classified[0].Classification != "summary" {
		t.Errorf("cached classification = %q, want %q", classified[0].Classification, "summary")
	}
	if classified[0].Summary != "A summary of the turn" {
		t.Errorf("cached summary = %q, want %q", classified[0].Summary, "A summary of the turn")
	}
}

func TestCacheTTL_Expired(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, ev := range simpleTextSSE("1|NONE|") {
			fmt.Fprint(w, ev)
		}
	}))
	defer srv.Close()

	bm, db := newTestBudgetManager(t, srv)
	// Use a very short TTL for testing.
	bm.cacheTTL = 1 * time.Millisecond

	turns := []turn{
		makeTurn("old topic alpha", "old response beta"),
	}
	currentMsg := "new question gamma"

	// Insert a cache entry with an old timestamp.
	content := turnText(turns[0])
	hash := cacheHash(content, currentMsg)
	oldTime := time.Now().UTC().Add(-1 * time.Hour)
	_, err := db.Exec(
		`INSERT INTO budget_cache (hash, classification, summary, created_at) VALUES (?, ?, ?, ?)`,
		hash, "full", "", oldTime,
	)
	if err != nil {
		t.Fatalf("insert old cache entry: %v", err)
	}

	// ClassifyTurns should ignore the expired cache entry and call LLM.
	classified, err := bm.ClassifyTurns(context.Background(),currentMsg, turns)
	if err != nil {
		t.Fatalf("ClassifyTurns: %v", err)
	}

	if callCount != 1 {
		t.Errorf("expected 1 LLM call (expired cache), got %d", callCount)
	}
	if classified[0].Classification != "none" {
		t.Errorf("classification = %q, want %q", classified[0].Classification, "none")
	}
}

func TestParseClassifications(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		expected int
		wantCls  []string
		wantSum  []string
	}{
		{
			name:     "basic three turns",
			text:     "TURN 1|FULL|\nTURN 2|SUMMARY|User discussed project deadlines\nTURN 3|NONE|",
			expected: 3,
			wantCls:  []string{"full", "summary", "none"},
			wantSum:  []string{"", "User discussed project deadlines", ""},
		},
		{
			name:     "plain numbers",
			text:     "1|FULL|\n2|NONE|\n3|SUMMARY|Weather discussion",
			expected: 3,
			wantCls:  []string{"full", "none", "summary"},
			wantSum:  []string{"", "", "Weather discussion"},
		},
		{
			name:     "partial results default to full",
			text:     "TURN 1|NONE|",
			expected: 3,
			wantCls:  []string{"none", "full", "full"},
			wantSum:  []string{"", "", ""},
		},
		{
			name:     "empty response defaults to full",
			text:     "",
			expected: 2,
			wantCls:  []string{"full", "full"},
			wantSum:  []string{"", ""},
		},
		{
			name:     "malformed lines ignored",
			text:     "garbage\nTURN 1|FULL|\nbad|data\nTURN 2|SUMMARY|Some summary",
			expected: 2,
			wantCls:  []string{"full", "summary"},
			wantSum:  []string{"", "Some summary"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			results := parseClassifications(tc.text, tc.expected)
			if len(results) != tc.expected {
				t.Fatalf("got %d results, want %d", len(results), tc.expected)
			}
			for i, r := range results {
				if r.Classification != tc.wantCls[i] {
					t.Errorf("result[%d].Classification = %q, want %q", i, r.Classification, tc.wantCls[i])
				}
				if r.Summary != tc.wantSum[i] {
					t.Errorf("result[%d].Summary = %q, want %q", i, r.Summary, tc.wantSum[i])
				}
			}
		})
	}
}

func TestFallback_LLMError(t *testing.T) {
	srv := newErrorSSEServer(t, 500)
	defer srv.Close()

	dbPath := filepath.Join(t.TempDir(), "fallback_test.db")
	store, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	client := newTestHaikuClient(srv.URL)
	bm, err := NewBudgetManager(store.DB(), client, true)
	if err != nil {
		t.Fatalf("NewBudgetManager: %v", err)
	}

	cb := NewContextBuilder(store)
	cb.SetBudget(bm)

	convID, err := store.CreateConversation(1, 1)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	// Add some messages.
	for i := 0; i < 3; i++ {
		uc, _ := json.Marshal(fmt.Sprintf("question %d about unique_xyz_%d", i, i))
		ac, _ := json.Marshal(fmt.Sprintf("answer %d", i))
		if err := store.AppendMessage(convID, "user", uc); err != nil {
			t.Fatalf("AppendMessage user: %v", err)
		}
		if err := store.AppendMessage(convID, "assistant", ac); err != nil {
			t.Fatalf("AppendMessage assistant: %v", err)
		}
	}

	// BuildContextWithBudget should fall back to standard BuildContext on LLM error.
	msgs, err := cb.BuildContextWithBudget(context.Background(),convID, "something completely different")
	if err != nil {
		t.Fatalf("BuildContextWithBudget: %v", err)
	}

	// Should get all 6 messages (3 turns x 2 messages each) from the fallback.
	if len(msgs) != 6 {
		t.Errorf("got %d messages, want 6 (fallback to full BuildContext)", len(msgs))
	}
}

func TestBuildContextWithBudget_ReducesMessages(t *testing.T) {
	// LLM classifies all turns: turn 1 = FULL, turn 2 = NONE, turn 3 = SUMMARY.
	sseResp := simpleTextSSE("1|FULL|\n2|NONE|\n3|SUMMARY|A brief summary")
	srv := newMockSSEServer(t, sseResp)
	defer srv.Close()

	dbPath := filepath.Join(t.TempDir(), "integration_test.db")
	store, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	client := newTestHaikuClient(srv.URL)
	bm, err := NewBudgetManager(store.DB(), client, true)
	if err != nil {
		t.Fatalf("NewBudgetManager: %v", err)
	}

	cb := NewContextBuilder(store)
	cb.SetBudget(bm)

	convID, err := store.CreateConversation(1, 1)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	// Add 3 turns. None match keywords for the current message.
	for i := 0; i < 3; i++ {
		uc, _ := json.Marshal(fmt.Sprintf("unrelated_topic_%d_aaa", i))
		ac, _ := json.Marshal(fmt.Sprintf("unrelated_answer_%d_bbb", i))
		if err := store.AppendMessage(convID, "user", uc); err != nil {
			t.Fatalf("AppendMessage user: %v", err)
		}
		if err := store.AppendMessage(convID, "assistant", ac); err != nil {
			t.Fatalf("AppendMessage assistant: %v", err)
		}
	}

	// Standard BuildContext returns all 6 messages.
	allMsgs, err := cb.BuildContext(convID)
	if err != nil {
		t.Fatalf("BuildContext: %v", err)
	}

	// Budget-aware returns fewer (turn 2 dropped, turn 3 summarized to 1 msg).
	budgetMsgs, err := cb.BuildContextWithBudget(context.Background(),convID, "zzz_completely_different_zzz")
	if err != nil {
		t.Fatalf("BuildContextWithBudget: %v", err)
	}

	if len(budgetMsgs) >= len(allMsgs) {
		t.Errorf("budget context (%d msgs) should be smaller than full context (%d msgs)",
			len(budgetMsgs), len(allMsgs))
	}

	// Turn 1 (FULL): 2 messages (user+assistant)
	// Turn 2 (NONE): 0 messages
	// Turn 3 (SUMMARY): 1 synthetic user message
	// Total expected: 3
	if len(budgetMsgs) != 3 {
		t.Errorf("got %d budget messages, want 3 (2 full + 0 none + 1 summary)", len(budgetMsgs))
	}

	// Verify the summary message is present.
	found := false
	for _, m := range budgetMsgs {
		var text string
		if json.Unmarshal(m.Content, &text) == nil && text == "A brief summary" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected synthetic summary message with text 'A brief summary'")
	}
}

func TestBuildContextWithBudget_DisabledFallsBack(t *testing.T) {
	srv := newErrorSSEServer(t, 500)
	defer srv.Close()

	dbPath := filepath.Join(t.TempDir(), "disabled_test.db")
	store, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	client := newTestHaikuClient(srv.URL)
	bm, err := NewBudgetManager(store.DB(), client, false) // disabled
	if err != nil {
		t.Fatalf("NewBudgetManager: %v", err)
	}

	cb := NewContextBuilder(store)
	cb.SetBudget(bm)

	convID, err := store.CreateConversation(1, 1)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	uc, _ := json.Marshal("hello")
	ac, _ := json.Marshal("world")
	if err := store.AppendMessage(convID, "user", uc); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if err := store.AppendMessage(convID, "assistant", ac); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	// With budget disabled, should still return all messages.
	msgs, err := cb.BuildContextWithBudget(context.Background(),convID, "hello")
	if err != nil {
		t.Fatalf("BuildContextWithBudget: %v", err)
	}
	if len(msgs) != 2 {
		t.Errorf("got %d messages, want 2 (budget disabled, full context)", len(msgs))
	}
}

func TestBuildContextWithBudget_NilBudgetFallsBack(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "nil_test.db")
	store, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	cb := NewContextBuilder(store)
	// No SetBudget call: budget is nil.

	convID, err := store.CreateConversation(1, 1)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	uc, _ := json.Marshal("hello")
	ac, _ := json.Marshal("world")
	if err := store.AppendMessage(convID, "user", uc); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if err := store.AppendMessage(convID, "assistant", ac); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	msgs, err := cb.BuildContextWithBudget(context.Background(),convID, "hello")
	if err != nil {
		t.Fatalf("BuildContextWithBudget: %v", err)
	}
	if len(msgs) != 2 {
		t.Errorf("got %d messages, want 2 (nil budget, full context)", len(msgs))
	}
}

func TestExtractKeywords(t *testing.T) {
	tests := []struct {
		msg  string
		want []string
	}{
		{"Tell me about the meeting", []string{"tell", "meeting"}},
		{"a b cd", nil},                         // all too short
		{"what about this?", nil},               // all stop words or short
		{"deploy the service!", []string{"deploy", "service"}},
	}

	for _, tc := range tests {
		got := extractKeywords(tc.msg)
		if len(got) != len(tc.want) {
			t.Errorf("extractKeywords(%q) = %v, want %v", tc.msg, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("extractKeywords(%q)[%d] = %q, want %q", tc.msg, i, got[i], tc.want[i])
			}
		}
	}
}

func TestMatchesKeyword(t *testing.T) {
	tests := []struct {
		content  string
		keywords []string
		want     bool
	}{
		{"We had a meeting yesterday", []string{"meeting"}, true},
		{"The deployment was successful", []string{"deploy"}, true}, // substring
		{"Nothing relevant here", []string{"meeting", "deploy"}, false},
		{"UPPERCASE MEETING", []string{"meeting"}, true}, // case insensitive
	}

	for _, tc := range tests {
		got := matchesKeyword(tc.content, tc.keywords)
		if got != tc.want {
			t.Errorf("matchesKeyword(%q, %v) = %v, want %v", tc.content, tc.keywords, got, tc.want)
		}
	}
}

func TestNewBudgetManager_CreatesTable(t *testing.T) {
	db := newTestDB(t)

	_, err := NewBudgetManager(db, nil, false)
	if err != nil {
		t.Fatalf("NewBudgetManager: %v", err)
	}

	// Verify table exists by inserting a row.
	_, err = db.Exec(
		`INSERT INTO budget_cache (hash, classification, summary, created_at) VALUES (?, ?, ?, ?)`,
		"test_hash", "full", "", time.Now().UTC(),
	)
	if err != nil {
		t.Fatalf("insert into budget_cache: %v", err)
	}
}
