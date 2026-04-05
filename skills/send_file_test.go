package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

// mockDocumentSender records SendDocument calls for test assertions.
type mockDocumentSender struct {
	lastChatID   int64
	lastFileName string
	lastData     []byte
	lastCaption  string
	err          error
}

func (m *mockDocumentSender) SendDocument(chatID int64, fileName string, data []byte, caption string) error {
	m.lastChatID = chatID
	m.lastFileName = fileName
	m.lastData = data
	m.lastCaption = caption
	return m.err
}

func TestSendFileSkill_Success(t *testing.T) {
	sender := &mockDocumentSender{}
	skill := NewSendFileSkill(sender)

	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 42})
	input, _ := json.Marshal(sendFileInput{Filename: "report.csv", Content: "a,b,c\n1,2,3"})

	result, err := skill.Execute(ctx, input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sender.lastChatID != 42 {
		t.Errorf("chatID = %d, want 42", sender.lastChatID)
	}
	if sender.lastFileName != "report.csv" {
		t.Errorf("fileName = %q, want %q", sender.lastFileName, "report.csv")
	}
	if string(sender.lastData) != "a,b,c\n1,2,3" {
		t.Errorf("data = %q, want %q", string(sender.lastData), "a,b,c\n1,2,3")
	}
	if result != "File sent: report.csv (11 bytes)" {
		t.Errorf("result = %q, want %q", result, "File sent: report.csv (11 bytes)")
	}
}

func TestSendFileSkill_PathTraversal(t *testing.T) {
	sender := &mockDocumentSender{}
	skill := NewSendFileSkill(sender)

	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 42})

	cases := []struct {
		name     string
		filename string
		wantName string // expected sanitized filename
	}{
		{"directory_traversal", "../../etc/passwd", "passwd"},
		{"absolute_path", "/etc/shadow", "shadow"},
		{"nested_path", "foo/bar/baz.txt", "baz.txt"},
		{"clean_name", "output.txt", "output.txt"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input, _ := json.Marshal(sendFileInput{Filename: tc.filename, Content: "data"})
			_, err := skill.Execute(ctx, input)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if sender.lastFileName != tc.wantName {
				t.Errorf("fileName = %q, want %q", sender.lastFileName, tc.wantName)
			}
		})
	}
}

func TestSendFileSkill_EmptyFilename(t *testing.T) {
	sender := &mockDocumentSender{}
	skill := NewSendFileSkill(sender)

	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 42})
	input, _ := json.Marshal(sendFileInput{Filename: "", Content: "data"})

	_, err := skill.Execute(ctx, input)
	if err == nil {
		t.Fatal("expected error for empty filename")
	}
}

func TestSendFileSkill_EmptyContent(t *testing.T) {
	sender := &mockDocumentSender{}
	skill := NewSendFileSkill(sender)

	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 42})
	input, _ := json.Marshal(sendFileInput{Filename: "file.txt", Content: ""})

	_, err := skill.Execute(ctx, input)
	if err == nil {
		t.Fatal("expected error for empty content")
	}
}

func TestSendFileSkill_NoChatContext(t *testing.T) {
	sender := &mockDocumentSender{}
	skill := NewSendFileSkill(sender)

	// No user context set — ChatID will be 0.
	ctx := context.Background()
	input, _ := json.Marshal(sendFileInput{Filename: "file.txt", Content: "data"})

	_, err := skill.Execute(ctx, input)
	if err == nil {
		t.Fatal("expected error for missing chat context")
	}
}

func TestSendFileSkill_SenderError(t *testing.T) {
	sender := &mockDocumentSender{err: fmt.Errorf("telegram API down")}
	skill := NewSendFileSkill(sender)

	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 42})
	input, _ := json.Marshal(sendFileInput{Filename: "file.txt", Content: "data"})

	_, err := skill.Execute(ctx, input)
	if err == nil {
		t.Fatal("expected error when sender fails")
	}
}
