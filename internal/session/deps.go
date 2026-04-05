package session

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jialuohu/curlycatclaw/internal/claude"
	"github.com/jialuohu/curlycatclaw/internal/mcp"
	"github.com/jialuohu/curlycatclaw/internal/memory"
	"github.com/jialuohu/curlycatclaw/internal/telegram"
)

// LLMClient abstracts the Claude API for testing.
type LLMClient interface {
	SendStreaming(ctx context.Context, params claude.SendParams) (*claude.Response, error)
}

// MessageStore abstracts storage operations used by the session actor.
type MessageStore interface {
	GetActiveConversation(userID, chatID int64, chatType string) (convID string, expiredConvID string, err error)
	AppendMessage(convID, role string, content json.RawMessage) error
	LogToolCall(convID, callID, name string, input json.RawMessage) error
	CompleteToolCall(callID string, output json.RawMessage, isError bool) error
	GetConversationMessages(convID string) ([]memory.Message, error)
	SaveSummary(convID string, userID, chatID int64, summary string, msgCount int, firstAt, lastAt time.Time) error
	SetSummarizationStatus(convID string, status string) error
	ConversationMeta(convID string) (userID, chatID int64, chatType string, msgCount int, firstAt, lastAt time.Time, err error)
	RecoverableSummarizations() ([]string, error)
	GetSummaryText(convID string) (string, error)
	GetMaxMessageRowid(convID string) (int64, error)
}

// FactProvider abstracts user fact retrieval for the session actor.
type FactProvider interface {
	GetFacts(userID int64) ([]memory.Fact, error)
	UpdateLastReferenced(factIDs []int64) error
}

// Summarizer abstracts conversation summarization.
type Summarizer interface {
	Summarize(ctx context.Context, messages []memory.Message) (string, error)
}

// ContextProvider abstracts conversation context building.
type ContextProvider interface {
	BuildContext(convID string) ([]memory.Message, error)
}

// ToolRouter abstracts MCP tool discovery and invocation.
type ToolRouter interface {
	CallTool(ctx context.Context, name string, args map[string]any, userID, chatID int64) (string, error)
	Tools() []mcp.ToolDef
}

// VectorIndexer abstracts vector store indexing for async message indexing.
type VectorIndexer interface {
	Index(ctx context.Context, id, text string, userID, chatID int64, source string) error
}

// TelegramTransport abstracts the Telegram channel for sending and receiving.
type TelegramTransport interface {
	Inbox() chan<- telegram.OutgoingMessage
	Updates() <-chan telegram.IncomingMessage
	SendTyping(chatID int64)
	SendDocument(chatID int64, fileName string, data []byte, caption string) error
}

// ObservationStore abstracts observation CRUD for the session actor.
type ObservationStore interface {
	SaveObservation(obs *memory.Observation) error
	GetRecentObservationTitles(convID string, limit int) ([]string, error)
	GetExtractionState(convID string) (*memory.ExtractionState, error)
	UpdateExtractionState(convID string, lastRowid int64, turnCount int, status string) error
	IncrementExtractionTurnCount(convID string) error
	ObservationExistsByHash(userID int64, hash string) (bool, error)
	DeleteObservation(id string, userID int64) error
	CountObservations(convID string) (int, error)
	GetObservationFactsByIDs(ids []string) (map[string][]string, error)
	RecoverableExtractions() ([]string, error)
	SaveEntities(obsID string, entities []memory.Entity) error
	DeleteEntitiesByObservation(obsID string) error
	SearchObservationsFTS(query string, userID int64, limit int) ([]memory.FTSResult, error)
	ObservationTextsAfter(afterID int64, limit int) ([]memory.MigrationText, int64, error)
	AllObservations(limit int) ([]memory.Observation, error)
}

// CLIClient abstracts the CLI subprocess manager for testing.
type CLIClient interface {
	GetOrCreate(ctx context.Context, userID, chatID int64, params claude.SpawnParams) (proc *claude.CLIProcess, isNew bool, err error)
	Remove(userID, chatID int64)
	Cleanup(maxIdle time.Duration)
	Shutdown(timeout time.Duration)
}
