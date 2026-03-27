package session

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/jialuohu/curlycatclaw/config"
	"github.com/jialuohu/curlycatclaw/internal/claude"
	"github.com/jialuohu/curlycatclaw/internal/mcp"
	"github.com/jialuohu/curlycatclaw/internal/memory"
	"github.com/jialuohu/curlycatclaw/internal/telegram"
	"github.com/jialuohu/curlycatclaw/skills"
)

const (
	maxToolRounds  = 10
	claudeTimeout  = 120 * time.Second
	mcpToolTimeout = 30 * time.Second
)

// Actor is the central session actor. It wires together Telegram messages,
// Claude API calls, MCP tool execution, and conversation memory.
type Actor struct {
	cfg    *config.Config
	claude *claude.Client
	tg     *telegram.Channel
	mcp    *mcp.Manager
	store  *memory.Store
	ctxb   *memory.ContextBuilder
	skills *skills.Registry
}

// New creates a new session actor.
func New(
	cfg *config.Config,
	claudeClient *claude.Client,
	tg *telegram.Channel,
	mcpMgr *mcp.Manager,
	store *memory.Store,
	skillReg *skills.Registry,
) *Actor {
	return &Actor{
		cfg:    cfg,
		claude: claudeClient,
		tg:     tg,
		mcp:    mcpMgr,
		store:  store,
		ctxb:   memory.NewContextBuilder(store),
		skills: skillReg,
	}
}

func (a *Actor) Name() string { return "session" }

// Run starts the session actor's event loop.
func (a *Actor) Run(ctx context.Context) error {
	slog.Info("session actor started")

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msg, ok := <-a.tg.Updates():
			if !ok {
				slog.Info("telegram updates channel closed, stopping session")
				return nil
			}
			if err := a.handleMessage(ctx, msg); err != nil {
				slog.Error("failed to handle message",
					"user_id", msg.UserID,
					"err", err,
				)
				a.trySend(telegram.OutgoingMessage{
					ChatID: msg.ChatID,
					Text:   "Sorry, something went wrong. Please try again.",
				})
			}
		}
	}
}

func (a *Actor) handleMessage(ctx context.Context, msg telegram.IncomingMessage) error {
	// Get or create conversation for this user.
	convID, err := a.store.GetActiveConversation(msg.UserID, msg.ChatID)
	if err != nil {
		return fmt.Errorf("get conversation: %w", err)
	}

	// Store the user message.
	userContent, _ := json.Marshal(msg.Text)
	if err := a.store.AppendMessage(convID, "user", userContent); err != nil {
		return fmt.Errorf("store user message: %w", err)
	}

	// Build context from conversation history.
	history, err := a.ctxb.BuildContext(convID)
	if err != nil {
		return fmt.Errorf("build context: %w", err)
	}

	// Convert memory messages to Anthropic SDK messages.
	messages := toAnthropicMessages(history)

	// Build system prompt with timezone.
	systemPrompt := a.buildSystemPrompt()

	// Collect all tools: MCP tools + built-in skills.
	tools := toAnthropicTools(a.mcp.Tools())
	tools = append(tools, toSkillTools(a.skills)...)

	// Run the tool_use loop.
	return a.toolUseLoop(ctx, msg.ChatID, convID, messages, systemPrompt, tools)
}

func (a *Actor) toolUseLoop(
	ctx context.Context,
	chatID int64,
	convID string,
	messages []anthropic.MessageParam,
	systemPrompt string,
	tools []anthropic.ToolUnionParam,
) error {
	for i := 0; i < maxToolRounds; i++ {
		claudeCtx, claudeCancel := context.WithTimeout(ctx, claudeTimeout)
		resp, err := a.claude.SendStreaming(claudeCtx, claude.SendParams{
			Messages:     messages,
			SystemPrompt: systemPrompt,
			Tools:        tools,
		})
		claudeCancel()
		if err != nil {
			return fmt.Errorf("claude send: %w", err)
		}

		// Store assistant response.
		assistantContent, _ := json.Marshal(resp)
		if err := a.store.AppendMessage(convID, "assistant", assistantContent); err != nil {
			slog.Error("failed to store assistant message", "err", err)
		}

		// If no tool calls, send the text response and we're done.
		if len(resp.ToolCalls) == 0 {
			if resp.TextContent != "" {
				a.trySend(telegram.OutgoingMessage{
					ChatID: chatID,
					Text:   resp.TextContent,
				})
			}
			return nil
		}

		// Build the assistant content blocks for the conversation continuation.
		var assistantBlocks []anthropic.ContentBlockParamUnion
		if resp.TextContent != "" {
			assistantBlocks = append(assistantBlocks, anthropic.NewTextBlock(resp.TextContent))
		}
		for _, call := range resp.ToolCalls {
			assistantBlocks = append(assistantBlocks, anthropic.NewToolUseBlock(call.ID, call.Input, call.Name))
		}

		// Execute tool calls and collect results.
		var toolResultBlocks []anthropic.ContentBlockParamUnion
		for _, call := range resp.ToolCalls {
			// Log tool call before execution.
			if err := a.store.LogToolCall(convID, call.ID, call.Name, call.Input); err != nil {
				slog.Error("failed to log tool call", "err", err)
			}

			// Try built-in skill first, then fall back to MCP.
			var result string
			var execErr error
			if skill := a.skills.Get(call.Name); skill != nil {
				mcpCtx, mcpCancel := context.WithTimeout(ctx, mcpToolTimeout)
				result, execErr = skill.Execute(mcpCtx, call.Input)
				mcpCancel()
			} else {
				var args map[string]any
				if err := json.Unmarshal(call.Input, &args); err != nil {
					args = map[string]any{"raw": string(call.Input)}
				}
				mcpCtx, mcpCancel := context.WithTimeout(ctx, mcpToolTimeout)
				result, execErr = a.mcp.CallTool(mcpCtx, call.Name, args)
				mcpCancel()
			}

			// Log tool result.
			resultJSON, _ := json.Marshal(result)
			if err := a.store.CompleteToolCall(call.ID, resultJSON, execErr != nil); err != nil {
				slog.Error("failed to complete tool call log", "err", err)
			}

			if execErr != nil {
				toolResultBlocks = append(toolResultBlocks,
					anthropic.NewToolResultBlock(call.ID, execErr.Error(), true),
				)
			} else {
				toolResultBlocks = append(toolResultBlocks,
					anthropic.NewToolResultBlock(call.ID, result, false),
				)
			}
		}

		// Store tool results to DB so conversation replay includes them.
		toolResultContent, _ := json.Marshal(toolResultBlocks)
		if err := a.store.AppendMessage(convID, "tool_result", toolResultContent); err != nil {
			slog.Error("failed to store tool result message", "err", err)
		}

		// Append assistant message + tool results and loop.
		messages = append(messages,
			anthropic.NewAssistantMessage(assistantBlocks...),
			anthropic.NewUserMessage(toolResultBlocks...),
		)
	}

	// Hit the loop limit.
	a.trySend(telegram.OutgoingMessage{
		ChatID: chatID,
		Text:   "I seem to be stuck in a tool-use loop. Please try rephrasing your request.",
	})
	return nil
}

