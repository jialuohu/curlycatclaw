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
	return s.insertConversation(s.db, userID, chatID)
}

// createConversationTx inserts a new conversation within an existing transaction.
func (s *Store) createConversationTx(tx *sql.Tx, userID int64, chatID int64) (string, error) {
	return s.insertConversation(tx, userID, chatID)
}

func (s *Store) insertConversation(e execer, userID int64, chatID int64) (string, error) {
	id, err := newUUID()
	if err != nil {
		return "", fmt.Errorf("memory: generate uuid: %w", err)
	}

	now := time.Now().UTC()
	_, err = e.Exec(
		`INSERT INTO conversations (id, user_id, chat_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		id, userID, chatID, now, now,
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
func (s *Store) GetActiveConversation(userID int64, chatID int64) (convID string, expiredConvID string, err error) {
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
		newID, cErr := s.createConversationTx(tx, userID, chatID)
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
		newID, cErr := s.createConversationTx(tx, userID, chatID)
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

// ConversationMeta returns the userID, chatID, and message count for a conversation.
func (s *Store) ConversationMeta(convID string) (userID, chatID int64, msgCount int, firstAt, lastAt time.Time, err error) {
	var firstStr, lastStr string
	err = s.db.QueryRow(
		`SELECT c.user_id, c.chat_id,
		        COUNT(m.id),
		        COALESCE(MIN(m.created_at), c.created_at),
		        COALESCE(MAX(m.created_at), c.updated_at)
		 FROM conversations c
		 LEFT JOIN messages m ON m.conversation_id = c.id
		 WHERE c.id = ?
		 GROUP BY c.id`,
		convID,
	).Scan(&userID, &chatID, &msgCount, &firstStr, &lastStr)
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
	s.db.Exec(addSummarizationStatus) //nolint:errcheck // column may already exist
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
