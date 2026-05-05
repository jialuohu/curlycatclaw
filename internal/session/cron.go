package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/jialuohu/curlycatclaw/config"
	"github.com/jialuohu/curlycatclaw/internal/claude"
	"github.com/jialuohu/curlycatclaw/internal/memory"
	"github.com/jialuohu/curlycatclaw/skills"
)

// CronExecutor runs Claude with a clean context for scheduled tasks.
// It implements skills.CronRunner.
type CronExecutor struct {
	cfg        *config.Config
	configPath string // path to config.toml, passed to MCP subprocesses spawned for cron runs
	db         *sql.DB // used by memory.EffectiveLocation for the timezone override; cron-fired prompts must render in the *runtime* effective TZ, not the startup one
	claude     LLMClient
	cliMgr     *claude.CLIManager
	mcp        ToolRouter
	skills     *skills.Registry
	facts      FactProvider
	sem        chan struct{} // bounds concurrent cron executions
}

// NewCronExecutor creates a CronExecutor. cliMgr may be nil if not using CLI mode.
// configPath is propagated into the --mcp-config JSON so the CLI subprocess's
// curlycatclaw-skills MCP server can load the same config as the interactive
// session (required for runtime MCP extensions like paper-search-mcp).
// db is used by memory.EffectiveLocation so the rendered "scheduled at" / "now"
// times in the cron system prompt reflect any runtime timezone override.
func NewCronExecutor(
	cfg *config.Config,
	configPath string,
	db *sql.DB,
	claudeClient LLMClient,
	cliMgr *claude.CLIManager,
	mcpMgr ToolRouter,
	skillReg *skills.Registry,
	factStore FactProvider,
) *CronExecutor {
	return &CronExecutor{
		cfg:        cfg,
		configPath: configPath,
		db:         db,
		claude:     claudeClient,
		cliMgr:     cliMgr,
		mcp:        mcpMgr,
		skills:     skillReg,
		facts:      factStore,
		sem:        make(chan struct{}, 3),
	}
}

// Execute runs a prompt through Claude with a clean context (facts only, no
// conversation history) and returns the text result. It supports tool use.
// scheduledAt is the intended fire time (passed through to the system prompt
// so Claude references the scheduled time rather than the wall time at execution).
func (ce *CronExecutor) Execute(ctx context.Context, userID, chatID int64, prompt, model, effort string, scheduledAt time.Time) (string, error) {
	// Acquire concurrency slot.
	select {
	case ce.sem <- struct{}{}:
		defer func() { <-ce.sem }()
	case <-ctx.Done():
		return "", fmt.Errorf("cron: context cancelled waiting for semaphore: %w", ctx.Err())
	}

	// Effort resolution: per-reminder effort wins over config default in BOTH
	// modes. Empty effort falls back to the config default so API-mode cron
	// honors `thinking_effort = "xhigh"` like the interactive session does
	// (prior to Apr 17 fix, runToolLoop constructed SendParams without
	// ThinkingEffort, silently dropping both per-reminder AND config-default
	// effort — the new [effort: X] display in list_reminders would lie about
	// being applied in API mode).
	effectiveEffort := effort
	if effectiveEffort == "" {
		effectiveEffort = string(ce.cfg.Claude.ThinkingEffort)
	}
	slog.Info("cron: executing", "user_id", userID, "chat_id", chatID, "scheduled_at", scheduledAt, "effort", effectiveEffort, "effort_source", map[bool]string{true: "per-reminder", false: "config-default"}[effort != ""])

	if ce.cfg.Claude.UseCLI() && ce.cliMgr != nil {
		return ce.executeWithCLI(ctx, userID, chatID, prompt, model, effectiveEffort, scheduledAt)
	}
	return ce.executeWithAPI(ctx, userID, chatID, prompt, effectiveEffort, scheduledAt)
}

// executeWithAPI runs the prompt through the direct Claude API with tool support.
func (ce *CronExecutor) executeWithAPI(ctx context.Context, userID, chatID int64, prompt, effort string, scheduledAt time.Time) (string, error) {
	systemPrompt := ce.buildSystemPrompt(userID, scheduledAt)

	// Collect tools from skills + MCP.
	tools := toSkillTools(ce.skills)
	tools = append(tools, toAnthropicTools(ce.mcp.Tools())...)

	messages := []anthropic.MessageParam{
		anthropic.NewUserMessage(anthropic.NewTextBlock(prompt)),
	}

	return ce.runToolLoop(ctx, userID, chatID, messages, systemPrompt, tools, effort)
}

