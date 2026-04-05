package session

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/jialuohu/curlycatclaw/config"
	"github.com/jialuohu/curlycatclaw/internal/claude"
	"github.com/jialuohu/curlycatclaw/skills"
)

// CronExecutor runs Claude with a clean context for scheduled tasks.
// It implements skills.CronRunner.
type CronExecutor struct {
	cfg      *config.Config
	claude   LLMClient
	cliMgr   *claude.CLIManager
	mcp      ToolRouter
	skills   *skills.Registry
	facts    FactProvider
	sem      chan struct{} // bounds concurrent cron executions
}

// NewCronExecutor creates a CronExecutor. cliMgr may be nil if not using CLI mode.
func NewCronExecutor(
	cfg *config.Config,
	claudeClient LLMClient,
	cliMgr *claude.CLIManager,
	mcpMgr ToolRouter,
	skillReg *skills.Registry,
	factStore FactProvider,
) *CronExecutor {
	return &CronExecutor{
		cfg:    cfg,
		claude: claudeClient,
		cliMgr: cliMgr,
		mcp:    mcpMgr,
		skills: skillReg,
		facts:  factStore,
		sem:    make(chan struct{}, 3),
	}
}

// Execute runs a prompt through Claude with a clean context (facts only, no
// conversation history) and returns the text result. It supports tool use.
func (ce *CronExecutor) Execute(ctx context.Context, userID, chatID int64, prompt, model string) (string, error) {
	// Acquire concurrency slot.
	select {
	case ce.sem <- struct{}{}:
		defer func() { <-ce.sem }()
	case <-ctx.Done():
		return "", fmt.Errorf("cron: context cancelled waiting for semaphore: %w", ctx.Err())
	}

	slog.Info("cron: executing", "user_id", userID, "chat_id", chatID)

	if ce.cfg.Claude.UseCLI() && ce.cliMgr != nil {
		return ce.executeWithCLI(ctx, userID, chatID, prompt, model)
	}

	return ce.executeWithAPI(ctx, userID, chatID, prompt)
}

// executeWithAPI runs the prompt through the direct Claude API with tool support.
func (ce *CronExecutor) executeWithAPI(ctx context.Context, userID, chatID int64, prompt string) (string, error) {
	systemPrompt := ce.buildSystemPrompt(userID)

	// Collect tools from skills + MCP.
	tools := toSkillTools(ce.skills)
	tools = append(tools, toAnthropicTools(ce.mcp.Tools())...)

	messages := []anthropic.MessageParam{
		anthropic.NewUserMessage(anthropic.NewTextBlock(prompt)),
	}

	return ce.runToolLoop(ctx, userID, chatID, messages, systemPrompt, tools)
}

// runToolLoop executes the Claude tool-use loop without streaming or DB writes.
func (ce *CronExecutor) runToolLoop(
	ctx context.Context,
	userID, chatID int64,
	messages []anthropic.MessageParam,
	systemPrompt string,
	tools []anthropic.ToolUnionParam,
) (string, error) {
	for i := 0; i < maxToolRounds; i++ {
		claudeCtx, claudeCancel := context.WithTimeout(ctx, claudeTimeout)
		resp, err := ce.claude.SendStreaming(claudeCtx, claude.SendParams{
			Messages:     messages,
			SystemPrompt: systemPrompt,
			Tools:        tools,
			// No OnPartialText — non-streaming for cron tasks.
		})
		claudeCancel()

		if err != nil {
			// Retry once on rate limit.
			if i == 0 && isRateLimitError(err) {
				slog.Warn("cron: rate limited, retrying in 30s", "err", err)
				select {
				case <-time.After(30 * time.Second):
					continue
				case <-ctx.Done():
					return "", ctx.Err()
				}
			}
			return "", fmt.Errorf("cron: claude: %w", err)
		}

		if len(resp.ToolCalls) == 0 {
			return resp.TextContent, nil
		}

		// Build assistant content blocks.
		assistantBlocks := make([]anthropic.ContentBlockParamUnion, 0, 1+len(resp.ToolCalls))
		if resp.TextContent != "" {
			assistantBlocks = append(assistantBlocks, anthropic.NewTextBlock(resp.TextContent))
		}
		for _, call := range resp.ToolCalls {
			assistantBlocks = append(assistantBlocks, anthropic.NewToolUseBlock(call.ID, call.Input, call.Name))
		}

		// Execute tool calls.
		toolResultBlocks := make([]anthropic.ContentBlockParamUnion, 0, len(resp.ToolCalls))
		for _, call := range resp.ToolCalls {
			var result string
			var execErr error
			func() {
				mcpCtx, mcpCancel := context.WithTimeout(ctx, mcpToolTimeout)
				defer mcpCancel()

				// Inject user context for user-scoped skills.
				skillCtx := skills.WithUser(mcpCtx, skills.UserInfo{UserID: userID, ChatID: chatID})

				if skill := ce.skills.Get(call.Name); skill != nil {
					result, execErr = skill.Execute(skillCtx, call.Input)
				} else {
					var args map[string]any
					if jsonErr := json.Unmarshal(call.Input, &args); jsonErr != nil {
						args = map[string]any{"raw": string(call.Input)}
					}
					result, execErr = ce.mcp.CallTool(skillCtx, call.Name, args, userID, chatID)
				}
			}()

			// Feed errors back to Claude as is_error tool results (not fatal).
			if execErr != nil {
				slog.Warn("cron: tool error", "tool", call.Name, "err", execErr)
				toolResultBlocks = append(toolResultBlocks,
					anthropic.NewToolResultBlock(call.ID, execErr.Error(), true),
				)
			} else {
				toolResultBlocks = append(toolResultBlocks,
					anthropic.NewToolResultBlock(call.ID, result, false),
				)
			}
		}

		messages = append(messages,
			anthropic.NewAssistantMessage(assistantBlocks...),
			anthropic.NewUserMessage(toolResultBlocks...),
		)
	}

	return "", fmt.Errorf("cron: tool loop exceeded %d rounds", maxToolRounds)
}

