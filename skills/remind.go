package skills

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/go-co-op/gocron/v2"
	cron "github.com/robfig/cron/v3"

	"github.com/jialuohu/curlycatclaw/internal/telegram"
)

// CronRunner executes a prompt through Claude with clean context.
// Implemented by session.CronExecutor; defined here to avoid circular imports.
type CronRunner interface {
	Execute(ctx context.Context, userID, chatID int64, prompt string) (string, error)
}

// InitRemindSkills creates the reminders table (if not exists) and returns
// the set_reminder, list_reminders, and cancel_reminder skills.
func InitRemindSkills(db *sql.DB, signalCh chan<- int64, loc *time.Location) ([]*Skill, error) {
	const schema = `CREATE TABLE IF NOT EXISTS reminders (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id    INTEGER NOT NULL,
		chat_id    INTEGER NOT NULL,
		message    TEXT NOT NULL,
		fire_at    DATETIME NOT NULL,
		cron_expr  TEXT,
		status     TEXT NOT NULL DEFAULT 'pending',
		created_at DATETIME NOT NULL
	)`
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("skills: create reminders table: %w", err)
	}

	// Migrate: add prompt column for Claude-powered cron tasks.
	if _, err := db.Exec(`ALTER TABLE reminders ADD COLUMN prompt TEXT`); err != nil {
		if !strings.Contains(err.Error(), "duplicate column") {
			return nil, fmt.Errorf("skills: add prompt column: %w", err)
		}
	}

	setSkill := &Skill{
		Name:        "set_reminder",
		Description: "Set a reminder that will fire at the specified time. Optionally make it recurring with a cron expression.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"message":{"type":"string","description":"Reminder label/message"},"fire_at":{"type":"string","description":"When to fire (ISO 8601 datetime, e.g. 2025-01-15T09:00:00)"},"recurring":{"type":"string","description":"Optional cron expression for recurring reminders (e.g. 0 9 * * MON-FRI)"},"prompt":{"type":"string","description":"Optional: if set, Claude executes this prompt at fire time with tool access (web_search, notes, facts, etc) and sends the result to your chat. Example: 'Check my notes and summarize what I need to do today'"}},"required":["message","fire_at"]}`),
		Execute:     makeSetReminderExecute(db, signalCh, loc),
	}

	listSkill := &Skill{
		Name:        "list_reminders",
		Description: "List reminders for the current user, optionally filtered by status.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"status":{"type":"string","description":"Filter by status: pending, fired, cancelled. Omit for all."}}}`),
		Execute:     makeListRemindersExecute(db, loc),
	}

	cancelSkill := &Skill{
		Name:        "cancel_reminder",
		Description: "Cancel an active reminder by its ID.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"integer","description":"Reminder ID to cancel"}},"required":["id"]}`),
		Execute:     makeCancelReminderExecute(db, signalCh),
	}

	return []*Skill{setSkill, listSkill, cancelSkill}, nil
}

type setReminderInput struct {
	Message   string `json:"message"`
	FireAt    string `json:"fire_at"`
	Recurring string `json:"recurring"`
	Prompt    string `json:"prompt"`
}

