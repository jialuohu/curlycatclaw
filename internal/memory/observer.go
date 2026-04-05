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
- decision: choices made during the conversation ("Agreed to use goroutines for concurrent extraction")
- preference: user preferences or behavioral patterns expressed ("User prefers concise responses without emojis")
- project_state: status updates on ongoing work ("Auth module complete, starting payment integration")
- commitment: promises, follow-ups, or deadlines the user set ("Promised to send the report by Friday")
- discovery: things learned about the user's world, team, or tools ("User's team uses Kubernetes for deployment")
- reference: important external references, URLs, or resources mentioned ("Key API docs at docs.example.com/api")

For each observation, return a JSON object with these exact fields:
{
  "type": "decision" | "preference" | "project_state" | "commitment" | "discovery" | "reference",
  "title": "one-line summary, max 100 chars",
  "summary": "1-2 sentences describing what happened and why it matters",
  "facts": ["atomic fact 1", "atomic fact 2"],
  "importance": 5,
  "entities": [{"name": "entity-name", "type": "person|project|file|tool"}]
}

entities: Extract mentioned people, projects, files, or tools. Only include entities directly relevant to the observation. Omit if none.

importance scale: 1=trivial preference, 5=standard decision, 8=major project milestone, 10=life-changing decision.

RESPOND WITH ONLY A JSON ARRAY. No explanation, no preamble, no markdown. Start your response with [ and end with ].
If nothing meaningful happened, return [].
Do NOT extract the same event twice. Check the "already captured" list below.`

const observerUserPromptTemplate = "Recent conversation segment:\n%s\n\nAlready captured in this conversation (do not duplicate):\n%s"

// minTranscriptChars is the minimum transcript length required to attempt extraction.
const minTranscriptChars = 200

// ObserverStore is the subset of store operations needed by ObservationExtractor.
type ObserverStore interface {
	SaveObservation(obs *Observation) error
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

	transcript := FormatTranscriptWithLimit(messages, maxTranscriptChars)

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

		if err := e.store.SaveObservation(&obs); err != nil {
			return saved, fmt.Errorf("observer: save: %w", err)
		}
		saved = append(saved, obs)
	}

	// Instrumentation: log extraction metrics.
	dupCount := len(raw) - len(saved)
	typeCounts := make(map[string]int)
	var totalImportance int
	for _, o := range saved {
		typeCounts[o.Type]++
		totalImportance += o.Importance
	}
	slog.Info("observation_extraction",
		"conv_id", convID,
		"transcript_runes", len([]rune(transcript)),
		"parsed", len(raw),
		"saved", len(saved),
		"dedup_hits", dupCount,
		"types", typeCounts,
		"avg_importance", func() float64 {
			if len(saved) == 0 {
				return 0
			}
			return float64(totalImportance) / float64(len(saved))
		}(),
	)

	return saved, nil
}

// rawObservation is the JSON shape returned by Claude.
type rawObservation struct {
	Type       string      `json:"type"`
	Title      string      `json:"title"`
	Summary    string      `json:"summary"`
	Facts      []string    `json:"facts"`
	Importance int         `json:"importance"`
	Entities   []rawEntity `json:"entities"`
}

// rawEntity is the JSON shape for entities returned by Claude.
type rawEntity struct {
	Name string `json:"name"`
	Type string `json:"type"`
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

	// Canonicalize entities (graceful degradation: bad entities are skipped).
	var entities []Entity
	for _, e := range r.Entities {
		name := canonicalizeEntityName(e.Name)
		if name == "" {
			continue
		}
		if !AllowedEntityTypes[e.Type] {
			continue
		}
		entities = append(entities, Entity{Name: name, Type: e.Type})
		if len(entities) >= 10 {
			break
		}
	}

	return Observation{
		Type:       r.Type,
		Title:      title,
		Summary:    summary,
		Facts:      facts,
		Entities:   entities,
		Importance: importance,
	}, true
}

// canonicalizeEntityName normalizes an entity name: lowercase, trim, collapse
// spaces, strip control chars, cap at 200 runes.
func canonicalizeEntityName(name string) string {
	name = sanitizeObservationString(name)
	name = strings.ToLower(name)
	// Collapse multiple spaces.
	prev := ""
	for prev != name {
		prev = name
		name = strings.ReplaceAll(name, "  ", " ")
	}
	return truncateRunes(name, 200)
}

// observationContentHash returns a deterministic SHA-256 hex digest of the
// lowercased, trimmed title and summary joined by "|".
func observationContentHash(title, summary string) string {
	input := strings.ToLower(strings.TrimSpace(title + "|" + summary))
	h := sha256.Sum256([]byte(input))
	return fmt.Sprintf("%x", h)
}

// sanitizeObservationString replaces control characters with spaces and
// trims whitespace. This prevents garbled concatenation when Claude returns
// multi-line text (e.g., "WAL mode\nenabled" becomes "WAL mode enabled").
func sanitizeObservationString(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsControl(r) {
			b.WriteRune(' ')
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
