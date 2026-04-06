// Package email implements background email-to-observation processing.
// It reads Gmail via the MCP Manager's GWS tools, applies a two-stage
// filter (cheap prefilter + LLM triage), and saves extracted observations
// into the existing observation pipeline.
package email

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/go-co-op/gocron/v2"

	"github.com/jialuohu/curlycatclaw/config"
	"github.com/jialuohu/curlycatclaw/internal/claude"
	"github.com/jialuohu/curlycatclaw/internal/memory"
)

// ToolRouter abstracts MCP tool invocation (same interface as session.ToolRouter).
type ToolRouter interface {
	CallTool(ctx context.Context, name string, args map[string]any, userID, chatID int64) (string, error)
}

// LLMClient abstracts the Claude API for email extraction.
// Uses Send (non-streaming) since background processing doesn't need streaming.
type LLMClient interface {
	Send(ctx context.Context, params claude.SendParams) (*claude.Response, error)
}

// IngestStore abstracts the storage operations needed by the ingest actor.
type IngestStore interface {
	SaveObservation(obs *memory.Observation) error
	ObservationExistsByHash(userID int64, hash string) (bool, error)
	SaveEntities(obsID string, entities []memory.Entity) error
	EnsureConversation(id string, userID int64) error
	GetEmailIngestState(account string) (mode, cursor, backfillCursor, status string, err error)
	UpdateEmailIngestState(account, mode, cursor, backfillCursor, status string) error
	UpdateEmailIngestStats(account string, scanned, prefiltered, llmTriaged, kept, failed int) error
	IsEmailProcessed(account, messageID string) (bool, error)
	RecordEmailProcessed(account, messageID, result string) error
	CleanupOldEmailProcessed(days int) (int64, error)
}

// VectorIndexer abstracts Qdrant observation indexing.
type VectorIndexer interface {
	IndexObservation(ctx context.Context, obs memory.Observation) error
}

// Actor is the background email ingest actor.
type Actor struct {
	cfg      config.EmailIngestConfig
	claude   LLMClient
	mcp      ToolRouter
	store    IngestStore
	vs       VectorIndexer
	ownerUID int64
	ownerCID int64
	logger   *slog.Logger

	mu           sync.Mutex
	dailyLLMUsed int
	dailyObsKept map[string]int  // account -> observations kept today
	lastResetDay string          // YYYY-MM-DD for daily counter reset
	cycleMu      sync.Mutex      // prevents concurrent runCycle executions
}

// New creates an EmailIngestActor.
func New(
	cfg config.EmailIngestConfig,
	claudeClient LLMClient,
	mcpRouter ToolRouter,
	store IngestStore,
	vs VectorIndexer,
	ownerUID, ownerCID int64,
) *Actor {
	return &Actor{
		cfg:          cfg,
		claude:       claudeClient,
		mcp:          mcpRouter,
		store:        store,
		vs:           vs,
		ownerUID:     ownerUID,
		ownerCID:     ownerCID,
		logger:       slog.With("actor", "email_ingest"),
		dailyObsKept: make(map[string]int),
	}
}

// Name implements actor.Actor.
func (a *Actor) Name() string { return "email_ingest" }

// Run implements actor.Actor. It starts a gocron scheduler for periodic
// email processing and runs until the context is cancelled.
func (a *Actor) Run(ctx context.Context) error {
	scheduler, err := gocron.NewScheduler()
	if err != nil {
		return fmt.Errorf("email_ingest: create scheduler: %w", err)
	}
	scheduler.Start()
	defer func() {
		if err := scheduler.Shutdown(); err != nil {
			a.logger.Error("scheduler shutdown error", "err", err)
		}
	}()

	interval := time.Duration(a.cfg.IntervalMinutes) * time.Minute
	_, err = scheduler.NewJob(
		gocron.DurationJob(interval),
		gocron.NewTask(func() {
			a.runCycle(ctx)
		}),
	)
	if err != nil {
		return fmt.Errorf("email_ingest: schedule job: %w", err)
	}

	// Run first cycle immediately.
	go a.runCycle(ctx)

	// Periodic cleanup of old processed messages.
	cleanupTicker := time.NewTicker(24 * time.Hour)
	defer cleanupTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-cleanupTicker.C:
			if deleted, err := a.store.CleanupOldEmailProcessed(7); err != nil {
				a.logger.Warn("cleanup failed", "err", err)
			} else if deleted > 0 {
				a.logger.Info("cleaned up old processed messages", "deleted", deleted)
			}
		}
	}
}

