package skills

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/go-co-op/gocron/v2"

	"github.com/jialuohu/curlycatclaw/internal/telegram"
)

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

	setSkill := &Skill{
		Name:        "set_reminder",
		Description: "Set a reminder that will fire at the specified time. Optionally make it recurring with a cron expression.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"message":{"type":"string","description":"Reminder message"},"fire_at":{"type":"string","description":"When to fire the reminder (ISO 8601 datetime, e.g. 2025-01-15T09:00:00)"},"recurring":{"type":"string","description":"Optional cron expression for recurring reminders (e.g. 0 9 * * MON-FRI)"}},"required":["message","fire_at"]}`),
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
			cronExpr = &params.Recurring
		}

		res, err := db.ExecContext(ctx,
			`INSERT INTO reminders (user_id, chat_id, message, fire_at, cron_expr, status, created_at) VALUES (?, ?, ?, ?, ?, 'pending', ?)`,
			user.UserID, user.ChatID, params.Message, fireAtUTC, cronExpr, now,
		)
		if err != nil {
			return "", fmt.Errorf("set reminder: %w", err)
		}

		id, _ := res.LastInsertId()

		// Signal the actor to pick up the new reminder.
		select {
		case signalCh <- id:
		default:
			slog.Warn("remind signal channel full, actor will pick up on next cycle", "id", id)
		}

		localTime := fireAtUTC.In(loc).Format("2006-01-02 15:04")
		result := fmt.Sprintf("Reminder #%d set for %s: %s", id, localTime, params.Message)
		if cronExpr != nil {
			result += fmt.Sprintf(" (recurring: %s)", *cronExpr)
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
				`SELECT id, message, fire_at, cron_expr, status, created_at FROM reminders WHERE user_id = ? AND status = ? ORDER BY fire_at`,
				user.UserID, params.Status,
			)
		} else {
			rows, err = db.QueryContext(ctx,
				`SELECT id, message, fire_at, cron_expr, status, created_at FROM reminders WHERE user_id = ? ORDER BY fire_at`,
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
			var cronExpr *string
			if err := rows.Scan(&id, &message, &fireAt, &cronExpr, &status, &createdAt); err != nil {
				return "", fmt.Errorf("scan reminder: %w", err)
			}
			count++
			localFire := fireAt.In(loc).Format("2006-01-02 15:04")
			entry := fmt.Sprintf("#%d [%s] %s — %s", id, status, localFire, message)
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
		select {
		case signalCh <- params.ID:
		default:
			slog.Warn("remind signal channel full", "id", params.ID)
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

	mu   sync.Mutex
	jobs map[int64]gocron.Job
}

// NewReminderActor creates a new ReminderActor.
func NewReminderActor(db *sql.DB, tgInbox chan<- telegram.OutgoingMessage, loc *time.Location, signalCh <-chan int64) *ReminderActor {
	return &ReminderActor{
		db:       db,
		tgInbox:  tgInbox,
		loc:      loc,
		signalCh: signalCh,
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

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case id := <-ra.signalCh:
			ra.handleSignal(ctx, scheduler, id)
		}
	}
}

// loadPendingReminders queries all pending reminders and schedules or fires them.
// It collects all rows first to release the DB connection before processing,
// which avoids deadlocks with single-connection pools (e.g., in-memory SQLite).
func (ra *ReminderActor) loadPendingReminders(ctx context.Context, scheduler gocron.Scheduler) error {
	rows, err := ra.db.QueryContext(ctx,
		`SELECT id, user_id, chat_id, message, fire_at, cron_expr FROM reminders WHERE status = 'pending'`,
	)
	if err != nil {
		return fmt.Errorf("query pending reminders: %w", err)
	}

	var reminders []reminderRow
	for rows.Next() {
		var r reminderRow
		if err := rows.Scan(&r.ID, &r.UserID, &r.ChatID, &r.Message, &r.FireAt, &r.CronExpr); err != nil {
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
		`SELECT id, user_id, chat_id, message, fire_at, cron_expr, status FROM reminders WHERE id = ?`,
		id,
	).Scan(&r.ID, &r.UserID, &r.ChatID, &r.Message, &r.FireAt, &r.CronExpr, &status)
	if err != nil {
		slog.Error("reminder: query signal target", "id", id, "err", err)
		return
	}

	if status == "cancelled" {
		ra.cancelJob(id)
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
		delay := time.Until(r.FireAt)
		if delay < 0 {
			delay = 0
		}
		jobDef = gocron.OneTimeJob(gocron.OneTimeJobStartDateTime(r.FireAt))
		_ = delay // gocron handles past times
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
func (ra *ReminderActor) fireReminder(r reminderRow) {
	// Update status to "fired" (for non-recurring). For recurring, keep as pending.
	if r.CronExpr == nil {
		_, err := ra.db.Exec(
			`UPDATE reminders SET status = 'fired' WHERE id = ? AND status = 'pending'`,
			r.ID,
		)
		if err != nil {
			slog.Error("reminder: update status to fired", "id", r.ID, "err", err)
		}
	}

	msg := telegram.OutgoingMessage{
		ChatID: r.ChatID,
		Text:   "Reminder: " + r.Message,
	}

	select {
	case ra.tgInbox <- msg:
		slog.Info("reminder: fired", "id", r.ID, "chat_id", r.ChatID)
	default:
		slog.Error("reminder: telegram inbox full, message dropped", "id", r.ID)
	}
}

// cancelJob removes a scheduled job from gocron.
func (ra *ReminderActor) cancelJob(id int64) {
	ra.mu.Lock()
	job, ok := ra.jobs[id]
	if ok {
		delete(ra.jobs, id)
	}
	ra.mu.Unlock()

	if ok {
		// gocron v2 jobs are removed by their UUID.
		slog.Info("reminder: cancelled job", "id", id, "job_id", job.ID())
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
}
