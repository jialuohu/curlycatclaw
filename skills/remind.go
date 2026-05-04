package skills

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/go-co-op/gocron/v2"
	cron "github.com/robfig/cron/v3"

	"github.com/jialuohu/curlycatclaw/config"
	"github.com/jialuohu/curlycatclaw/internal/telegram"
)

// CronRunner executes a prompt through Claude with clean context.
// Implemented by session.CronExecutor; defined here to avoid circular imports.
// model is optional: if non-empty, it overrides the default model for this execution.
// effort is optional: if non-empty, it overrides the default thinking effort
// (low/medium/high/xhigh/max). Validated at the set_reminder boundary; the cron
// runner passes it through.
// scheduledAt is the time the task was scheduled to fire (may differ from wall time if
// execution lagged) so Claude can reference the intended time, not the lagged one.
type CronRunner interface {
	Execute(ctx context.Context, userID, chatID int64, prompt, model, effort string, scheduledAt time.Time) (string, error)
}

// ConversationPersister records cron-fired turns in the same conversation
// history as interactive chats. Without this, cron output is sent only to
// Telegram — the agent's next turn has no record that the cron message
// existed, which caused the "why did you send me this?" incident where the
// agent fabricated timestamps for a message it never saw in its context.
// Implemented by memory.Store; defined here to avoid coupling skills to
// concrete storage.
type ConversationPersister interface {
	GetActiveConversation(userID, chatID int64, chatType string) (convID string, expiredConvID string, err error)
	AppendMessage(convID string, role string, content json.RawMessage) error
}

// listRemindersLimit caps how many rows list_reminders returns in a single
// call so a user with hundreds of reminders can't blow the agent's context
// budget. Pair with listPromptPreviewRunes — the per-prompt truncation
// keeps each row small even when full prompts exist.
const listRemindersLimit = 50

// listPromptPreviewRunes caps the prompt body shown inline in list output.
// Counted in runes (not bytes) so multi-byte CJK characters truncate
// predictably. Full bodies remain available via get_reminder.
const listPromptPreviewRunes = 500

// promptBodyCloser is the literal closing tag for the trust delimiter
// wrapper. Centralized so renderPromptBody can both escape complete
// occurrences in the body AND strip any partial-prefix tail left over
// from truncation.
const promptBodyCloser = "</user_prompt_body>"

// cronExecTimeout caps the total wall-clock budget for a single cron
// task: Claude's whole streaming run plus every MCP tool round-trip it
// makes. Compound tasks like a daily paper digest (Zotero search,
// per-paper read/score, ReadLater write per paper, with extended
// thinking) routinely run 8-15 minutes. The original 5-minute cap
// fired `cron: CLI send: context deadline exceeded` for those
// workloads on every fire. 20 minutes leaves real headroom while still
// bounding a genuinely hung CLI subprocess.
const cronExecTimeout = 20 * time.Minute

// renderPromptBody writes a prompt body to b wrapped in <user_prompt_body>
// delimiter tags. The wrapper signals to the agent that the contents are
// quoted user-supplied data, not instructions to act on — the same trust
// boundary pattern used by the ingest pipeline's untrusted-content prompts.
// Every line is indented 4 spaces so a hostile prompt containing what
// looks like a sibling reminder header (`#N [cron:pending]...`) can't
// spoof one (real entries start at column 0). If maxRunes > 0 and the
// body exceeds it, truncates and reports it via the returned bool.
//
// Order-of-operations matters: truncate FIRST, then escape, then strip
// any partial closing-tag prefix at the tail. The earlier "escape first,
// truncate second" version had a bypass: a body like `<482 X's></user_prompt_body>FAKE`
// became 501 runes after escape, then truncation cut the escaped form
// mid-stream and leaked `</user_prompt_b...` into output, which a fuzzy
// LLM parser could read as a closing tag.
func renderPromptBody(b *strings.Builder, prompt string, maxRunes int) (truncated bool) {
	body := prompt
	runes := []rune(body)
	if maxRunes > 0 && len(runes) > maxRunes {
		body = string(runes[:maxRunes])
		truncated = true
	}
	// Escape AFTER truncation so the rewrite can't itself be cut mid-stream.
	body = strings.ReplaceAll(body, promptBodyCloser, "</user_prompt_body_>")
	// Strip any trailing prefix of the closing tag that survived escape
	// (truncation can cut a body's closing tag in half, leaving e.g.
	// `</user_prompt_b` at the tail with no `>` to match in the escape).
	for i := len(promptBodyCloser) - 1; i > 0; i-- {
		if strings.HasSuffix(body, promptBodyCloser[:i]) {
			body = body[:len(body)-i]
			break
		}
	}
	if truncated {
		body += "..."
	}
	b.WriteString("\n  prompt: <user_prompt_body>")
	for line := range strings.SplitSeq(body, "\n") {
		b.WriteString("\n    ")
		b.WriteString(line)
	}
	b.WriteString("\n  </user_prompt_body>")
	return truncated
}

