package eval

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// Create minimal schema needed for scorer tests.
	schema := `
		PRAGMA journal_mode=WAL;
		CREATE TABLE conversations (
			id TEXT PRIMARY KEY, user_id INTEGER, chat_id INTEGER,
			created_at DATETIME, updated_at DATETIME
		);
		CREATE TABLE messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			conversation_id TEXT, role TEXT, content TEXT, created_at DATETIME
		);
		CREATE TABLE tool_calls (
			id TEXT PRIMARY KEY, conversation_id TEXT, name TEXT,
			input TEXT, output TEXT, is_error BOOLEAN DEFAULT FALSE, created_at DATETIME
		);
		CREATE TABLE interaction_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			conversation_id TEXT, user_id INTEGER, chat_id INTEGER,
			event_type TEXT, payload TEXT, created_at DATETIME
		);
	`
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	return db
}

func seedConversation(t *testing.T, db *sql.DB, convID string, userID, chatID int64) {
	t.Helper()
	now := time.Now().UTC()
	db.Exec(`INSERT INTO conversations (id, user_id, chat_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		convID, userID, chatID, now, now)
}

func seedMessage(t *testing.T, db *sql.DB, convID, role, content string) {
	t.Helper()
	db.Exec(`INSERT INTO messages (conversation_id, role, content, created_at) VALUES (?, ?, ?, ?)`,
		convID, role, content, time.Now().UTC())
}

func seedToolCall(t *testing.T, db *sql.DB, convID, id, name string, isError bool, output string) {
	t.Helper()
	db.Exec(`INSERT INTO tool_calls (id, conversation_id, name, input, output, is_error, created_at) VALUES (?, ?, ?, '{}', ?, ?, ?)`,
		id, convID, name, output, isError, time.Now().UTC())
}

func seedInteractionEvent(t *testing.T, db *sql.DB, convID string, userID, chatID int64, eventType, payload string) {
	t.Helper()
	db.Exec(`INSERT INTO interaction_events (conversation_id, user_id, chat_id, event_type, payload, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		convID, userID, chatID, eventType, payload, time.Now().UTC())
}

func TestScoreConversation_PerfectScore(t *testing.T) {
	db := newTestDB(t)
	scorer := NewScorer(db)

	seedConversation(t, db, "conv1", 42, 100)
	seedMessage(t, db, "conv1", "user", `"hello"`)
	seedMessage(t, db, "conv1", "assistant", `"hi there"`)
	seedToolCall(t, db, "conv1", "tc1", "search", false, "results")
	seedToolCall(t, db, "conv1", "tc2", "search", false, "more results")

	sig, err := scorer.ScoreConversation("conv1")
	if err != nil {
		t.Fatalf("ScoreConversation: %v", err)
	}

	score := sig.Score()
	if score < 0.99 {
		t.Errorf("expected near-perfect score >= 0.99, got %.4f", score)
	}
	if sig.TotalToolCalls != 2 {
		t.Errorf("expected 2 tool calls, got %d", sig.TotalToolCalls)
	}
	if sig.FailedToolCalls != 0 {
		t.Errorf("expected 0 failures, got %d", sig.FailedToolCalls)
	}
}

func TestScoreConversation_WithFailures(t *testing.T) {
	db := newTestDB(t)
	scorer := NewScorer(db)

	seedConversation(t, db, "conv1", 42, 100)
	seedMessage(t, db, "conv1", "user", `"do something"`)
	seedMessage(t, db, "conv1", "assistant", `"wrong answer"`)
	seedMessage(t, db, "conv1", "user", `"no, that's incorrect"`)
	seedMessage(t, db, "conv1", "assistant", `"let me try again"`)
	seedToolCall(t, db, "conv1", "tc1", "search", true, "error: not found")
	seedToolCall(t, db, "conv1", "tc2", "search", false, "ok")
	seedInteractionEvent(t, db, "conv1", 42, 100, "retry", "")
	seedInteractionEvent(t, db, "conv1", 42, 100, "effort_override", "max")

	sig, err := scorer.ScoreConversation("conv1")
	if err != nil {
		t.Fatalf("ScoreConversation: %v", err)
	}

	if sig.CorrectionCount != 1 {
		t.Errorf("expected 1 correction, got %d", sig.CorrectionCount)
	}
	if sig.RetryCount != 1 {
		t.Errorf("expected 1 retry, got %d", sig.RetryCount)
	}
	if sig.EffortOverrides != 1 {
		t.Errorf("expected 1 effort override, got %d", sig.EffortOverrides)
	}
	if sig.FailedToolCalls != 1 {
		t.Errorf("expected 1 failed tool call, got %d", sig.FailedToolCalls)
	}

	score := sig.Score()
	if score >= 1.0 || score <= 0.0 {
		t.Errorf("expected degraded score between 0 and 1, got %.4f", score)
	}
}

func TestScoreFormula_EdgeCases(t *testing.T) {
	tests := []struct {
		name     string
		signals  ConversationSignals
		expected float64
	}{
		{
			name:     "zero everything",
			signals:  ConversationSignals{},
			expected: 1.0, // no tools, no corrections, no retries, no effort
		},
		{
			name:     "all tools failed",
			signals:  ConversationSignals{TotalToolCalls: 5, FailedToolCalls: 5},
			expected: 0.65, // 0.35*0 + 0.30*1 + 0.20*1 + 0.15*1
		},
		{
			name:     "max corrections",
			signals:  ConversationSignals{CorrectionCount: 10},
			expected: 0.70, // 0.35*1 + 0.30*0 + 0.20*1 + 0.15*1
		},
		{
			name:     "everything bad",
			signals:  ConversationSignals{TotalToolCalls: 1, FailedToolCalls: 1, CorrectionCount: 5, RetryCount: 5, EffortOverrides: 3},
			expected: 0.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.signals.Score()
			diff := got - tt.expected
			if diff < -0.01 || diff > 0.01 {
				t.Errorf("Score() = %.4f, want %.4f", got, tt.expected)
			}
		})
	}
}

