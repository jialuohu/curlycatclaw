package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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
	if !strings.Contains(result, "File queued: report.csv (11 bytes)") {
		t.Errorf("result = %q, want it to contain %q", result, "File queued: report.csv (11 bytes)")
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

// TestSendFileSkill_DataURI_Decodes verifies the explicit data-URI path decodes
// the base64 payload and drops the prefix.
func TestSendFileSkill_DataURI_Decodes(t *testing.T) {
	sender := &mockDocumentSender{}
	skill := NewSendFileSkill(sender)

	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 42})
	// "Hello!" encoded as base64 is "SGVsbG8h".
	content := "data:text/plain;base64,SGVsbG8h"
	input, _ := json.Marshal(sendFileInput{Filename: "greet.txt", Content: content})

	if _, err := skill.Execute(ctx, input); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(sender.lastData) != "Hello!" {
		t.Errorf("data = %q, want %q", string(sender.lastData), "Hello!")
	}
}

// TestSendFileSkill_DataURI_InvalidBase64 verifies an explicit data-URI prefix
// with broken base64 is rejected rather than silently shipping the garbage.
func TestSendFileSkill_DataURI_InvalidBase64(t *testing.T) {
	sender := &mockDocumentSender{}
	skill := NewSendFileSkill(sender)

	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 42})
	// Clearly broken base64 after the prefix.
	input, _ := json.Marshal(sendFileInput{Filename: "x.png", Content: "data:image/png;base64,$$$not_base64$$$"})

	_, err := skill.Execute(ctx, input)
	if err == nil {
		t.Fatal("expected error when data URI payload is not valid base64")
	}
}

// TestSendFileSkill_ShortText_NotDecoded verifies short strings that happen to
// pass base64 validation (e.g. "YWJj" = "abc") are NOT auto-decoded. Prior bug:
// any 4-char base64-looking content was silently decoded and replaced.
func TestSendFileSkill_ShortText_NotDecoded(t *testing.T) {
	sender := &mockDocumentSender{}
	skill := NewSendFileSkill(sender)

	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 42})
	// "YWJj" is valid base64 for "abc" but is also a legitimate literal.
	input, _ := json.Marshal(sendFileInput{Filename: "note.txt", Content: "YWJj"})

	if _, err := skill.Execute(ctx, input); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(sender.lastData) != "YWJj" {
		t.Errorf("data = %q, want %q (raw, not decoded)", string(sender.lastData), "YWJj")
	}
}

// TestSendFileSkill_LongBase64_Decoded verifies raw base64 (no data URI prefix)
// is still decoded when it's long enough and contains non-alphanum base64 chars.
func TestSendFileSkill_LongBase64_Decoded(t *testing.T) {
	sender := &mockDocumentSender{}
	skill := NewSendFileSkill(sender)

	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 42})
	// 48+ chars, contains a '/', decodes to a PNG-like byte string.
	// Payload: base64 of 48 bytes starting with the PNG magic header.
	content := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJ"
	input, _ := json.Marshal(sendFileInput{Filename: "dot.png", Content: content})

	if _, err := skill.Execute(ctx, input); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(sender.lastData) == len(content) {
		t.Errorf("data was not decoded; len = %d equals raw content", len(sender.lastData))
	}
	if len(sender.lastData) == 0 {
		t.Errorf("decoded data is empty")
	}
}
