package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
)

// DocumentSender abstracts the Telegram document send capability.
type DocumentSender interface {
	SendDocument(chatID int64, fileName string, data []byte, caption string) error
}

// NewSendFileSkill creates a skill that lets Claude send a file to the user
// in the current Telegram chat.
func NewSendFileSkill(sender DocumentSender) *Skill {
	return &Skill{
		Name:        "send_file",
		Description: "Send a file to the user in the current Telegram chat. Use this to deliver generated code, data exports, reports, or any content as a downloadable file.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"filename":{"type":"string","description":"Name for the file (e.g. report.csv, code.go)"},"content":{"type":"string","description":"The file content as text"}},"required":["filename","content"]}`),
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

		return fmt.Sprintf("File sent: %s (%d bytes)", safeName, len(data)), nil
	}
}
