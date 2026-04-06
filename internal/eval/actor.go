package eval

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/go-co-op/gocron/v2"

	"github.com/jialuohu/curlycatclaw/internal/memory"
	"github.com/jialuohu/curlycatclaw/internal/telegram"
)

// ActorConfig holds the configuration for the EvalActor.
type ActorConfig struct {
	DBPath             string  // path to SQLite database
	Schedule           string  // cron expression (5-field)
	LookbackHours      int     // hours of history per run
	ScoreThreshold     float64 // below this triggers failure mining
	ReportChatID       int64   // Telegram chat ID for eval reports
	MaxCandidatesPerRun int    // cap on memory candidates per run
}

// Actor is a supervised actor that runs the self-evaluation pipeline on a schedule.
// It follows the same pattern as ReminderActor: internal gocron scheduler,
// implements actor.Actor interface (Name + Run).
type Actor struct {
	cfg          ActorConfig
	store        *memory.Store
	db           *sql.DB // separate read-only connection for scanning
	scorer       *Scorer
	miner        *Miner
	reporter     *Reporter
	candidateGen *CandidateGenerator // nil if no LLM configured
	gate         *Gate
}

// NewActor creates an EvalActor. llm may be nil to skip candidate generation.
func NewActor(cfg ActorConfig, store *memory.Store, tg chan<- telegram.OutgoingMessage, llm LLMCaller) (*Actor, error) {
	// Open a separate read-only connection for scanning (WAL mode allows concurrent reads).
	readDB, err := sql.Open("sqlite", cfg.DBPath+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("eval: open read-only db: %w", err)
	}
	readDB.SetMaxOpenConns(1)

	maxCandidates := cfg.MaxCandidatesPerRun
	if maxCandidates <= 0 {
		maxCandidates = 5
	}

	var cg *CandidateGenerator
	if llm != nil {
		cg = NewCandidateGenerator(llm, maxCandidates)
	}

	return &Actor{
		cfg:          cfg,
		store:        store,
		db:           readDB,
		scorer:       NewScorer(readDB),
		miner:        NewMiner(readDB),
		reporter:     NewReporter(tg),
		candidateGen: cg,
		gate:         NewGate(store.DB(), false), // Phase 2: no auto-commit
	}, nil
}

// Name implements actor.Actor.
func (a *Actor) Name() string { return "eval" }

// Run implements actor.Actor. It starts the gocron scheduler and runs the eval
// pipeline on the configured schedule. Blocks until ctx is cancelled.
func (a *Actor) Run(ctx context.Context) error {
	// Mark any stale eval runs from previous crashes as failed.
	a.recoverStaleRuns()

	scheduler, err := gocron.NewScheduler()
	if err != nil {
		return fmt.Errorf("eval: create scheduler: %w", err)
	}

	_, err = scheduler.NewJob(
		gocron.CronJob(a.cfg.Schedule, false),
		gocron.NewTask(func() {
			a.runPipeline(ctx)
		}),
	)
	if err != nil {
		return fmt.Errorf("eval: schedule job: %w", err)
	}

	scheduler.Start()
	slog.Info("eval: actor started", "schedule", a.cfg.Schedule)

	<-ctx.Done()
	if err := scheduler.Shutdown(); err != nil {
		slog.Warn("eval: scheduler shutdown error", "err", err)
	}
	a.db.Close()
	return ctx.Err()
}

