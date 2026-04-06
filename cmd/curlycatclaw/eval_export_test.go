package main

import (
	"encoding/json"
	"testing"
)

func TestExtractText_SimpleString(t *testing.T) {
	raw := json.RawMessage(`"Hello, this is a user message"`)
	got := extractText(raw)
	if got != "Hello, this is a user message" {
		t.Errorf("expected simple string, got %q", got)
	}
}

func TestExtractText_ContentBlocks(t *testing.T) {
	raw := json.RawMessage(`[{"type":"text","text":"First paragraph"},{"type":"text","text":"Second paragraph"}]`)
	got := extractText(raw)
	want := "First paragraph\nSecond paragraph"
	if got != want {
		t.Errorf("expected %q, got %q", want, got)
	}
}

func TestExtractText_MixedBlocks(t *testing.T) {
	raw := json.RawMessage(`[{"type":"text","text":"Hello"},{"type":"tool_use","id":"t1"},{"type":"text","text":"World"}]`)
	got := extractText(raw)
	want := "Hello\nWorld"
	if got != want {
		t.Errorf("expected %q, got %q", want, got)
	}
}

func TestExtractText_EmptyTextBlocks(t *testing.T) {
	raw := json.RawMessage(`[{"type":"text","text":""},{"type":"tool_use","id":"t1"}]`)
	got := extractText(raw)
	if got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

func TestExtractText_InvalidJSON(t *testing.T) {
	raw := json.RawMessage(`not valid json`)
	got := extractText(raw)
	if got != "" {
		t.Errorf("expected empty string for invalid JSON, got %q", got)
	}
}

func TestExtractText_EmptyString(t *testing.T) {
	raw := json.RawMessage(`""`)
	got := extractText(raw)
	if got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}