// executeWithCLI runs the prompt through a one-shot CLI subprocess.
func (ce *CronExecutor) executeWithCLI(ctx context.Context, userID, chatID int64, prompt, model string) (string, error) {
	proc, err := ce.cliMgr.SpawnOneShot(ctx, claude.SpawnParams{
		SystemPrompt: ce.buildSystemPrompt(userID),
		InitialMsg:   claude.BuildUserMessage(prompt),
		Model:        model,
	})
	if err != nil {
		return "", fmt.Errorf("cron: spawn CLI: %w", err)
	}
	defer proc.Kill()

	// Send collects all events until a ResultEvent.
	// The initial message was already sent during spawn.
	var text strings.Builder
	events, err := proc.Send(ctx, nil, func(delta string) {
		text.WriteString(delta)
	}, nil)
	if err != nil {
		return "", fmt.Errorf("cron: CLI send: %w", err)
	}

	// Check for result event errors.
	for _, ev := range events {
		if res, ok := ev.(claude.ResultEvent); ok {
			if res.IsError {
				errMsg := strings.Join(res.Errors, "; ")
				if errMsg == "" {
					errMsg = "unknown CLI error"
				}
				return "", fmt.Errorf("cron: CLI error: %s", errMsg)
			}
			if text.Len() == 0 {
				text.WriteString(res.Result)
			}
		}
	}

	if text.Len() == 0 {
		return "", fmt.Errorf("cron: CLI returned empty response")
	}
	return text.String(), nil
}

// buildSystemPrompt creates a minimal system prompt for cron tasks.
// Includes timezone and user facts, but no conversation summaries or memory instructions.
func (ce *CronExecutor) buildSystemPrompt(userID int64) string {
	loc := ce.cfg.Location()
	now := time.Now().In(loc)

	var sb strings.Builder
	sb.WriteString("You are executing a scheduled task. Be concise and actionable.\n\n")
	fmt.Fprintf(&sb, "The user's timezone is %s. Current local time: %s.\n", ce.cfg.Timezone, now.Format("2006-01-02 15:04 MST"))

	// Include user facts (Tier 1) so Claude knows about the user.
	if ce.cfg.Memory.Enabled && ce.facts != nil {
		facts, err := ce.facts.GetFacts(userID)
		if err != nil {
			slog.Warn("cron: buildSystemPrompt: get facts", "err", err)
		} else if len(facts) > 0 {
			sb.WriteString("\n## What I know about you\n")
			sb.WriteString("<user_facts>\n")
			for _, f := range facts {
				fmt.Fprintf(&sb, "[id=%d] %s (%s)\n", f.ID, f.Fact, f.Category)
			}
			sb.WriteString("</user_facts>\n")
		}
	}

	return sb.String()
}

// isRateLimitError checks if the error is a rate limit (429) error.
func isRateLimitError(err error) bool {
	return strings.Contains(err.Error(), "429") || strings.Contains(err.Error(), "rate")
}
