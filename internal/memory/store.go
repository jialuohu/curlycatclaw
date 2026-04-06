package memory

import (
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
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
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("memory: rows affected: %w", err)
	}
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

// EmbedderState tracks the active embedder and any in-progress migration.
// Singleton row (id=1) in the embedder_state table.
type EmbedderState struct {
	ActiveEmbedder    string
	ActiveVersion     int
	MigratingEmbedder string // empty if not migrating
	MigratingVersion  int
	MigrationStatus   string // "", "running", "completing", "failed"
	LastMsgID         int64
	LastNoteID        int64
	LastSummaryID     int64
	OldEmbedderType   string
	OldEmbedderModel  string
	OldEmbedderDim    int
	StartedAt         *time.Time
	UpdatedAt         time.Time
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

// GetEmbedderState returns the current embedder state, or nil if not yet initialized.
func (s *Store) GetEmbedderState() (*EmbedderState, error) {
	row := s.db.QueryRow(`
		SELECT active_embedder, active_version,
		       COALESCE(migrating_embedder, ''), COALESCE(migrating_version, 0),
		       COALESCE(migration_status, ''),
		       last_msg_id, last_note_id, last_summary_id,
		       COALESCE(old_embedder_type, ''), COALESCE(old_embedder_model, ''), COALESCE(old_embedder_dim, 0),
		       started_at, updated_at
		FROM embedder_state WHERE id = 1`)

	var st EmbedderState
	var startedAt sql.NullString
	var updatedAt string
	err := row.Scan(
		&st.ActiveEmbedder, &st.ActiveVersion,
		&st.MigratingEmbedder, &st.MigratingVersion,
		&st.MigrationStatus,
		&st.LastMsgID, &st.LastNoteID, &st.LastSummaryID,
		&st.OldEmbedderType, &st.OldEmbedderModel, &st.OldEmbedderDim,
		&startedAt, &updatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("memory: get embedder state: %w", err)
	}

	if startedAt.Valid {
		t, err := parseTimeStr(startedAt.String)
		if err == nil {
			st.StartedAt = &t
		}
	}
	if t, err := parseTimeStr(updatedAt); err == nil {
		st.UpdatedAt = t
	}
	return &st, nil
}

// InitEmbedderState inserts the initial embedder state row (version 0).
// Does nothing if a row already exists.
func (s *Store) InitEmbedderState(embedderName string) error {
	_, err := s.db.Exec(`
		INSERT OR IGNORE INTO embedder_state (id, active_embedder, active_version, updated_at)
		VALUES (1, ?, 0, datetime('now'))`,
		embedderName,
	)
	if err != nil {
		return fmt.Errorf("memory: init embedder state: %w", err)
	}
	return nil
}

// UpdateEmbedderConfig persists the current embedder identity at steady state (A6).
// Called on every normal startup so old config is always available for migration.
func (s *Store) UpdateEmbedderConfig(embedderName, embedderType, model string, dim int) error {
	_, err := s.db.Exec(`
		UPDATE embedder_state SET
			active_embedder = ?,
			old_embedder_type = ?,
			old_embedder_model = ?,
			old_embedder_dim = ?,
			updated_at = datetime('now')
		WHERE id = 1 AND (migration_status IS NULL OR migration_status = '')`,
		embedderName, embedderType, model, dim,
	)
	if err != nil {
		return fmt.Errorf("memory: update embedder config: %w", err)
	}
	return nil
}

// StartMigration transitions the embedder state to "running".
func (s *Store) StartMigration(newEmbedder string, newVersion int, oldType, oldModel string, oldDim int) error {
	_, err := s.db.Exec(`
		UPDATE embedder_state SET
			migrating_embedder = ?,
			migrating_version = ?,
			migration_status = 'running',
			last_msg_id = 0, last_note_id = 0, last_summary_id = 0,
			old_embedder_type = ?,
			old_embedder_model = ?,
			old_embedder_dim = ?,
			started_at = datetime('now'),
			updated_at = datetime('now')
		WHERE id = 1`,
		newEmbedder, newVersion, oldType, oldModel, oldDim,
	)
	if err != nil {
		return fmt.Errorf("memory: start migration: %w", err)
	}
	return nil
}

// UpdateMigrationCursor saves the last-seen row IDs for resumable migration.
func (s *Store) UpdateMigrationCursor(lastMsgID, lastNoteID, lastSummaryID int64) error {
	_, err := s.db.Exec(`
		UPDATE embedder_state SET
			last_msg_id = ?, last_note_id = ?, last_summary_id = ?,
			updated_at = datetime('now')
		WHERE id = 1`,
		lastMsgID, lastNoteID, lastSummaryID,
	)
	if err != nil {
		return fmt.Errorf("memory: update migration cursor: %w", err)
	}
	return nil
}

// SetMigrationStatus updates only the status field (e.g., "completing", "failed").
func (s *Store) SetMigrationStatus(status string) error {
	_, err := s.db.Exec(`
		UPDATE embedder_state SET migration_status = ?, updated_at = datetime('now')
		WHERE id = 1`, status,
	)
	if err != nil {
		return fmt.Errorf("memory: set migration status: %w", err)
	}
	return nil
}

// CompleteMigration clears migration fields and updates the active embedder/version.
// Preserves the new embedder's type/model/dim so the next migration can reconstruct it.
func (s *Store) CompleteMigration(newEmbedder string, newVersion int, newType, newModel string, newDim int) error {
	_, err := s.db.Exec(`
		UPDATE embedder_state SET
			active_embedder = ?, active_version = ?,
			migrating_embedder = NULL, migrating_version = NULL,
			migration_status = NULL,
			last_msg_id = 0, last_note_id = 0, last_summary_id = 0,
			old_embedder_type = ?, old_embedder_model = ?, old_embedder_dim = ?,
			started_at = NULL,
			updated_at = datetime('now')
		WHERE id = 1`,
		newEmbedder, newVersion, newType, newModel, newDim,
	)
	if err != nil {
		return fmt.Errorf("memory: complete migration: %w", err)
	}
	return nil
}

// MessageTextsAfter returns the next batch of messages with m.id > afterID.
// Returns the texts and the max message row ID seen (for cursor).
func (s *Store) MessageTextsAfter(afterID int64, limit int) ([]MigrationText, int64, error) {
	rows, err := s.db.Query(
		`SELECT m.id, m.role, m.content, c.user_id, c.chat_id, m.created_at
		 FROM messages m
		 JOIN conversations c ON m.conversation_id = c.id
		 WHERE m.id > ?
		 ORDER BY m.id ASC
		 LIMIT ?`, afterID, limit,
	)
	if err != nil {
		return nil, afterID, fmt.Errorf("memory: message texts after: %w", err)
	}
	defer rows.Close()

	var results []MigrationText
	maxID := afterID
	for rows.Next() {
		var id int64
		var role, content string
		var userID, chatID int64
		var createdAt string
		if err := rows.Scan(&id, &role, &content, &userID, &chatID, &createdAt); err != nil {
			return nil, maxID, fmt.Errorf("memory: scan message text: %w", err)
		}
		if id > maxID {
			maxID = id
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
		return nil, maxID, fmt.Errorf("memory: iterate message texts: %w", err)
	}
	return results, maxID, nil
}

// NoteTextsAfter returns the next batch of notes with id > afterID.
// Returns nil (no error) if the notes table does not exist.
func (s *Store) NoteTextsAfter(afterID int64, limit int) ([]MigrationText, int64, error) {
	var tableName string
	err := s.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='notes'`).Scan(&tableName)
	if err != nil {
		return nil, 0, nil // table doesn't exist
	}

	rows, err := s.db.Query(
		`SELECT id, user_id, chat_id, title, content, created_at FROM notes
		 WHERE id > ?
		 ORDER BY id ASC
		 LIMIT ?`, afterID, limit,
	)
	if err != nil {
		return nil, afterID, fmt.Errorf("memory: note texts after: %w", err)
	}
	defer rows.Close()

	var results []MigrationText
	maxID := afterID
	for rows.Next() {
		var id, userID, chatID int64
		var title, content, createdAt string
		if err := rows.Scan(&id, &userID, &chatID, &title, &content, &createdAt); err != nil {
			return nil, maxID, fmt.Errorf("memory: scan note text: %w", err)
		}
		if id > maxID {
			maxID = id
		}

		results = append(results, MigrationText{
			ID:        fmt.Sprintf("note-%d", id),
			Text:      title + "\n" + content,
			UserID:    userID,
			ChatID:    chatID,
			Source:    "note",
			CreatedAt: createdAt,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, maxID, fmt.Errorf("memory: iterate note texts: %w", err)
	}
	return results, maxID, nil
}

// SummaryTextsAfter returns the next batch of summaries with id > afterID.
func (s *Store) SummaryTextsAfter(afterID int64, limit int) ([]MigrationText, int64, error) {
	rows, err := s.db.Query(
		`SELECT id, conversation_id, user_id, chat_id, summary, COALESCE(chat_type, '')
		 FROM conversation_summaries
		 WHERE id > ?
		 ORDER BY id ASC
		 LIMIT ?`, afterID, limit,
	)
	if err != nil {
		return nil, afterID, fmt.Errorf("memory: summary texts after: %w", err)
	}
	defer rows.Close()

	var results []MigrationText
	maxID := afterID
	for rows.Next() {
		var id int64
		var convID string
		var userID, chatID int64
		var summary, chatType string
		if err := rows.Scan(&id, &convID, &userID, &chatID, &summary, &chatType); err != nil {
			return nil, maxID, fmt.Errorf("memory: scan summary text: %w", err)
		}
		if id > maxID {
			maxID = id
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
		return nil, maxID, fmt.Errorf("memory: iterate summary texts: %w", err)
	}
	return results, maxID, nil
}

// SaveObservation inserts an observation and its facts in a transaction.
// If obs.ID is empty, a new UUID is generated.
func (s *Store) SaveObservation(obs *Observation) error {
	if obs.ID == "" {
		id, err := newUUID()
		if err != nil {
			return fmt.Errorf("memory: generate observation uuid: %w", err)
		}
		obs.ID = id
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("memory: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is a no-op

	now := time.Now().UTC()
	_, err = tx.Exec(
		`INSERT INTO observations (id, conversation_id, user_id, chat_id, chat_type, type, title, summary, importance, source_msg_start, source_msg_end, content_hash, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		obs.ID, obs.ConversationID, obs.UserID, obs.ChatID, obs.ChatType,
		obs.Type, obs.Title, obs.Summary, obs.Importance,
		obs.SourceMsgStart, obs.SourceMsgEnd, obs.ContentHash, now,
	)
	if err != nil {
		return fmt.Errorf("memory: insert observation: %w", err)
	}

	for _, fact := range obs.Facts {
		_, err = tx.Exec(
			`INSERT INTO observation_facts (observation_id, fact) VALUES (?, ?)`,
			obs.ID, fact,
		)
		if err != nil {
			return fmt.Errorf("memory: insert observation fact: %w", err)
		}
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("memory: commit observation tx: %w", err)
	}
	return nil
}

// GetRecentObservationTitles returns recent observation titles for a conversation,
// ordered newest first. Used for dedup context during extraction.
func (s *Store) GetRecentObservationTitles(convID string, limit int) ([]string, error) {
	rows, err := s.db.Query(
		`SELECT title FROM observations WHERE conversation_id = ? AND archived_at IS NULL ORDER BY created_at DESC LIMIT ?`,
		convID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("memory: get recent observation titles: %w", err)
	}
	defer rows.Close()

	var titles []string
	for rows.Next() {
		var title string
		if err := rows.Scan(&title); err != nil {
			return nil, fmt.Errorf("memory: scan observation title: %w", err)
		}
		titles = append(titles, title)
	}
	return titles, rows.Err()
}

// GetRecentObservationsByType returns recent observations of a given type for a user,
// ordered newest first. Used to feed existing observations to extraction prompt for
// supersession detection. Returns id, title, summary for each observation.
func (s *Store) GetRecentObservationsByType(userID int64, obsType string, limit int) ([]Observation, error) {
	rows, err := s.db.Query(
		`SELECT id, title, summary FROM observations
		 WHERE user_id = ? AND type = ? AND archived_at IS NULL
		 ORDER BY created_at DESC LIMIT ?`,
		userID, obsType, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("memory: get recent observations by type: %w", err)
	}
	defer rows.Close()

	var obs []Observation
	for rows.Next() {
		var o Observation
		if err := rows.Scan(&o.ID, &o.Title, &o.Summary); err != nil {
			return nil, fmt.Errorf("memory: scan observation by type: %w", err)
		}
		obs = append(obs, o)
	}
	return obs, rows.Err()
}

// GetRecentObservations returns observations created within the last maxAge for a user.
// Used to ensure freshly-extracted observations are visible in the current conversation,
// regardless of semantic search match score.
func (s *Store) GetRecentObservations(userID int64, maxAge time.Duration, limit int) ([]Observation, error) {
	cutoff := time.Now().UTC().Add(-maxAge).Format("2006-01-02 15:04:05")
	rows, err := s.db.Query(
		`SELECT id, type, title, summary, importance, created_at FROM observations
		 WHERE user_id = ? AND archived_at IS NULL AND created_at >= ?
		 ORDER BY created_at DESC LIMIT ?`,
		userID, cutoff, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("memory: get recent observations: %w", err)
	}
	defer rows.Close()

	var obs []Observation
	for rows.Next() {
		var o Observation
		if err := rows.Scan(&o.ID, &o.Type, &o.Title, &o.Summary, &o.Importance, &o.CreatedAt); err != nil {
			return nil, fmt.Errorf("memory: scan recent observation: %w", err)
		}
		obs = append(obs, o)
	}
	return obs, rows.Err()
}

// GetExtractionState returns the extraction state for a conversation, or nil if not found.
func (s *Store) GetExtractionState(convID string) (*ExtractionState, error) {
	var st ExtractionState
	var lastExtractionAt sql.NullString
	var lastMsgAt sql.NullString
	err := s.db.QueryRow(
		`SELECT conversation_id, last_extracted_msg_rowid, last_extraction_at, last_msg_at, turn_count_since_extraction, status
		 FROM observation_extraction_state WHERE conversation_id = ?`,
		convID,
	).Scan(&st.ConversationID, &st.LastExtractedMsgRowid, &lastExtractionAt, &lastMsgAt, &st.TurnCount, &st.Status)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("memory: get extraction state: %w", err)
	}

	if lastExtractionAt.Valid {
		t, pErr := parseTimeStr(lastExtractionAt.String)
		if pErr == nil {
			st.LastExtractionAt = &t
		}
	}
	if lastMsgAt.Valid {
		t, pErr := parseTimeStr(lastMsgAt.String)
		if pErr == nil {
			st.LastMsgAt = t
		}
	}
	return &st, nil
}

// UpdateExtractionState upserts the extraction state for a conversation.
func (s *Store) UpdateExtractionState(convID string, lastRowid int64, turnCount int, status string) error {
	now := time.Now().UTC()
	_, err := s.db.Exec(
		`INSERT INTO observation_extraction_state (conversation_id, last_extracted_msg_rowid, last_extraction_at, turn_count_since_extraction, status, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(conversation_id) DO UPDATE SET
		   last_extracted_msg_rowid = excluded.last_extracted_msg_rowid,
		   last_extraction_at = excluded.last_extraction_at,
		   turn_count_since_extraction = excluded.turn_count_since_extraction,
		   status = excluded.status,
		   updated_at = excluded.updated_at`,
		convID, lastRowid, now, turnCount, status, now,
	)
	if err != nil {
		return fmt.Errorf("memory: update extraction state: %w", err)
	}
	return nil
}

// IncrementExtractionTurnCount upserts the extraction state, incrementing turn_count by 1
// and setting last_msg_at to the current time.
func (s *Store) IncrementExtractionTurnCount(convID string) error {
	now := time.Now().UTC()
	_, err := s.db.Exec(
		`INSERT INTO observation_extraction_state (conversation_id, last_extracted_msg_rowid, turn_count_since_extraction, last_msg_at, status, updated_at)
		 VALUES (?, 0, 1, ?, 'idle', ?)
		 ON CONFLICT(conversation_id) DO UPDATE SET
		   turn_count_since_extraction = turn_count_since_extraction + 1,
		   last_msg_at = excluded.last_msg_at,
		   updated_at = excluded.updated_at`,
		convID, now, now,
	)
	if err != nil {
		return fmt.Errorf("memory: increment extraction turn count: %w", err)
	}
	return nil
}

// ObservationExistsByHash checks if an observation with the given content_hash
// already exists for the given user (dedup scoped to user_id).
func (s *Store) ObservationExistsByHash(userID int64, hash string) (bool, error) {
	var exists bool
	err := s.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM observations WHERE user_id = ? AND content_hash = ? AND archived_at IS NULL)`,
		userID, hash,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("memory: check observation hash: %w", err)
	}
	return exists, nil
}

// DeleteObservation deletes an observation and its facts in a transaction.
// IDOR-protected: the observation must belong to the given userID.
func (s *Store) DeleteObservation(id string, userID int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("memory: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is a no-op

	// Verify ownership before deleting.
	var ownerID int64
	err = tx.QueryRow(`SELECT user_id FROM observations WHERE id = ?`, id).Scan(&ownerID)
	if err == sql.ErrNoRows {
		return fmt.Errorf("observation: observation %q not found", id)
	}
	if err != nil {
		return fmt.Errorf("memory: lookup observation: %w", err)
	}
	if ownerID != userID {
		return fmt.Errorf("observation: observation %q not found", id)
	}

	// Delete related rows first (no CASCADE). Order: relations, entities, facts, observation.
	if _, err = tx.Exec(`DELETE FROM observation_relations WHERE source_id = ? OR target_id = ?`, id, id); err != nil {
		return fmt.Errorf("memory: delete observation relations: %w", err)
	}
	if _, err = tx.Exec(`DELETE FROM observation_entities WHERE observation_id = ?`, id); err != nil {
		return fmt.Errorf("memory: delete observation entities: %w", err)
	}
	if _, err = tx.Exec(`DELETE FROM observation_facts WHERE observation_id = ?`, id); err != nil {
		return fmt.Errorf("memory: delete observation facts: %w", err)
	}

	if _, err = tx.Exec(`DELETE FROM observations WHERE id = ?`, id); err != nil {
		return fmt.Errorf("memory: delete observation: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("memory: commit delete observation tx: %w", err)
	}
	return nil
}

// ArchiveObservation soft-deletes an observation by setting archived_at.
// IDOR protection: only the owning user can archive.
func (s *Store) ArchiveObservation(id string, userID int64) error {
	res, err := s.db.Exec(
		`UPDATE observations SET archived_at = CURRENT_TIMESTAMP WHERE id = ? AND user_id = ? AND archived_at IS NULL`,
		id, userID,
	)
	if err != nil {
		return fmt.Errorf("memory: archive observation: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("memory: observation %s not found or already archived", id)
	}
	return nil
}

// RestoreObservation un-archives a soft-deleted observation.
// IDOR protection: only the owning user can restore.
func (s *Store) RestoreObservation(id string, userID int64) error {
	res, err := s.db.Exec(
		`UPDATE observations SET archived_at = NULL WHERE id = ? AND user_id = ? AND archived_at IS NOT NULL`,
		id, userID,
	)
	if err != nil {
		return fmt.Errorf("memory: restore observation: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("memory: observation %s not found or not archived", id)
	}
	return nil
}

// UpdateObservation updates only the non-zero fields of an observation.
// Empty strings and importance=0 are treated as "don't change".
// IDOR protection: only the owning user can update.
func (s *Store) UpdateObservation(id string, userID int64, title, summary, obsType string, importance int) error {
	var setClauses []string
	var args []any
	if title != "" {
		setClauses = append(setClauses, "title = ?")
		args = append(args, title)
	}
	if summary != "" {
		setClauses = append(setClauses, "summary = ?")
		args = append(args, summary)
	}
	if obsType != "" {
		setClauses = append(setClauses, "type = ?")
		args = append(args, obsType)
	}
	if importance > 0 {
		setClauses = append(setClauses, "importance = ?")
		args = append(args, importance)
	}
	if len(setClauses) == 0 {
		return fmt.Errorf("memory: no fields to update")
	}
	query := "UPDATE observations SET " + strings.Join(setClauses, ", ") + " WHERE id = ? AND user_id = ? AND archived_at IS NULL"
	args = append(args, id, userID)
	res, err := s.db.Exec(query, args...)
	if err != nil {
		return fmt.Errorf("memory: update observation: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("memory: observation %s not found or wrong user", id)
	}
	return nil
}

// CountObservations returns the number of observations in a conversation.
func (s *Store) CountObservations(convID string) (int, error) {
	var count int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM observations WHERE conversation_id = ? AND archived_at IS NULL`,
		convID,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("memory: count observations: %w", err)
	}
	return count, nil
}

