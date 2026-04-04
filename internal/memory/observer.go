package memory

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"
)

const observerSystemPrompt = `You are a memory observer for a personal AI assistant. You watch conversations and extract meaningful events that should be remembered across future sessions.

Extract ONLY meaningful conversational events. Skip routine greetings, transient requests, and small talk. Focus on:
- decisions: choices made during the conversation
- preference: user preferences or behavioral patterns expressed
- project_state: status updates on ongoing work

For each observation, return a JSON object with these exact fields:
{
  "type": "decision" | "preference" | "project_state",
  "title": "one-line summary, max 100 chars",
  "summary": "1-2 sentences describing what happened and why it matters",
  "facts": ["atomic fact 1", "atomic fact 2"],
  "importance": 5
}

importance scale: 1=trivial preference, 5=standard decision, 8=major project milestone, 10=life-changing decision.

Return a JSON array. If nothing meaningful happened, return [].
Do NOT extract the same event twice. Check the "already captured" list below.`

const observerUserPromptTemplate = "Recent conversation segment:\n%s\n\nAlready captured in this conversation (do not duplicate):\n%s"

// minTranscriptChars is the minimum transcript length required to attempt extraction.
const minTranscriptChars = 200

// ObserverStore is the subset of store operations needed by ObservationExtractor.
type ObserverStore interface {
	SaveObservation(obs Observation) error
	GetRecentObservationTitles(convID string, limit int) ([]string, error)
	ObservationExistsByHash(userID int64, hash string) (bool, error)
	CountObservations(convID string) (int, error)
	GetMessagesSinceRowid(convID string, afterRowid, upToRowid int64) ([]Message, error)
}

// ObservationExtractor extracts structured observations from conversation
// transcripts by calling Claude and parsing the response.
type ObservationExtractor struct {
	send  func(ctx context.Context, system, user string) (string, error)
	store ObserverStore
}

// NewObservationExtractor creates an extractor that calls sendFn for LLM
// extraction and uses store for persistence and dedup checks.
func NewObservationExtractor(
	sendFn func(ctx context.Context, system, user string) (string, error),
	store ObserverStore,
) *ObservationExtractor {
	return &ObservationExtractor{send: sendFn, store: store}
}

// Extract loads recent messages, asks Claude to extract observations, validates
// and deduplicates the results, and saves them. Returns the saved observations.
func (e *ObservationExtractor) Extract(
	ctx context.Context,
	convID string,
	userID, chatID int64,
	chatType string,
	afterRowid, upToRowid int64,
	maxPerConv int,
	maxTranscriptChars int,
) ([]Observation, error) {
	messages, err := e.store.GetMessagesSinceRowid(convID, afterRowid, upToRowid)
	if err != nil {
		return nil, fmt.Errorf("observer: load messages: %w", err)
	}

	transcript := FormatTranscript(messages)
	transcript = truncateToChars(transcript, maxTranscriptChars)

	if len([]rune(transcript)) < minTranscriptChars {
		return nil, nil
	}

	titles, err := e.store.GetRecentObservationTitles(convID, 50)
	if err != nil {
		return nil, fmt.Errorf("observer: load titles: %w", err)
	}

	alreadyCaptured := "none"
	if len(titles) > 0 {
		alreadyCaptured = strings.Join(titles, "\n")
	}

	userPrompt := fmt.Sprintf(observerUserPromptTemplate, transcript, alreadyCaptured)
	resp, err := e.send(ctx, observerSystemPrompt, userPrompt)
	if err != nil {
		return nil, fmt.Errorf("observer: claude call: %w", err)
	}

	raw, err := parseObservationJSON(resp)
	if err != nil {
		slog.Warn("observer: invalid JSON from Claude", "error", err, "conv_id", convID)
		return nil, nil // bad JSON is not a fatal error
	}

	now := time.Now().UTC()
	var saved []Observation
	for _, r := range raw {
		obs, ok := validateRawObservation(r)
		if !ok {
			continue
		}

		hash := observationContentHash(obs.Title, obs.Summary)

		exists, err := e.store.ObservationExistsByHash(userID, hash)
		if err != nil {
			return saved, fmt.Errorf("observer: dedup check: %w", err)
		}
		if exists {
			continue
		}

		count, err := e.store.CountObservations(convID)
		if err != nil {
			return saved, fmt.Errorf("observer: count check: %w", err)
		}
		if count >= maxPerConv {
			break
		}

		obs.ConversationID = convID
		obs.UserID = userID
		obs.ChatID = chatID
		obs.ChatType = chatType
		obs.SourceMsgStart = afterRowid
		obs.SourceMsgEnd = upToRowid
		obs.ContentHash = hash
		obs.CreatedAt = now

		if err := e.store.SaveObservation(obs); err != nil {
			return saved, fmt.Errorf("observer: save: %w", err)
		}
		saved = append(saved, obs)
	}

	return saved, nil
}

