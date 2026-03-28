package session

import (
	"context"
	"encoding/json"

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
	GetActiveConversation(userID, chatID int64) (string, error)
	AppendMessage(convID, role string, content json.RawMessage) error
	LogToolCall(convID, callID, name string, input json.RawMessage) error
	CompleteToolCall(callID string, output json.RawMessage, isError bool) error
}

// ContextProvider abstracts conversation context building.
type ContextProvider interface {
	BuildContextWithBudget(ctx context.Context, convID, currentMsg string) ([]memory.Message, error)
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
}