func makeSetReminderExecute(db *sql.DB, signalCh chan<- int64, loc *time.Location) func(ctx context.Context, input json.RawMessage) (string, error) {
	return func(ctx context.Context, input json.RawMessage) (string, error) {
		var params setReminderInput
		if err := json.Unmarshal(input, &params); err != nil {
			return "", fmt.Errorf("invalid input: %w", err)
		}
		if params.Message == "" {
			return "", fmt.Errorf("message is required")
		}
		if params.FireAt == "" {
			return "", fmt.Errorf("fire_at is required")
		}

		fireAt, err := time.ParseInLocation("2006-01-02T15:04:05", params.FireAt, loc)
		if err != nil {
			// Try with timezone offset.
			fireAt, err = time.Parse(time.RFC3339, params.FireAt)
			if err != nil {
				return "", fmt.Errorf("invalid fire_at format (use ISO 8601, e.g. 2025-01-15T09:00:00): %w", err)
			}
		}

		user := GetUser(ctx)
		now := time.Now().UTC()
		fireAtUTC := fireAt.UTC()

		var cronExpr *string
		if params.Recurring != "" {
			// Validate the cron expression at input time so the user gets
			// immediate feedback instead of a silent scheduling failure later.
			if _, parseErr := cron.ParseStandard(params.Recurring); parseErr != nil {
				return "", fmt.Errorf("invalid cron expression %q: %w", params.Recurring, parseErr)
			}
			cronExpr = &params.Recurring
		}

		var prompt *string
		if params.Prompt != "" {
			prompt = &params.Prompt
		}

		res, err := db.ExecContext(ctx,
			`INSERT INTO reminders (user_id, chat_id, message, fire_at, cron_expr, prompt, status, created_at) VALUES (?, ?, ?, ?, ?, ?, 'pending', ?)`,
			user.UserID, user.ChatID, params.Message, fireAtUTC, cronExpr, prompt, now,
		)
		if err != nil {
			return "", fmt.Errorf("set reminder: %w", err)
		}

		id, _ := res.LastInsertId()

		// Signal the actor to pick up the new reminder.
		signalTimer := time.NewTimer(5 * time.Second)
		defer signalTimer.Stop()
		select {
		case signalCh <- id:
		case <-signalTimer.C:
			slog.Error("remind signal channel full after 5s", "id", id)
			return "", fmt.Errorf("reminder saved but scheduler is unresponsive; it will activate on next restart")
		}

		localTime := fireAtUTC.In(loc).Format("2006-01-02 15:04")
		result := fmt.Sprintf("Reminder #%d set for %s: %s", id, localTime, params.Message)
		if cronExpr != nil {
			result += fmt.Sprintf(" (recurring: %s)", *cronExpr)
		}
		if prompt != nil {
			result += " [cron: Claude will execute the prompt at fire time]"
		}
		return result, nil
	}
}

type listRemindersInput struct {
	Status string `json:"status"`
}

func makeListRemindersExecute(db *sql.DB, loc *time.Location) func(ctx context.Context, input json.RawMessage) (string, error) {
	return func(ctx context.Context, input json.RawMessage) (string, error) {
		var params listRemindersInput
		if err := json.Unmarshal(input, &params); err != nil {
			return "", fmt.Errorf("invalid input: %w", err)
		}

		user := GetUser(ctx)

		var rows *sql.Rows
		var err error
		if params.Status != "" {
			rows, err = db.QueryContext(ctx,
				`SELECT id, message, fire_at, cron_expr, prompt, status, created_at FROM reminders WHERE user_id = ? AND status = ? ORDER BY fire_at`,
				user.UserID, params.Status,
			)
		} else {
			rows, err = db.QueryContext(ctx,
				`SELECT id, message, fire_at, cron_expr, prompt, status, created_at FROM reminders WHERE user_id = ? ORDER BY fire_at`,
				user.UserID,
			)
		}
		if err != nil {
			return "", fmt.Errorf("list reminders: %w", err)
		}
		defer rows.Close()

		var result string
		count := 0
		for rows.Next() {
			var id int64
			var message, status string
			var fireAt, createdAt time.Time
			var cronExpr, prompt *string
			if err := rows.Scan(&id, &message, &fireAt, &cronExpr, &prompt, &status, &createdAt); err != nil {
				return "", fmt.Errorf("scan reminder: %w", err)
			}
			count++
			localFire := fireAt.In(loc).Format("2006-01-02 15:04")
			tag := status
			if prompt != nil {
				tag = "cron:" + status
			}
			entry := fmt.Sprintf("#%d [%s] %s — %s", id, tag, localFire, message)
			if cronExpr != nil {
				entry += fmt.Sprintf(" (recurring: %s)", *cronExpr)
			}
			result += entry + "\n"
		}
		if err := rows.Err(); err != nil {
			return "", fmt.Errorf("iterate reminders: %w", err)
		}

		if count == 0 {
			if params.Status != "" {
				return fmt.Sprintf("No %s reminders found", params.Status), nil
			}
			return "No reminders found", nil
		}

		return result, nil
	}
}

