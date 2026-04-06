package ingest

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

	"github.com/jialuohu/curlycatclaw/internal/claude"
	"github.com/jialuohu/curlycatclaw/internal/memory"
)

// LLMClient abstracts the Claude API for extraction.
type LLMClient interface {
	Send(ctx context.Context, params claude.SendParams) (*claude.Response, error)
}

// IngestStore abstracts the storage operations needed by the ingest actor.
type IngestStore interface {
	SaveObservation(obs *memory.Observation) error
	ObservationExistsByHash(userID int64, hash string) (bool, error)
	SaveEntities(obsID string, entities []memory.Entity) error
	EnsureConversation(id string, userID int64) error

	// Generic ingest state (source-partitioned).
	GetIngestState(source, partition string) (mode, cursor, backfillCursor, status string, err error)
	UpdateIngestState(source, partition, mode, cursor, backfillCursor, status string) error
	UpdateIngestStats(source, partition string, scanned, prefiltered, llmTriaged, kept, failed int) error
	IsItemProcessed(source, itemID string) (bool, error)
	GetItemFingerprint(source, itemID string) (string, error)
	RecordItemProcessed(source, itemID, result, fingerprint string) error
	CleanupOldProcessedItems(days int) (int64, error)
}

// VectorIndexer abstracts Qdrant observation indexing.
type VectorIndexer interface {
	IndexObservation(ctx context.Context, obs memory.Observation) error
}

// SourceEntry holds a configured source with its extraction settings.
type SourceEntry struct {
	Source         Source
	Extractor      Extractor
	TrustLevel     TrustLevel
	ExtractionMode ExtractionMode
	ChatType       string // e.g., "email", "obsidian", "notion"
	Partition      string // e.g., Gmail account name; empty for non-partitioned
	Interval       time.Duration
	BackfillDays   int
	MaxDailyObs    int
	MaxDailyLLM    int
	MinImportance  int
	MaxBodyChars   int
}

// Actor is the generic background ingest actor.
type Actor struct {
	sources  []SourceEntry
	llm      LLMClient
	store    IngestStore
	vs       VectorIndexer
	ownerUID int64
	ownerCID int64
	logger   *slog.Logger

	mu           sync.Mutex
	dailyLLMUsed map[string]int // source -> LLM calls today
	dailyObsKept map[string]int // source -> observations kept today
	lastResetDay string         // YYYY-MM-DD for daily counter reset
	cycleMu      sync.Mutex     // prevents concurrent runCycle executions
}

// New creates an IngestActor.
func New(
	sources []SourceEntry,
	llmClient LLMClient,
	store IngestStore,
	vs VectorIndexer,
	ownerUID, ownerCID int64,
) *Actor {
	return &Actor{
		sources:      sources,
		llm:          llmClient,
		store:        store,
		vs:           vs,
		ownerUID:     ownerUID,
		ownerCID:     ownerCID,
		logger:       slog.With("actor", "ingest"),
		dailyLLMUsed: make(map[string]int),
		dailyObsKept: make(map[string]int),
	}
}

// Name implements actor.Actor.
func (a *Actor) Name() string { return "ingest" }

// Run implements actor.Actor. Starts a unified gocron scheduler.
func (a *Actor) Run(ctx context.Context) error {
	// Recover any stale "running" states from a previous crash.
	a.recoverStaleStates()
	scheduler, err := gocron.NewScheduler()
	if err != nil {
		return fmt.Errorf("ingest: create scheduler: %w", err)
	}
	scheduler.Start()
	defer func() {
		if err := scheduler.Shutdown(); err != nil {
			a.logger.Error("scheduler shutdown error", "err", err)
		}
	}()

	// Find shortest interval among sources for the unified cycle timer.
	interval := 15 * time.Minute
	for _, entry := range a.sources {
		if entry.Interval > 0 && entry.Interval < interval {
			interval = entry.Interval
		}
	}

	_, err = scheduler.NewJob(
		gocron.DurationJob(interval),
		gocron.NewTask(func() {
			a.runCycle(ctx)
		}),
	)
	if err != nil {
		return fmt.Errorf("ingest: schedule job: %w", err)
	}

	// Run first cycle after a brief delay to let MCP servers finish tool discovery.
	go func() {
		select {
		case <-time.After(10 * time.Second):
			a.runCycle(ctx)
		case <-ctx.Done():
		}
	}()

	// Periodic cleanup of old processed items.
	cleanupTicker := time.NewTicker(24 * time.Hour)
	defer cleanupTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-cleanupTicker.C:
			if deleted, err := a.store.CleanupOldProcessedItems(7); err != nil {
				a.logger.Warn("cleanup failed", "err", err)
			} else if deleted > 0 {
				a.logger.Info("cleaned up old processed items", "deleted", deleted)
			}
		}
	}
}

