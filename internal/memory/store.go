package memory

import (
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
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

// Close closes the underlying database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// CreateConversation inserts a new conversation for userID and returns its UUID.
func (s *Store) CreateConversation(userID int64) (string, error) {
	id, err := newUUID()
	if err != nil {
		return "", fmt.Errorf("memory: generate uuid: %w", err)
	}

	now := time.Now().UTC()
	_, err = s.db.Exec(
		`INSERT INTO conversations (id, user_id, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		id, userID, now, now,
	)
	if err != nil {
		return "", fmt.Errorf("memory: create conversation: %w", err)
	}
	return id, nil
}

// AppendMessage adds a message to an existing conversation.
func (s *Store) AppendMessage(convID string, role string, content json.RawMessage) error {
	now := time.Now().UTC()

	_, err := s.db.Exec(
		`INSERT INTO messages (conversation_id, role, content, created_at) VALUES (?, ?, ?, ?)`,
		convID, role, string(content), now,
	)
	if err != nil {
		return fmt.Errorf("memory: append message: %w", err)
	}

	_, err = s.db.Exec(
		`UPDATE conversations SET updated_at = ? WHERE id = ?`,
		now, convID,
	)
	if err != nil {
		return fmt.Errorf("memory: update conversation timestamp: %w", err)
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

// GetActiveConversation returns the most recent conversation for userID.
// If no conversation exists or the most recent one is older than 4 hours,
// a new conversation is created.
func (s *Store) GetActiveConversation(userID int64) (string, error) {
	var id string
	var updatedAt time.Time

	err := s.db.QueryRow(
		`SELECT id, updated_at FROM conversations WHERE user_id = ? ORDER BY updated_at DESC LIMIT 1`,
		userID,
	).Scan(&id, &updatedAt)

	if err == sql.ErrNoRows {
		return s.CreateConversation(userID)
	}
	if err != nil {
		return "", fmt.Errorf("memory: get active conversation: %w", err)
	}

	if time.Since(updatedAt) > 4*time.Hour {
		return s.CreateConversation(userID)
	}

	return id, nil
}

// migrate creates the schema tables if they do not exist.
func (s *Store) migrate() error {
	const schema = `
	CREATE TABLE IF NOT EXISTS conversations (
		id         TEXT PRIMARY KEY,
		user_id    INTEGER NOT NULL,
		created_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL
	);

	CREATE INDEX IF NOT EXISTS idx_conversations_user_updated
		ON conversations (user_id, updated_at DESC);

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
	`

	_, err := s.db.Exec(schema)
	return err
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