// GetObservationFactsByIDs batch-loads facts for the given observation IDs.
// Returns a map of observation_id -> []fact.
func (s *Store) GetObservationFactsByIDs(ids []string) (map[string][]string, error) {
	if len(ids) == 0 {
		return map[string][]string{}, nil
	}

	// Build parameterized query with placeholders.
	placeholders := make([]byte, 0, len(ids)*2-1)
	args := make([]any, len(ids))
	for i, id := range ids {
		if i > 0 {
			placeholders = append(placeholders, ',')
		}
		placeholders = append(placeholders, '?')
		args[i] = id
	}

	rows, err := s.db.Query(
		`SELECT observation_id, fact FROM observation_facts WHERE observation_id IN (`+string(placeholders)+`) ORDER BY rowid ASC`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("memory: get observation facts: %w", err)
	}
	defer rows.Close()

	result := make(map[string][]string, len(ids))
	for rows.Next() {
		var obsID, fact string
		if err := rows.Scan(&obsID, &fact); err != nil {
			return nil, fmt.Errorf("memory: scan observation fact: %w", err)
		}
		result[obsID] = append(result[obsID], fact)
	}
	return result, rows.Err()
}

// RecoverableExtractions returns conversation IDs with extraction status
// IN ('failed', 'pending'), for crash recovery.
func (s *Store) RecoverableExtractions() ([]string, error) {
	rows, err := s.db.Query(
		`SELECT conversation_id FROM observation_extraction_state WHERE status IN ('failed', 'pending') ORDER BY updated_at ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("memory: recoverable extractions: %w", err)
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

// GetMaxMessageRowid returns the maximum rowid from messages for a conversation.
// Used by extraction to know the cursor boundary.
func (s *Store) GetMaxMessageRowid(convID string) (int64, error) {
	var maxRowid sql.NullInt64
	err := s.db.QueryRow(
		`SELECT MAX(id) FROM messages WHERE conversation_id = ?`,
		convID,
	).Scan(&maxRowid)
	if err != nil {
		return 0, fmt.Errorf("memory: get max message rowid: %w", err)
	}
	if !maxRowid.Valid {
		return 0, nil
	}
	return maxRowid.Int64, nil
}

// GetMessagesSinceRowid loads messages in the rowid range (afterRowid, upToRowid]
// for a conversation, ordered by id ascending. Used by extraction.
func (s *Store) GetMessagesSinceRowid(convID string, afterRowid, upToRowid int64) ([]Message, error) {
	rows, err := s.db.Query(
		`SELECT role, content FROM messages WHERE conversation_id = ? AND id > ? AND id <= ? ORDER BY id ASC`,
		convID, afterRowid, upToRowid,
	)
	if err != nil {
		return nil, fmt.Errorf("memory: get messages since rowid: %w", err)
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

	// Index for PendingSummarizations() and RecoverableSummarizations() queries
	// which filter on summarization_status and sort by updated_at.
	s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_conversations_sum_status ON conversations (summarization_status, updated_at ASC)`) //nolint:errcheck

	const observationSchema = `
	CREATE TABLE IF NOT EXISTS observations (
		rowid INTEGER PRIMARY KEY AUTOINCREMENT,
		id TEXT UNIQUE NOT NULL,
		conversation_id TEXT NOT NULL,
		user_id INTEGER NOT NULL,
		chat_id INTEGER NOT NULL,
		chat_type TEXT NOT NULL DEFAULT 'private',
		type TEXT NOT NULL,
		title TEXT NOT NULL,
		summary TEXT NOT NULL,
		importance INTEGER NOT NULL DEFAULT 5,
		source_msg_start INTEGER,
		source_msg_end INTEGER,
		content_hash TEXT NOT NULL,
		created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		FOREIGN KEY (conversation_id) REFERENCES conversations(id)
	);
	CREATE TABLE IF NOT EXISTS observation_facts (
		rowid INTEGER PRIMARY KEY AUTOINCREMENT,
		observation_id TEXT NOT NULL,
		fact TEXT NOT NULL,
		FOREIGN KEY (observation_id) REFERENCES observations(id)
	);
	CREATE TABLE IF NOT EXISTS observation_extraction_state (
		conversation_id TEXT PRIMARY KEY,
		last_extracted_msg_rowid INTEGER NOT NULL DEFAULT 0,
		last_extraction_at TIMESTAMP,
		last_msg_at TIMESTAMP,
		turn_count_since_extraction INTEGER NOT NULL DEFAULT 0,
		status TEXT NOT NULL DEFAULT 'idle',
		updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_observations_user ON observations(user_id);
	CREATE UNIQUE INDEX IF NOT EXISTS idx_observations_user_hash ON observations(user_id, content_hash);
	CREATE INDEX IF NOT EXISTS idx_observation_facts_obs ON observation_facts(observation_id);
	`
	if _, err := s.db.Exec(observationSchema); err != nil {
		return fmt.Errorf("memory: create observation tables: %w", err)
	}

	// Phase 2: FTS5 virtual tables for keyword search on observations.
	const observationFTSSchema = `
	CREATE VIRTUAL TABLE IF NOT EXISTS observations_fts USING fts5(
		title, summary, content=observations, content_rowid=rowid
	);
	CREATE VIRTUAL TABLE IF NOT EXISTS observation_facts_fts USING fts5(
		fact, content=observation_facts, content_rowid=rowid
	);
	`
	if _, err := s.db.Exec(observationFTSSchema); err != nil {
		return fmt.Errorf("memory: create FTS5 tables: %w", err)
	}

	// Triggers to keep FTS5 in sync with content tables.
	ftsTriggersSQL := []string{
		`CREATE TRIGGER IF NOT EXISTS observations_ai AFTER INSERT ON observations BEGIN
			INSERT INTO observations_fts(rowid, title, summary) VALUES (new.rowid, new.title, new.summary);
		END`,
		`CREATE TRIGGER IF NOT EXISTS observations_ad AFTER DELETE ON observations BEGIN
			INSERT INTO observations_fts(observations_fts, rowid, title, summary) VALUES('delete', old.rowid, old.title, old.summary);
		END`,
		`CREATE TRIGGER IF NOT EXISTS observation_facts_ai AFTER INSERT ON observation_facts BEGIN
			INSERT INTO observation_facts_fts(rowid, fact) VALUES (new.rowid, new.fact);
		END`,
		`CREATE TRIGGER IF NOT EXISTS observation_facts_ad AFTER DELETE ON observation_facts BEGIN
			INSERT INTO observation_facts_fts(observation_facts_fts, rowid, fact) VALUES('delete', old.rowid, old.fact);
		END`,
	}
	for _, sql := range ftsTriggersSQL {
		if _, err := s.db.Exec(sql); err != nil {
			return fmt.Errorf("memory: create FTS5 trigger: %w", err)
		}
	}

	// Phase 2: entity extraction table.
	const entitySchema = `
	CREATE TABLE IF NOT EXISTS observation_entities (
		rowid INTEGER PRIMARY KEY AUTOINCREMENT,
		observation_id TEXT NOT NULL,
		name TEXT NOT NULL,
		entity_type TEXT NOT NULL,
		FOREIGN KEY (observation_id) REFERENCES observations(id)
	);
	CREATE INDEX IF NOT EXISTS idx_obs_entities_obs ON observation_entities(observation_id);
	CREATE VIRTUAL TABLE IF NOT EXISTS observation_entities_fts USING fts5(
		name, content=observation_entities, content_rowid=rowid
	);
	`
	if _, err := s.db.Exec(entitySchema); err != nil {
		return fmt.Errorf("memory: create entity tables: %w", err)
	}

	// Entity FTS5 sync triggers.
	entityTriggers := []string{
		`CREATE TRIGGER IF NOT EXISTS obs_entities_ai AFTER INSERT ON observation_entities BEGIN
			INSERT INTO observation_entities_fts(rowid, name) VALUES (new.rowid, new.name);
		END`,
		`CREATE TRIGGER IF NOT EXISTS obs_entities_ad AFTER DELETE ON observation_entities BEGIN
			INSERT INTO observation_entities_fts(observation_entities_fts, rowid, name) VALUES('delete', old.rowid, old.name);
		END`,
	}
	for _, sql := range entityTriggers {
		if _, err := s.db.Exec(sql); err != nil {
			return fmt.Errorf("memory: create entity FTS5 trigger: %w", err)
		}
	}

	// Phase 2: observation relations for supersession (advisory, not hiding).
	const observationRelationsSchema = `
	CREATE TABLE IF NOT EXISTS observation_relations (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		source_id TEXT NOT NULL,
		target_id TEXT NOT NULL,
		relation_type TEXT NOT NULL,
		confidence REAL NOT NULL,
		confirmed BOOLEAN DEFAULT 0,
		created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		FOREIGN KEY (source_id) REFERENCES observations(id),
		FOREIGN KEY (target_id) REFERENCES observations(id)
	);
	CREATE INDEX IF NOT EXISTS idx_obs_relations_source ON observation_relations(source_id);
	CREATE INDEX IF NOT EXISTS idx_obs_relations_target ON observation_relations(target_id);
	CREATE UNIQUE INDEX IF NOT EXISTS idx_obs_relations_unique ON observation_relations(source_id, target_id, relation_type);
	`
	if _, err := s.db.Exec(observationRelationsSchema); err != nil {
		return fmt.Errorf("memory: create observation_relations table: %w", err)
	}

	// Phase 3: add archived_at column for soft delete (idempotent ALTER TABLE).
	s.db.Exec(`ALTER TABLE observations ADD COLUMN archived_at TIMESTAMP`) //nolint:errcheck

	// Phase 3: FTS5 UPDATE triggers (INSERT/DELETE triggers already exist).
	ftsUpdateTriggers := []string{
		`CREATE TRIGGER IF NOT EXISTS observations_au AFTER UPDATE OF title, summary ON observations BEGIN
			INSERT INTO observations_fts(observations_fts, rowid, title, summary) VALUES('delete', old.rowid, old.title, old.summary);
			INSERT INTO observations_fts(rowid, title, summary) VALUES (new.rowid, new.title, new.summary);
		END`,
	}
	for _, sql := range ftsUpdateTriggers {
		if _, err := s.db.Exec(sql); err != nil {
			return fmt.Errorf("memory: create FTS5 update trigger: %w", err)
		}
	}

	// Backfill FTS5 indexes from existing data (idempotent, safe to run on every startup).
	s.RebuildFTS() //nolint:errcheck // best-effort: FTS5 tables may not have data yet

	const embedderStateSchema = `
	CREATE TABLE IF NOT EXISTS embedder_state (
		id                 INTEGER PRIMARY KEY CHECK (id = 1),
		active_embedder    TEXT NOT NULL,
		active_version     INTEGER NOT NULL DEFAULT 0,
		migrating_embedder TEXT,
		migrating_version  INTEGER,
		migration_status   TEXT,
		last_msg_id        INTEGER DEFAULT 0,
		last_note_id       INTEGER DEFAULT 0,
		last_summary_id    INTEGER DEFAULT 0,
		old_embedder_type  TEXT,
		old_embedder_model TEXT,
		old_embedder_dim   INTEGER,
		started_at         DATETIME,
		updated_at         DATETIME NOT NULL
	);`
	if _, err := s.db.Exec(embedderStateSchema); err != nil {
		return fmt.Errorf("memory: create embedder_state: %w", err)
	}

	// Generic ingest tables (replaces email_ingest_state + email_processed_messages).
	const ingestSchema = `
	CREATE TABLE IF NOT EXISTS ingest_state (
		source TEXT NOT NULL,
		partition TEXT NOT NULL DEFAULT '',
		mode TEXT NOT NULL DEFAULT 'incremental',
		cursor TEXT NOT NULL DEFAULT '',
		backfill_cursor TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL DEFAULT 'idle',
		stats_scanned INTEGER NOT NULL DEFAULT 0,
		stats_prefiltered INTEGER NOT NULL DEFAULT 0,
		stats_llm_triaged INTEGER NOT NULL DEFAULT 0,
		stats_kept INTEGER NOT NULL DEFAULT 0,
		stats_failed INTEGER NOT NULL DEFAULT 0,
		last_run_at DATETIME,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (source, partition)
	);
	CREATE TABLE IF NOT EXISTS ingest_processed_items (
		source TEXT NOT NULL,
		item_id TEXT NOT NULL,
		result TEXT NOT NULL,
		fingerprint TEXT NOT NULL DEFAULT '',
		attempt_count INTEGER NOT NULL DEFAULT 1,
		processed_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (source, item_id)
	);
	CREATE INDEX IF NOT EXISTS idx_ingest_processed_at ON ingest_processed_items(processed_at);
	`
	if _, err := s.db.Exec(ingestSchema); err != nil {
		return fmt.Errorf("memory: create ingest tables: %w", err)
	}

	// Migrate legacy email_ingest tables if they exist.
	s.migrateEmailIngestTables()

	// Eval Phase 0: event model tables for self-evaluation pipeline.
	const evalEventSchema = `
	CREATE TABLE IF NOT EXISTS interaction_events (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		conversation_id TEXT NOT NULL,
		user_id         INTEGER NOT NULL,
		chat_id         INTEGER NOT NULL,
		event_type      TEXT NOT NULL,
		payload         TEXT,
		created_at      DATETIME NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_interaction_events_conv
		ON interaction_events (conversation_id, created_at);
	CREATE INDEX IF NOT EXISTS idx_interaction_events_type
		ON interaction_events (event_type, created_at);

	CREATE TABLE IF NOT EXISTS telegram_message_map (
		chat_id              INTEGER NOT NULL,
		telegram_message_id  INTEGER NOT NULL,
		conversation_id      TEXT NOT NULL,
		created_at           DATETIME NOT NULL
	);
	CREATE UNIQUE INDEX IF NOT EXISTS idx_tg_msg_map_lookup
		ON telegram_message_map (chat_id, telegram_message_id);

	CREATE TABLE IF NOT EXISTS eval_reactions (
		id                  INTEGER PRIMARY KEY AUTOINCREMENT,
		conversation_id     TEXT NOT NULL,
		user_id             INTEGER NOT NULL,
		chat_id             INTEGER NOT NULL,
		telegram_message_id INTEGER NOT NULL,
		reaction            TEXT NOT NULL,
		created_at          DATETIME NOT NULL
	);
	CREATE UNIQUE INDEX IF NOT EXISTS idx_eval_reactions_user_msg
		ON eval_reactions (user_id, chat_id, telegram_message_id);
	CREATE INDEX IF NOT EXISTS idx_eval_reactions_conv
		ON eval_reactions (conversation_id, created_at);
	`
	if _, err := s.db.Exec(evalEventSchema); err != nil {
		return fmt.Errorf("memory: create eval event tables: %w", err)
	}

	// Time-based indexes for eval scanning queries.
	s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_messages_created_at ON messages (created_at)`)       //nolint:errcheck
	s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_tool_calls_created_at ON tool_calls (created_at)`)   //nolint:errcheck

	// Eval Phase 1: pipeline state tables.
	const evalPipelineSchema = `
	CREATE TABLE IF NOT EXISTS eval_runs (
		id TEXT PRIMARY KEY,
		started_at DATETIME NOT NULL,
		completed_at DATETIME,
		conversations_scanned INTEGER DEFAULT 0,
		failures_found INTEGER DEFAULT 0,
		candidates_generated INTEGER DEFAULT 0,
		candidates_committed INTEGER DEFAULT 0,
		status TEXT NOT NULL DEFAULT 'running',
		summary TEXT
	);

	CREATE TABLE IF NOT EXISTS failure_clusters (
		id TEXT PRIMARY KEY,
		eval_run_id TEXT NOT NULL REFERENCES eval_runs(id),
		cluster_type TEXT NOT NULL,
		description TEXT NOT NULL,
		conversation_ids TEXT NOT NULL,
		message_rowids TEXT NOT NULL DEFAULT '[]',
		tool_call_ids TEXT,
		severity INTEGER NOT NULL,
		frequency INTEGER NOT NULL DEFAULT 1,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_failure_clusters_run ON failure_clusters(eval_run_id);

	CREATE TABLE IF NOT EXISTS memory_candidates (
		id TEXT PRIMARY KEY,
		eval_run_id TEXT NOT NULL REFERENCES eval_runs(id),
		failure_cluster_id TEXT REFERENCES failure_clusters(id),
		candidate_type TEXT NOT NULL,
		title TEXT NOT NULL,
		content TEXT NOT NULL,
		evidence TEXT NOT NULL,
		confidence REAL NOT NULL,
		predicted_impact TEXT,
		status TEXT NOT NULL DEFAULT 'pending',
		replay_score REAL,
		committed_observation_id TEXT,
		user_id INTEGER NOT NULL DEFAULT 0,
		chat_id INTEGER NOT NULL DEFAULT 0,
		reviewed_at DATETIME,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_memory_candidates_run ON memory_candidates(eval_run_id);
	CREATE INDEX IF NOT EXISTS idx_memory_candidates_status ON memory_candidates(status);

	CREATE TABLE IF NOT EXISTS eval_scores (
		id TEXT PRIMARY KEY,
		conversation_id TEXT NOT NULL,
		eval_run_id TEXT NOT NULL REFERENCES eval_runs(id),
		overall_score REAL NOT NULL,
		tool_success_rate REAL,
		correction_count INTEGER DEFAULT 0,
		retry_count INTEGER DEFAULT 0,
		effort_override_count INTEGER DEFAULT 0,
		llm_quality_score REAL,
		details TEXT,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_eval_scores_conv ON eval_scores(conversation_id);
	CREATE INDEX IF NOT EXISTS idx_eval_scores_run ON eval_scores(eval_run_id);
	`
	if _, err := s.db.Exec(evalPipelineSchema); err != nil {
		return fmt.Errorf("memory: create eval pipeline tables: %w", err)
	}

	return nil
}