// runCycle processes all configured accounts. The cycleMu prevents
// concurrent executions (immediate first run + gocron overlap).
func (a *Actor) runCycle(ctx context.Context) {
	if !a.cycleMu.TryLock() {
		a.logger.Debug("skipping cycle, previous still running")
		return
	}
	defer a.cycleMu.Unlock()

	a.resetDailyCounters()

	accounts, err := a.discoverAccounts(ctx)
	if err != nil {
		a.logger.Error("failed to discover accounts", "err", err)
		return
	}

	for _, account := range accounts {
		if ctx.Err() != nil {
			return
		}
		a.processAccount(ctx, account)
	}
}

// discoverAccounts calls gws_list_accounts via MCP to find configured Gmail accounts.
func (a *Actor) discoverAccounts(ctx context.Context) ([]string, error) {
	args := make(map[string]any)
	result, err := a.mcp.CallTool(ctx, "gws__gws_list_accounts", args, a.ownerUID, a.ownerCID)
	if err != nil {
		// Fallback: if list_accounts fails, there might be no multi-account.
		// Try with empty account (single-account mode).
		return []string{""}, nil
	}

	// Parse account names from JSON array response.
	var accounts []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(result), &accounts); err != nil {
		// Non-JSON response; try single-account mode.
		return []string{""}, nil
	}

	var names []string
	for _, acc := range accounts {
		names = append(names, acc.Name)
	}
	if len(names) == 0 {
		return []string{""}, nil
	}
	return names, nil
}

// processAccount handles incremental sync or backfill for a single account.
func (a *Actor) processAccount(ctx context.Context, account string) {
	mode, cursor, backfillCursor, status, err := a.store.GetEmailIngestState(account)
	if err != nil {
		a.logger.Error("get ingest state", "account", account, "err", err)
		return
	}

	if status == "running" {
		a.logger.Warn("skipping account, previous run still in progress", "account", account)
		return
	}

	if err := a.store.UpdateEmailIngestState(account, mode, cursor, backfillCursor, "running"); err != nil {
		a.logger.Error("set running state", "account", account, "err", err)
		return
	}

	defer func() {
		// Reset status to idle. Re-read current state to avoid clobbering
		// cursor progress made by runBackfill during execution.
		curMode, curCursor, curBfCursor, _, readErr := a.store.GetEmailIngestState(account)
		if readErr != nil {
			a.logger.Error("re-read state for idle reset", "account", account, "err", readErr)
			return
		}
		if err := a.store.UpdateEmailIngestState(account, curMode, curCursor, curBfCursor, "idle"); err != nil {
			a.logger.Error("reset idle state", "account", account, "err", err)
		}
	}()

	switch mode {
	case "backfill":
		a.runBackfill(ctx, account, backfillCursor)
	case "incremental":
		a.runIncremental(ctx, account)
	default:
		a.logger.Warn("unknown mode, defaulting to incremental", "mode", mode, "account", account)
		a.runIncremental(ctx, account)
	}
}

// runIncremental processes new emails from the last day (dedup handles overlap).
func (a *Actor) runIncremental(ctx context.Context, account string) {
	messages, err := a.searchGmail(ctx, account, "newer_than:1d")
	if err != nil {
		a.logger.Error("gmail search", "account", account, "err", err)
		return
	}

	stats := a.processMessages(ctx, account, messages)
	if err := a.store.UpdateEmailIngestStats(account, stats.scanned, stats.prefiltered, stats.llmTriaged, stats.kept, stats.failed); err != nil {
		a.logger.Warn("update stats", "err", err)
	}

	a.logger.Info("incremental sync complete",
		"account", account,
		"scanned", stats.scanned,
		"prefiltered", stats.prefiltered,
		"kept", stats.kept,
		"failed", stats.failed,
	)
}

