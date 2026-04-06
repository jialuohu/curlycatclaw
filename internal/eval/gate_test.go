package eval

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func newGateTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	db.Exec(`CREATE TABLE memory_candidates (
		id TEXT PRIMARY KEY, eval_run_id TEXT, failure_cluster_id TEXT,
		candidate_type TEXT, title TEXT, content TEXT, evidence TEXT,
		confidence REAL, predicted_impact TEXT, status TEXT DEFAULT 'pending',
		replay_score REAL, committed_observation_id TEXT,
		user_id INTEGER DEFAULT 0, chat_id INTEGER DEFAULT 0,
		reviewed_at DATETIME, created_at DATETIME
	)`)

	return db
}

func TestGate_DiscardLowConfidence(t *testing.T) {
	db := newGateTestDB(t)
	gate := NewGate(db, false)

	candidates := []MemoryCandidate{
		{ID: "c1", Confidence: 0.3, Title: "low", CreatedAt: time.Now()},
		{ID: "c2", Confidence: 0.1, Title: "very low", CreatedAt: time.Now()},
	}

	results := gate.Process(candidates)
	for _, r := range results {
		if r.Action != "discarded" {
			t.Errorf("candidate %s: got %s, want discarded", r.CandidateID, r.Action)
		}
	}
}

func TestGate_PendingMediumConfidence(t *testing.T) {
	db := newGateTestDB(t)
	gate := NewGate(db, false)

	candidates := []MemoryCandidate{
		{ID: "c1", Confidence: 0.7, Title: "medium", CreatedAt: time.Now()},
		{ID: "c2", Confidence: 0.95, Title: "high", CreatedAt: time.Now()},
	}

	results := gate.Process(candidates)
	// Phase 2: no auto-commit, all go to pending
	for _, r := range results {
		if r.Action != "pending" {
			t.Errorf("candidate %s: got %s, want pending (Phase 2 = no auto-commit)", r.CandidateID, r.Action)
		}
	}
}

func TestGate_AutoCommitPhase3(t *testing.T) {
	db := newGateTestDB(t)
	gate := NewGate(db, true) // Phase 3: auto-commit enabled

	candidates := []MemoryCandidate{
		{ID: "c1", Confidence: 0.95, Title: "high confidence", CreatedAt: time.Now()},
		{ID: "c2", Confidence: 0.7, Title: "medium", CreatedAt: time.Now()},
		{ID: "c3", Confidence: 0.3, Title: "low", CreatedAt: time.Now()},
	}

	results := gate.Process(candidates)
	expected := map[string]string{"c1": "auto_committed", "c2": "pending", "c3": "discarded"}
	for _, r := range results {
		want := expected[r.CandidateID]
		if r.Action != want {
			t.Errorf("candidate %s: got %s, want %s", r.CandidateID, r.Action, want)
		}
	}
}

func TestGate_ApproveReject(t *testing.T) {
	db := newGateTestDB(t)
	gate := NewGate(db, false)

	// Insert a pending candidate.
	gate.Process([]MemoryCandidate{
		{ID: "c1", Confidence: 0.8, Title: "test", CreatedAt: time.Now()},
	})

	// Approve it.
	if err := gate.ApproveCandidate("c1"); err != nil {
		t.Fatalf("ApproveCandidate: %v", err)
	}

	// Verify status changed.
	var status string
	db.QueryRow("SELECT status FROM memory_candidates WHERE id = 'c1'").Scan(&status)
	if status != "approved" {
		t.Errorf("status = %s, want approved", status)
	}

	// Approve again should fail (no longer pending).
	if err := gate.ApproveCandidate("c1"); err == nil {
		t.Error("expected error approving non-pending candidate")
	}

	// Test reject on a new candidate.
	gate.Process([]MemoryCandidate{
		{ID: "c2", Confidence: 0.6, Title: "reject me", CreatedAt: time.Now()},
	})
	if err := gate.RejectCandidate("c2"); err != nil {
		t.Fatalf("RejectCandidate: %v", err)
	}
	db.QueryRow("SELECT status FROM memory_candidates WHERE id = 'c2'").Scan(&status)
	if status != "rejected" {
		t.Errorf("status = %s, want rejected", status)
	}
}

func TestCandidate_ParseResponse(t *testing.T) {
	resp := `{
		"type": "observation",
		"title": "Tool X times out under load",
		"summary": "The search tool frequently times out when processing large result sets.",
		"facts": ["search tool has 30s timeout", "large queries exceed timeout"],
		"confidence": 0.8,
		"predicted_impact": "Prevents repeated timeout errors by pre-filtering queries"
	}`

	cluster := FailureCluster{
		ID:          "fc1",
		ClusterType: "tool_error",
		Description: "search tool timed out 3 times",
	}

	candidate, err := parseCandidateResponse(resp, "run1", cluster)
	if err != nil {
		t.Fatalf("parseCandidateResponse: %v", err)
	}
	if candidate.Title != "Tool X times out under load" {
		t.Errorf("title = %q", candidate.Title)
	}
	if candidate.Confidence != 0.8 {
		t.Errorf("confidence = %f, want 0.8", candidate.Confidence)
	}
	if candidate.CandidateType != "observation" {
		t.Errorf("type = %q, want observation", candidate.CandidateType)
	}
}

func TestCandidate_ParseResponse_WithFences(t *testing.T) {
	resp := "```json\n{\"type\":\"observation\",\"title\":\"test\",\"summary\":\"test summary\",\"facts\":[],\"confidence\":0.5,\"predicted_impact\":\"helps\"}\n```"

	candidate, err := parseCandidateResponse(resp, "run1", FailureCluster{ID: "fc1"})
	if err != nil {
		t.Fatalf("parseCandidateResponse with fences: %v", err)
	}
	if candidate.Title != "test" {
		t.Errorf("title = %q, want test", candidate.Title)
	}
}

func TestCandidate_ParseResponse_Invalid(t *testing.T) {
	_, err := parseCandidateResponse("not json", "run1", FailureCluster{ID: "fc1"})
	if err == nil {
		t.Error("expected error for invalid JSON")
	}

	_, err = parseCandidateResponse(`{"type":"observation","title":"","summary":"","confidence":0.5}`, "run1", FailureCluster{ID: "fc1"})
	if err == nil {
		t.Error("expected error for empty title")
	}
}
