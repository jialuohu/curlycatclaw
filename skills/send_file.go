package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
)

// FileQueuer abstracts queuing a file for later delivery (MCP server mode).
type FileQueuer interface {
	QueueFile(chatID int64, fileName string, data []byte) error
}

// QueuedDocumentSender implements DocumentSender by writing to a SQLite queue.
// Used in MCP server mode where direct Telegram access is unavailable.
type QueuedDocumentSender struct {
	Queue FileQueuer
}

func (q *QueuedDocumentSender) SendDocument(chatID int64, fileName string, data []byte, _ string) error {
	return q.Queue.QueueFile(chatID, fileName, data)
}

// DocumentSender abstracts the Telegram document send capability.
type DocumentSender interface {
	SendDocument(chatID int64, fileName string, data []byte, caption string) error
}

// NewSendFileSkill creates a skill that lets Claude send a file to the user
// in the current Telegram chat.
func NewSendFileSkill(sender DocumentSender) *Skill {
	return &Skill{
		Name:        "send_file",
		Description: "Queue a file for delivery to the user in Telegram. Pass the content directly as a string — do NOT write to disk first. The file is delivered after your response completes. Call this exactly once per file.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"filename":{"type":"string","description":"Name for the file (e.g. report.csv, code.go)"},"content":{"type":"string","description":"The full file content as a string. Pass it directly, do not use a file path."}},"required":["filename","content"]}`),
		Execute:     makeSendFileExecute(sender),
	}
}

type sendFileInput struct {
	Filename string `json:"filename"`
	Content  string `json:"content"`
}

func makeSendFileExecute(sender DocumentSender) func(ctx context.Context, input json.RawMessage) (string, error) {
	return func(ctx context.Context, input json.RawMessage) (string, error) {
		var params sendFileInput
		if err := json.Unmarshal(input, &params); err != nil {
			return "", fmt.Errorf("invalid input: %w", err)
		}
		if params.Filename == "" {
			return "", fmt.Errorf("filename is required")
		}
		if params.Content == "" {
			return "", fmt.Errorf("content is required")
		}

		// Sanitize filename: strip any directory components to prevent path traversal.
		safeName := filepath.Base(params.Filename)
		if safeName == "." || safeName == string(filepath.Separator) {
			return "", fmt.Errorf("invalid filename: %q", params.Filename)
		}

		user := GetUser(ctx)
		if user.ChatID == 0 {
			return "", fmt.Errorf("no chat context available")
		}

		data := []byte(params.Content)
		if err := sender.SendDocument(user.ChatID, safeName, data, ""); err != nil {
			return "", fmt.Errorf("send document: %w", err)
		}

		return fmt.Sprintf("File queued: %s (%d bytes). It will be delivered to the user as a Telegram document when this response completes. Do NOT retry or call send_file again for the same file.", safeName, len(data)), nil
	}
}