// LogInteractionEvent records a user interaction event (e.g., /effort, /retry, /debug).
func (s *Store) LogInteractionEvent(convID string, userID, chatID int64, eventType, payload string) error {
	_, err := s.db.Exec(
		`INSERT INTO interaction_events (conversation_id, user_id, chat_id, event_type, payload, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		convID, userID, chatID, eventType, payload, time.Now().UTC(),
	)
	if err != nil {
		return fmt.Errorf("memory: log interaction event: %w", err)
	}
	return nil
}

// MapTelegramMessage records the association between a Telegram message ID and a conversation.
// This allows looking up which conversation a Telegram reaction belongs to.
func (s *Store) MapTelegramMessage(chatID int64, tgMsgID int, convID string) error {
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO telegram_message_map (chat_id, telegram_message_id, conversation_id, created_at)
		 VALUES (?, ?, ?, ?)`,
		chatID, tgMsgID, convID, time.Now().UTC(),
	)
	if err != nil {
		return fmt.Errorf("memory: map telegram message: %w", err)
	}
	return nil
}

// LookupConversationByTelegramMessage returns the conversation ID for a Telegram message.
func (s *Store) LookupConversationByTelegramMessage(chatID int64, tgMsgID int) (string, error) {
	var convID string
	err := s.db.QueryRow(
		`SELECT conversation_id FROM telegram_message_map WHERE chat_id = ? AND telegram_message_id = ?`,
		chatID, tgMsgID,
	).Scan(&convID)
	if err != nil {
		return "", fmt.Errorf("memory: lookup telegram message: %w", err)
	}
	return convID, nil
}