// rawObservation is the JSON shape returned by Claude.
type rawObservation struct {
	Type       string   `json:"type"`
	Title      string   `json:"title"`
	Summary    string   `json:"summary"`
	Facts      []string `json:"facts"`
	Importance int      `json:"importance"`
}

// parseObservationJSON extracts a []rawObservation from Claude's response,
// stripping markdown code fences if present.
func parseObservationJSON(resp string) ([]rawObservation, error) {
	resp = strings.TrimSpace(resp)

	// Strip markdown code fences: ```json ... ``` or ``` ... ```
	if strings.HasPrefix(resp, "```") {
		// Remove opening fence (with optional language tag).
		if idx := strings.Index(resp, "\n"); idx != -1 {
			resp = resp[idx+1:]
		}
		// Remove closing fence.
		if idx := strings.LastIndex(resp, "```"); idx > 0 {
			resp = resp[:idx]
		}
		resp = strings.TrimSpace(resp)
	}

	var raw []rawObservation
	if err := json.Unmarshal([]byte(resp), &raw); err != nil {
		return nil, fmt.Errorf("parse observation array: %w", err)
	}
	return raw, nil
}

// validateRawObservation validates and normalizes a single raw observation.
// Returns the cleaned Observation and true if valid, or a zero value and false
// if the observation should be skipped.
func validateRawObservation(r rawObservation) (Observation, bool) {
	if !AllowedObservationTypes[r.Type] {
		return Observation{}, false
	}

	title := sanitizeObservationString(r.Title)
	summary := sanitizeObservationString(r.Summary)
	if title == "" || summary == "" {
		return Observation{}, false
	}

	title = truncateRunes(title, 200)
	summary = truncateRunes(summary, 1000)

	// Clamp importance to [1, 10].
	importance := r.Importance
	if importance < 1 {
		importance = 1
	}
	if importance > 10 {
		importance = 10
	}

	// Sanitize and cap facts.
	var facts []string
	for _, f := range r.Facts {
		f = sanitizeObservationString(f)
		if f != "" {
			facts = append(facts, f)
		}
		if len(facts) >= 10 {
			break
		}
	}

	return Observation{
		Type:       r.Type,
		Title:      title,
		Summary:    summary,
		Facts:      facts,
		Importance: importance,
	}, true
}

// observationContentHash returns a deterministic SHA-256 hex digest of the
// lowercased, trimmed title and summary joined by "|".
func observationContentHash(title, summary string) string {
	input := strings.ToLower(strings.TrimSpace(title + "|" + summary))
	h := sha256.Sum256([]byte(input))
	return fmt.Sprintf("%x", h)
}

// sanitizeObservationString strips control characters (keeping spaces) and
// trims whitespace. Mirrors the sanitizeFact pattern in facts.go.
func sanitizeObservationString(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsControl(r) && r != ' ' {
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// truncateRunes truncates s to at most maxRunes runes, preserving valid UTF-8.
func truncateRunes(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) > maxRunes {
		return string(runes[:maxRunes])
	}
	return s
}

// truncateToChars truncates a string to at most maxChars runes.
func truncateToChars(s string, maxChars int) string {
	if maxChars <= 0 {
		return s
	}
	return truncateRunes(s, maxChars)
}