// runCycle processes all sources sequentially.
func (a *Actor) runCycle(ctx context.Context) {
	if !a.cycleMu.TryLock() {
		a.logger.Debug("skipping cycle, previous still running")
		return
	}
	defer a.cycleMu.Unlock()

	a.resetDailyCounters()

	for _, entry := range a.sources {
		if ctx.Err() != nil {
			return
		}
		a.processSource(ctx, entry)
	}
}

// processSource handles a single source's ingest cycle.
func (a *Actor) processSource(ctx context.Context, entry SourceEntry) {
	src := entry.Source
	srcName := src.Name()
	partition := entry.Partition

	mode, cursor, backfillCursor, status, err := a.store.GetIngestState(srcName, partition)
	if err != nil {
		a.logger.Error("get ingest state", "source", srcName, "err", err)
		return
	}

	if status == "running" {
		a.logger.Warn("skipping source, previous run still in progress", "source", srcName)
		return
	}

	if err := a.store.UpdateIngestState(srcName, partition, mode, cursor, backfillCursor, "running"); err != nil {
		a.logger.Error("set running state", "source", srcName, "err", err)
		return
	}

	defer func() {
		curMode, curCursor, curBfCursor, _, readErr := a.store.GetIngestState(srcName, partition)
		if readErr != nil {
			a.logger.Error("re-read state for idle reset", "source", srcName, "err", readErr)
			return
		}
		if err := a.store.UpdateIngestState(srcName, partition, curMode, curCursor, curBfCursor, "idle"); err != nil {
			a.logger.Error("reset idle state", "source", srcName, "err", err)
		}
	}()

	switch mode {
	case "backfill":
		a.runBackfill(ctx, entry, backfillCursor)
	case "incremental", "":
		a.runIncremental(ctx, entry)
	default:
		a.logger.Warn("unknown mode, defaulting to incremental", "mode", mode, "source", srcName)
		a.runIncremental(ctx, entry)
	}
}

// runIncremental processes new items from a source.
func (a *Actor) runIncremental(ctx context.Context, entry SourceEntry) {
	src := entry.Source
	srcName := src.Name()

	// Use nil cursor for incremental (source decides its own default).
	items, _, err := src.Discover(ctx, nil)
	if err != nil {
		a.logger.Error("discover", "source", srcName, "err", err)
		return
	}

	stats := a.processItems(ctx, entry, items)
	if err := a.store.UpdateIngestStats(srcName, entry.Partition, stats.scanned, stats.prefiltered, stats.llmTriaged, stats.kept, stats.failed); err != nil {
		a.logger.Warn("update stats", "err", err)
	}

	a.logger.Info("incremental sync complete",
		"source", srcName,
		"scanned", stats.scanned,
		"prefiltered", stats.prefiltered,
		"kept", stats.kept,
		"failed", stats.failed,
	)
}

