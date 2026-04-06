package eval

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// Miner detects failure patterns from scored conversations.
type Miner struct {
	db *sql.DB
}

// NewMiner creates a Miner with a database connection.
func NewMiner(db *sql.DB) *Miner {
	return &Miner{db: db}
}

// MineFailures analyzes scored conversations from an eval run and clusters
// them into failure patterns. A conversation is a failure candidate if its
// overall score is below the threshold OR any single signal is extreme.
func (m *Miner) MineFailures(runID string, scores []EvalScore, threshold float64) ([]FailureCluster, error) {
	var clusters []FailureCluster

	for _, score := range scores {
		if score.OverallScore >= threshold {
			continue
		}

		// Find what caused the low score by querying signals.
		toolClusters, err := m.mineToolErrors(runID, score.ConversationID)
		if err != nil {
			return nil, fmt.Errorf("eval: mine tool errors for %s: %w", score.ConversationID, err)
		}
		clusters = append(clusters, toolClusters...)

		// If corrections were detected, create a correction cluster.
		if score.CorrectionCount >= 2 {
			clusters = append(clusters, FailureCluster{
				ID:              newID(),
				EvalRunID:       runID,
				ClusterType:     "correction",
				Description:     fmt.Sprintf("%d corrections detected in conversation", score.CorrectionCount),
				ConversationIDs: []string{score.ConversationID},
				Severity:        clamp(score.CorrectionCount*2, 1, 10),
				Frequency:       score.CorrectionCount,
				CreatedAt:       time.Now().UTC(),
			})
		}

		// If retries were detected, create a retry cluster.
		if score.RetryCount >= 2 {
			clusters = append(clusters, FailureCluster{
				ID:              newID(),
				EvalRunID:       runID,
				ClusterType:     "user_retry",
				Description:     fmt.Sprintf("%d retries in conversation", score.RetryCount),
				ConversationIDs: []string{score.ConversationID},
				Severity:        clamp(score.RetryCount*2, 1, 10),
				Frequency:       score.RetryCount,
				CreatedAt:       time.Now().UTC(),
			})
		}

		// If effort overrides were used, create an effort cluster.
		if score.EffortOverrideCount >= 1 {
			clusters = append(clusters, FailureCluster{
				ID:              newID(),
				EvalRunID:       runID,
				ClusterType:     "effort_override",
				Description:     fmt.Sprintf("%d effort overrides in conversation (user needed deeper thinking)", score.EffortOverrideCount),
				ConversationIDs: []string{score.ConversationID},
				Severity:        clamp(score.EffortOverrideCount*3, 1, 10),
				Frequency:       score.EffortOverrideCount,
				CreatedAt:       time.Now().UTC(),
			})
		}
	}

	return clusters, nil
}

// mineToolErrors groups tool call errors by (tool_name, error_substring) for a conversation.
func (m *Miner) mineToolErrors(runID, convID string) ([]FailureCluster, error) {
	rows, err := m.db.Query(
		`SELECT name, output, id FROM tool_calls
		 WHERE conversation_id = ? AND is_error = TRUE
		 ORDER BY name`,
		convID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type toolErr struct {
		name      string
		errorText string
		callIDs   []string
	}

	// Group by tool name.
	groups := make(map[string]*toolErr)
	for rows.Next() {
		var name, output, callID string
		if err := rows.Scan(&name, &output, &callID); err != nil {
			continue
		}
		key := name
		if g, ok := groups[key]; ok {
			g.callIDs = append(g.callIDs, callID)
		} else {
			// Truncate error output for the description.
			errText := output
			if len(errText) > 200 {
				errText = errText[:200] + "..."
			}
			groups[key] = &toolErr{name: name, errorText: errText, callIDs: []string{callID}}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var clusters []FailureCluster
	for _, g := range groups {
		callIDsJSON, _ := json.Marshal(g.callIDs)
		clusters = append(clusters, FailureCluster{
			ID:              newID(),
			EvalRunID:       runID,
			ClusterType:     "tool_error",
			Description:     fmt.Sprintf("Tool %q failed %d time(s): %s", g.name, len(g.callIDs), g.errorText),
			ConversationIDs: []string{convID},
			ToolCallIDs:     g.callIDs,
			Severity:        clamp(len(g.callIDs)*3, 1, 10),
			Frequency:       len(g.callIDs),
			CreatedAt:       time.Now().UTC(),
		})
		_ = callIDsJSON // used for persistence in actor
	}

	return clusters, nil
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func newID() string {
	b := make([]byte, 16)
	rand.Read(b) //nolint:errcheck
	return hex.EncodeToString(b)
}