// runBackfill processes historical emails in date-range windows.
func (a *Actor) runBackfill(ctx context.Context, account, cursor string) {
	now := time.Now()
	startDate := now.AddDate(0, 0, -a.cfg.BackfillDays)

	// Parse cursor as date if present.
	windowStart := startDate
	if cursor != "" {
		if t, err := time.Parse("2006/01/02", cursor); err == nil {
			windowStart = t
		}
	}

	windowSize := 7 // days per window
	batchCount := 0

	for windowStart.Before(now) {
		if ctx.Err() != nil {
			return
		}

		windowEnd := windowStart.AddDate(0, 0, windowSize)
		if windowEnd.After(now) {
			windowEnd = now
		}

		query := fmt.Sprintf("after:%s before:%s",
			windowStart.Format("2006/01/02"),
			windowEnd.Format("2006/01/02"),
		)

		messages, err := a.searchGmail(ctx, account, query)
		if err != nil {
			a.logger.Error("backfill search", "account", account, "window", query, "err", err)
			// Don't advance cursor on error.
			return
		}

		stats := a.processMessages(ctx, account, messages)
		if err := a.store.UpdateEmailIngestStats(account, stats.scanned, stats.prefiltered, stats.llmTriaged, stats.kept, stats.failed); err != nil {
			a.logger.Warn("update backfill stats", "err", err)
		}

		batchCount++
		// Only advance cursor after successful batch processing.
		newCursor := windowEnd.Format("2006/01/02")
		if err := a.store.UpdateEmailIngestState(account, "backfill", "", newCursor, "running"); err != nil {
			a.logger.Error("update backfill cursor", "err", err)
			return
		}

		windowStart = windowEnd

		// Rate limit between windows.
		time.Sleep(time.Second)
	}

	// Backfill complete — transition to incremental.
	a.logger.Info("backfill complete, switching to incremental", "account", account, "batches", batchCount)
	if err := a.store.UpdateEmailIngestState(account, "incremental", "", "", "idle"); err != nil {
		a.logger.Error("transition to incremental", "err", err)
	}
}

type processStats struct {
	scanned     int
	prefiltered int
	llmTriaged  int
	kept        int
	failed      int
}

