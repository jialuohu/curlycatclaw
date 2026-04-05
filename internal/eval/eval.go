// Package eval implements background self-evaluation for curlycatclaw conversations.
// It scores conversations using deterministic signals, mines failure patterns,
// and proposes memory candidates for improvement.
package eval

import "time"

// EvalRun represents a single execution of the evaluation pipeline.
type EvalRun struct {
	ID                   string
	StartedAt            time.Time
	CompletedAt          time.Time
	ConversationsScanned int
	FailuresFound        int
	CandidatesGenerated  int
	CandidatesCommitted  int
	Status               string // "running", "completed", "failed"
	Summary              string // JSON: scores, patterns, insights
}

// FailureCluster represents a group of related failure signals detected in conversations.
type FailureCluster struct {
	ID              string
	EvalRunID       string
	ClusterType     string // "tool_error", "user_retry", "correction", "effort_override", "silent_wrongness"
	Description     string
	ConversationIDs []string
	MessageRowIDs   []int64
	ToolCallIDs     []string
	Severity        int // 1-10
	Frequency       int
	CreatedAt       time.Time
}

// MemoryCandidate represents a proposed memory update from the eval pipeline.
type MemoryCandidate struct {
	ID                    string
	EvalRunID             string
	FailureClusterID      string
	CandidateType         string // "observation", "fact", "prompt_note"
	Title                 string
	Content               string // JSON: observation schema or fact text
	Evidence              string // JSON: citations to messages/tool_calls
	Confidence            float64
	PredictedImpact       string
	Status                string // "pending", "approved", "rejected", "auto_committed", "expired"
	ReplayScore           float64
	CommittedObservationID string
	UserID                int64
	ChatID                int64
	ReviewedAt            time.Time
	CreatedAt             time.Time
}

// EvalScore represents a per-conversation quality score.
type EvalScore struct {
	ID                  string
	ConversationID      string
	EvalRunID           string
	OverallScore        float64 // 0.0-1.0 composite
	ToolSuccessRate     float64
	CorrectionCount     int
	RetryCount          int
	EffortOverrideCount int
	LLMQualityScore     float64 // 0.0-1.0 (Phase 2+)
	Details             string  // JSON scoring breakdown
	CreatedAt           time.Time
}

// ConversationSignals holds deterministic signals extracted from a conversation
// without any LLM calls. These are the inputs to the scoring formula.
type ConversationSignals struct {
	ConversationID    string
	UserID            int64
	ChatID            int64
	TotalToolCalls    int
	FailedToolCalls   int     // is_error = true in tool_calls
	RetryCount        int     // consecutive same-tool calls with similar input, or /retry events
	CorrectionCount   int     // user messages containing correction patterns
	EffortOverrides   int     // /effort command usage
	SupersessionCount int     // observation_relations created
	MessageCount      int
	Duration          time.Duration
}

// Score computes the deterministic composite score (0.0-1.0) from raw signals.
// Formula: 0.35*tool + 0.30*correction + 0.20*retry + 0.15*effort
func (s ConversationSignals) Score() float64 {
	toolScore := 1.0
	if s.TotalToolCalls > 0 {
		toolScore = 1.0 - min(float64(s.FailedToolCalls)/float64(s.TotalToolCalls), 1.0)
	}
	correctionScore := 1.0 - min(float64(s.CorrectionCount)/5.0, 1.0)
	retryScore := 1.0 - min(float64(s.RetryCount)/5.0, 1.0)
	effortScore := 1.0 - min(float64(s.EffortOverrides)/3.0, 1.0)

	return 0.35*toolScore + 0.30*correctionScore + 0.20*retryScore + 0.15*effortScore
}

func min(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
