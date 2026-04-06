package eval

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// correctionPrefixes are phrases that indicate a user is correcting the bot.
// Matched against the first 80 characters of user messages following assistant messages.
var correctionPrefixes = []string{
	"no,", "wrong", "that's not", "not what i", "i meant",
	"that's incorrect", "try again", "you got", "actually,",
}

// correctionExclusions are patterns that cancel a correction match (false positives).
var correctionExclusions = []string{
	"no problem", "no worries", "actually, good", "actually, right",
	"actually, never mind", "no that's fine", "no that looks",
}

// Scorer extracts deterministic quality signals from conversations stored in SQLite.
// It does not use LLM calls — all signals come from database queries and heuristics.
type Scorer struct {
	db *sql.DB
}

// NewScorer creates a Scorer with a read-only database connection.
func NewScorer(db *sql.DB) *Scorer {
	return &Scorer{db: db}
}

// ScoreConversation extracts signals for a single conversation and returns
// the raw signals along with the composite score.
func (s *Scorer) ScoreConversation(convID string) (*ConversationSignals, error) {
	sig := &ConversationSignals{ConversationID: convID}

	// Get conversation metadata.
	err := s.db.QueryRow(
		`SELECT user_id, chat_id FROM conversations WHERE id = ?`, convID,
	).Scan(&sig.UserID, &sig.ChatID)
	if err != nil {
		return nil, fmt.Errorf("eval: get conversation: %w", err)
	}

	// Count total messages and compute duration.
	var msgCount int
	var firstAt, lastAt sql.NullString
	err = s.db.QueryRow(
		`SELECT COUNT(*), MIN(created_at), MAX(created_at) FROM messages WHERE conversation_id = ?`,
		convID,
	).Scan(&msgCount, &firstAt, &lastAt)
	if err != nil {
		return nil, fmt.Errorf("eval: count messages: %w", err)
	}
	sig.MessageCount = msgCount

	if firstAt.Valid && lastAt.Valid {
		if ft, err := time.Parse("2006-01-02 15:04:05", firstAt.String); err == nil {
			if lt, err := time.Parse("2006-01-02 15:04:05", lastAt.String); err == nil {
				sig.Duration = lt.Sub(ft)
			}
		}
	}

	// Tool call success/failure counts.
	err = s.db.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(CASE WHEN is_error THEN 1 ELSE 0 END), 0)
		 FROM tool_calls WHERE conversation_id = ?`, convID,
	).Scan(&sig.TotalToolCalls, &sig.FailedToolCalls)
	if err != nil {
		return nil, fmt.Errorf("eval: count tool calls: %w", err)
	}

	// Count interaction events (retries, effort overrides).
	// Events may have empty conversation_id (logged before conversation lookup),
	// so also match by (user_id, chat_id) within the conversation's time window.
	rows, err := s.db.Query(
		`SELECT event_type FROM interaction_events
		 WHERE conversation_id = ?
		    OR (conversation_id = '' AND user_id = ? AND chat_id = ?
		        AND created_at BETWEEN ? AND ?)`,
		convID, sig.UserID, sig.ChatID,
		firstAt.String, lastAt.String,
	)
	if err != nil {
		return nil, fmt.Errorf("eval: query interaction events: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var eventType string
		if err := rows.Scan(&eventType); err != nil {
			continue
		}
		switch eventType {
		case "retry":
			sig.RetryCount++
		case "effort_override":
			sig.EffortOverrides++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("eval: scan interaction events: %w", err)
	}

	// Detect corrections using keyword heuristics on message pairs.
	sig.CorrectionCount, err = s.countCorrections(convID)
	if err != nil {
		return nil, fmt.Errorf("eval: count corrections: %w", err)
	}

	return sig, nil
}

// countCorrections counts user messages that appear to be correcting the bot.
// A correction is detected when a user message immediately follows an assistant
// message and starts with a negation/correction phrase within the first 80 chars.
func (s *Scorer) countCorrections(convID string) (int, error) {
	rows, err := s.db.Query(
		`SELECT role, content FROM messages WHERE conversation_id = ? ORDER BY id ASC`,
		convID,
	)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var corrections int
	var prevRole string

	for rows.Next() {
		var role, content string
		if err := rows.Scan(&role, &content); err != nil {
			continue
		}

		if role == "user" && prevRole == "assistant" {
			if isCorrection(content) {
				corrections++
			}
		}
		prevRole = role
	}
	return corrections, rows.Err()
}

// isCorrection checks if a message text looks like a user correction.
func isCorrection(text string) bool {
	lower := strings.ToLower(text)

	// Only check the first 80 chars.
	check := lower
	if len(check) > 80 {
		check = check[:80]
	}

	// Check exclusions first.
	for _, excl := range correctionExclusions {
		if strings.Contains(check, excl) {
			return false
		}
	}

	// Check if any correction prefix appears.
	for _, prefix := range correctionPrefixes {
		if strings.Contains(check, prefix) {
			return true
		}
	}

	return false
}
