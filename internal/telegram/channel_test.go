package telegram

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestChunkMessage_Short verifies that a message under 4096 runes returns a
// single chunk with the content unchanged.
func TestChunkMessage_Short(t *testing.T) {
	msg := "Hello, world!"
	chunks := chunkMessage(msg)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	if chunks[0] != msg {
		t.Fatalf("chunk content mismatch: got %q", chunks[0])
	}
}

// TestChunkMessage_LongSplitsOnParagraph verifies that a message with multiple
// paragraphs that together exceed 4096 runes is split on paragraph boundaries.
// splitParagraphs requires two consecutive blank lines (\n\n\n) to detect a
// paragraph break when scanning line-by-line.
func TestChunkMessage_LongSplitsOnParagraph(t *testing.T) {
	// Build two paragraphs, each ~2500 runes, totalling >4096.
	para1 := strings.Repeat("a", 2500)
	para2 := strings.Repeat("b", 2500)
	msg := para1 + "\n\n\n" + para2

	chunks := chunkMessage(msg)
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(chunks))
	}
	if chunks[0] != para1 {
		t.Errorf("first chunk should be para1")
	}
	if chunks[1] != para2 {
		t.Errorf("second chunk should be para2")
	}
}

// TestChunkMessage_PreservesCodeFences verifies that a ``` code block spanning
// a paragraph boundary is not split across chunks.
func TestChunkMessage_PreservesCodeFences(t *testing.T) {
	// Preamble that fills most of the space, followed by a code block with
	// an internal blank-line pair (which would normally split paragraphs).
	preamble := strings.Repeat("x", 3000)
	codeBlock := "```\nline1\n\n\nline2\n```"
	msg := preamble + "\n\n\n" + codeBlock

	chunks := chunkMessage(msg)

	// The code block should appear in exactly one chunk, not split.
	found := false
	for _, c := range chunks {
		if strings.Contains(c, "line1") && strings.Contains(c, "line2") && strings.Contains(c, "```") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("code fence block was split across chunks; chunks: %v", chunks)
	}
}

// TestChunkMessage_Unicode verifies that messages with multi-byte characters
// (CJK, emoji) do not cause mid-rune splits.
func TestChunkMessage_Unicode(t *testing.T) {
	// Each CJK character is 3 bytes in UTF-8. Build a string of exactly
	// 4097 runes so it must be split.
	cjk := strings.Repeat("\u4e16", 4097) // "世" repeated
	chunks := chunkMessage(cjk)

	if len(chunks) < 2 {
		t.Fatalf("expected at least 2 chunks, got %d", len(chunks))
	}

	for i, c := range chunks {
		if !utf8.ValidString(c) {
			t.Errorf("chunk %d is not valid UTF-8", i)
		}
		if utf8.RuneCountInString(c) > maxMessageLen {
			t.Errorf("chunk %d exceeds maxMessageLen: %d runes", i, utf8.RuneCountInString(c))
		}
	}
}

// TestChunkMessage_SingleLongParagraph verifies that a single paragraph
// exceeding 4096 runes that contains internal newlines is split at a newline.
func TestChunkMessage_SingleLongParagraph(t *testing.T) {
	// Build a single paragraph (no \n\n) with lines separated by \n.
	line := strings.Repeat("L", 200) // 200 runes per line
	lineCount := 25                   // 25 * 200 = 5000 runes + 24 newlines
	lines := make([]string, lineCount)
	for i := range lines {
		lines[i] = line
	}
	msg := strings.Join(lines, "\n")

	chunks := chunkMessage(msg)
	if len(chunks) < 2 {
		t.Fatalf("expected at least 2 chunks, got %d", len(chunks))
	}

	for i, c := range chunks {
		if utf8.RuneCountInString(c) > maxMessageLen {
			t.Errorf("chunk %d exceeds maxMessageLen: %d runes", i, utf8.RuneCountInString(c))
		}
	}

	// Reassemble and verify no content was lost (accounting for trimmed newlines).
	rejoined := strings.Join(chunks, "\n")
	// The total rune count should match the original.
	if utf8.RuneCountInString(rejoined) != utf8.RuneCountInString(msg) {
		t.Errorf("content lost during chunking: original %d runes, reassembled %d runes",
			utf8.RuneCountInString(msg), utf8.RuneCountInString(rejoined))
	}
}

