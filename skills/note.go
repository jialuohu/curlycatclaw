package skills

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// InitNoteSkills creates the notes table (if not exists) and returns
// the save_note and search_notes skills. The db should be the same
// *sql.DB used by the memory store.
func InitNoteSkills(db *sql.DB) ([]*Skill, error) {
	const schema = `CREATE TABLE IF NOT EXISTS notes (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id    INTEGER NOT NULL DEFAULT 0,
		chat_id    INTEGER NOT NULL DEFAULT 0,
		title      TEXT NOT NULL,
		content    TEXT NOT NULL,
		created_at DATETIME NOT NULL
	)`
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("skills: create notes table: %w", err)
	}

	saveSkill := &Skill{
		Name:        "save_note",
		Description: "Save a note with a title and content for later retrieval.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"title":{"type":"string","description":"Title of the note"},"content":{"type":"string","description":"Content of the note"}},"required":["title","content"]}`),
		Execute:     makeSaveNoteExecute(db),
	}

	searchSkill := &Skill{
		Name:        "search_notes",
		Description: "Search saved notes by keyword. Returns matching notes.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"Keyword to search for in note titles and content"}},"required":["query"]}`),
		Execute:     makeSearchNotesExecute(db),
	}

	return []*Skill{saveSkill, searchSkill}, nil
}

type saveNoteInput struct {
	Title   string `json:"title"`
	Content string `json:"content"`
}

func makeSaveNoteExecute(db *sql.DB) func(ctx context.Context, input json.RawMessage) (string, error) {
	return func(ctx context.Context, input json.RawMessage) (string, error) {
		var params saveNoteInput
		if err := json.Unmarshal(input, &params); err != nil {
			return "", fmt.Errorf("invalid input: %w", err)
		}
		if params.Title == "" {
			return "", fmt.Errorf("title is required")
		}
		if params.Content == "" {
			return "", fmt.Errorf("content is required")
		}

		user := GetUser(ctx)
		now := time.Now().UTC()
		_, err := db.ExecContext(ctx,
			`INSERT INTO notes (user_id, chat_id, title, content, created_at) VALUES (?, ?, ?, ?, ?)`,
			user.UserID, user.ChatID, params.Title, params.Content, now,
		)
		if err != nil {
			return "", fmt.Errorf("save note: %w", err)
		}

		return "Note saved: " + params.Title, nil
	}
}

type searchNotesInput struct {
	Query string `json:"query"`
}

func makeSearchNotesExecute(db *sql.DB) func(ctx context.Context, input json.RawMessage) (string, error) {
	return func(ctx context.Context, input json.RawMessage) (string, error) {
		var params searchNotesInput
		if err := json.Unmarshal(input, &params); err != nil {
			return "", fmt.Errorf("invalid input: %w", err)
		}
		if params.Query == "" {
			return "", fmt.Errorf("query is required")
		}

		user := GetUser(ctx)
		pattern := "%" + params.Query + "%"
		rows, err := db.QueryContext(ctx,
			`SELECT title, content, created_at FROM notes WHERE user_id = ? AND (title LIKE ? OR content LIKE ?) ORDER BY created_at DESC`,
			user.UserID, pattern, pattern,
		)
		if err != nil {
			return "", fmt.Errorf("search notes: %w", err)
		}
		defer rows.Close()

		var sb strings.Builder
		count := 0
		for rows.Next() {
			var title, content string
			var createdAt time.Time
			if err := rows.Scan(&title, &content, &createdAt); err != nil {
				return "", fmt.Errorf("scan note: %w", err)
			}
			count++
			fmt.Fprintf(&sb, "--- %s ---\n", title)
			fmt.Fprintf(&sb, "Created: %s\n", createdAt.Format("2006-01-02 15:04"))
			sb.WriteString(content)
			sb.WriteString("\n\n")
		}
		if err := rows.Err(); err != nil {
			return "", fmt.Errorf("iterate notes: %w", err)
		}

		if count == 0 {
			return fmt.Sprintf("No notes found matching '%s'", params.Query), nil
		}

		return strings.TrimSpace(sb.String()), nil
	}
}
