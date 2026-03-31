package memory

import (
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	_ "modernc.org/sqlite"
)

// Store provides SQLite-backed conversation and message persistence.
// It is designed to be used as a single-writer actor with WAL mode enabled.
type Store struct {
	db *sql.DB
}

// Message represents a single message in a conversation.
type Message struct {
	Role    string          `json:"role"`    // "user", "assistant", "tool_result"
	Content json.RawMessage `json:"content"` // full content block(s) as JSON
}

// NewStore creates or opens a SQLite database at dbPath, configures WAL mode
// and a 5-second busy timeout, and runs schema migrations.
func NewStore(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("memory: open db: %w", err)
	}

	// Single connection — this store is the sole writer.
	db.SetMaxOpenConns(1)

	// Enable WAL mode for concurrent reads while writing.
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("memory: set WAL mode: %w", err)
	}

	// 5-second busy timeout so readers don't fail immediately.
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		db.Close()
		return nil, fmt.Errorf("memory: set busy timeout: %w", err)
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("memory: migrate: %w", err)
	}

	return s, nil
}

// DB returns the underlying *sql.DB for shared use (e.g., built-in skills).
func (s *Store) DB() *sql.DB {
	return s.db
}

// Close checkpoints the WAL and closes the underlying database connection.
func (s *Store) Close() error {
	if _, err := s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		slog.Warn("wal checkpoint failed", "err", err)
	}
	return s.db.Close()
}

// execer abstracts *sql.DB and *sql.Tx for shared insert logic.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// CreateConversation inserts a new conversation for userID and chatID and returns its UUID.
func (s *Store) CreateConversation(userID int64, chatID int64) (string, error) {
	return s.insertConversation(s.db, userID, chatID, "")
}

// createConversationTx inserts a new conversation within an existing transaction.
func (s *Store) createConversationTx(tx *sql.Tx, userID int64, chatID int64, chatType string) (string, error) {
	return s.insertConversation(tx, userID, chatID, chatType)
}