type cancelReminderInput struct {
	ID int64 `json:"id"`
}

func makeCancelReminderExecute(db *sql.DB, signalCh chan<- int64) func(ctx context.Context, input json.RawMessage) (string, error) {
	return func(ctx context.Context, input json.RawMessage) (string, error) {
		var params cancelReminderInput
		if err := json.Unmarshal(input, &params); err != nil {
			return "", fmt.Errorf("invalid input: %w", err)
		}
		if params.ID == 0 {
			return "", fmt.Errorf("id is required")
		}

		user := GetUser(ctx)
		res, err := db.ExecContext(ctx,
			`UPDATE reminders SET status = 'cancelled' WHERE id = ? AND user_id = ? AND status = 'pending'`,
			params.ID, user.UserID,
		)
		if err != nil {
			return "", fmt.Errorf("cancel reminder: %w", err)
		}

		affected, _ := res.RowsAffected()
		if affected == 0 {
			return "", fmt.Errorf("reminder #%d not found or already fired/cancelled", params.ID)
		}

		// Signal the actor to cancel the scheduled job.
		signalTimer := time.NewTimer(5 * time.Second)
		defer signalTimer.Stop()
		select {
		case signalCh <- params.ID:
		case <-signalTimer.C:
			slog.Error("remind signal channel full after 5s", "id", params.ID)
			return "", fmt.Errorf("reminder #%d cancelled in database but scheduler did not acknowledge; it may still fire once", params.ID)
		}

		return fmt.Sprintf("Reminder #%d cancelled", params.ID), nil
	}
}

// ---------------------------------------------------------------------------
// ReminderActor
// ---------------------------------------------------------------------------

// ReminderActor is an actor that schedules and fires reminders using gocron.
type ReminderActor struct {
	db       *sql.DB
	tgInbox  chan<- telegram.OutgoingMessage
	loc      *time.Location
	signalCh <-chan int64
	cronExec CronRunner // nil = no cron task support (static text only)

	mu   sync.Mutex
	jobs map[int64]gocron.Job
}

// NewReminderActor creates a new ReminderActor. cronExec may be nil to disable
// Claude-powered cron tasks (reminders with prompts will fall back to static text).
func NewReminderActor(db *sql.DB, tgInbox chan<- telegram.OutgoingMessage, loc *time.Location, signalCh <-chan int64, cronExec CronRunner) *ReminderActor {
	return &ReminderActor{
		db:       db,
		tgInbox:  tgInbox,
		loc:      loc,
		signalCh: signalCh,
		cronExec: cronExec,
		jobs:     make(map[int64]gocron.Job),
	}
}

// Name implements actor.Actor.
func (ra *ReminderActor) Name() string { return "reminder" }

// Run implements actor.Actor. It starts a gocron scheduler, loads all pending
// reminders, fires past-due ones immediately, and schedules future ones.
// It then listens for signals to add or cancel reminders.
func (ra *ReminderActor) Run(ctx context.Context) error {
	scheduler, err := gocron.NewScheduler()
	if err != nil {
		return fmt.Errorf("reminder: create scheduler: %w", err)
	}
	scheduler.Start()
	defer func() {
		if err := scheduler.Shutdown(); err != nil {
			slog.Error("reminder: scheduler shutdown error", "err", err)
		}
	}()

	// Load all pending reminders on startup.
	if err := ra.loadPendingReminders(ctx, scheduler); err != nil {
		slog.Error("reminder: failed to load pending reminders", "err", err)
	}

	// Poll DB periodically for reminders created by the MCP server subprocess,
	// which writes to the same SQLite DB but can't signal this actor's channel.
	pollTicker := time.NewTicker(10 * time.Second)
	defer pollTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case id := <-ra.signalCh:
			ra.handleSignal(ctx, scheduler, id)
		case <-pollTicker.C:
			ra.pollNewReminders(ctx, scheduler)
		}
	}
}

