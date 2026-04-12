package skills

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
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
		Description: "Queue a file for delivery to the user in Telegram. Pass the content directly as a string — do NOT write to disk first. For binary files (images, PDFs), pass the base64-encoded content and it will be auto-decoded. The file is delivered after your response completes. Call this exactly once per file.",
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

		// Detect and decode base64-encoded binary content (e.g., PNG images from MCP tools).
		// Data URI prefix ("data:image/png;base64,...") is an explicit signal — if it's
		// present, the content MUST decode as base64 or we return an error rather than
		// shipping a file that contains the garbage prefix.
		content := params.Content
		hasDataURI := false
		if idx := strings.Index(content, ";base64,"); idx >= 0 {
			content = content[idx+8:]
			hasDataURI = true
		}
		decoded, decodeErr := base64.StdEncoding.DecodeString(strings.TrimSpace(content))
		switch {
		case hasDataURI && decodeErr != nil:
			return "", fmt.Errorf("data URI content is not valid base64: %w", decodeErr)
		case hasDataURI:
			data = decoded
		case decodeErr == nil && len(decoded) > 0:
			// No data URI prefix — best-effort decode. Heuristic: only treat as
			// base64 if it's long enough that accidental collision with plain
			// text (e.g., a 4-char word like "YWJj" that happens to decode)
			// is unlikely. 40 chars ≈ 30 bytes, enough to fit typical headers.
			if len(strings.TrimSpace(content)) >= 40 {
				data = decoded
			}
		}

		if err := sender.SendDocument(user.ChatID, safeName, data, ""); err != nil {
			return "", fmt.Errorf("send document: %w", err)
		}

		return fmt.Sprintf("File queued: %s (%d bytes). It will be delivered to the user as a Telegram document when this response completes. Do NOT retry or call send_file again for the same file.", safeName, len(data)), nil
	}
}