func TestIsCorrection(t *testing.T) {
	tests := []struct {
		text string
		want bool
	}{
		{"no, that's wrong", true},
		{"actually, I meant something else", true},
		{"that's incorrect, try again", true},
		{"wrong answer", true},
		{"no problem, that looks good", false},
		{"no worries!", false},
		{"actually, good job!", false},
		{"thanks!", false},
		{"can you also do X?", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.text, func(t *testing.T) {
			got := isCorrection(tt.text)
			if got != tt.want {
				t.Errorf("isCorrection(%q) = %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}

func TestMineFailures_ToolErrors(t *testing.T) {
	db := newTestDB(t)
	miner := NewMiner(db)

	seedConversation(t, db, "conv1", 42, 100)
	seedToolCall(t, db, "conv1", "tc1", "search", true, "timeout")
	seedToolCall(t, db, "conv1", "tc2", "search", true, "timeout")

	scores := []EvalScore{
		{ConversationID: "conv1", OverallScore: 0.3, CorrectionCount: 0, RetryCount: 0},
	}

	clusters, err := miner.MineFailures("run1", scores, 0.6)
	if err != nil {
		t.Fatalf("MineFailures: %v", err)
	}

	if len(clusters) == 0 {
		t.Fatal("expected at least 1 cluster")
	}

	foundToolError := false
	for _, c := range clusters {
		if c.ClusterType == "tool_error" {
			foundToolError = true
			if c.Frequency != 2 {
				t.Errorf("expected frequency 2, got %d", c.Frequency)
			}
		}
	}
	if !foundToolError {
		t.Error("expected a tool_error cluster")
	}
}

func TestMineFailures_SkipsHighScores(t *testing.T) {
	db := newTestDB(t)
	miner := NewMiner(db)

	scores := []EvalScore{
		{ConversationID: "conv1", OverallScore: 0.9},
	}

	clusters, err := miner.MineFailures("run1", scores, 0.6)
	if err != nil {
		t.Fatalf("MineFailures: %v", err)
	}
	if len(clusters) != 0 {
		t.Errorf("expected 0 clusters for high score, got %d", len(clusters))
	}
}

// Suppress unused import warning for json (used by seedMessage with raw JSON content).
var _ = json.Marshal
