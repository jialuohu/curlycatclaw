package memory

import "time"

// Observation represents a structured memory extracted from a conversation.
// Observations fill the gap between point facts (200 chars) and coarse
// summaries (2-3 sentences per conversation).
type Observation struct {
	ID             string    // UUID
	ConversationID string    // FK to conversations
	UserID         int64
	ChatID         int64
	ChatType       string // private/group/supergroup
	Type           string // decision/preference/project_state/commitment/discovery/reference
	Title          string // 1-line summary (~100 chars)
	Summary        string // 1-2 sentence description
	Facts          []string
	Entities       []Entity
	Importance     int   // 1-10 salience score
	SourceMsgStart int64 // messages.rowid range start
	SourceMsgEnd   int64 // messages.rowid range end
	ContentHash    string
	CreatedAt      time.Time
}

// ObservationResult combines Qdrant search results with SQLite-hydrated facts.
type ObservationResult struct {
	ID         string
	Type       string
	Title      string
	Summary    string
	Facts      []string // hydrated from observation_facts table
	Importance int
	CreatedAt  string  // ISO 8601
	Score      float32 // combined relevance score
}

// ExtractionState tracks where each conversation's observation extraction left off.
type ExtractionState struct {
	ConversationID        string
	LastExtractedMsgRowid int64
	LastExtractionAt      *time.Time
	LastMsgAt             time.Time
	TurnCount             int
	Status                string // idle/pending/failed
}

// Entity represents a named entity extracted alongside an observation.
type Entity struct {
	Name string // canonicalized: lowercase, trimmed, collapsed spaces
	Type string // person/project/file/tool
}

// AllowedEntityTypes is the whitelist of valid entity types.
var AllowedEntityTypes = map[string]bool{
	"person":  true,
	"project": true,
	"file":    true,
	"tool":    true,
}

// AllowedObservationTypes is the whitelist of valid observation types.
var AllowedObservationTypes = map[string]bool{
	"decision":      true,
	"preference":    true,
	"project_state": true,
	"commitment":    true,
	"discovery":     true,
	"reference":     true,
}
