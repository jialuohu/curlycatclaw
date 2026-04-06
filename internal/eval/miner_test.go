package eval

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestClamp(t *testing.T) {
	tests := []struct {
		v, lo, hi, want int
	}{
		{5, 1, 10, 5},   // within range
		{0, 1, 10, 1},   // below min
		{15, 1, 10, 10}, // above max
		{1, 1, 10, 1},   // at min
		{10, 1, 10, 10}, // at max
		{-5, 0, 100, 0}, // negative below zero min
	}
	for _, tt := range tests {
		got := clamp(tt.v, tt.lo, tt.hi)
		if got != tt.want {
			t.Errorf("clamp(%d, %d, %d) = %d, want %d", tt.v, tt.lo, tt.hi, got, tt.want)
		}
	}
}

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE tool_calls (
		id TEXT PRIMARY KEY,
		conversation_id TEXT,
		name TEXT,
		output TEXT,
		is_error BOOLEAN DEFAULT FALSE
	)`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestMineFailures_HighScoresSkipped(t *testing.T) {
	db := testDB(t)
	m := NewMiner(db)
	scores := []EvalScore{
		{ConversationID: "c1", OverallScore: 0.9, CorrectionCount: 0, RetryCount: 0, EffortOverrideCount: 0},
		{ConversationID: "c2", OverallScore: 0.8, CorrectionCount: 1, RetryCount: 0, EffortOverrideCount: 0},
	}
	clusters, err := m.MineFailures("run1", scores, 0.6)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(clusters) != 0 {
		t.Errorf("expected 0 clusters for high-scoring conversations, got %d", len(clusters))
	}
}

func TestMineFailures_CorrectionCluster(t *testing.T) {
	db := testDB(t)
	m := NewMiner(db)
	scores := []EvalScore{
		{ConversationID: "c1", OverallScore: 0.4, CorrectionCount: 3, RetryCount: 0, EffortOverrideCount: 0},
	}
	clusters, err := m.MineFailures("run1", scores, 0.5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	found := false
	for _, c := range clusters {
		if c.ClusterType == "correction" {
			found = true
			if c.Frequency != 3 {
				t.Errorf("expected frequency 3, got %d", c.Frequency)
			}
			if c.Severity != 6 { // clamp(3*2, 1, 10) = 6
				t.Errorf("expected severity 6, got %d", c.Severity)
			}
		}
	}
	if !found {
		t.Error("expected correction cluster for low-scoring conversation with 3 corrections")
	}
}

func TestMineFailures_RetryCluster(t *testing.T) {
	db := testDB(t)
	m := NewMiner(db)
	scores := []EvalScore{
		{ConversationID: "c1", OverallScore: 0.3, CorrectionCount: 0, RetryCount: 4, EffortOverrideCount: 0},
	}
	clusters, err := m.MineFailures("run1", scores, 0.5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	found := false
	for _, c := range clusters {
		if c.ClusterType == "user_retry" {
			found = true
			if c.Frequency != 4 {
				t.Errorf("expected frequency 4, got %d", c.Frequency)
			}
			if c.Severity != 8 { // clamp(4*2, 1, 10) = 8
				t.Errorf("expected severity 8, got %d", c.Severity)
			}
		}
	}
	if !found {
		t.Error("expected user_retry cluster")
	}
}

func TestMineFailures_EffortOverrideCluster(t *testing.T) {
	db := testDB(t)
	m := NewMiner(db)
	scores := []EvalScore{
		{ConversationID: "c1", OverallScore: 0.4, CorrectionCount: 0, RetryCount: 0, EffortOverrideCount: 2},
	}
	clusters, err := m.MineFailures("run1", scores, 0.5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	found := false
	for _, c := range clusters {
		if c.ClusterType == "effort_override" {
			found = true
			if c.Frequency != 2 {
				t.Errorf("expected frequency 2, got %d", c.Frequency)
			}
			if c.Severity != 6 { // clamp(2*3, 1, 10) = 6
				t.Errorf("expected severity 6, got %d", c.Severity)
			}
		}
	}
	if !found {
		t.Error("expected effort_override cluster")
	}
}

func TestMineToolErrors(t *testing.T) {
	db := testDB(t)
	// Insert tool call errors.
	_, err := db.Exec(`INSERT INTO tool_calls (id, conversation_id, name, output, is_error) VALUES
		('tc1', 'c1', 'web_search', 'timeout after 30s', TRUE),
		('tc2', 'c1', 'web_search', 'connection refused', TRUE),
		('tc3', 'c1', 'read_file', 'file not found: /tmp/missing.txt', TRUE)`)
	if err != nil {
		t.Fatal(err)
	}

	m := NewMiner(db)
	clusters, err := m.mineToolErrors("run1", "c1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(clusters) != 2 {
		t.Fatalf("expected 2 tool error clusters, got %d", len(clusters))
	}

	// Find the web_search cluster (2 errors).
	for _, c := range clusters {
		if c.ClusterType != "tool_error" {
			t.Errorf("expected cluster type tool_error, got %s", c.ClusterType)
		}
		if len(c.ToolCallIDs) == 2 {
			// web_search cluster
			if c.Severity != 6 { // clamp(2*3, 1, 10) = 6
				t.Errorf("expected severity 6 for 2 errors, got %d", c.Severity)
			}
		}
	}
}

func TestMineToolErrors_NoErrors(t *testing.T) {
	db := testDB(t)
	m := NewMiner(db)
	clusters, err := m.mineToolErrors("run1", "c1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(clusters) != 0 {
		t.Errorf("expected 0 clusters for no errors, got %d", len(clusters))
	}
}

func TestMineToolErrors_LongOutputTruncated(t *testing.T) {
	db := testDB(t)
	longOutput := make([]byte, 300)
	for i := range longOutput {
		longOutput[i] = 'x'
	}
	_, err := db.Exec(`INSERT INTO tool_calls (id, conversation_id, name, output, is_error) VALUES (?, ?, ?, ?, TRUE)`,
		"tc1", "c1", "tool", string(longOutput))
	if err != nil {
		t.Fatal(err)
	}

	m := NewMiner(db)
	clusters, err := m.mineToolErrors("run1", "c1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(clusters) != 1 {
		t.Fatalf("expected 1 cluster, got %d", len(clusters))
	}
	// Description should contain truncated output (200 chars + "...")
	if len(clusters[0].Description) > 250 {
		t.Errorf("description should be truncated, got %d chars", len(clusters[0].Description))
	}
}

func TestNewID_UniqueAndCorrectLength(t *testing.T) {
	id1 := newID()
	id2 := newID()
	if id1 == id2 {
		t.Error("newID should generate unique IDs")
	}
	if len(id1) != 32 { // 16 bytes = 32 hex chars
		t.Errorf("expected 32 hex chars, got %d", len(id1))
	}
}
