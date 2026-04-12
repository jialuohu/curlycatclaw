// Package personality loads and validates the agent's persona configuration.
package personality

import (
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"
)

const (
	defaultPersonality = "You are a helpful personal assistant."
	maxFileSize        = 20 * 1024 // 20KB
)

// Persona holds the loaded personality content and metadata.
type Persona struct {
	Content     string // personality text injected into the system prompt
	ContentHash string // SHA-256 hex digest for drift detection/logging
	FilePath    string // source file path (empty for default)
}

// Load reads a personality file and returns a Persona.
// It validates: non-empty (after trimming whitespace), valid UTF-8, and <= 20KB.
func Load(filePath string) (*Persona, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("personality: %w", err)
	}
	if len(data) > maxFileSize {
		return nil, fmt.Errorf("personality: file %q is %d bytes, max allowed is 20KB (%d bytes)", filePath, len(data), maxFileSize)
	}
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("personality: file %q contains invalid UTF-8", filePath)
	}
	content := strings.TrimSpace(string(data))
	if content == "" {
		return nil, fmt.Errorf("personality: file %q is empty after trimming whitespace", filePath)
	}
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
	return &Persona{
		Content:     content,
		ContentHash: hash,
		FilePath:    filePath,
	}, nil
}

// Save validates and writes personality content to filePath.
// It applies the same validation as Load: non-empty, valid UTF-8, <= 20KB.
func Save(filePath, content string) (*Persona, error) {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return nil, fmt.Errorf("personality: content is empty after trimming whitespace")
	}
	if len(trimmed) > maxFileSize {
		return nil, fmt.Errorf("personality: content is %d bytes, max allowed is 20KB (%d bytes)", len(trimmed), maxFileSize)
	}
	if !utf8.Valid([]byte(trimmed)) {
		return nil, fmt.Errorf("personality: content contains invalid UTF-8")
	}
	if err := os.WriteFile(filePath, []byte(trimmed+"\n"), 0644); err != nil {
		return nil, fmt.Errorf("personality: write %q: %w", filePath, err)
	}
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(trimmed)))
	return &Persona{
		Content:     trimmed,
		ContentHash: hash,
		FilePath:    filePath,
	}, nil
}

// Default returns a Persona with the hardcoded default personality.
func Default() *Persona {
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(defaultPersonality)))
	return &Persona{
		Content:     defaultPersonality,
		ContentHash: hash,
	}
}