// LogEvalReaction records a Telegram reaction (thumbs up/down) on a bot message.
func (s *Store) LogEvalReaction(convID string, userID, chatID int64, tgMsgID int, reaction string) error {
	_, err := s.db.Exec(
		`INSERT INTO eval_reactions (conversation_id, user_id, chat_id, telegram_message_id, reaction, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(user_id, chat_id, telegram_message_id) DO UPDATE SET reaction = excluded.reaction, created_at = excluded.created_at`,
		convID, userID, chatID, tgMsgID, reaction, time.Now().UTC(),
	)
	if err != nil {
		return fmt.Errorf("memory: log eval reaction: %w", err)
	}
	return nil
}

// InteractionEvent represents a recorded user interaction event.
type InteractionEvent struct {
	ID             int64
	ConversationID string
	UserID         int64
	ChatID         int64
	EventType      string
	Payload        string
	CreatedAt      time.Time
}

// GetInteractionEvents returns interaction events for a conversation, ordered by time.
func (s *Store) GetInteractionEvents(convID string) ([]InteractionEvent, error) {
	rows, err := s.db.Query(
		`SELECT id, conversation_id, user_id, chat_id, event_type, COALESCE(payload, ''), created_at
		 FROM interaction_events WHERE conversation_id = ? ORDER BY created_at ASC`,
		convID,
	)
	if err != nil {
		return nil, fmt.Errorf("memory: get interaction events: %w", err)
	}
	defer rows.Close()

	var events []InteractionEvent
	for rows.Next() {
		var e InteractionEvent
		var ts string
		if err := rows.Scan(&e.ID, &e.ConversationID, &e.UserID, &e.ChatID, &e.EventType, &e.Payload, &ts); err != nil {
			return nil, fmt.Errorf("memory: scan interaction event: %w", err)
		}
		e.CreatedAt, _ = parseTimeStr(ts)
		events = append(events, e)
	}
	return events, rows.Err()
}