// processMessages applies the two-stage filter to a batch of email message IDs
// and returns processing statistics.
func (a *Actor) processMessages(ctx context.Context, account string, messages []gmailMessageRef) processStats {
	var stats processStats
	for _, ref := range messages {
		if ctx.Err() != nil {
			return stats
		}

		stats.scanned++

		// Check if already processed.
		processed, err := a.store.IsEmailProcessed(account, ref.ID)
		if err != nil {
			a.logger.Warn("check processed", "msg_id", ref.ID, "err", err)
		}
		if processed {
			continue
		}

		// Check daily limits.
		if a.dailyLLMExceeded() {
			a.logger.Warn("daily LLM call limit reached, stopping")
			return stats
		}
		if a.dailyObsExceeded(account) {
			a.logger.Warn("daily observation limit reached", "account", account)
			return stats
		}

		// Stage 1: prefilter on metadata.
		preResult := Prefilter(EmailMessage{
			MessageID: ref.ID,
			From:      ref.From,
			Subject:   ref.Subject,
			Labels:    ref.Labels,
			Body:      ref.Snippet,
		}, a.cfg.Labels, a.cfg.SkipSenders)

		if preResult != PrefilterPass {
			stats.prefiltered++
			_ = a.store.RecordEmailProcessed(account, ref.ID, "prefiltered")
			continue
		}

		// Read full message content.
		fullMsg, err := a.readGmail(ctx, account, ref.ID)
		if err != nil {
			a.logger.Warn("read gmail message", "msg_id", ref.ID, "err", err)
			stats.failed++
			_ = a.store.RecordEmailProcessed(account, ref.ID, "failed")
			continue
		}

		// Stage 2: LLM triage + extraction.
		fullMsg.Body = StripQuotedReplies(fullMsg.Body, 4000)
		if len(strings.TrimSpace(fullMsg.Body)) < 20 {
			stats.prefiltered++
			_ = a.store.RecordEmailProcessed(account, ref.ID, "prefiltered")
			continue
		}

		a.incrementLLMCount()
		stats.llmTriaged++

		observations, err := ExtractFromEmail(ctx, a.sendToLLM, *fullMsg, a.cfg.MinImportance)
		if err != nil {
			a.logger.Warn("extraction failed", "msg_id", ref.ID, "err", err)
			stats.failed++
			_ = a.store.RecordEmailProcessed(account, ref.ID, "failed")
			// Rate limit between LLM calls.
			time.Sleep(time.Second)
			continue
		}

		if len(observations) == 0 {
			_ = a.store.RecordEmailProcessed(account, ref.ID, "llm_filtered")
			time.Sleep(time.Second)
			continue
		}

		// Save observations.
		savedCount := 0
		for i := range observations {
			obs := &observations[i]

			// Dedup check.
			exists, err := a.store.ObservationExistsByHash(a.ownerUID, obs.ContentHash)
			if err != nil {
				a.logger.Warn("dedup check", "err", err)
				continue
			}
			if exists {
				continue
			}

			// Set observation metadata.
			convID := fmt.Sprintf("email:%s:%s", account, fullMsg.ThreadID)
			obs.ChatType = "email"
			obs.ConversationID = convID
			obs.UserID = a.ownerUID
			obs.ChatID = a.ownerCID
			obs.CreatedAt = time.Now().UTC()

			// Ensure FK-safe conversation row exists.
			if err := a.store.EnsureConversation(convID, a.ownerUID); err != nil {
				a.logger.Warn("ensure conversation", "err", err)
				continue
			}

			if err := a.store.SaveObservation(obs); err != nil {
				a.logger.Warn("save observation", "err", err)
				continue
			}

			// Save entities (best-effort).
			if len(obs.Entities) > 0 {
				if err := a.store.SaveEntities(obs.ID, obs.Entities); err != nil {
					a.logger.Warn("save entities", "obs_id", obs.ID, "err", err)
				}
			}

			// Index in Qdrant (best-effort).
			if a.vs != nil {
				if err := a.vs.IndexObservation(ctx, *obs); err != nil {
					a.logger.Warn("index observation", "obs_id", obs.ID, "err", err)
				}
			}

			savedCount++
			a.incrementObsCount(account)
		}

		if savedCount > 0 {
			stats.kept += savedCount
			_ = a.store.RecordEmailProcessed(account, ref.ID, "extracted")
		} else {
			_ = a.store.RecordEmailProcessed(account, ref.ID, "llm_filtered")
		}

		// Rate limit between LLM calls.
		time.Sleep(time.Second)
	}
	return stats
}

// sendToLLM wraps the Claude client as an LLMSender for extraction.
func (a *Actor) sendToLLM(ctx context.Context, system, user string) (string, error) {
	resp, err := a.claude.Send(ctx, claude.SendParams{
		SystemPrompt: system,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(user)),
		},
		MaxTokens: 2048,
	})
	if err != nil {
		return "", err
	}
	return resp.TextContent, nil
}

// gmailMessageRef holds metadata from a Gmail search result.
type gmailMessageRef struct {
	ID      string
	From    string
	Subject string
	Snippet string
	Labels  []string
}