// runBackfill processes historical items in windows (source-specific).
func (a *Actor) runBackfill(ctx context.Context, entry SourceEntry, cursor string) {
	src := entry.Source
	srcName := src.Name()

	var cursorJSON json.RawMessage
	if cursor != "" {
		cursorJSON, _ = json.Marshal(cursor)
	}

	items, newCursor, err := src.Discover(ctx, cursorJSON)
	if err != nil {
		a.logger.Error("backfill discover", "source", srcName, "err", err)
		return
	}

	stats := a.processItems(ctx, entry, items)
	if err := a.store.UpdateIngestStats(srcName, entry.Partition, stats.scanned, stats.prefiltered, stats.llmTriaged, stats.kept, stats.failed); err != nil {
		a.logger.Warn("update backfill stats", "err", err)
	}

	// Advance cursor.
	var newCursorStr string
	if newCursor != nil {
		_ = json.Unmarshal(newCursor, &newCursorStr)
	}

	if len(items) == 0 {
		// Backfill complete — transition to incremental.
		a.logger.Info("backfill complete, switching to incremental", "source", srcName)
		if err := a.store.UpdateIngestState(srcName, entry.Partition, "incremental", "", "", "running"); err != nil {
			a.logger.Error("transition to incremental", "err", err)
		}
	} else {
		if err := a.store.UpdateIngestState(srcName, entry.Partition, "backfill", "", newCursorStr, "running"); err != nil {
			a.logger.Error("update backfill cursor", "err", err)
		}
	}
}

type processStats struct {
	scanned     int
	prefiltered int
	llmTriaged  int
	kept        int
	failed      int
}

// processItems applies the filter/extract pipeline to a batch of items.
func (a *Actor) processItems(ctx context.Context, entry SourceEntry, items []ItemRef) processStats {
	src := entry.Source
	srcName := src.Name()
	var stats processStats

	for _, item := range items {
		if ctx.Err() != nil {
			return stats
		}

		stats.scanned++

		// Check if already processed.
		processed, err := a.store.IsItemProcessed(srcName, item.ID)
		if err != nil {
			a.logger.Warn("check processed", "item_id", item.ID, "err", err)
		}

		// For already-processed items, only re-process if the source
		// supports mutable content (ContentFingerprint) and it changed.
		// This enables re-extraction of edited Obsidian notes and Notion pages.
		if processed {
			oldFingerprint, _ := a.store.GetItemFingerprint(srcName, item.ID)
			if oldFingerprint == "" {
				// No fingerprint stored — immutable source (Gmail), skip.
				continue
			}
			// Mutable source: read content to check fingerprint.
			content, err := src.Read(ctx, item.ID)
			if err != nil {
				continue // can't read, skip
			}
			if content.ContentFingerprint == "" || content.ContentFingerprint == oldFingerprint {
				continue // unchanged or no fingerprint support
			}
			// Content changed — fall through to re-extract.
		}

		// Check daily limits.
		if a.dailyLLMExceeded(srcName, entry.MaxDailyLLM) {
			a.logger.Warn("daily LLM call limit reached", "source", srcName)
			return stats
		}
		if a.dailyObsExceeded(srcName, entry.MaxDailyObs) {
			a.logger.Warn("daily observation limit reached", "source", srcName)
			return stats
		}

		// Prefilter.
		if !src.Prefilter(item) {
			stats.prefiltered++
			_ = a.store.RecordItemProcessed(srcName, item.ID, "prefiltered", "")
			continue
		}

		// Read full content (or re-read if we already read for fingerprint check).
		content, err := src.Read(ctx, item.ID)
		if err != nil {
			a.logger.Warn("read item", "source", srcName, "item_id", item.ID, "err", err)
			stats.failed++
			_ = a.store.RecordItemProcessed(srcName, item.ID, "failed", "")
			continue
		}

		// Strip quoted replies for email sources.
		if entry.ChatType == "email" {
			maxChars := entry.MaxBodyChars
			if maxChars == 0 {
				maxChars = 4000
			}
			content.Body = StripQuotedReplies(content.Body, maxChars)
			if len(strings.TrimSpace(content.Body)) < 20 {
				stats.prefiltered++
				_ = a.store.RecordItemProcessed(srcName, item.ID, "prefiltered", content.ContentFingerprint)
				continue
			}
		}

		// Truncate body for non-email sources too.
		if entry.MaxBodyChars > 0 && entry.ChatType != "email" {
			runes := []rune(content.Body)
			if len(runes) > entry.MaxBodyChars {
				content.Body = string(runes[:entry.MaxBodyChars])
			}
		}

		// Extract observations.
		if entry.Extractor == nil {
			a.logger.Error("no extractor configured, skipping source", "source", srcName)
			return stats
		}
		a.incrementLLMCount(srcName)
		stats.llmTriaged++

		observations, err := entry.Extractor.Extract(ctx, content, entry.TrustLevel, entry.MinImportance)
		if err != nil {
			a.logger.Warn("extraction failed", "source", srcName, "item_id", item.ID, "err", err)
			stats.failed++
			_ = a.store.RecordItemProcessed(srcName, item.ID, "failed", content.ContentFingerprint)
			time.Sleep(time.Second)
			continue
		}

		if len(observations) == 0 {
			_ = a.store.RecordItemProcessed(srcName, item.ID, "llm_filtered", content.ContentFingerprint)
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
			convID := fmt.Sprintf("%s:%s:%s", entry.ChatType, srcName, item.ID)
			if content.Metadata["thread_id"] != "" {
				convID = fmt.Sprintf("%s:%s:%s", entry.ChatType, srcName, content.Metadata["thread_id"])
			}

			obs.ChatType = entry.ChatType
			obs.ConversationID = convID
			obs.UserID = a.ownerUID
			obs.ChatID = a.ownerCID
			obs.CreatedAt = time.Now().UTC()

			if err := a.store.EnsureConversation(convID, a.ownerUID); err != nil {
				a.logger.Warn("ensure conversation", "err", err)
				continue
			}

			if err := a.store.SaveObservation(obs); err != nil {
				a.logger.Warn("save observation", "err", err)
				continue
			}

			if len(obs.Entities) > 0 {
				if err := a.store.SaveEntities(obs.ID, obs.Entities); err != nil {
					a.logger.Warn("save entities", "obs_id", obs.ID, "err", err)
				}
			}

			if a.vs != nil {
				if err := a.vs.IndexObservation(ctx, *obs); err != nil {
					a.logger.Warn("index observation", "obs_id", obs.ID, "err", err)
				}
			}

			savedCount++
			a.incrementObsCount(srcName)
		}

		result := "llm_filtered"
		if savedCount > 0 {
			stats.kept += savedCount
			result = "extracted"
		}
		_ = a.store.RecordItemProcessed(srcName, item.ID, result, content.ContentFingerprint)

		// Rate limit between LLM calls.
		time.Sleep(time.Second)
	}
	return stats
}

