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

// Default returns a Persona with the hardcoded default personality.
func Default() *Persona {
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(defaultPersonality)))
	return &Persona{
		Content:     defaultPersonality,
		ContentHash: hash,
	}
}