// runToolLoop executes the Claude tool-use loop without streaming or DB writes.
// effort is a config.Effort string (empty = no extended thinking); applyThinking
// in the claude package maps high/xhigh/max to budget_tokens presets.
func (ce *CronExecutor) runToolLoop(
	ctx context.Context,
	userID, chatID int64,
	messages []anthropic.MessageParam,
	systemPrompt string,
	tools []anthropic.ToolUnionParam,
	effort string,
) (string, error) {
	for i := 0; i < maxToolRounds; i++ {
		claudeCtx, claudeCancel := context.WithTimeout(ctx, claudeTimeout)
		resp, err := ce.claude.SendStreaming(claudeCtx, claude.SendParams{
			Messages:       messages,
			SystemPrompt:   systemPrompt,
			Tools:          tools,
			ThinkingEffort: config.Effort(effort),
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
// buildSpawnParams assembles the SpawnParams for a cron-triggered CLI run.
// Extracted from executeWithCLI so tests can assert that the MCP config is
// populated — without that, cron-fired tasks spawn a CLI subprocess with no
// MCP servers and lose access to runtime extensions like paper-search-mcp
// (the v0.36.7 incident where a paper digest fell back to WebSearch).
func (ce *CronExecutor) buildSpawnParams(userID, chatID int64, prompt, model, effort string, scheduledAt time.Time) claude.SpawnParams {
	return claude.SpawnParams{
		SystemPrompt: ce.buildSystemPrompt(userID, scheduledAt),
		MCPConfig:    buildMCPConfigForUser(ce.cfg, ce.configPath, userID, chatID),
		InitialMsg:   claude.BuildUserMessage(prompt),
		Model:        model,
		Effort:       effort,
		// HomeDir parity with Actor.handleMessage in actor.go: cron-fired CLI
		// subprocesses (and every MCP server they spawn) must run with
		// HOME=IsolatedHome so any tool that reads $HOME/.local/share or other
		// XDG paths sees the same files the interactive session does. Without
		// this, vibe-trading-mcp's openai-codex provider looked up
		// /data/.local/share/oauth-cli-kit/auth/codex.json (the daemon's
		// HOME=/data) instead of /data/claude-home/.local/share/... where
		// the OAuth token actually lives, so every cron-fired swarm aborted
		// with `OAuth credentials not found` (status=failed, 0 tokens).
		HomeDir: ce.cfg.Claude.IsolatedHome,
	}
}

func (ce *CronExecutor) executeWithCLI(ctx context.Context, userID, chatID int64, prompt, model, effort string, scheduledAt time.Time) (string, error) {
	proc, err := ce.cliMgr.SpawnOneShot(ctx, ce.buildSpawnParams(userID, chatID, prompt, model, effort, scheduledAt))
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
		// proc.Send accumulates streamed text deltas into `text` via the
		// callback before returning. If a context deadline (or any other
		// error) interrupts the stream mid-flight, every delta that already
		// arrived is committed. Surface that as a partial result alongside
		// the error so fireCronTask can render "this is what completed
		// before we ran out of time" instead of throwing the work away.
		// Empty text falls through unchanged (caller still sees the error).
		if text.Len() > 0 {
			return text.String(), fmt.Errorf("cron: CLI send: %w", err)
		}
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
// Includes timezone, the scheduled fire time, and user facts, but no conversation
// summaries or memory instructions.
// NOTE: cron tasks intentionally use a fixed prompt, not the configured personality.
// Cron tasks are operational (reminders, scheduled checks), not persona-driven.
func (ce *CronExecutor) buildSystemPrompt(userID int64, scheduledAt time.Time) string {
	loc, _ := memory.EffectiveLocation(ce.cfg, ce.db)
	now := time.Now().In(loc)
	scheduledLocal := scheduledAt.In(loc)

	var sb strings.Builder
	sb.WriteString("You are executing a scheduled task. Be concise and actionable.\n\n")
	fmt.Fprintf(&sb, "The user's timezone is %s.\n", loc.String())
	fmt.Fprintf(&sb, "This task was SCHEDULED to fire at: %s.\n", scheduledLocal.Format("2006-01-02 15:04 MST"))
	fmt.Fprintf(&sb, "Current local time at execution: %s.\n", now.Format("2006-01-02 15:04 MST"))
	sb.WriteString("When referencing \"this reminder\" or the scheduled time in your reply, use the SCHEDULED time above, not the current execution time. They may differ by minutes if execution lagged.\n")

	// Tool-discovery hint: MCP tools are namespaced like
	// `mcp__<config-server>__<upstream>__<tool>` when the task prompt
	// references a bare name (e.g. `search_papers`), search your tool
	// inventory for the matching suffix rather than declaring the tool
	// missing. Cron prompts are often authored with the upstream's bare
	// names; without this hint the agent sees `mcp__curlycatclaw-skills__
	// paper-search-mcp__search_papers`, fails to match `search_papers`
	// literally, and falls back to WebSearch — silently losing the tools.
	sb.WriteString("\nMCP tools are namespaced as `mcp__<server>__<tool>` (sometimes with an extra proxied segment). If this task's prompt names a tool by its bare name (e.g. `search_papers`), find the matching namespaced tool in your inventory (e.g. `mcp__curlycatclaw-skills__paper-search-mcp__search_papers`) and call THAT. Do NOT declare a tool missing without first searching for its suffix in your tool list.\n")

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