// InitRemindSkills creates the reminders table (if not exists) and returns
// the set_reminder, list_reminders, get_reminder, cancel_reminder,
// delete_reminder, and update_reminder skills.
// InitRemindSkills creates the reminders table and returns the reminder skill
// set. locFn returns the currently effective *time.Location and is called fresh
// at the top of each skill invocation so a runtime timezone change (via the
// set_timezone skill) is picked up without requiring a process restart. Pass
// `func() *time.Location { return cfg.Location() }` if you don't need the
// override path.
func InitRemindSkills(db *sql.DB, signalCh chan<- int64, locFn func() *time.Location) ([]*Skill, error) {
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
	// Migrate: add effort column for per-reminder thinking-effort override.
	if _, err := db.Exec(`ALTER TABLE reminders ADD COLUMN effort TEXT`); err != nil {
		if !strings.Contains(err.Error(), "duplicate column") {
			return nil, fmt.Errorf("skills: add effort column: %w", err)
		}
	}
	// Migrate: add model column for per-reminder model override.
	if _, err := db.Exec(`ALTER TABLE reminders ADD COLUMN model TEXT`); err != nil {
		if !strings.Contains(err.Error(), "duplicate column") {
			return nil, fmt.Errorf("skills: add model column: %w", err)
		}
	}

	setSkill := &Skill{
		Name:        "set_reminder",
		Description: "Set a reminder that will fire at the specified time. Optionally make it recurring with a cron expression.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"message":{"type":"string","description":"Reminder label/message"},"fire_at":{"type":"string","description":"When to fire (ISO 8601 datetime, e.g. 2025-01-15T09:00:00)"},"recurring":{"type":"string","description":"Optional cron expression for recurring reminders (e.g. 0 9 * * MON-FRI)"},"prompt":{"type":"string","description":"Optional: if set, Claude executes this prompt at fire time with tool access (web_search, notes, facts, etc) and sends the result to your chat. Example: 'Check my notes and summarize what I need to do today'"},"model":{"type":"string","description":"Optional: model to use for this reminder's prompt (e.g. claude-haiku-4-5 for cheap tasks, claude-sonnet-4-6 for complex ones). Defaults to the main session model."},"effort":{"type":"string","enum":["","low","medium","high","xhigh","max"],"description":"Optional: thinking effort for this reminder's Claude run. Overrides the global thinking_effort config default. xhigh requires Claude Opus 4.7+."}},"required":["message","fire_at"]}`),
		Execute:     makeSetReminderExecute(db, signalCh, locFn),
	}

	listSkill := &Skill{
		Name:        "list_reminders",
		Description: "List reminders for the current user (50 rows per page). Returns only active (pending) reminders by default; pass status=\"all\" to include cancelled and fired history, or a specific status (pending/cancelled/fired) to filter. Includes a 500-rune preview of the prompt body and model override for cron tasks. Use get_reminder to fetch the full prompt body before refining a long cron task with update_reminder. For pagination, pass offset=50 to see rows 51-100, offset=100 for 101-150, etc. The overflow notice shows the exact offset to use for the next page.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"status":{"type":"string","description":"Filter: pending (default), cancelled, fired, or \"all\" to include history."},"offset":{"type":"integer","minimum":0,"description":"Zero-indexed offset for pagination. Default 0 (first page). Pass 50 for the second page, 100 for the third, etc. Must be >= 0."}}}`),
		Execute:     makeListRemindersExecute(db, locFn),
	}

	getSkill := &Skill{
		Name:        "get_reminder",
		Description: "Get full details for a single reminder by ID, including the FULL prompt body (list_reminders truncates prompts to 500 chars). Use this when refining a long cron task to see the complete prompt before calling update_reminder. Returns 'reminder not found' for IDs that don't exist or belong to another user.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"integer","description":"Reminder ID"}},"required":["id"]}`),
		Execute:     makeGetReminderExecute(db, locFn),
	}

	cancelSkill := &Skill{
		Name:        "cancel_reminder",
		Description: "Cancel an active reminder by its ID. Leaves the row in the DB as a tombstone; use delete_reminder afterwards to purge it.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"integer","description":"Reminder ID to cancel"}},"required":["id"]}`),
		Execute:     makeCancelReminderExecute(db, signalCh),
	}

	deleteSkill := &Skill{
		Name:        "delete_reminder",
		Description: "Permanently delete a cancelled or fired reminder by its ID. Refuses to delete active (pending) reminders — cancel_reminder first, then delete_reminder.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"integer","description":"Reminder ID to delete (must be cancelled or fired)"}},"required":["id"]}`),
		Execute:     makeDeleteReminderExecute(db),
	}

	updateSkill := &Skill{
		Name:        "update_reminder",
		Description: "Update an existing pending reminder in place. Partial update: only fields you provide change; omitted fields stay as-is. Use this to rename/refine a reminder's title or prompt without losing the original prompt body (common for recurring cron tasks). Only updates pending reminders — for cancelled/fired rows, create a fresh one with set_reminder. fire_at is ignored for recurring reminders (cron_expr drives schedule).",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"integer","description":"Reminder ID to update"},"message":{"type":"string","description":"New label/title (omit to keep current)"},"fire_at":{"type":"string","description":"New ISO 8601 fire time (omit to keep current). Ignored if the reminder is recurring."},"recurring":{"type":"string","description":"New cron expression (omit to keep current). Cannot clear — for one-time, recreate with set_reminder."},"prompt":{"type":"string","description":"New Claude prompt (omit to keep current)."},"model":{"type":"string","description":"New model override (omit to keep current)."},"effort":{"type":"string","enum":["","low","medium","high","xhigh","max"],"description":"New thinking effort override (omit to keep current). xhigh requires Claude Opus 4.7+."}},"required":["id"]}`),
		Execute:     makeUpdateReminderExecute(db, signalCh, locFn),
	}

	return []*Skill{setSkill, listSkill, getSkill, cancelSkill, deleteSkill, updateSkill}, nil
}

type setReminderInput struct {
	Message   string `json:"message"`
	FireAt    string `json:"fire_at"`
	Recurring string `json:"recurring"`
	Prompt    string `json:"prompt"`
	Model     string `json:"model"`
	Effort    string `json:"effort"`
}

// validateReminderMessage enforces the 2000-rune cap shared by set_reminder
// and update_reminder, and rejects newlines so a hostile message can't spoof
// a sibling reminder header at column 0 in list_reminders/get_reminder
// output. Returns nil for empty strings — callers decide whether empty is
// "don't change" (update) or "required" (set).
func validateReminderMessage(s string) error {
	if len([]rune(s)) > 2000 {
		return fmt.Errorf("message too long (max 2000 characters)")
	}
	if strings.ContainsAny(s, "\r\n") {
		return fmt.Errorf("message must not contain newlines")
	}
	return nil
}

// validateReminderPrompt enforces the 5000-rune cap. Empty is accepted (means
// "no prompt" for set, "don't change" for update). Newlines ARE allowed —
// prompts are wrapped in <user_prompt_body> tags by renderPromptBody and
// every line is indented, so multi-line content can't spoof headers.
func validateReminderPrompt(s string) error {
	if len([]rune(s)) > 5000 {
		return fmt.Errorf("prompt too long (max 5000 characters)")
	}
	return nil
}

// validateReminderEffort accepts only the canonical effort levels
// (delegated to config.ValidEffort, the single source of truth for the
// enum). Returns nil for empty strings — callers decide whether empty
// means "don't change" (update) or "use config default" (set).
func validateReminderEffort(s string) error {
	if s == "" {
		return nil
	}
	if !config.ValidEffort(config.Effort(s)) {
		return fmt.Errorf("effort must be one of low, medium, high, xhigh, max; got %q", s)
	}
	return nil
}

// validateReminderModel caps the model identifier at 100 runes and rejects
// newlines for the same anti-spoof reason as validateReminderMessage —
// the model renders raw as `[model: X]` on the title line. Real Anthropic
// model IDs are short alphanumeric+dash strings (e.g. "claude-haiku-4-5"),
// well under the cap. Returns nil for empty strings.
func validateReminderModel(s string) error {
	if len([]rune(s)) > 100 {
		return fmt.Errorf("model too long (max 100 characters)")
	}
	if strings.ContainsAny(s, "\r\n") {
		return fmt.Errorf("model must not contain newlines")
	}
	return nil
}

// sanitizeForHeader replaces newlines with single spaces so that a value
// rendered inline on a reminder header line can never split into multiple
// lines and spoof a sibling. Belt-and-suspenders defense for any
// pre-validation row that may carry embedded newlines (writes go through
// validateReminderMessage/Model now, but historical data and any future
// direct DB writer aren't covered by validation).
func sanitizeForHeader(s string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
}

// parseFireAt parses an ISO 8601 timestamp in the configured location, falling
// back to RFC3339 for inputs carrying their own offset. Shared by set_reminder
// and update_reminder so both accept the same formats.
func parseFireAt(s string, loc *time.Location) (time.Time, error) {
	fireAt, err := time.ParseInLocation("2006-01-02T15:04:05", s, loc)
	if err == nil {
		return fireAt, nil
	}
	fireAt, rfcErr := time.Parse(time.RFC3339, s)
	if rfcErr != nil {
		return time.Time{}, fmt.Errorf("invalid fire_at format (use ISO 8601, e.g. 2025-01-15T09:00:00): %w", rfcErr)
	}
	return fireAt, nil
}

// validateCronExpr parses via robfig/cron/v3 standard parser so users get
// immediate feedback rather than a silent scheduling failure later.
func validateCronExpr(s string) error {
	if _, err := cron.ParseStandard(s); err != nil {
		return fmt.Errorf("invalid cron expression %q: %w", s, err)
	}
	return nil
}

func makeSetReminderExecute(db *sql.DB, signalCh chan<- int64, locFn func() *time.Location) func(ctx context.Context, input json.RawMessage) (string, error) {
	return func(ctx context.Context, input json.RawMessage) (string, error) {
		// Snapshot the location once per invocation so any concurrent
		// set_timezone call can't change parsing semantics mid-execution.
		loc := locFn()
		var params setReminderInput
		if err := json.Unmarshal(input, &params); err != nil {
			return "", fmt.Errorf("invalid input: %w", err)
		}
		if params.Message == "" {
			return "", fmt.Errorf("message is required")
		}
		if err := validateReminderMessage(params.Message); err != nil {
			return "", err
		}
		if err := validateReminderPrompt(params.Prompt); err != nil {
			return "", err
		}
		if err := validateReminderModel(params.Model); err != nil {
			return "", err
		}
		if err := validateReminderEffort(params.Effort); err != nil {
			return "", err
		}
		if params.FireAt == "" {
			return "", fmt.Errorf("fire_at is required")
		}

		fireAt, err := parseFireAt(params.FireAt, loc)
		if err != nil {
			return "", err
		}

		user := GetUser(ctx)
		now := time.Now().UTC()
		fireAtUTC := fireAt.UTC()

		var cronExpr *string
		if params.Recurring != "" {
			if err := validateCronExpr(params.Recurring); err != nil {
				return "", err
			}
			cronExpr = &params.Recurring
		}

		var prompt *string
		if params.Prompt != "" {
			prompt = &params.Prompt
		}
		var model *string
		if params.Model != "" {
			model = &params.Model
		}
		var effort *string
		if params.Effort != "" {
			effort = &params.Effort
		}

		res, err := db.ExecContext(ctx,
			`INSERT INTO reminders (user_id, chat_id, message, fire_at, cron_expr, prompt, model, effort, status, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?)`,
			user.UserID, user.ChatID, params.Message, fireAtUTC, cronExpr, prompt, model, effort, now,
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
	Offset int    `json:"offset"`
}

func makeListRemindersExecute(db *sql.DB, locFn func() *time.Location) func(ctx context.Context, input json.RawMessage) (string, error) {
	return func(ctx context.Context, input json.RawMessage) (string, error) {
		loc := locFn()
		var params listRemindersInput
		if err := json.Unmarshal(input, &params); err != nil {
			return "", fmt.Errorf("invalid input: %w", err)
		}
		if params.Offset < 0 {
			return "", fmt.Errorf("offset must be >= 0")
		}

		user := GetUser(ctx)

		// Default: active reminders only. Pass status="all" to include
		// cancelled + fired history. Empty status is coerced to "pending"
		// so `list_reminders` doesn't dump tombstones by default — this
		// matches the common "what's scheduled right now?" query shape.
		statusFilter := params.Status
		if statusFilter == "" {
			statusFilter = "pending"
		}

		// Fetch limit+1 so we can detect "more rows exist" without a
		// second COUNT query. If we get back limit+1 rows, the user has
		// hit the cap and we render only the first `limit` plus an
		// overflow notice pointing at the next offset. Pagination is
		// offset-based (not cursor-based) because the underlying dataset
		// is small (typically <100 rows) and ORDER BY (fire_at, id) is
		// already stable — a cursor buys no durability here.
		var rows *sql.Rows
		var err error
		if statusFilter != "all" {
			rows, err = db.QueryContext(ctx,
				`SELECT id, message, fire_at, cron_expr, prompt, model, effort, status, created_at FROM reminders WHERE user_id = ? AND status = ? ORDER BY fire_at, id LIMIT ? OFFSET ?`,
				user.UserID, statusFilter, listRemindersLimit+1, params.Offset,
			)
		} else {
			rows, err = db.QueryContext(ctx,
				`SELECT id, message, fire_at, cron_expr, prompt, model, effort, status, created_at FROM reminders WHERE user_id = ? ORDER BY fire_at, id LIMIT ? OFFSET ?`,
				user.UserID, listRemindersLimit+1, params.Offset,
			)
		}
		if err != nil {
			return "", fmt.Errorf("list reminders: %w", err)
		}
		defer rows.Close()

		// Accumulate rows in `body`; the page header (when paginating) is
		// prepended AFTER we know count > 0, so a paged-past-end response
		// doesn't carry a stray "(page starting at offset=N)" that the
		// empty-page early-return would discard anyway.
		var body strings.Builder
		count := 0
		overflow := false
		for rows.Next() {
			if count >= listRemindersLimit {
				overflow = true
				break
			}
			var id int64
			var message, status string
			var fireAt, createdAt time.Time
			var cronExpr, prompt, model, effort *string
			if err := rows.Scan(&id, &message, &fireAt, &cronExpr, &prompt, &model, &effort, &status, &createdAt); err != nil {
				return "", fmt.Errorf("scan reminder: %w", err)
			}
			count++
			localFire := fireAt.In(loc).Format("2006-01-02 15:04")
			tag := status
			if prompt != nil {
				tag = "cron:" + status
			}
			// Sanitize message + model at render so any pre-validation
			// row with embedded newlines can't spoof a sibling header.
			// validate*() also rejects newlines now, but render-time
			// sanitization is the belt that catches the suspenders.
			fmt.Fprintf(&body, "#%d [%s] %s — %s", id, tag, localFire, sanitizeForHeader(message))
			if cronExpr != nil {
				fmt.Fprintf(&body, " (recurring: %s)", sanitizeForHeader(*cronExpr))
			}
			if model != nil && *model != "" {
				fmt.Fprintf(&body, " [model: %s]", sanitizeForHeader(*model))
			}
			if effort != nil && *effort != "" {
				fmt.Fprintf(&body, " [effort: %s]", sanitizeForHeader(*effort))
			}
			if prompt != nil && *prompt != "" {
				truncated := renderPromptBody(&body, *prompt, listPromptPreviewRunes)
				if truncated {
					fmt.Fprintf(&body, "\n  (truncated; use get_reminder id=%d for full body)", id)
				}
			}
			body.WriteByte('\n')
		}
		if err := rows.Err(); err != nil {
			return "", fmt.Errorf("iterate reminders: %w", err)
		}

		if count == 0 {
			// Empty-page messages differ by offset: at offset=0 the user
			// has no reminders of this kind; at offset>0 they've paged
			// past the end. Include the status filter in both branches
			// so the agent can tell whether the empty result is a
			// filter-too-narrow or an out-of-range-offset situation.
			filterHint := ""
			if statusFilter != "all" {
				filterHint = fmt.Sprintf(" %s", statusFilter)
			}
			if params.Offset > 0 {
				return fmt.Sprintf("No%s reminders at offset=%d. Try a smaller offset (or omit offset to start from the beginning).", filterHint, params.Offset), nil
			}
			if statusFilter == "all" {
				return "No reminders found", nil
			}
			return fmt.Sprintf("No %s reminders found (pass status=\"all\" to see cancelled/fired history)", statusFilter), nil
		}

		// Prepend page header now that we know count > 0 (no header on a
		// paged-past-end response).
		var result strings.Builder
		if params.Offset > 0 {
			fmt.Fprintf(&result, "(page starting at offset=%d)\n", params.Offset)
		}
		result.WriteString(body.String())

		if overflow {
			// Guard against integer overflow near math.MaxInt. Practically
			// unreachable (would need trillions of reminders) but the
			// wrap would suggest a negative next offset, which the agent
			// would then reject via the validator — noisy self-correction
			// we'd rather not emit.
			if params.Offset > math.MaxInt-listRemindersLimit {
				result.WriteString("\n(more results exist; cannot compute next offset due to overflow — narrow the filter instead.)\n")
			} else {
				nextOffset := params.Offset + listRemindersLimit
				fmt.Fprintf(&result, "\n(more results exist — call list_reminders again with offset=%d for the next page.)\n", nextOffset)
			}
		}

		return result.String(), nil
	}
}

// makeGetReminderExecute returns a single reminder by ID with the FULL
// prompt body (no truncation). IDOR-safe: cross-user IDs return the same
// "not found" string as missing IDs so callers can't probe for existence.
func makeGetReminderExecute(db *sql.DB, locFn func() *time.Location) func(ctx context.Context, input json.RawMessage) (string, error) {
	return func(ctx context.Context, input json.RawMessage) (string, error) {
		loc := locFn()
		var params struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(input, &params); err != nil {
			return "", fmt.Errorf("invalid input: %w", err)
		}
		if params.ID <= 0 {
			return "", fmt.Errorf("id must be a positive integer")
		}

		user := GetUser(ctx)

		var (
			id                int64
			message, status   string
			fireAt, createdAt time.Time
			cronExpr, prompt  *string
			model, effort     *string
		)
		err := db.QueryRowContext(ctx,
			`SELECT id, message, fire_at, cron_expr, prompt, model, effort, status, created_at FROM reminders WHERE id = ? AND user_id = ?`,
			params.ID, user.UserID,
		).Scan(&id, &message, &fireAt, &cronExpr, &prompt, &model, &effort, &status, &createdAt)
		if err == sql.ErrNoRows {
			return fmt.Sprintf("reminder #%d not found", params.ID), nil
		}
		if err != nil {
			return "", fmt.Errorf("get reminder: %w", err)
		}

		var result strings.Builder
		localFire := fireAt.In(loc).Format("2006-01-02 15:04")
		localCreated := createdAt.In(loc).Format("2006-01-02 15:04")
		tag := status
		if prompt != nil {
			tag = "cron:" + status
		}
		fmt.Fprintf(&result, "#%d [%s] %s — %s", id, tag, localFire, sanitizeForHeader(message))
		if cronExpr != nil {
			fmt.Fprintf(&result, " (recurring: %s)", sanitizeForHeader(*cronExpr))
		}
		if model != nil && *model != "" {
			fmt.Fprintf(&result, " [model: %s]", sanitizeForHeader(*model))
		}
		if effort != nil && *effort != "" {
			fmt.Fprintf(&result, " [effort: %s]", sanitizeForHeader(*effort))
		}
		fmt.Fprintf(&result, "\n  created: %s", localCreated)
		if prompt != nil && *prompt != "" {
			renderPromptBody(&result, *prompt, 0) // 0 = no truncation
		}
		return result.String(), nil
	}
}

type cancelReminderInput struct {
	ID int64 `json:"id"`
}

type deleteReminderInput struct {
	ID int64 `json:"id"`
}

type updateReminderInput struct {
	ID        int64  `json:"id"`
	Message   string `json:"message"`
	FireAt    string `json:"fire_at"`
	Recurring string `json:"recurring"`
	Prompt    string `json:"prompt"`
	Model     string `json:"model"`
	Effort    string `json:"effort"`
}

// makeUpdateReminderExecute patches a pending reminder in place. Empty-string
// means "don't change", matching the convention in update_observation. The
// signal is fired on every successful update (not just schedule changes) so
// the actor drops the stale gocron closure and picks up new message/prompt/
// model values on the next fire.
func makeUpdateReminderExecute(db *sql.DB, signalCh chan<- int64, locFn func() *time.Location) func(ctx context.Context, input json.RawMessage) (string, error) {
	return func(ctx context.Context, input json.RawMessage) (string, error) {
		loc := locFn()
		var params updateReminderInput
		if err := json.Unmarshal(input, &params); err != nil {
			return "", fmt.Errorf("invalid input: %w", err)
		}
		if params.ID == 0 {
			return "", fmt.Errorf("id is required")
		}
		if params.Message == "" && params.FireAt == "" && params.Recurring == "" && params.Prompt == "" && params.Model == "" && params.Effort == "" {
			return "", fmt.Errorf("at least one field (message, fire_at, recurring, prompt, model, effort) must be provided")
		}

		user := GetUser(ctx)

		// Look up first so we can produce the right error: not-found vs
		// not-pending. Cross-user IDs collapse to "not found" to avoid
		// leaking existence of another user's rows (IDOR-safe, same shape
		// as delete_reminder). We also pull cron_expr because we need to
		// know the current recurrence state to validate fire_at updates.
		var status string
		var currentCronExpr *string
		err := db.QueryRowContext(ctx,
			`SELECT status, cron_expr FROM reminders WHERE id = ? AND user_id = ?`,
			params.ID, user.UserID,
		).Scan(&status, &currentCronExpr)
		if err == sql.ErrNoRows {
			return "", fmt.Errorf("reminder #%d not found", params.ID)
		}
		if err != nil {
			return "", fmt.Errorf("lookup reminder: %w", err)
		}
		if status != "pending" {
			return "", fmt.Errorf("reminder #%d is %s, not pending; call set_reminder to create a new one", params.ID, status)
		}

		// Reject fire_at on recurring reminders: gocron uses cron_expr not
		// fire_at to pick next run, but fire_at is stored and passed to
		// CronExecutor as the reported scheduledAt. Letting update_reminder
		// rewrite it silently corrupts what Claude reports as the scheduled
		// fire time. The only legitimate way to reschedule a recurring
		// reminder is via `recurring`. Callers updating fire_at AND clearing
		// to one-time are not supported (can't clear optional fields here —
		// that requires cancel+recreate).
		if params.FireAt != "" && currentCronExpr != nil && params.Recurring == "" {
			return "", fmt.Errorf("reminder #%d is recurring; fire_at is not applicable. Use `recurring` to change the cron schedule, or cancel + set_reminder to convert to one-time", params.ID)
		}

		// Validate each provided field before touching the DB.
		setClauses := make([]string, 0, 5)
		args := make([]any, 0, 7)

		if params.Message != "" {
			if err := validateReminderMessage(params.Message); err != nil {
				return "", err
			}
			setClauses = append(setClauses, "message = ?")
			args = append(args, params.Message)
		}
		if params.FireAt != "" {
			fireAt, err := parseFireAt(params.FireAt, loc)
			if err != nil {
				return "", err
			}
			setClauses = append(setClauses, "fire_at = ?")
			args = append(args, fireAt.UTC())
		}
		if params.Recurring != "" {
			if err := validateCronExpr(params.Recurring); err != nil {
				return "", err
			}
			setClauses = append(setClauses, "cron_expr = ?")
			args = append(args, params.Recurring)
		}
		if params.Prompt != "" {
			if err := validateReminderPrompt(params.Prompt); err != nil {
				return "", err
			}
			setClauses = append(setClauses, "prompt = ?")
			args = append(args, params.Prompt)
		}
		if params.Model != "" {
			if err := validateReminderModel(params.Model); err != nil {
				return "", err
			}
			setClauses = append(setClauses, "model = ?")
			args = append(args, params.Model)
		}
		if params.Effort != "" {
			if err := validateReminderEffort(params.Effort); err != nil {
				return "", err
			}
			setClauses = append(setClauses, "effort = ?")
			args = append(args, params.Effort)
		}

		// TOCTOU guard: gate UPDATE with status='pending'. If the row flipped
		// to cancelled/fired between our lookup and this update, rowsAffected
		// will be 0 and we surface a retry-friendly error instead of silently
		// patching a tombstone.
		query := fmt.Sprintf(
			`UPDATE reminders SET %s WHERE id = ? AND user_id = ? AND status = 'pending'`,
			strings.Join(setClauses, ", "),
		)
		args = append(args, params.ID, user.UserID)

		res, err := db.ExecContext(ctx, query, args...)
		if err != nil {
			return "", fmt.Errorf("update reminder: %w", err)
		}
		affected, _ := res.RowsAffected()
		if affected == 0 {
			return "", fmt.Errorf("reminder #%d is no longer pending; refresh with list_reminders and retry", params.ID)
		}

		// Always signal the actor so it re-reads from DB and replaces the
		// stale gocron closure. Without this, message/prompt/model edits
		// wouldn't take effect until the next container restart because the
		// original closure captures reminderRow by value at schedule time.
		signalTimer := time.NewTimer(5 * time.Second)
		defer signalTimer.Stop()
		select {
		case signalCh <- params.ID:
		case <-signalTimer.C:
			slog.Error("remind signal channel full after 5s", "id", params.ID)
			return "", fmt.Errorf("reminder #%d updated in database but scheduler is unresponsive; changes will take effect on next restart", params.ID)
		}

		return fmt.Sprintf("Updated reminder #%d", params.ID), nil
	}
}

func makeDeleteReminderExecute(db *sql.DB) func(ctx context.Context, input json.RawMessage) (string, error) {
	return func(ctx context.Context, input json.RawMessage) (string, error) {
		var params deleteReminderInput
		if err := json.Unmarshal(input, &params); err != nil {
			return "", fmt.Errorf("invalid input: %w", err)
		}
		if params.ID == 0 {
			return "", fmt.Errorf("id is required")
		}

		user := GetUser(ctx)

		// Look up the row first so we can give the right error — "not found"
		// vs "is active, cancel first". Without this, both cases collapse to
		// a single opaque "0 rows affected" that the agent can't autorepair.
		var status string
		err := db.QueryRowContext(ctx,
			`SELECT status FROM reminders WHERE id = ? AND user_id = ?`,
			params.ID, user.UserID,
		).Scan(&status)
		if err == sql.ErrNoRows {
			return "", fmt.Errorf("reminder #%d not found", params.ID)
		}
		if err != nil {
			return "", fmt.Errorf("lookup reminder: %w", err)
		}
		if status == "pending" {
			return "", fmt.Errorf("reminder #%d is active (status=pending); call cancel_reminder first, then delete_reminder", params.ID)
		}

		// Safe to delete: status is 'cancelled' or 'fired'. User scope re-checked
		// in WHERE as defense in depth even though the lookup already enforced it.
		res, err := db.ExecContext(ctx,
			`DELETE FROM reminders WHERE id = ? AND user_id = ? AND status != 'pending'`,
			params.ID, user.UserID,
		)
		if err != nil {
			return "", fmt.Errorf("delete reminder: %w", err)
		}
		affected, _ := res.RowsAffected()
		if affected == 0 {
			// Only reachable if status raced from cancelled → pending between
			// the lookup and the delete, which shouldn't happen in practice but
			// is cheap to guard against.
			return "", fmt.Errorf("reminder #%d not deleted (status may have changed); retry", params.ID)
		}

		return fmt.Sprintf("Deleted reminder #%d (was %s)", params.ID, status), nil
	}
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
	db         *sql.DB
	tgInbox    chan<- telegram.OutgoingMessage
	locFn      func() *time.Location // returns the current effective location; called fresh for each TZ-aware operation
	signalCh   <-chan int64
	tzChangeCh <-chan struct{}       // nil-safe: nil channel never fires; set_timezone signals here
	cronExec   CronRunner            // nil = no cron task support (static text only)
	persister  ConversationPersister // nil = cron output goes to Telegram only, not chat history

	mu      sync.Mutex
	jobs    map[int64]gocron.Job
	jobMeta map[int64]scheduleSnapshot // per-id snapshot of schedule-affecting fields at last schedule time
	lastLoc *time.Location             // last location applied to the gocron scheduler; gate to skip no-op rebuilds. Read/written only from the Run goroutine.
}

// scheduleSnapshot records the schedule-affecting fields (FireAt for one-time,
// CronExpr for recurring) captured when a reminder was last scheduled. Used by
// pollNewReminders to detect in-DB edits that arrived via a channel that
// doesn't reach this actor — specifically the MCP subprocess's drained
// signalCh in CLI mode. Without this, update_reminder would be a no-op in CLI
// mode: DB gets updated, scheduler keeps firing with the old timing.
type scheduleSnapshot struct {
	FireAt   time.Time
	CronExpr *string
}

// NewReminderActor creates a new ReminderActor. cronExec may be nil to disable
// Claude-powered cron tasks (reminders with prompts will fall back to static text).
// tzChangeCh is the wakeup signal from the set_timezone skill; nil disables
// signal-driven rebuild (the 10s poll path still detects DB-only TZ changes).
// locFn returns the current effective location and is called fresh on each
// scheduler-rebuild check, so a runtime override picked up via memory.SetTimezoneOverride
// reaches the gocron scheduler without a process restart.
func NewReminderActor(
	db *sql.DB,
	tgInbox chan<- telegram.OutgoingMessage,
	locFn func() *time.Location,
	signalCh <-chan int64,
	tzChangeCh <-chan struct{},
	cronExec CronRunner,
) *ReminderActor {
	return &ReminderActor{
		db:         db,
		tgInbox:    tgInbox,
		locFn:      locFn,
		signalCh:   signalCh,
		tzChangeCh: tzChangeCh,
		cronExec:   cronExec,
		jobs:       make(map[int64]gocron.Job),
		jobMeta:    make(map[int64]scheduleSnapshot),
	}
}

// SetConversationPersister enables writing cron-task turns to the conversation
// history so the interactive agent sees its own cron output on the next user
// turn. Optional — if unset, cron messages are sent to Telegram only (the
// original pre-v0.36.8 behavior). Must be called before Run() to avoid racing
// with fireCronTask goroutines.
func (ra *ReminderActor) SetConversationPersister(p ConversationPersister) {
	ra.persister = p
}

// Name implements actor.Actor.
func (ra *ReminderActor) Name() string { return "reminder" }

// newCronScheduler returns a gocron scheduler bound to loc so cron expressions
// like "0 6 * * *" evaluate in the user's configured timezone, not the
// container's local time. Without this, a UTC container with a PDT-configured
// user fires "0 6 * * *" at 06:00 UTC = 23:00 PDT the previous day — 7 hours
// early. Regression guard: skills/remind_test.go:TestNewCronScheduler_FiresInConfiguredLocation.
func newCronScheduler(loc *time.Location) (gocron.Scheduler, error) {
	return gocron.NewScheduler(gocron.WithLocation(loc))
}

// Run implements actor.Actor. It starts a gocron scheduler, loads all pending
// reminders, fires past-due ones immediately, and schedules future ones.
// It then listens for signals to add or cancel reminders.
//
// Two TZ-change paths feed maybeRebuildScheduler: the explicit tzChangeCh
// signal from the set_timezone skill (immediate in API mode), and a check on
// every 10s pollTicker (catches CLI-mode set_timezone calls where the signal
// channel drains to /dev/null in the MCP subprocess, same precedent as
// cancel_reminder).
func (ra *ReminderActor) Run(ctx context.Context) error {
	initLoc := ra.locFn()
	ra.mu.Lock()
	ra.lastLoc = initLoc
	ra.mu.Unlock()
	scheduler, err := newCronScheduler(initLoc)
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
		case <-ra.tzChangeCh:
			scheduler = ra.maybeRebuildScheduler(ctx, scheduler)
		case <-pollTicker.C:
			ra.pollNewReminders(ctx, scheduler)
			// CLI-mode parity: the set_timezone skill in the MCP subprocess
			// can't reach tzChangeCh, so detect TZ drift via DB read here.
			scheduler = ra.maybeRebuildScheduler(ctx, scheduler)
		}
	}
}

// maybeRebuildScheduler compares the current effective location against
// ra.lastLoc and, if they differ, shuts down `current`, returns a fresh
// scheduler bound to the new location with all pending reminders re-loaded.
// Returns `current` unchanged when no rebuild is needed.
//
// gocron.WithLocation is fixed at scheduler-creation time, so swapping
// timezones requires a full Shutdown + new Scheduler. We wrap Shutdown in a
// 30s timeout: an in-flight cron-Claude task can otherwise hold us hostage,
// since gocron.Shutdown blocks until every running job returns. After the
// timeout we abandon the old scheduler and create a fresh one anyway. The
// orphaned scheduler GCs naturally once any stuck job exits.
func (ra *ReminderActor) maybeRebuildScheduler(ctx context.Context, current gocron.Scheduler) gocron.Scheduler {
	newLoc := ra.locFn()
	ra.mu.Lock()
	prevLoc := ra.lastLoc
	ra.mu.Unlock()
	if prevLoc != nil && newLoc.String() == prevLoc.String() {
		return current
	}

	oldName := "<nil>"
	if prevLoc != nil {
		oldName = prevLoc.String()
	}
	slog.Info("reminder: timezone changed, rebuilding scheduler", "old", oldName, "new", newLoc.String())

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- current.Shutdown() }()
	select {
	case err := <-shutdownDone:
		if err != nil {
			slog.Error("reminder: scheduler shutdown error during TZ rebuild", "err", err)
		}
	case <-time.After(30 * time.Second):
		slog.Error("reminder: scheduler shutdown timed out after 30s during TZ rebuild; abandoning old scheduler. An in-flight cron task may still be running")
	}

	// Drop tracked job references; loadPendingReminders re-populates them
	// against the new scheduler.
	ra.mu.Lock()
	ra.jobs = make(map[int64]gocron.Job)
	ra.jobMeta = make(map[int64]scheduleSnapshot)
	ra.mu.Unlock()

	next, err := newCronScheduler(newLoc)
	if err != nil {
		slog.Error("reminder: new scheduler creation failed during TZ rebuild; falling back to old location", "err", err, "loc", newLoc.String())
		// Fallback: keep the old location so the actor stays alive.
		next, err = newCronScheduler(prevLoc)
		if err != nil {
			slog.Error("reminder: fallback scheduler creation also failed; reminders are now offline until supervisor restart", "err", err)
			return current
		}
	} else {
		ra.mu.Lock()
		ra.lastLoc = newLoc
		ra.mu.Unlock()
	}
	next.Start()

	if err := ra.loadPendingReminders(ctx, next); err != nil {
		slog.Error("reminder: failed to load pending reminders after TZ rebuild", "err", err)
	}
	return next
}

// loadPendingReminders queries all pending reminders and schedules or fires them.
// It collects all rows first to release the DB connection before processing,
// which avoids deadlocks with single-connection pools (e.g., in-memory SQLite).
func (ra *ReminderActor) loadPendingReminders(ctx context.Context, scheduler gocron.Scheduler) error {
	rows, err := ra.db.QueryContext(ctx,
		`SELECT id, user_id, chat_id, message, fire_at, cron_expr, prompt, model, effort FROM reminders WHERE status = 'pending'`,
	)
	if err != nil {
		return fmt.Errorf("query pending reminders: %w", err)
	}

	var reminders []reminderRow
	for rows.Next() {
		var r reminderRow
		if err := rows.Scan(&r.ID, &r.UserID, &r.ChatID, &r.Message, &r.FireAt, &r.CronExpr, &r.Prompt, &r.Model, &r.Effort); err != nil {
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

	for _, r := range reminders {
		ra.scheduleOrFire(scheduler, r)
	}
	return nil
}

// scheduleOrFire schedules a reminder via gocron, or fires it immediately if
// it's a past-due one-time reminder (gocron.OneTimeJob behavior with a past
// start time is implementation-defined — may silently no-op, which would mean
// users never get their reminder). Used by loadPendingReminders on startup,
// handleSignal on API-mode update signals, and pollNewReminders Phase 1.5
// for CLI-mode schedule drift, so all three paths have matching semantics
// when update_reminder sets fire_at to a past time.
func (ra *ReminderActor) scheduleOrFire(scheduler gocron.Scheduler, r reminderRow) {
	if r.CronExpr == nil && r.FireAt.Before(time.Now().UTC()) {
		ra.fireReminder(r)
		return
	}
	ra.scheduleReminder(scheduler, r)
}

// handleSignal processes a signal for a reminder ID. It queries the reminder
// and syncs the gocron schedule to current DB state — cancelling if the row
// is cancelled, (re)scheduling if pending. For updates (where the row was
// already pending and is now pending with different fields), it cancels the
// existing job first before creating a new one. Without that, scheduleReminder
// would leave the old gocron job in the scheduler firing at the old time while
// adding a second job at the new time. It would also keep the gocron closure
// capturing a stale reminderRow, so title/prompt updates wouldn't surface
// until the next container restart.
func (ra *ReminderActor) handleSignal(ctx context.Context, scheduler gocron.Scheduler, id int64) {
	var r reminderRow
	var status string
	err := ra.db.QueryRowContext(ctx,
		`SELECT id, user_id, chat_id, message, fire_at, cron_expr, prompt, model, effort, status FROM reminders WHERE id = ?`,
		id,
	).Scan(&r.ID, &r.UserID, &r.ChatID, &r.Message, &r.FireAt, &r.CronExpr, &r.Prompt, &r.Model, &r.Effort, &status)
	if err != nil {
		slog.Error("reminder: query signal target", "id", id, "err", err)
		return
	}

	if status == "cancelled" {
		ra.cancelJob(scheduler, id)
		return
	}

	if status == "pending" {
		ra.mu.Lock()
		_, existing := ra.jobs[id]
		ra.mu.Unlock()
		if existing {
			ra.cancelJob(scheduler, id)
		}
		ra.scheduleOrFire(scheduler, r)
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
			delete(ra.jobMeta, r.ID)
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
	ra.jobMeta[r.ID] = scheduleSnapshot{FireAt: r.FireAt, CronExpr: r.CronExpr}
	ra.mu.Unlock()

	slog.Info("reminder: scheduled", "id", r.ID, "fire_at", r.FireAt)
}

// fireReminder sends the reminder message via Telegram and updates the DB status.
// If the reminder has a prompt and CronRunner is available, it invokes Claude instead.
// Re-reads the full row from DB before firing so message/prompt/model edits
// made via update_reminder propagate without requiring a reschedule. This
// matters especially in CLI mode where update_reminder's signal drains to
// /dev/null in the MCP subprocess; the scheduled gocron closure captured the
// row by value at schedule time, so without this re-read, the fire would use
// stale content. Status is still checked to skip fires for cancelled rows.
func (ra *ReminderActor) fireReminder(r reminderRow) {
	var fresh reminderRow
	var status string
	err := ra.db.QueryRow(
		`SELECT id, user_id, chat_id, message, fire_at, cron_expr, prompt, model, effort, status FROM reminders WHERE id = ?`,
		r.ID,
	).Scan(&fresh.ID, &fresh.UserID, &fresh.ChatID, &fresh.Message, &fresh.FireAt, &fresh.CronExpr, &fresh.Prompt, &fresh.Model, &fresh.Effort, &status)
	if err != nil {
		slog.Error("reminder: failed to re-read before firing", "id", r.ID, "err", err)
		return
	}
	if status != "pending" {
		slog.Info("reminder: skipping fire, status changed", "id", r.ID, "status", status)
		return
	}

	// Preserve the originally-scheduled fire time so cron-task output reports
	// the scheduled time we actually fired at (not a fire_at that may have
	// been rewritten post-schedule). fresh is otherwise authoritative.
	fresh.FireAt = r.FireAt

	if fresh.Prompt != nil && *fresh.Prompt != "" && ra.cronExec != nil {
		ra.fireCronTask(fresh)
		return
	}

	msg := telegram.OutgoingMessage{
		ChatID: fresh.ChatID,
		Text:   "Reminder: " + fresh.Message,
	}

	// Blocking send with 5s timeout. Reminders are too important to silently drop.
	if !ra.trySendTelegram(fresh.ID, msg) {
		return // Don't update status — will retry on next startup.
	}

	ra.markFiredIfOneTime(fresh)
}

// fireCronTask invokes Claude with the reminder's prompt and sends the result.
func (ra *ReminderActor) fireCronTask(r reminderRow) {
	ctx, cancel := context.WithTimeout(context.Background(), cronExecTimeout)
	defer cancel()

	slog.Info("reminder: executing cron task", "id", r.ID, "chat_id", r.ChatID)

	cronModel := ""
	if r.Model != nil {
		cronModel = *r.Model
	}
	cronEffort := ""
	if r.Effort != nil {
		cronEffort = *r.Effort
	}
	result, err := ra.cronExec.Execute(ctx, r.UserID, r.ChatID, *r.Prompt, cronModel, cronEffort, r.FireAt)
	if err != nil {
		slog.Error("reminder: cron task failed", "id", r.ID, "err", err)
		errText := fmt.Sprintf("[Cron task failed] %s: %v", r.Message, err)
		errMsg := telegram.OutgoingMessage{ChatID: r.ChatID, Text: errText, HTML: true}
		ra.trySendTelegram(r.ID, errMsg)
		ra.persistCronTurn(r, errText)
		// For one-time: still mark fired (error was delivered).
		// For recurring: keep pending (will retry next schedule).
		if r.CronExpr == nil {
			ra.markFiredIfOneTime(r)
		}
		return
	}

	// HTML: true so the Telegram channel runs mdhtml.ConvertSafe and sends
	// with ParseMode=HTML. Without it, Claude's markdown output (**bold**,
	// ### headers, [links](url), - bullets) renders as raw characters in
	// Telegram. Interactive session sets HTML: true on every Claude-output
	// send (internal/session/actor.go); the cron path needs the same.
	assistantText := fmt.Sprintf("**%s**\n\n%s", r.Message, result)
	msg := telegram.OutgoingMessage{ChatID: r.ChatID, Text: assistantText, HTML: true}
	if !ra.trySendTelegram(r.ID, msg) {
		return
	}
	ra.persistCronTurn(r, assistantText)

	ra.markFiredIfOneTime(r)
}

// persistCronTurn records the cron-fired turn in the conversation history so
// the interactive agent can see its own cron output on the next user turn.
// Best-effort: every failure is logged as a warning but never aborts the flow
// — the Telegram message was already delivered, and persistence is a
// supporting concern. No-op when no ConversationPersister is configured.
func (ra *ReminderActor) persistCronTurn(r reminderRow, assistantText string) {
	if ra.persister == nil {
		return
	}
	convID, _, err := ra.persister.GetActiveConversation(r.UserID, r.ChatID, "")
	if err != nil {
		slog.Warn("reminder: persist cron turn — get conversation", "id", r.ID, "err", err)
		return
	}
	prompt := ""
	if r.Prompt != nil {
		prompt = *r.Prompt
	}
	userText := fmt.Sprintf("[Cron reminder fired: %s]\nScheduled: %s\nPrompt: %s",
		r.Message,
		r.FireAt.In(ra.locFn()).Format("2006-01-02 15:04 MST"),
		prompt)
	userContent, _ := json.Marshal(userText) // Marshal cannot fail for a Go string.
	if err := ra.persister.AppendMessage(convID, "user", userContent); err != nil {
		slog.Warn("reminder: persist cron turn — user message", "id", r.ID, "err", err)
		return
	}
	assistantContent, _ := json.Marshal(assistantText)
	if err := ra.persister.AppendMessage(convID, "assistant", assistantContent); err != nil {
		slog.Warn("reminder: persist cron turn — assistant message", "id", r.ID, "err", err)
	}
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
// Also detects cancelled reminders and removes their gocron jobs.
func (ra *ReminderActor) pollNewReminders(ctx context.Context, scheduler gocron.Scheduler) {
	// Phase 1: Detect cancelled reminders that still have active gocron jobs.
	// This handles the CLI mode case where cancel_reminder updates the DB but
	// the signal never reaches this process (MCP subprocess drains it).
	ra.mu.Lock()
	var trackedIDs []int64
	for id := range ra.jobs {
		trackedIDs = append(trackedIDs, id)
	}
	ra.mu.Unlock()

	for _, id := range trackedIDs {
		var status string
		if err := ra.db.QueryRowContext(ctx, `SELECT status FROM reminders WHERE id = ?`, id).Scan(&status); err != nil {
			continue // Row deleted or other error, skip.
		}
		if status == "cancelled" || status == "fired" {
			slog.Info("reminder: poll detected cancelled/fired job, removing", "id", id, "status", status)
			ra.cancelJob(scheduler, id)
		}
	}

	// Phase 1.5: Detect schedule-affecting updates on tracked pending rows.
	// In CLI mode, update_reminder signals a channel that drains to /dev/null
	// in the MCP subprocess, so the live actor never hears about fire_at /
	// cron_expr changes. We compare DB against jobMeta (the snapshot captured
	// when the job was last scheduled) and reschedule on divergence. Without
	// this, update_reminder would appear to succeed but the scheduler would
	// keep firing at the old time until the next container restart. Message /
	// prompt / model edits are handled separately by fireReminder's DB
	// re-read — they don't require a reschedule.
	for _, id := range trackedIDs {
		var fireAt time.Time
		var cronExpr *string
		var status string
		if err := ra.db.QueryRowContext(ctx,
			`SELECT fire_at, cron_expr, status FROM reminders WHERE id = ?`,
			id,
		).Scan(&fireAt, &cronExpr, &status); err != nil {
			continue
		}
		if status != "pending" {
			continue // Handled by Phase 1 above.
		}

		ra.mu.Lock()
		snap, ok := ra.jobMeta[id]
		ra.mu.Unlock()
		if !ok {
			continue // Just-cancelled by Phase 1 or never scheduled; skip.
		}

		fireChanged := !snap.FireAt.Equal(fireAt)
		cronChanged := (snap.CronExpr == nil) != (cronExpr == nil) ||
			(snap.CronExpr != nil && cronExpr != nil && *snap.CronExpr != *cronExpr)
		if !fireChanged && !cronChanged {
			continue
		}

		// Reload the full row (still filtering on pending — Finding 1C: between
		// the first status check and here, the row could have flipped to
		// fired/cancelled; without this filter we'd schedule a phantom job).
		var r reminderRow
		err := ra.db.QueryRowContext(ctx,
			`SELECT id, user_id, chat_id, message, fire_at, cron_expr, prompt, model, effort FROM reminders WHERE id = ? AND status = 'pending'`,
			id,
		).Scan(&r.ID, &r.UserID, &r.ChatID, &r.Message, &r.FireAt, &r.CronExpr, &r.Prompt, &r.Model, &r.Effort)
		if err == sql.ErrNoRows {
			continue // Status raced to non-pending; Phase 1 (next tick) will clean up the stale job.
		}
		if err != nil {
			slog.Error("reminder: poll reload for reschedule", "id", id, "err", err)
			continue
		}
		slog.Info("reminder: poll detected schedule change, rescheduling", "id", id, "fire_changed", fireChanged, "cron_changed", cronChanged)
		ra.cancelJob(scheduler, id)
		ra.scheduleOrFire(scheduler, r)
	}

	// Phase 2: Find new pending reminders not yet scheduled.
	rows, err := ra.db.QueryContext(ctx,
		`SELECT id, user_id, chat_id, message, fire_at, cron_expr, prompt, model, effort FROM reminders WHERE status = 'pending'`,
	)
	if err != nil {
		slog.Error("reminder: poll query failed", "err", err)
		return
	}

	var unscheduled []reminderRow
	for rows.Next() {
		var r reminderRow
		if err := rows.Scan(&r.ID, &r.UserID, &r.ChatID, &r.Message, &r.FireAt, &r.CronExpr, &r.Prompt, &r.Model, &r.Effort); err != nil {
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
	if err := rows.Err(); err != nil {
		slog.Error("reminder: poll rows iteration error", "err", err)
	}
	rows.Close()

	for _, r := range unscheduled {
		slog.Info("reminder: poll found unscheduled reminder", "id", r.ID)
		ra.scheduleOrFire(scheduler, r)
	}
}

// cancelJob removes a scheduled job from gocron.
func (ra *ReminderActor) cancelJob(scheduler gocron.Scheduler, id int64) {
	ra.mu.Lock()
	job, ok := ra.jobs[id]
	if ok {
		delete(ra.jobs, id)
		delete(ra.jobMeta, id)
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
	Model    *string
	Effort   *string
}