func (a *Actor) trySend(msg telegram.OutgoingMessage) {
	select {
	case a.tg.Inbox() <- msg:
	default:
		slog.Warn("telegram inbox full, dropping message", "chat_id", msg.ChatID)
	}
}

func (a *Actor) buildSystemPrompt() string {
	loc := a.cfg.Location()
	now := time.Now().In(loc)
	return fmt.Sprintf(`You are a helpful personal assistant.

The user's timezone is %s. Current local time: %s.
Always use this timezone for scheduling, time references, and "today/tomorrow/yesterday."
When the user says "3pm" they mean 3pm in their timezone, not UTC.`,
		a.cfg.Timezone, now.Format("2006-01-02 15:04 MST"))
}

// toAnthropicMessages converts memory Messages to Anthropic SDK MessageParam.
func toAnthropicMessages(msgs []memory.Message) []anthropic.MessageParam {
	var result []anthropic.MessageParam
	for _, m := range msgs {
		// Attempt to unmarshal content as plain string (user messages).
		var text string
		if err := json.Unmarshal(m.Content, &text); err == nil {
			switch m.Role {
			case "user":
				result = append(result, anthropic.NewUserMessage(anthropic.NewTextBlock(text)))
			case "assistant":
				result = append(result, anthropic.NewAssistantMessage(anthropic.NewTextBlock(text)))
			}
			continue
		}

		// For complex content (stored Response objects), extract text.
		var resp claude.Response
		if err := json.Unmarshal(m.Content, &resp); err == nil && resp.TextContent != "" {
			switch m.Role {
			case "assistant":
				var blocks []anthropic.ContentBlockParamUnion
				blocks = append(blocks, anthropic.NewTextBlock(resp.TextContent))
				for _, tc := range resp.ToolCalls {
					blocks = append(blocks, anthropic.NewToolUseBlock(tc.ID, tc.Input, tc.Name))
				}
				result = append(result, anthropic.NewAssistantMessage(blocks...))
			default:
				result = append(result, anthropic.NewUserMessage(anthropic.NewTextBlock(resp.TextContent)))
			}
			continue
		}

		// Fallback: use raw content as text.
		result = append(result, anthropic.MessageParam{
			Role: anthropic.MessageParamRole(m.Role),
			Content: []anthropic.ContentBlockParamUnion{
				anthropic.NewTextBlock(string(m.Content)),
			},
		})
	}
	return result
}

// toAnthropicTools converts MCP ToolDefs to Anthropic SDK tool params.
func toAnthropicTools(tools []mcp.ToolDef) []anthropic.ToolUnionParam {
	result := make([]anthropic.ToolUnionParam, 0, len(tools))
	for _, t := range tools {
		// Parse the JSON schema from MCP into the SDK's ToolInputSchemaParam.
		var schema anthropic.ToolInputSchemaParam
		if err := json.Unmarshal(t.InputSchema, &schema); err != nil {
			slog.Warn("mcp: failed to parse tool input schema, using empty",
				"tool", t.Name, "err", err)
			schema = anthropic.ToolInputSchemaParam{}
		}

		result = append(result, anthropic.ToolUnionParam{
			OfTool: &anthropic.ToolParam{
				Name:        t.Name,
				Description: anthropic.String(t.Description),
				InputSchema: schema,
			},
		})
	}
	return result
}

// toSkillTools converts built-in skills to Anthropic SDK tool params.
func toSkillTools(reg *skills.Registry) []anthropic.ToolUnionParam {
	all := reg.All()
	result := make([]anthropic.ToolUnionParam, 0, len(all))
	for _, s := range all {
		var schema anthropic.ToolInputSchemaParam
		if err := json.Unmarshal(s.InputSchema, &schema); err != nil {
			schema = anthropic.ToolInputSchemaParam{}
		}
		result = append(result, anthropic.ToolUnionParam{
			OfTool: &anthropic.ToolParam{
				Name:        s.Name,
				Description: anthropic.String(s.Description),
				InputSchema: schema,
			},
		})
	}
	return result
}