// searchGmail calls the GWS MCP server to search Gmail.
func (a *Actor) searchGmail(ctx context.Context, account, query string) ([]gmailMessageRef, error) {
	// Deep-copy args to prevent _user_context mutation across calls.
	args := map[string]any{
		"q": query,
	}
	if account != "" {
		args["account"] = account
	}

	result, err := a.mcp.CallTool(ctx, "gws__gws_gmail_search", args, a.ownerUID, a.ownerCID)
	if err != nil {
		return nil, fmt.Errorf("gmail search: %w", err)
	}

	return parseGmailSearchResult(result), nil
}

// readGmail reads a full email message by ID.
func (a *Actor) readGmail(ctx context.Context, account, messageID string) (*EmailMessage, error) {
	args := map[string]any{
		"message_id": messageID,
	}
	if account != "" {
		args["account"] = account
	}

	result, err := a.mcp.CallTool(ctx, "gws__gws_gmail_read", args, a.ownerUID, a.ownerCID)
	if err != nil {
		return nil, fmt.Errorf("gmail read: %w", err)
	}

	return parseGmailReadResult(result, account, messageID), nil
}

// parseGmailSearchResult extracts message references from the search result string.
// The gws CLI returns JSON; we parse what we can and gracefully degrade.
func parseGmailSearchResult(result string) []gmailMessageRef {
	// Try JSON array parse first.
	var messages []struct {
		ID      string   `json:"id"`
		From    string   `json:"from"`
		Subject string   `json:"subject"`
		Snippet string   `json:"snippet"`
		Labels  []string `json:"labelIds"`
	}
	if err := json.Unmarshal([]byte(result), &messages); err != nil {
		// Try as object with messages field.
		var wrapper struct {
			Messages []struct {
				ID      string   `json:"id"`
				From    string   `json:"from"`
				Subject string   `json:"subject"`
				Snippet string   `json:"snippet"`
				Labels  []string `json:"labelIds"`
			} `json:"messages"`
		}
		if err := json.Unmarshal([]byte(result), &wrapper); err != nil {
			slog.Warn("email_ingest: failed to parse gmail search result", "err", err)
			return nil
		}
		messages = wrapper.Messages
	}

	var refs []gmailMessageRef
	for _, m := range messages {
		refs = append(refs, gmailMessageRef{
			ID:      m.ID,
			From:    m.From,
			Subject: m.Subject,
			Snippet: m.Snippet,
			Labels:  m.Labels,
		})
	}
	return refs
}

// parseGmailReadResult extracts email content from the read result string.
func parseGmailReadResult(result, account, messageID string) *EmailMessage {
	var msg struct {
		ID       string   `json:"id"`
		ThreadID string   `json:"threadId"`
		From     string   `json:"from"`
		To       string   `json:"to"`
		Subject  string   `json:"subject"`
		Date     string   `json:"date"`
		Body     string   `json:"body"`
		Labels   []string `json:"labelIds"`
	}
	if err := json.Unmarshal([]byte(result), &msg); err != nil {
		// If JSON parsing fails, treat the raw result as the body.
		return &EmailMessage{
			MessageID: messageID,
			Account:   account,
			Body:      result,
		}
	}

	return &EmailMessage{
		MessageID: msg.ID,
		ThreadID:  msg.ThreadID,
		Account:   account,
		From:      msg.From,
		To:        msg.To,
		Subject:   msg.Subject,
		Date:      msg.Date,
		Body:      msg.Body,
		Labels:    msg.Labels,
	}
}

// Daily counter management.

func (a *Actor) resetDailyCounters() {
	a.mu.Lock()
	defer a.mu.Unlock()
	today := time.Now().Format("2006-01-02")
	if today != a.lastResetDay {
		a.dailyLLMUsed = 0
		a.dailyObsKept = make(map[string]int)
		a.lastResetDay = today
	}
}

func (a *Actor) dailyLLMExceeded() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.dailyLLMUsed >= a.cfg.MaxDailyLLMCalls
}

func (a *Actor) dailyObsExceeded(account string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.dailyObsKept[account] >= a.cfg.MaxDailyObservations
}

func (a *Actor) incrementLLMCount() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.dailyLLMUsed++
}

func (a *Actor) incrementObsCount(account string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.dailyObsKept[account]++
}
