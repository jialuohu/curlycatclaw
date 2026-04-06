// Package ingest implements a generic knowledge source ingestion pipeline.
// It discovers items from configured sources (Gmail via MCP, Obsidian via
// filesystem, Notion via MCP), extracts observations using Claude or
// passthrough parsing, and saves them to the observation pipeline.
package ingest

import (
	"context"
	"encoding/json"

	"github.com/jialuohu/curlycatclaw/internal/memory"
)

// Source abstracts a knowledge source for the ingest pipeline.
// Each implementation owns its cursor format and prefilter logic.
type Source interface {
	// Name returns the source's configured name (e.g., "gmail", "obsidian").
	Name() string

	// Discover returns items that have changed since the given cursor.
	// The cursor is an opaque blob owned by the source implementation.
	// A nil cursor means "start from the beginning" (or backfill start).
	Discover(ctx context.Context, cursor json.RawMessage) (items []ItemRef, newCursor json.RawMessage, err error)

	// Read returns the full content of an item by its ID.
	Read(ctx context.Context, id string) (Content, error)

	// Prefilter returns true if the item should be processed.
	// Source implementations apply their own config-driven rules.
	Prefilter(item ItemRef) bool
}

// ItemRef holds metadata from a discovery result, enough for prefiltering
// without reading the full content.
type ItemRef struct {
	ID       string
	Title    string            // subject, filename, page title
	Snippet  string            // preview text for cheap prefiltering
	Metadata map[string]string // source-specific (labels, path, etc.)
}

// Content holds the full content of an item after Read().
type Content struct {
	ID       string
	SourceID string            // e.g., "gmail:personal", "obsidian:vault"
	Title    string
	Body     string
	Author   string
	Date     string
	Metadata map[string]string // source-specific extras

	// ContentFingerprint is a hash of the body content, used to detect
	// changes in mutable sources (e.g., edited Obsidian notes).
	// Sources that support mutation should set this.
	ContentFingerprint string
}

// TrustLevel controls which observation types are allowed and which
// extraction prompt template is used.
type TrustLevel string

const (
	TrustUntrusted TrustLevel = "untrusted" // email, third-party: blocks preference/commitment
	TrustTrusted   TrustLevel = "trusted"   // personal notes: all types allowed
)

// ExtractionMode controls how observations are extracted from content.
type ExtractionMode string

const (
	ExtractionLLM         ExtractionMode = "llm"         // Claude triage + extraction
	ExtractionPassthrough ExtractionMode = "passthrough"  // parse YAML front matter directly
	ExtractionHybrid      ExtractionMode = "hybrid"       // passthrough for front matter, LLM fallback
)

// LLMSender sends a system+user prompt to Claude and returns the text response.
type LLMSender func(ctx context.Context, system, user string) (string, error)

// Extractor converts content into observations.
type Extractor interface {
	Extract(ctx context.Context, content Content, trustLevel TrustLevel, minImportance int) ([]memory.Observation, error)
}