// loadPendingReminders queries all pending reminders and schedules or fires them.
// It collects all rows first to release the DB connection before processing,
// which avoids deadlocks with single-connection pools (e.g., in-memory SQLite).
func (ra *ReminderActor) loadPendingReminders(ctx context.Context, scheduler gocron.Scheduler) error {
	rows, err := ra.db.QueryContext(ctx,
		`SELECT id, user_id, chat_id, message, fire_at, cron_expr, prompt FROM reminders WHERE status = 'pending'`,
	)
	if err != nil {
		return fmt.Errorf("query pending reminders: %w", err)
	}

	var reminders []reminderRow
	for rows.Next() {
		var r reminderRow
		if err := rows.Scan(&r.ID, &r.UserID, &r.ChatID, &r.Message, &r.FireAt, &r.CronExpr, &r.Prompt); err != nil {
			slog.Error("reminder: scan row", "err", err)
			continue
		}
		reminders = append(reminders, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	now := time.Now().UTC()
	for _, r := range reminders {
		if r.FireAt.Before(now) && r.CronExpr == nil {
			// Past-due, non-recurring: fire immediately.
			ra.fireReminder(r)
		} else {
			ra.scheduleReminder(scheduler, r)
		}
	}
	return nil
}

// handleSignal processes a signal for a reminder ID. It queries the reminder
// and either schedules or cancels it.
func (ra *ReminderActor) handleSignal(ctx context.Context, scheduler gocron.Scheduler, id int64) {
	var r reminderRow
	var status string
	err := ra.db.QueryRowContext(ctx,
		`SELECT id, user_id, chat_id, message, fire_at, cron_expr, prompt, status FROM reminders WHERE id = ?`,
		id,
	).Scan(&r.ID, &r.UserID, &r.ChatID, &r.Message, &r.FireAt, &r.CronExpr, &r.Prompt, &status)
	if err != nil {
		slog.Error("reminder: query signal target", "id", id, "err", err)
		return
	}

	if status == "cancelled" {
		ra.cancelJob(scheduler, id)
		return
	}

	if status == "pending" {
		ra.scheduleReminder(scheduler, r)
	}
}

// scheduleReminder adds a gocron job for the given reminder.
func (ra *ReminderActor) scheduleReminder(scheduler gocron.Scheduler, r reminderRow) {
	var jobDef gocron.JobDefinition
	if r.CronExpr != nil {
		jobDef = gocron.CronJob(*r.CronExpr, false)
	} else {
		jobDef = gocron.OneTimeJob(gocron.OneTimeJobStartDateTime(r.FireAt))
	}

	task := gocron.NewTask(func() {
		ra.fireReminder(r)
		// For one-time reminders, clean up the job map.
		if r.CronExpr == nil {
			ra.mu.Lock()
			delete(ra.jobs, r.ID)
			ra.mu.Unlock()
		}
	})

	job, err := scheduler.NewJob(jobDef, task)
	if err != nil {
		slog.Error("reminder: schedule job", "id", r.ID, "err", err)
		return
	}

	ra.mu.Lock()
	ra.jobs[r.ID] = job
	ra.mu.Unlock()

	slog.Info("reminder: scheduled", "id", r.ID, "fire_at", r.FireAt)
}

// fireReminder sends the reminder message via Telegram and updates the DB status.
// If the reminder has a prompt and CronRunner is available, it invokes Claude instead.
func (ra *ReminderActor) fireReminder(r reminderRow) {
	if r.Prompt != nil && *r.Prompt != "" && ra.cronExec != nil {
		ra.fireCronTask(r)
		return
	}

	msg := telegram.OutgoingMessage{
		ChatID: r.ChatID,
		Text:   "Reminder: " + r.Message,
	}

	// Blocking send with 5s timeout. Reminders are too important to silently drop.
	if !ra.trySendTelegram(r.ID, msg) {
		return // Don't update status — will retry on next startup.
	}

	ra.markFiredIfOneTime(r)
}

// fireCronTask invokes Claude with the reminder's prompt and sends the result.
func (ra *ReminderActor) fireCronTask(r reminderRow) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	slog.Info("reminder: executing cron task", "id", r.ID, "chat_id", r.ChatID)

	result, err := ra.cronExec.Execute(ctx, r.UserID, r.ChatID, *r.Prompt)
	if err != nil {
		slog.Error("reminder: cron task failed", "id", r.ID, "err", err)
		errMsg := telegram.OutgoingMessage{
			ChatID: r.ChatID,
			Text:   fmt.Sprintf("[Cron task failed] %s: %v", r.Message, err),
		}
		ra.trySendTelegram(r.ID, errMsg)
		// For one-time: still mark fired (error was delivered).
		// For recurring: keep pending (will retry next schedule).
		if r.CronExpr == nil {
			ra.markFiredIfOneTime(r)
		}
		return
	}

	msg := telegram.OutgoingMessage{
		ChatID: r.ChatID,
		Text:   fmt.Sprintf("**%s**\n\n%s", r.Message, result),
	}
	if !ra.trySendTelegram(r.ID, msg) {
		return
	}

	ra.markFiredIfOneTime(r)
}

// trySendTelegram attempts to send a message to Telegram with a 5s timeout.
// Returns true if sent, false if timed out.
func (ra *ReminderActor) trySendTelegram(reminderID int64, msg telegram.OutgoingMessage) bool {
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case ra.tgInbox <- msg:
		slog.Info("reminder: fired", "id", reminderID, "chat_id", msg.ChatID)
		return true
	case <-timer.C:
		slog.Error("reminder: telegram inbox full after 5s, keeping pending for retry", "id", reminderID)
		return false
	}
}