// GetInteractionEventsByUser returns events with empty conversation_id for a user/chat pair.
// These are events logged before conversation lookup (e.g., /effort, /retry commands).
func (s *Store) GetInteractionEventsByUser(userID, chatID int64) ([]InteractionEvent, error) {
	rows, err := s.db.Query(
		`SELECT id, conversation_id, user_id, chat_id, event_type, COALESCE(payload, ''), created_at
		 FROM interaction_events WHERE conversation_id = '' AND user_id = ? AND chat_id = ? ORDER BY created_at ASC`,
		userID, chatID,
	)
	if err != nil {
		return nil, fmt.Errorf("memory: get interaction events by user: %w", err)
	}
	defer rows.Close()

	var events []InteractionEvent
	for rows.Next() {
		var e InteractionEvent
		var ts string
		if err := rows.Scan(&e.ID, &e.ConversationID, &e.UserID, &e.ChatID, &e.EventType, &e.Payload, &ts); err != nil {
			return nil, fmt.Errorf("memory: scan interaction event: %w", err)
		}
		e.CreatedAt, _ = parseTimeStr(ts)
		events = append(events, e)
	}
	return events, rows.Err()
}

// ConversationMeta holds lightweight metadata for a conversation.
type ConversationMeta struct {
	ID        string
	UserID    int64
	ChatID    int64
	UpdatedAt time.Time
}