// sendToLLM wraps the Claude client as an LLMSender.
func (a *Actor) sendToLLM(ctx context.Context, system, user string) (string, error) {
	resp, err := a.llm.Send(ctx, claude.SendParams{
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

// MakeLLMExtractor creates an LLMExtractor using the actor's Claude client.
func (a *Actor) MakeLLMExtractor() *LLMExtractor {
	return &LLMExtractor{Send: a.sendToLLM}
}

// Daily counter management.

func (a *Actor) resetDailyCounters() {
	a.mu.Lock()
	defer a.mu.Unlock()
	today := time.Now().Format("2006-01-02")
	if today != a.lastResetDay {
		a.dailyLLMUsed = make(map[string]int)
		a.dailyObsKept = make(map[string]int)
		a.lastResetDay = today
	}
}

func (a *Actor) dailyLLMExceeded(source string, max int) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.dailyLLMUsed[source] >= max
}

func (a *Actor) dailyObsExceeded(source string, max int) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.dailyObsKept[source] >= max
}

func (a *Actor) incrementLLMCount(source string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.dailyLLMUsed[source]++
}

func (a *Actor) incrementObsCount(source string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.dailyObsKept[source]++
}

// recoverStaleStates resets any ingest states stuck in "running" from a
// previous crash, so sources aren't permanently skipped after a restart.
func (a *Actor) recoverStaleStates() {
	for _, entry := range a.sources {
		srcName := entry.Source.Name()
		_, _, _, status, err := a.store.GetIngestState(srcName, entry.Partition)
		if err != nil {
			continue
		}
		if status == "running" {
			a.logger.Info("recovering stale running state", "source", srcName, "partition", entry.Partition)
			_ = a.store.UpdateIngestState(srcName, entry.Partition, "", "", "", "idle")
		}
	}
}