// markFiredIfOneTime marks a one-time (non-recurring) reminder as fired.
func (ra *ReminderActor) markFiredIfOneTime(r reminderRow) {
	if r.CronExpr == nil {
		_, err := ra.db.Exec(
			`UPDATE reminders SET status = 'fired' WHERE id = ? AND status = 'pending'`,
			r.ID,
		)
		if err != nil {
			slog.Error("reminder: update status to fired", "id", r.ID, "err", err)
		}
	}
}

// pollNewReminders checks for pending reminders not yet scheduled (created by
// the MCP server subprocess which shares the DB but not the signal channel).
func (ra *ReminderActor) pollNewReminders(ctx context.Context, scheduler gocron.Scheduler) {
	rows, err := ra.db.QueryContext(ctx,
		`SELECT id, user_id, chat_id, message, fire_at, cron_expr, prompt FROM reminders WHERE status = 'pending'`,
	)
	if err != nil {
		slog.Error("reminder: poll query failed", "err", err)
		return
	}

	var unscheduled []reminderRow
	for rows.Next() {
		var r reminderRow
		if err := rows.Scan(&r.ID, &r.UserID, &r.ChatID, &r.Message, &r.FireAt, &r.CronExpr, &r.Prompt); err != nil {
			slog.Error("reminder: poll scan", "err", err)
			continue
		}
		ra.mu.Lock()
		_, tracked := ra.jobs[r.ID]
		ra.mu.Unlock()
		if !tracked {
			unscheduled = append(unscheduled, r)
		}
	}
	rows.Close()

	now := time.Now().UTC()
	for _, r := range unscheduled {
		slog.Info("reminder: poll found unscheduled reminder", "id", r.ID)
		if r.FireAt.Before(now) && r.CronExpr == nil {
			ra.fireReminder(r)
		} else {
			ra.scheduleReminder(scheduler, r)
		}
	}
}

// cancelJob removes a scheduled job from gocron.
func (ra *ReminderActor) cancelJob(scheduler gocron.Scheduler, id int64) {
	ra.mu.Lock()
	job, ok := ra.jobs[id]
	if ok {
		delete(ra.jobs, id)
	}
	ra.mu.Unlock()

	if ok {
		// Actually remove the job from the scheduler so it stops firing.
		if err := scheduler.RemoveJob(job.ID()); err != nil {
			slog.Error("reminder: failed to remove job from scheduler", "id", id, "job_id", job.ID(), "err", err)
		} else {
			slog.Info("reminder: cancelled job", "id", id, "job_id", job.ID())
		}
	}
}

// reminderRow holds a row from the reminders table.
type reminderRow struct {
	ID       int64
	UserID   int64
	ChatID   int64
	Message  string
	FireAt   time.Time
	CronExpr *string
	Prompt   *string
}