// runPipeline executes one full evaluation cycle: scan → score → mine → report.
func (a *Actor) runPipeline(ctx context.Context) {
	// Overlap guard: check if a run is already in progress.
	var running int
	a.store.DB().QueryRow(`SELECT COUNT(*) FROM eval_runs WHERE status = 'running'`).Scan(&running) //nolint:errcheck
	if running > 0 {
		slog.Info("eval: skipping run, another is in progress")
		return
	}

	runID := newID()
	now := time.Now().UTC()
	_, err := a.store.DB().Exec(
		`INSERT INTO eval_runs (id, started_at, status) VALUES (?, ?, 'running')`,
		runID, now,
	)
	if err != nil {
		slog.Error("eval: failed to create run", "err", err)
		return
	}

	slog.Info("eval: pipeline started", "run_id", runID)

	// 1. SCAN — load conversations from the lookback window.
	since := now.Add(-time.Duration(a.cfg.LookbackHours) * time.Hour)
	convs, err := a.store.GetConversationsSince(since)
	if err != nil {
		slog.Error("eval: scan failed", "err", err)
		a.completeRun(runID, "failed", 0, 0, 0, err.Error())
		return
	}

	if len(convs) == 0 {
		slog.Info("eval: no conversations in lookback window")
		a.completeRun(runID, "completed", 0, 0, 0, "no conversations")
		return
	}

	// 2. SCORE — compute quality signals for each conversation.
	var scores []EvalScore
	for _, conv := range convs {
		if ctx.Err() != nil {
			a.completeRun(runID, "failed", len(convs), 0, 0, "context cancelled")
			return
		}

		sig, err := a.scorer.ScoreConversation(conv.ID)
		if err != nil {
			slog.Warn("eval: score failed", "conv_id", conv.ID, "err", err)
			continue
		}

		score := EvalScore{
			ID:                  newID(),
			ConversationID:      conv.ID,
			EvalRunID:           runID,
			OverallScore:        sig.Score(),
			ToolSuccessRate:     1.0,
			CorrectionCount:     sig.CorrectionCount,
			RetryCount:          sig.RetryCount,
			EffortOverrideCount: sig.EffortOverrides,
			CreatedAt:           now,
		}
		if sig.TotalToolCalls > 0 {
			score.ToolSuccessRate = 1.0 - float64(sig.FailedToolCalls)/float64(sig.TotalToolCalls)
		}

		// Persist score.
		detailsJSON, _ := json.Marshal(sig)
		_, err = a.store.DB().Exec(
			`INSERT INTO eval_scores (id, conversation_id, eval_run_id, overall_score, tool_success_rate, correction_count, retry_count, effort_override_count, details, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			score.ID, score.ConversationID, score.EvalRunID, score.OverallScore,
			score.ToolSuccessRate, score.CorrectionCount, score.RetryCount,
			score.EffortOverrideCount, string(detailsJSON), score.CreatedAt,
		)
		if err != nil {
			slog.Warn("eval: persist score failed", "err", err)
		}

		scores = append(scores, score)
	}

	// 3. MINE — cluster low-scoring conversations into failure patterns.
	clusters, err := a.miner.MineFailures(runID, scores, a.cfg.ScoreThreshold)
	if err != nil {
		slog.Error("eval: mining failed", "err", err)
		a.completeRun(runID, "failed", len(convs), 0, 0, err.Error())
		return
	}

	// Persist clusters.
	for _, c := range clusters {
		convIDsJSON, _ := json.Marshal(c.ConversationIDs)
		toolIDsJSON, _ := json.Marshal(c.ToolCallIDs)
		_, err := a.store.DB().Exec(
			`INSERT INTO failure_clusters (id, eval_run_id, cluster_type, description, conversation_ids, message_rowids, tool_call_ids, severity, frequency, created_at)
			 VALUES (?, ?, ?, ?, ?, '[]', ?, ?, ?, ?)`,
			c.ID, c.EvalRunID, c.ClusterType, c.Description, string(convIDsJSON),
			string(toolIDsJSON), c.Severity, c.Frequency, c.CreatedAt,
		)
		if err != nil {
			slog.Warn("eval: persist cluster failed", "err", err)
		}
	}

	// 4. PROPOSE — generate memory candidates for failure clusters.
	var candidates []MemoryCandidate
	if a.candidateGen != nil && len(clusters) > 0 {
		candidates = a.candidateGen.GenerateCandidates(ctx, runID, clusters)
		if len(candidates) > 0 {
			results := a.gate.Process(candidates)
			var pending int
			for _, r := range results {
				if r.Action == "pending" {
					pending++
				}
			}
			slog.Info("eval: candidates processed", "total", len(candidates), "pending", pending)
		}
	}

	// 5. REPORT — send summary to Telegram.
	run := EvalRun{
		ID:                   runID,
		StartedAt:            now,
		ConversationsScanned: len(convs),
		FailuresFound:        len(clusters),
		CandidatesGenerated:  len(candidates),
	}
	if a.cfg.ReportChatID != 0 {
		a.reporter.SendReport(a.cfg.ReportChatID, run, scores, clusters)
	}

	a.completeRun(runID, "completed", len(convs), len(clusters), len(candidates), "")
	slog.Info("eval: pipeline completed",
		"run_id", runID,
		"conversations", len(convs),
		"scores", len(scores),
		"clusters", len(clusters),
	)
}

// completeRun updates an eval_run's status and summary.
func (a *Actor) completeRun(runID, status string, scanned, failures, candidates int, errMsg string) {
	summary := fmt.Sprintf(`{"scanned":%d,"failures":%d,"candidates":%d,"error":%q}`,
		scanned, failures, candidates, errMsg)
	_, err := a.store.DB().Exec(
		`UPDATE eval_runs SET status = ?, completed_at = ?, conversations_scanned = ?, failures_found = ?, candidates_generated = ?, summary = ? WHERE id = ?`,
		status, time.Now().UTC(), scanned, failures, candidates, summary, runID,
	)
	if err != nil {
		slog.Error("eval: failed to complete run", "run_id", runID, "err", err)
	}
}

// recoverStaleRuns marks any eval_runs stuck in 'running' status as 'failed'.
func (a *Actor) recoverStaleRuns() {
	result, err := a.store.DB().Exec(
		`UPDATE eval_runs SET status = 'failed', completed_at = ?, summary = '{"error":"stale run recovered on startup"}' WHERE status = 'running'`,
		time.Now().UTC(),
	)
	if err != nil {
		slog.Warn("eval: recover stale runs failed", "err", err)
		return
	}
	if n, _ := result.RowsAffected(); n > 0 {
		slog.Info("eval: recovered stale runs", "count", n)
	}
}