// GetConversationsSince returns conversations that had messages after the given time.
func (s *Store) GetConversationsSince(after time.Time) ([]ConversationMeta, error) {
	rows, err := s.db.Query(
		`SELECT DISTINCT c.id, c.user_id, c.chat_id, c.updated_at
		 FROM conversations c
		 JOIN messages m ON m.conversation_id = c.id
		 WHERE m.created_at > ?
		 ORDER BY c.updated_at DESC`,
		after.UTC(),
	)
	if err != nil {
		return nil, fmt.Errorf("memory: get conversations since: %w", err)
	}
	defer rows.Close()

	var convs []ConversationMeta
	for rows.Next() {
		var c ConversationMeta
		var ts string
		if err := rows.Scan(&c.ID, &c.UserID, &c.ChatID, &ts); err != nil {
			return nil, fmt.Errorf("memory: scan conversation: %w", err)
		}
		c.UpdatedAt, _ = parseTimeStr(ts)
		convs = append(convs, c)
	}
	return convs, rows.Err()
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

// FTSResult holds a keyword search result from the observations_fts table.
type FTSResult struct {
	ObsRowid int64
	ObsID    string
	Title    string
	Summary  string
	Rank     float64
}

// SearchObservationsFTS performs keyword search on observations using FTS5.
// Returns results sorted by BM25 relevance. The query is escaped for safe MATCH use.
func (s *Store) SearchObservationsFTS(query string, userID int64, limit int) ([]FTSResult, error) {
	escaped := EscapeFTS5Query(query)
	if escaped == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.db.Query(
		`SELECT o.rowid, o.id, o.title, o.summary, f.rank
		 FROM observations_fts f
		 JOIN observations o ON o.rowid = f.rowid
		 WHERE observations_fts MATCH ? AND o.user_id = ? AND o.archived_at IS NULL
		 ORDER BY f.rank
		 LIMIT ?`,
		escaped, userID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("memory: fts5 search: %w", err)
	}
	defer rows.Close()

	var results []FTSResult
	for rows.Next() {
		var r FTSResult
		if err := rows.Scan(&r.ObsRowid, &r.ObsID, &r.Title, &r.Summary, &r.Rank); err != nil {
			return results, fmt.Errorf("memory: fts5 scan: %w", err)
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// RebuildFTS rebuilds the FTS5 indexes from the content tables.
// Call on startup if schema version changed or if FTS5 might be out of sync.
func (s *Store) RebuildFTS() error {
	if _, err := s.db.Exec(`INSERT INTO observations_fts(observations_fts) VALUES('rebuild')`); err != nil {
		return fmt.Errorf("memory: rebuild observations_fts: %w", err)
	}
	if _, err := s.db.Exec(`INSERT INTO observation_facts_fts(observation_facts_fts) VALUES('rebuild')`); err != nil {
		return fmt.Errorf("memory: rebuild observation_facts_fts: %w", err)
	}
	if _, err := s.db.Exec(`INSERT INTO observation_entities_fts(observation_entities_fts) VALUES('rebuild')`); err != nil {
		return fmt.Errorf("memory: rebuild observation_entities_fts: %w", err)
	}
	return nil
}

// AllObservations returns all observations with their facts for reindexing.
func (s *Store) AllObservations(limit int) ([]Observation, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.Query(
		`SELECT id, conversation_id, user_id, chat_id, COALESCE(chat_type, 'private'),
		        type, title, summary, importance, created_at
		 FROM observations WHERE archived_at IS NULL ORDER BY rowid ASC LIMIT ?`, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("memory: all observations: %w", err)
	}
	defer rows.Close()

	var obs []Observation
	for rows.Next() {
		var o Observation
		if err := rows.Scan(&o.ID, &o.ConversationID, &o.UserID, &o.ChatID, &o.ChatType,
			&o.Type, &o.Title, &o.Summary, &o.Importance, &o.CreatedAt); err != nil {
			return obs, fmt.Errorf("memory: scan observation: %w", err)
		}
		obs = append(obs, o)
	}
	if err := rows.Err(); err != nil {
		return obs, err
	}

	// Hydrate facts.
	if len(obs) > 0 {
		ids := make([]string, len(obs))
		for i, o := range obs {
			ids[i] = o.ID
		}
		factsMap, err := s.GetObservationFactsByIDs(ids)
		if err != nil {
			return obs, fmt.Errorf("memory: hydrate facts: %w", err)
		}
		for i := range obs {
			obs[i].Facts = factsMap[obs[i].ID]
		}
	}

	return obs, nil
}

// ObservationTextsAfter returns observation texts for migration backfill.
// Each observation yields "Title. Summary" as the text for embedding.
func (s *Store) ObservationTextsAfter(afterID int64, limit int) ([]MigrationText, int64, error) {
	rows, err := s.db.Query(
		`SELECT rowid, id, user_id, chat_id, title, summary, COALESCE(chat_type, 'private')
		 FROM observations
		 WHERE rowid > ? AND archived_at IS NULL
		 ORDER BY rowid ASC
		 LIMIT ?`, afterID, limit,
	)
	if err != nil {
		return nil, afterID, fmt.Errorf("memory: observation texts after: %w", err)
	}
	defer rows.Close()

	var results []MigrationText
	maxID := afterID
	for rows.Next() {
		var rowid int64
		var obsID string
		var userID, chatID int64
		var title, summary, chatType string
		if err := rows.Scan(&rowid, &obsID, &userID, &chatID, &title, &summary, &chatType); err != nil {
			return nil, maxID, fmt.Errorf("memory: scan observation text: %w", err)
		}
		if rowid > maxID {
			maxID = rowid
		}
		results = append(results, MigrationText{
			ID:       obsID,
			Text:     title + ". " + summary,
			UserID:   userID,
			ChatID:   chatID,
			Source:   "observation",
			ChatType: chatType,
		})
	}
	return results, maxID, rows.Err()
}

// SaveEntities stores entities for an observation. Does not fail on empty slice.
func (s *Store) SaveEntities(obsID string, entities []Entity) error {
	if len(entities) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("memory: begin entity tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	stmt, err := tx.Prepare(`INSERT INTO observation_entities (observation_id, name, entity_type) VALUES (?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("memory: prepare entity insert: %w", err)
	}
	defer stmt.Close()

	for _, e := range entities {
		if _, err := stmt.Exec(obsID, e.Name, e.Type); err != nil {
			return fmt.Errorf("memory: insert entity: %w", err)
		}
	}
	return tx.Commit()
}

// GetEntitiesByObservationIDs returns entities grouped by observation ID.
func (s *Store) GetEntitiesByObservationIDs(ids []string) (map[string][]Entity, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	query := fmt.Sprintf(
		`SELECT observation_id, name, entity_type FROM observation_entities WHERE observation_id IN (%s)`,
		strings.Join(placeholders, ","),
	)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("memory: get entities by IDs: %w", err)
	}
	defer rows.Close()

	result := make(map[string][]Entity)
	for rows.Next() {
		var obsID, name, eType string
		if err := rows.Scan(&obsID, &name, &eType); err != nil {
			return result, err
		}
		result[obsID] = append(result[obsID], Entity{Name: name, Type: eType})
	}
	return result, rows.Err()
}

// EntitySearchResult holds a result from entity FTS5 search.
type EntitySearchResult struct {
	ObservationID string
	Name          string
	EntityType    string
}

// SearchEntitiesFTS performs keyword search on entity names using FTS5.
func (s *Store) SearchEntitiesFTS(query string, entityType string, userID int64, limit int) ([]EntitySearchResult, error) {
	escaped := EscapeFTS5Query(query)
	if escaped == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}

	var rows *sql.Rows
	var err error
	if entityType != "" {
		rows, err = s.db.Query(
			`SELECT e.observation_id, e.name, e.entity_type
			 FROM observation_entities_fts f
			 JOIN observation_entities e ON e.rowid = f.rowid
			 JOIN observations o ON o.id = e.observation_id
			 WHERE observation_entities_fts MATCH ? AND o.user_id = ? AND e.entity_type = ?
			 ORDER BY f.rank
			 LIMIT ?`,
			escaped, userID, entityType, limit,
		)
	} else {
		rows, err = s.db.Query(
			`SELECT e.observation_id, e.name, e.entity_type
			 FROM observation_entities_fts f
			 JOIN observation_entities e ON e.rowid = f.rowid
			 JOIN observations o ON o.id = e.observation_id
			 WHERE observation_entities_fts MATCH ? AND o.user_id = ?
			 ORDER BY f.rank
			 LIMIT ?`,
			escaped, userID, limit,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("memory: entity fts search: %w", err)
	}
	defer rows.Close()

	var results []EntitySearchResult
	for rows.Next() {
		var r EntitySearchResult
		if err := rows.Scan(&r.ObservationID, &r.Name, &r.EntityType); err != nil {
			return results, err
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// DeleteEntitiesByObservation removes all entities for a given observation.
func (s *Store) DeleteEntitiesByObservation(obsID string) error {
	_, err := s.db.Exec(`DELETE FROM observation_entities WHERE observation_id = ?`, obsID)
	if err != nil {
		return fmt.Errorf("memory: delete entities: %w", err)
	}
	return nil
}

// ObservationRelation represents a relationship between two observations.
type ObservationRelation struct {
	ID           int64
	SourceID     string // newer observation
	TargetID     string // older observation
	RelationType string // supersedes, refines, contradicts
	Confidence   float64
	Confirmed    bool
	CreatedAt    time.Time
}

// AddObservationRelation creates a relation between two observations.
// IDOR protection: both source and target must belong to the same user.
// Uses INSERT OR IGNORE for concurrency safety (first write wins on duplicate).
// Confidence is clamped to [0.0, 1.0].
func (s *Store) AddObservationRelation(sourceID, targetID, relationType string, confidence float64, userID int64) error {
	if confidence < 0 {
		confidence = 0
	}
	if confidence > 1 {
		confidence = 1
	}
	res, err := s.db.Exec(
		`INSERT OR IGNORE INTO observation_relations (source_id, target_id, relation_type, confidence)
		 SELECT ?, ?, ?, ?
		 WHERE EXISTS (SELECT 1 FROM observations WHERE id = ? AND user_id = ?)
		   AND EXISTS (SELECT 1 FROM observations WHERE id = ? AND user_id = ?)`,
		sourceID, targetID, relationType, confidence,
		sourceID, userID, targetID, userID,
	)
	if err != nil {
		return fmt.Errorf("memory: add observation relation: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("memory: observation relation not created (invalid IDs or wrong user)")
	}
	return nil
}

// GetObservationRelations returns all relations where obsID is source or target.
// Scoped by user_id via JOIN to observations.
func (s *Store) GetObservationRelations(obsID string, userID int64) ([]ObservationRelation, error) {
	rows, err := s.db.Query(
		`SELECT r.id, r.source_id, r.target_id, r.relation_type, r.confidence, r.confirmed, r.created_at
		 FROM observation_relations r
		 JOIN observations src ON src.id = r.source_id
		 JOIN observations tgt ON tgt.id = r.target_id
		 WHERE (r.source_id = ? OR r.target_id = ?) AND src.user_id = ? AND tgt.user_id = ?`,
		obsID, obsID, userID, userID,
	)
	if err != nil {
		return nil, fmt.Errorf("memory: get observation relations: %w", err)
	}
	defer rows.Close()

	var results []ObservationRelation
	for rows.Next() {
		var r ObservationRelation
		if err := rows.Scan(&r.ID, &r.SourceID, &r.TargetID, &r.RelationType, &r.Confidence, &r.Confirmed, &r.CreatedAt); err != nil {
			return results, err
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// DeleteSupersessionRelation removes supersession relations targeting a given observation.
// Used by /keep_both and /revert commands to undo a supersession.
func (s *Store) DeleteSupersessionRelation(targetID string, userID int64) error {
	_, err := s.db.Exec(
		`DELETE FROM observation_relations
		 WHERE target_id = ? AND relation_type = 'supersedes'
		   AND EXISTS (SELECT 1 FROM observations WHERE id = observation_relations.target_id AND user_id = ?)`,
		targetID, userID,
	)
	if err != nil {
		return fmt.Errorf("memory: delete supersession relation: %w", err)
	}
	return nil
}

// GetSupersessionSourceID returns the source observation ID that superseded the given target.
// Used by /revert to archive the replacement observation.
func (s *Store) GetSupersessionSourceID(targetID string, userID int64) (string, error) {
	var sourceID string
	err := s.db.QueryRow(
		`SELECT r.source_id FROM observation_relations r
		 JOIN observations o ON o.id = r.source_id
		 WHERE r.target_id = ? AND r.relation_type = 'supersedes' AND o.user_id = ?
		 LIMIT 1`,
		targetID, userID,
	).Scan(&sourceID)
	if err != nil {
		return "", err
	}
	return sourceID, nil
}

// GetSupersededObservationIDs returns observation IDs that have been superseded
// (are targets of a 'supersedes' relation with confidence >= threshold) for the given user.
// On DB error, returns an empty map (graceful degradation: stale obs may appear).
func (s *Store) GetSupersededObservationIDs(userID int64, confidenceThreshold float64) (map[string]bool, error) {
	rows, err := s.db.Query(
		`SELECT r.target_id
		 FROM observation_relations r
		 JOIN observations o ON o.id = r.target_id
		 WHERE r.relation_type = 'supersedes'
		   AND (r.confirmed = 1 OR r.confidence >= ?)
		   AND o.user_id = ?`,
		confidenceThreshold, userID,
	)
	if err != nil {
		return make(map[string]bool), nil // graceful degradation
	}
	defer rows.Close()

	ids := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return ids, err
		}
		ids[id] = true
	}
	return ids, rows.Err()
}

// EscapeFTS5Query wraps user input in double quotes for safe use in FTS5
// MATCH queries, escaping any embedded double quotes by doubling them.
// Returns an empty string for blank input (callers should skip MATCH in that case).
func EscapeFTS5Query(query string) string {
	trimmed := strings.TrimSpace(query)
	if trimmed == "" {
		return ""
	}
	escaped := strings.ReplaceAll(trimmed, `"`, `""`)
	return `"` + escaped + `"`
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

// EnsureConversation creates a lightweight conversation row if it doesn't exist.
// Used by email ingest to satisfy FK constraints with synthetic conversation IDs.
func (s *Store) EnsureConversation(id string, userID int64) error {
	now := time.Now().UTC()
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO conversations (id, user_id, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		id, userID, now, now,
	)
	if err != nil {
		return fmt.Errorf("memory: ensure conversation: %w", err)
	}
	return nil
}

// migrateEmailIngestTables migrates legacy email_ingest_state and
// email_processed_messages tables into the new generic ingest tables.
func (s *Store) migrateEmailIngestTables() {
	// Check if old table exists.
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='email_ingest_state'`).Scan(&count)
	if err != nil || count == 0 {
		return
	}

	// Migrate state rows.
	if _, err := s.db.Exec(`
		INSERT OR IGNORE INTO ingest_state (source, partition, mode, cursor, backfill_cursor, status,
			stats_scanned, stats_prefiltered, stats_llm_triaged, stats_kept, stats_failed,
			last_run_at, updated_at)
		SELECT 'gmail', account, mode, cursor, backfill_cursor, status,
			stats_scanned, stats_prefiltered, stats_llm_triaged, stats_kept, stats_failed,
			last_run_at, updated_at
		FROM email_ingest_state
	`); err != nil {
		slog.Warn("ingest: legacy state migration failed", "err", err)
	}

	// Migrate processed messages.
	if _, err := s.db.Exec(`
		INSERT OR IGNORE INTO ingest_processed_items (source, item_id, result, attempt_count, processed_at)
		SELECT 'gmail', message_id, result, attempt_count, processed_at
		FROM email_processed_messages
	`); err != nil {
		slog.Warn("ingest: legacy processed items migration failed", "err", err)
	}
}

// GetIngestState returns the ingest state for a source+partition.
func (s *Store) GetIngestState(source, partition string) (mode, cursor, backfillCursor, status string, err error) {
	err = s.db.QueryRow(
		`SELECT mode, cursor, backfill_cursor, status FROM ingest_state WHERE source = ? AND partition = ?`,
		source, partition,
	).Scan(&mode, &cursor, &backfillCursor, &status)
	if err == sql.ErrNoRows {
		return "backfill", "", "", "idle", nil
	}
	if err != nil {
		return "", "", "", "", fmt.Errorf("memory: get ingest state: %w", err)
	}
	return
}

// UpdateIngestState upserts the ingest state for a source+partition.
func (s *Store) UpdateIngestState(source, partition, mode, cursor, backfillCursor, status string) error {
	_, err := s.db.Exec(
		`INSERT INTO ingest_state (source, partition, mode, cursor, backfill_cursor, status, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(source, partition) DO UPDATE SET mode=excluded.mode, cursor=excluded.cursor,
		 backfill_cursor=excluded.backfill_cursor, status=excluded.status, updated_at=CURRENT_TIMESTAMP`,
		source, partition, mode, cursor, backfillCursor, status,
	)
	if err != nil {
		return fmt.Errorf("memory: update ingest state: %w", err)
	}
	return nil
}

// UpdateIngestStats updates the statistics counters for a source+partition.
func (s *Store) UpdateIngestStats(source, partition string, scanned, prefiltered, llmTriaged, kept, failed int) error {
	_, err := s.db.Exec(
		`UPDATE ingest_state SET
		 stats_scanned = stats_scanned + ?,
		 stats_prefiltered = stats_prefiltered + ?,
		 stats_llm_triaged = stats_llm_triaged + ?,
		 stats_kept = stats_kept + ?,
		 stats_failed = stats_failed + ?,
		 last_run_at = CURRENT_TIMESTAMP,
		 updated_at = CURRENT_TIMESTAMP
		 WHERE source = ? AND partition = ?`,
		scanned, prefiltered, llmTriaged, kept, failed, source, partition,
	)
	if err != nil {
		return fmt.Errorf("memory: update ingest stats: %w", err)
	}
	return nil
}

// IsItemProcessed checks if an item has been successfully processed (not failed).
func (s *Store) IsItemProcessed(source, itemID string) (bool, error) {
	var result string
	err := s.db.QueryRow(
		`SELECT result FROM ingest_processed_items WHERE source = ? AND item_id = ?`,
		source, itemID,
	).Scan(&result)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("memory: check item processed: %w", err)
	}
	return result != "failed", nil
}

// GetItemFingerprint returns the content fingerprint for a processed item.
func (s *Store) GetItemFingerprint(source, itemID string) (string, error) {
	var fingerprint string
	err := s.db.QueryRow(
		`SELECT fingerprint FROM ingest_processed_items WHERE source = ? AND item_id = ?`,
		source, itemID,
	).Scan(&fingerprint)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("memory: get item fingerprint: %w", err)
	}
	return fingerprint, nil
}

// RecordItemProcessed records the processing result for an item.
func (s *Store) RecordItemProcessed(source, itemID, result, fingerprint string) error {
	_, err := s.db.Exec(
		`INSERT INTO ingest_processed_items (source, item_id, result, fingerprint)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(source, item_id) DO UPDATE SET result=excluded.result, fingerprint=excluded.fingerprint,
		 attempt_count=attempt_count+1, processed_at=CURRENT_TIMESTAMP`,
		source, itemID, result, fingerprint,
	)
	if err != nil {
		return fmt.Errorf("memory: record item processed: %w", err)
	}
	return nil
}

// CleanupOldProcessedItems removes entries older than the given number of days.
func (s *Store) CleanupOldProcessedItems(days int) (int64, error) {
	result, err := s.db.Exec(
		`DELETE FROM ingest_processed_items WHERE processed_at < datetime('now', ?)`,
		fmt.Sprintf("-%d days", days),
	)
	if err != nil {
		return 0, fmt.Errorf("memory: cleanup old processed items: %w", err)
	}
	return result.RowsAffected()
}
