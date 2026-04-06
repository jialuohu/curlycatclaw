package eval

import (
	"database/sql"
	"fmt"
	"log/slog"
	"time"
)

const (
	// DiscardThreshold: candidates below this are dropped silently.
	DiscardThreshold = 0.5
	// AutoCommitThreshold: Phase 3 — candidates above this auto-commit.
	// In Phase 2, all candidates >= DiscardThreshold go to human review.
	AutoCommitThreshold = 0.9
)

// Gate decides the fate of each memory candidate based on confidence.
type Gate struct {
	db        *sql.DB
	autoCommit bool // Phase 3 enables this
}

// NewGate creates a CommitGate. Set autoCommit=false for Phase 2 (human-only).
func NewGate(db *sql.DB, autoCommit bool) *Gate {
	return &Gate{db: db, autoCommit: autoCommit}
}

// GateResult indicates what happened to a candidate.
type GateResult struct {
	CandidateID string
	Action      string // "discarded", "pending", "auto_committed"
}

// Process evaluates a batch of candidates and persists their status.
func (g *Gate) Process(candidates []MemoryCandidate) []GateResult {
	var results []GateResult

	for _, c := range candidates {
		var action string

		if c.Confidence < DiscardThreshold {
			action = "discarded"
			c.Status = "rejected"
		} else if g.autoCommit && c.Confidence >= AutoCommitThreshold {
			action = "auto_committed"
			c.Status = "auto_committed"
		} else {
			action = "pending"
			c.Status = "pending"
		}

		if err := g.persistCandidate(c); err != nil {
			slog.Warn("eval: failed to persist candidate", "id", c.ID, "err", err)
			continue
		}

		results = append(results, GateResult{CandidateID: c.ID, Action: action})
		slog.Info("eval: candidate gated", "id", c.ID, "confidence", c.Confidence, "action", action)
	}

	return results
}

// ApproveCandidate marks a candidate as approved (called from Telegram callback).
func (g *Gate) ApproveCandidate(candidateID string) error {
	result, err := g.db.Exec(
		`UPDATE memory_candidates SET status = 'approved', reviewed_at = ? WHERE id = ? AND status = 'pending'`,
		time.Now().UTC(), candidateID,
	)
	if err != nil {
		return fmt.Errorf("eval: approve candidate: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("eval: candidate %s not found or not pending", candidateID)
	}
	return nil
}

// RejectCandidate marks a candidate as rejected (called from Telegram callback).
func (g *Gate) RejectCandidate(candidateID string) error {
	result, err := g.db.Exec(
		`UPDATE memory_candidates SET status = 'rejected', reviewed_at = ? WHERE id = ? AND status = 'pending'`,
		time.Now().UTC(), candidateID,
	)
	if err != nil {
		return fmt.Errorf("eval: reject candidate: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("eval: candidate %s not found or not pending", candidateID)
	}
	return nil
}

// GetPendingCandidates returns all candidates awaiting human review.
func (g *Gate) GetPendingCandidates() ([]MemoryCandidate, error) {
	rows, err := g.db.Query(
		`SELECT id, eval_run_id, failure_cluster_id, candidate_type, title, content, evidence, confidence, predicted_impact, status, user_id, chat_id, created_at
		 FROM memory_candidates WHERE status = 'pending' ORDER BY confidence DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("eval: get pending candidates: %w", err)
	}
	defer rows.Close()

	var candidates []MemoryCandidate
	for rows.Next() {
		var c MemoryCandidate
		var ts string
		if err := rows.Scan(&c.ID, &c.EvalRunID, &c.FailureClusterID, &c.CandidateType,
			&c.Title, &c.Content, &c.Evidence, &c.Confidence, &c.PredictedImpact,
			&c.Status, &c.UserID, &c.ChatID, &ts); err != nil {
			continue
		}
		candidates = append(candidates, c)
	}
	return candidates, rows.Err()
}

func (g *Gate) persistCandidate(c MemoryCandidate) error {
	_, err := g.db.Exec(
		`INSERT INTO memory_candidates (id, eval_run_id, failure_cluster_id, candidate_type, title, content, evidence, confidence, predicted_impact, status, user_id, chat_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.EvalRunID, c.FailureClusterID, c.CandidateType, c.Title, c.Content,
		c.Evidence, c.Confidence, c.PredictedImpact, c.Status, c.UserID, c.ChatID, c.CreatedAt,
	)
	return err
}