// TestChunkMessage_HardCut verifies that a single paragraph exceeding 4096
// runes with NO newlines is hard-cut at a rune boundary.
func TestChunkMessage_HardCut(t *testing.T) {
	msg := strings.Repeat("Z", 5000) // 5000 runes, no newlines
	chunks := chunkMessage(msg)

	if len(chunks) < 2 {
		t.Fatalf("expected at least 2 chunks, got %d", len(chunks))
	}

	for i, c := range chunks {
		if utf8.RuneCountInString(c) > maxMessageLen {
			t.Errorf("chunk %d exceeds maxMessageLen: %d runes", i, utf8.RuneCountInString(c))
		}
		if !utf8.ValidString(c) {
			t.Errorf("chunk %d is not valid UTF-8", i)
		}
	}

	// Verify all content preserved.
	if strings.Join(chunks, "") != msg {
		t.Error("hard-cut lost content")
	}
}

// TestSplitParagraphs_Basic verifies that text separated by consecutive blank
// lines produces distinct paragraphs. splitParagraphs splits when it sees two
// blank lines in a row (i.e., three newlines: \n\n\n).
func TestSplitParagraphs_Basic(t *testing.T) {
	text := "First paragraph.\n\n\nSecond paragraph.\n\n\nThird paragraph."
	paras := splitParagraphs(text)

	if len(paras) != 3 {
		t.Fatalf("expected 3 paragraphs, got %d: %v", len(paras), paras)
	}
	if paras[0] != "First paragraph." {
		t.Errorf("para 0: got %q", paras[0])
	}
	if paras[1] != "Second paragraph." {
		t.Errorf("para 1: got %q", paras[1])
	}
	if paras[2] != "Third paragraph." {
		t.Errorf("para 2: got %q", paras[2])
	}
}

// TestSplitParagraphs_CodeFencePreserved verifies that consecutive blank lines
// inside a fenced code block do not cause a paragraph split.
func TestSplitParagraphs_CodeFencePreserved(t *testing.T) {
	// Use \n\n\n (two blank lines) as the paragraph separator outside the
	// fence, and place the same separator inside the fence to prove it is
	// preserved.
	text := "Before.\n\n\n```\ncode line 1\n\n\ncode line 2\n```\n\n\nAfter."
	paras := splitParagraphs(text)

	if len(paras) != 3 {
		t.Fatalf("expected 3 paragraphs, got %d: %v", len(paras), paras)
	}

	// The middle paragraph must contain the full code block including the
	// internal blank lines.
	if !strings.Contains(paras[1], "code line 1") || !strings.Contains(paras[1], "code line 2") {
		t.Errorf("code fence was split; middle paragraph: %q", paras[1])
	}
}

// TestIsAllowed_EmptyList verifies that an empty allowed map permits any user.
func TestIsAllowed_EmptyList(t *testing.T) {
	ch := &Channel{
		allowed: map[int64]struct{}{},
	}

	if !ch.isAllowed(12345) {
		t.Error("expected any user to be allowed with empty map")
	}
	if !ch.isAllowed(0) {
		t.Error("expected user 0 to be allowed with empty map")
	}
}

// TestIsAllowed_Restricted verifies that a populated allowed map correctly
// permits and denies users.
func TestIsAllowed_Restricted(t *testing.T) {
	ch := &Channel{
		allowed: map[int64]struct{}{
			100: {},
			200: {},
		},
	}

	if !ch.isAllowed(100) {
		t.Error("user 100 should be allowed")
	}
	if !ch.isAllowed(200) {
		t.Error("user 200 should be allowed")
	}
	if ch.isAllowed(300) {
		t.Error("user 300 should be denied")
	}
	if ch.isAllowed(0) {
		t.Error("user 0 should be denied")
	}
}