func (s *Store) insertConversation(e execer, userID int64, chatID int64, chatType string) (string, error) {
	id, err := newUUID()
	if err != nil {
		return "", fmt.Errorf("memory: generate uuid: %w", err)
	}

	now := time.Now().UTC()
	_, err = e.Exec(
		`INSERT INTO conversations (id, user_id, chat_id, chat_type, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		id, userID, chatID, chatType, now, now,
	)
	if err != nil {
		return "", fmt.Errorf("memory: create conversation: %w", err)
	}
	return id, nil
}

// AppendMessage adds a message to an existing conversation.
func (s *Store) AppendMessage(convID string, role string, content json.RawMessage) error {
	now := time.Now().UTC()

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("memory: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is a no-op

	_, err = tx.Exec(
		`INSERT INTO messages (conversation_id, role, content, created_at) VALUES (?, ?, ?, ?)`,
		convID, role, string(content), now,
	)
	if err != nil {
		return fmt.Errorf("memory: append message: %w", err)
	}

	_, err = tx.Exec(
		`UPDATE conversations SET updated_at = ? WHERE id = ?`,
		now, convID,
	)
	if err != nil {
		return fmt.Errorf("memory: update conversation timestamp: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("memory: commit tx: %w", err)
	}

	return nil
}

// GetMessages returns the most recent limit messages for a conversation,
// ordered oldest-first (chronological).
func (s *Store) GetMessages(convID string, limit int) ([]Message, error) {
	rows, err := s.db.Query(
		`SELECT role, content FROM (
			SELECT role, content, id FROM messages
			WHERE conversation_id = ?
			ORDER BY id DESC
			LIMIT ?
		) sub ORDER BY id ASC`,
		convID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("memory: get messages: %w", err)
	}
	defer rows.Close()

	var msgs []Message
	for rows.Next() {
		var m Message
		var content string
		if err := rows.Scan(&m.Role, &content); err != nil {
			return nil, fmt.Errorf("memory: scan message: %w", err)
		}
		m.Content = json.RawMessage(content)
		msgs = append(msgs, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("memory: iterate messages: %w", err)
	}

	return msgs, nil
}

// LogToolCall records a tool invocation before execution.
func (s *Store) LogToolCall(convID string, callID string, name string, input json.RawMessage) error {
	now := time.Now().UTC()

	_, err := s.db.Exec(
		`INSERT INTO tool_calls (id, conversation_id, name, input, is_error, created_at) VALUES (?, ?, ?, ?, FALSE, ?)`,
		callID, convID, name, string(input), now,
	)
	if err != nil {
		return fmt.Errorf("memory: log tool call: %w", err)
	}
	return nil
}

// CompleteToolCall updates a tool call record with its output after execution.
func (s *Store) CompleteToolCall(callID string, output json.RawMessage, isError bool) error {
	result, err := s.db.Exec(
		`UPDATE tool_calls SET output = ?, is_error = ? WHERE id = ?`,
		string(output), isError, callID,
	)
	if err != nil {
		return fmt.Errorf("memory: complete tool call: %w", err)
	}

	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("memory: check rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("memory: tool call %q not found", callID)
	}
	return nil
}

// GetActiveConversation returns the most recent conversation for userID and chatID.
// If no conversation exists or the most recent one is older than 4 hours,
// a new conversation is created. When an expired conversation is replaced,
// expiredConvID returns the old conversation's ID (for summarization).
//
// The check-and-create is wrapped in a transaction for defense-in-depth.
// With MaxOpenConns(1), all operations are serialized through a single
// connection, making this effectively exclusive even as a deferred transaction.
func (s *Store) GetActiveConversation(userID, chatID int64, chatType string) (convID string, expiredConvID string, err error) {
	// With MaxOpenConns(1), all operations are serialized through the single
	// connection. The transaction provides atomicity for check-then-create.
	tx, err := s.db.Begin()
	if err != nil {
		return "", "", fmt.Errorf("memory: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is a no-op

	var id string
	var updatedAt time.Time

	qErr := tx.QueryRow(
		`SELECT id, updated_at FROM conversations WHERE user_id = ? AND chat_id = ? ORDER BY updated_at DESC LIMIT 1`,
		userID, chatID,
	).Scan(&id, &updatedAt)

	if qErr == sql.ErrNoRows {
		newID, cErr := s.createConversationTx(tx, userID, chatID, chatType)
		if cErr != nil {
			return "", "", cErr
		}
		if cErr = tx.Commit(); cErr != nil {
			return "", "", fmt.Errorf("memory: commit tx: %w", cErr)
		}
		return newID, "", nil
	}
	if qErr != nil {
		return "", "", fmt.Errorf("memory: get active conversation: %w", qErr)
	}

	if time.Since(updatedAt) > 4*time.Hour {
		newID, cErr := s.createConversationTx(tx, userID, chatID, chatType)
		if cErr != nil {
			return "", "", cErr
		}
		if cErr = tx.Commit(); cErr != nil {
			return "", "", fmt.Errorf("memory: commit tx: %w", cErr)
		}
		return newID, id, nil
	}

	// Read-only path — deferred Rollback() handles cleanup, no commit needed.
	return id, "", nil
}

// GetConversationMessages loads all messages for a conversation, oldest first.
func (s *Store) GetConversationMessages(convID string) ([]Message, error) {
	rows, err := s.db.Query(
		`SELECT role, content FROM messages WHERE conversation_id = ? ORDER BY id ASC`,
		convID,
	)
	if err != nil {
		return nil, fmt.Errorf("memory: get conversation messages: %w", err)
	}
	defer rows.Close()

	var msgs []Message
	for rows.Next() {
		var m Message
		var content string
		if err := rows.Scan(&m.Role, &content); err != nil {
			return nil, fmt.Errorf("memory: scan message: %w", err)
		}
		m.Content = json.RawMessage(content)
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

// SaveSummary stores a conversation summary.
func (s *Store) SaveSummary(convID string, userID, chatID int64, summary string, msgCount int, firstAt, lastAt time.Time) error {
	now := time.Now().UTC()
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO conversation_summaries
		 (conversation_id, user_id, chat_id, summary, message_count, first_message_at, last_message_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		convID, userID, chatID, summary, msgCount, firstAt, lastAt, now,
	)
	if err != nil {
		return fmt.Errorf("memory: save summary: %w", err)
	}
	return nil
}

// Summary represents a stored conversation summary.
type Summary struct {
	ID        int64
	Summary   string
	CreatedAt string
}

// ListSummaries returns all summaries for a user, newest first.
func (s *Store) ListSummaries(userID int64) ([]Summary, error) {
	rows, err := s.db.Query(
		`SELECT id, summary, created_at FROM conversation_summaries WHERE user_id = ? ORDER BY created_at DESC`,
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("memory: list summaries: %w", err)
	}
	defer rows.Close()

	var summaries []Summary
	for rows.Next() {
		var s Summary
		if err := rows.Scan(&s.ID, &s.Summary, &s.CreatedAt); err != nil {
			return nil, err
		}
		summaries = append(summaries, s)
	}
	return summaries, rows.Err()
}

// DeleteSummary deletes a summary by ID, scoped to the user (IDOR protection).
func (s *Store) DeleteSummary(summaryID int64, userID int64) error {
	result, err := s.db.Exec(
		`DELETE FROM conversation_summaries WHERE id = ? AND user_id = ?`,
		summaryID, userID,
	)
	if err != nil {
		return fmt.Errorf("memory: delete summary: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("summary: summary %d not found", summaryID)
	}
	return nil
}

// SetSummarizationStatus updates the summarization status on a conversation.
func (s *Store) SetSummarizationStatus(convID string, status string) error {
	_, err := s.db.Exec(
		`UPDATE conversations SET summarization_status = ? WHERE id = ?`,
		status, convID,
	)
	if err != nil {
		return fmt.Errorf("memory: set summarization status: %w", err)
	}
	return nil
}

// PendingSummarizations returns conversation IDs that have summarization_status = 'pending'.
func (s *Store) PendingSummarizations() ([]string, error) {
	rows, err := s.db.Query(
		`SELECT id FROM conversations WHERE summarization_status = 'pending'`,
	)
	if err != nil {
		return nil, fmt.Errorf("memory: pending summarizations: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// RecoverableSummarizations returns conversation IDs that need summarization retry.
// This includes conversations stuck in "pending" (crash during summarization),
// "failed" (transient error), or "indexed_failed" (summary saved but vector index failed).
func (s *Store) RecoverableSummarizations() ([]string, error) {
	rows, err := s.db.Query(
		`SELECT id FROM conversations WHERE summarization_status IN ('pending', 'failed', 'indexed_failed') ORDER BY updated_at ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("memory: recoverable summarizations: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// GetSummaryText retrieves an existing summary for a conversation, if one exists.
// Used during recovery to re-index without re-generating the summary.
func (s *Store) GetSummaryText(convID string) (string, error) {
	var summary string
	err := s.db.QueryRow(
		`SELECT summary FROM conversation_summaries WHERE conversation_id = ?`, convID,
	).Scan(&summary)
	if err == sql.ErrNoRows {
		return "", nil // no summary found is not an error
	}
	if err != nil {
		return "", fmt.Errorf("memory: get summary text: %w", err)
	}
	return summary, nil
}

// ConversationMeta returns the userID, chatID, chatType, and message count for a conversation.
func (s *Store) ConversationMeta(convID string) (userID, chatID int64, chatType string, msgCount int, firstAt, lastAt time.Time, err error) {
	var firstStr, lastStr string
	var ct sql.NullString
	err = s.db.QueryRow(
		`SELECT c.user_id, c.chat_id, c.chat_type,
		        COUNT(m.id),
		        COALESCE(MIN(m.created_at), c.created_at),
		        COALESCE(MAX(m.created_at), c.updated_at)
		 FROM conversations c
		 LEFT JOIN messages m ON m.conversation_id = c.id
		 WHERE c.id = ?
		 GROUP BY c.id`,
		convID,
	).Scan(&userID, &chatID, &ct, &msgCount, &firstStr, &lastStr)
	chatType = ct.String
	if err != nil {
		err = fmt.Errorf("memory: conversation meta: %w", err)
		return
	}
	firstAt, err = parseTimeStr(firstStr)
	if err != nil {
		err = fmt.Errorf("memory: parse firstAt %q: %w", firstStr, err)
		return
	}
	lastAt, err = parseTimeStr(lastStr)
	if err != nil {
		err = fmt.Errorf("memory: parse lastAt %q: %w", lastStr, err)
	}
	return
}

// MigrationText holds text content and its metadata for re-embedding.
type MigrationText struct {
	ID        string
	Text      string
	UserID    int64
	ChatID    int64
	Source    string // "message", "note", "summary"
	ChatType  string // for summaries
	CreatedAt string
}

// AllMessageTexts returns all messages with extractable text for migration.
// It extracts readable text from the JSON content field using the same logic
// as the summarizer (extractText).
func (s *Store) AllMessageTexts() ([]MigrationText, error) {
	rows, err := s.db.Query(
		`SELECT m.id, m.role, m.content, c.user_id, c.chat_id, m.created_at
		 FROM messages m
		 JOIN conversations c ON m.conversation_id = c.id
		 ORDER BY m.id ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("memory: all message texts: %w", err)
	}
	defer rows.Close()

	var results []MigrationText
	for rows.Next() {
		var id int64
		var role, content string
		var userID, chatID int64
		var createdAt string
		if err := rows.Scan(&id, &role, &content, &userID, &chatID, &createdAt); err != nil {
			return nil, fmt.Errorf("memory: scan message text: %w", err)
		}

		msg := Message{Role: role, Content: json.RawMessage(content)}
		text := extractText(msg)
		if text == "" {
			continue
		}

		results = append(results, MigrationText{
			ID:        fmt.Sprintf("msg-%d", id),
			Text:      text,
			UserID:    userID,
			ChatID:    chatID,
			Source:    "message",
			CreatedAt: createdAt,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("memory: iterate message texts: %w", err)
	}
	return results, nil
}

// AllNoteTexts returns all notes for migration. Text is title + content.
// Returns nil (no error) if the notes table does not exist.
func (s *Store) AllNoteTexts() ([]MigrationText, error) {
	// Notes table is created by skills.InitNoteSkills, not by store migrations.
	// Check if the table exists before querying.
	var tableName string
	err := s.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='notes'`).Scan(&tableName)
	if err != nil {
		return nil, nil // table doesn't exist, no notes to migrate
	}

	rows, err := s.db.Query(
		`SELECT id, user_id, chat_id, title, content FROM notes ORDER BY id ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("memory: all note texts: %w", err)
	}
	defer rows.Close()

	var results []MigrationText
	for rows.Next() {
		var id, userID, chatID int64
		var title, content string
		if err := rows.Scan(&id, &userID, &chatID, &title, &content); err != nil {
			return nil, fmt.Errorf("memory: scan note text: %w", err)
		}

		text := title + "\n" + content
		results = append(results, MigrationText{
			ID:     fmt.Sprintf("note-%d", id),
			Text:   text,
			UserID: userID,
			ChatID: chatID,
			Source: "note",
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("memory: iterate note texts: %w", err)
	}
	return results, nil
}

// AllSummaryTexts returns all conversation summaries for migration.
func (s *Store) AllSummaryTexts() ([]MigrationText, error) {
	rows, err := s.db.Query(
		`SELECT conversation_id, user_id, chat_id, summary, COALESCE(chat_type, '') FROM conversation_summaries ORDER BY id ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("memory: all summary texts: %w", err)
	}
	defer rows.Close()

	var results []MigrationText
	for rows.Next() {
		var convID string
		var userID, chatID int64
		var summary, chatType string
		if err := rows.Scan(&convID, &userID, &chatID, &summary, &chatType); err != nil {
			return nil, fmt.Errorf("memory: scan summary text: %w", err)
		}

		results = append(results, MigrationText{
			ID:       convID,
			Text:     summary,
			UserID:   userID,
			ChatID:   chatID,
			Source:   "summary",
			ChatType: chatType,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("memory: iterate summary texts: %w", err)
	}
	return results, nil
}

// migrate creates the schema tables if they do not exist.
func (s *Store) migrate() error {
	const schema = `
	CREATE TABLE IF NOT EXISTS conversations (
		id         TEXT PRIMARY KEY,
		user_id    INTEGER NOT NULL,
		chat_id    INTEGER NOT NULL DEFAULT 0,
		created_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL
	);

	CREATE INDEX IF NOT EXISTS idx_conversations_user_chat_updated
		ON conversations (user_id, chat_id, updated_at DESC);

	CREATE TABLE IF NOT EXISTS messages (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		conversation_id TEXT NOT NULL REFERENCES conversations(id),
		role            TEXT NOT NULL,
		content         TEXT NOT NULL,
		tool_call_id    TEXT,
		created_at      DATETIME NOT NULL
	);

	CREATE INDEX IF NOT EXISTS idx_messages_conversation
		ON messages (conversation_id, id);

	CREATE TABLE IF NOT EXISTS tool_calls (
		id              TEXT PRIMARY KEY,
		conversation_id TEXT NOT NULL REFERENCES conversations(id),
		name            TEXT NOT NULL,
		input           TEXT NOT NULL,
		output          TEXT,
		is_error        BOOLEAN NOT NULL DEFAULT FALSE,
		created_at      DATETIME NOT NULL
	);

	CREATE INDEX IF NOT EXISTS idx_tool_calls_conversation
		ON tool_calls (conversation_id);

	CREATE TABLE IF NOT EXISTS user_facts (
		id                 INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id            INTEGER NOT NULL,
		fact               TEXT NOT NULL,
		category           TEXT NOT NULL DEFAULT 'general',
		source             TEXT NOT NULL DEFAULT 'explicit',
		last_referenced_at DATETIME,
		created_at         DATETIME NOT NULL,
		updated_at         DATETIME NOT NULL
	);

	CREATE INDEX IF NOT EXISTS idx_user_facts_user ON user_facts (user_id);

	CREATE TABLE IF NOT EXISTS conversation_summaries (
		id               INTEGER PRIMARY KEY AUTOINCREMENT,
		conversation_id  TEXT NOT NULL UNIQUE,
		user_id          INTEGER NOT NULL,
		chat_id          INTEGER NOT NULL DEFAULT 0,
		summary          TEXT NOT NULL,
		message_count    INTEGER NOT NULL DEFAULT 0,
		first_message_at DATETIME,
		last_message_at  DATETIME,
		created_at       DATETIME NOT NULL
	);

	CREATE INDEX IF NOT EXISTS idx_conv_summaries_user_chat
		ON conversation_summaries (user_id, chat_id, created_at DESC);
	`

	// After running the main schema, add the summarization_status column
	// to conversations if it doesn't exist (safe for existing databases).
	const addSummarizationStatus = `
	ALTER TABLE conversations ADD COLUMN summarization_status TEXT;
	`

	if _, err := s.db.Exec(schema); err != nil {
		return err
	}

	// Safe migration: add column if it doesn't exist (ALTER TABLE errors are ignored).
	s.db.Exec(addSummarizationStatus)                                              //nolint:errcheck // column may already exist
	s.db.Exec(`ALTER TABLE conversations ADD COLUMN chat_type TEXT DEFAULT ''`)     //nolint:errcheck // column may already exist
	s.db.Exec(`ALTER TABLE conversation_summaries ADD COLUMN chat_type TEXT DEFAULT ''`) //nolint:errcheck // column may already exist
	return nil
}

// parseTimeStr attempts to parse a time string returned by SQLite in various formats.
func parseTimeStr(s string) (time.Time, error) {
	for _, layout := range []string{
		"2006-01-02 15:04:05.999999999 -0700 MST",
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05-07:00",
		"2006-01-02T15:04:05Z",
		"2006-01-02 15:04:05",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized time format: %q", s)
}

// newUUID generates a version-4 UUID using crypto/rand.
func newUUID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	// Set version (4) and variant (RFC 4122).
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80

	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16],
	), nil
}
