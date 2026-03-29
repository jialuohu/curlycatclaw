package memory

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/jialuohu/curlycatclaw/internal/claude"

	"github.com/anthropics/anthropic-sdk-go"
)

const (
	defaultCacheTTL    = 7 * 24 * time.Hour
	cleanupProbability = 0.01
)

// stopWords is the set of common English words to exclude from keyword matching.
var stopWords = map[string]bool{
	"that": true, "this": true, "with": true, "from": true,
	"have": true, "been": true, "were": true, "they": true,
	"what": true, "when": true, "where": true, "which": true,
	"their": true, "there": true, "will": true, "would": true,
	"could": true, "should": true, "about": true, "some": true,
	"them": true, "then": true, "than": true, "your": true,
	"just": true, "like": true, "also": true, "very": true,
	"into": true, "over": true, "such": true, "make": true,
	"know": true, "only": true, "come": true, "each": true,
	"well": true, "does": true, "most": true, "more": true,
	"much": true, "want": true, "need": true, "here": true,
	"back": true, "good": true, "still": true, "even": true,
	"please": true, "think": true, "going": true,
}

// ClassifiedTurn holds a turn along with its classification and optional summary.
type ClassifiedTurn struct {
	Turn           turn
	Classification string // "full", "summary", or "none"
	Summary        string // populated when Classification is "summary"
}

// BudgetManager classifies conversation turns by relevance to the current
// message, allowing the context builder to reduce token usage by summarizing
// or dropping irrelevant turns.
type BudgetManager struct {
	db       *sql.DB
	client   *claude.Client
	enabled  bool
	cacheTTL time.Duration
}

// NewBudgetManager creates a BudgetManager and ensures the budget_cache table exists.
func NewBudgetManager(db *sql.DB, client *claude.Client, enabled bool) (*BudgetManager, error) {
	bm := &BudgetManager{
		db:       db,
		client:   client,
		enabled:  enabled,
		cacheTTL: defaultCacheTTL,
	}

	const schema = `CREATE TABLE IF NOT EXISTS budget_cache (
		hash           TEXT PRIMARY KEY,
		classification TEXT NOT NULL,
		summary        TEXT,
		created_at     DATETIME NOT NULL
	)`
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("budget: create cache table: %w", err)
	}

	const indexSchema = `CREATE INDEX IF NOT EXISTS idx_budget_cache_created ON budget_cache(created_at)`
	if _, err := db.Exec(indexSchema); err != nil {
		return nil, fmt.Errorf("budget: create cache index: %w", err)
	}

	return bm, nil
}

// ClassifyTurns classifies each turn by relevance to currentMsg.
// It uses a three-tier strategy: keyword fast-path, cache lookup, and LLM classification.
func (bm *BudgetManager) ClassifyTurns(ctx context.Context, currentMsg string, turns []turn) ([]ClassifiedTurn, error) {
	if len(turns) == 0 {
		return nil, nil
	}

	// Probabilistic cache cleanup.
	if rand.Float64() < cleanupProbability {
		bm.cleanupCache()
	}

	result := make([]ClassifiedTurn, len(turns))
	keywords := extractKeywords(currentMsg)

	// Track which turns still need LLM classification.
	var uncachedIndices []int

	for i, t := range turns {
		result[i].Turn = t
		content := turnText(t)

		// 1. Keyword fast-path.
		if matchesKeyword(content, keywords) {
			result[i].Classification = "full"
			continue
		}

		// 2. Cache lookup.
		hash := cacheHash(content, currentMsg)
		cls, summary, found := bm.cacheGet(hash)
		if found {
			result[i].Classification = cls
			result[i].Summary = summary
			continue
		}

		// Needs LLM classification.
		uncachedIndices = append(uncachedIndices, i)
	}

	// 3. LLM classification for uncached turns.
	if len(uncachedIndices) > 0 {
		classifications, err := bm.classifyViaLLM(ctx, currentMsg, turns, uncachedIndices)
		if err != nil {
			return nil, fmt.Errorf("budget: llm classify: %w", err)
		}

		// Apply LLM results and write to cache.
		for j, idx := range uncachedIndices {
			if j < len(classifications) {
				result[idx].Classification = classifications[j].Classification
				result[idx].Summary = classifications[j].Summary
			} else {
				// Fallback: if we didn't get enough results, include fully.
				result[idx].Classification = "full"
			}

			// Cache the result.
			content := turnText(turns[idx])
			hash := cacheHash(content, currentMsg)
			bm.cacheSet(hash, result[idx].Classification, result[idx].Summary)
		}
	}

	return result, nil
}

// classifyViaLLM sends a single Haiku request to classify multiple turns.
func (bm *BudgetManager) classifyViaLLM(ctx context.Context, currentMsg string, turns []turn, indices []int) ([]struct {
	Classification string
	Summary        string
}, error) {
	// Build the prompt with numbered turns.
	var sb strings.Builder
	for i, idx := range indices {
		content := turnText(turns[idx])
		// Truncate very long turns for the classification prompt.
		if len(content) > 500 {
			content = content[:500] + "..."
		}
		fmt.Fprintf(&sb, "TURN %d:\n%s\n\n", i+1, content)
	}

	systemPrompt := "Classify each conversation turn's relevance to the user's current message. " +
		"For each turn, respond with: FULL (include entire turn), SUMMARY (include only a 1-line summary), or NONE (drop). " +
		"Format: one line per turn: TURN_NUMBER|CLASSIFICATION|SUMMARY_IF_APPLICABLE"

	userMsg := fmt.Sprintf("Current message: %s\n\nConversation turns to classify:\n%s", currentMsg, sb.String())

	llmCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	resp, err := bm.client.SendStreaming(llmCtx, claude.SendParams{
		Messages:     []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(userMsg))},
		SystemPrompt: systemPrompt,
		MaxTokens:    1024,
	})
	if err != nil {
		return nil, err
	}

	return parseClassifications(resp.TextContent, len(indices)), nil
}

// parseClassifications parses the LLM response into classification results.
// Expected format: "TURN_NUMBER|CLASSIFICATION|SUMMARY" one per line.
func parseClassifications(text string, expected int) []struct {
	Classification string
	Summary        string
} {
	results := make([]struct {
		Classification string
		Summary        string
	}, expected)

	// Default everything to "full" in case parsing fails.
	for i := range results {
		results[i].Classification = "full"
	}

	lines := strings.Split(strings.TrimSpace(text), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		parts := strings.SplitN(line, "|", 3)
		if len(parts) < 2 {
			continue
		}

		// Parse turn number.
		numStr := strings.TrimSpace(parts[0])
		var turnNum int
		if _, err := fmt.Sscanf(numStr, "TURN %d", &turnNum); err != nil {
			// Try plain number.
			if _, err := fmt.Sscanf(numStr, "%d", &turnNum); err != nil {
				continue
			}
		}

		// Turn numbers are 1-based.
		idx := turnNum - 1
		if idx < 0 || idx >= expected {
			continue
		}

		cls := strings.ToLower(strings.TrimSpace(parts[1]))
		switch cls {
		case "full", "summary", "none":
			results[idx].Classification = cls
		default:
			continue
		}

		if cls == "summary" && len(parts) >= 3 {
			results[idx].Summary = strings.TrimSpace(parts[2])
		}
	}

	return results
}

// extractKeywords extracts meaningful words from a message for keyword matching.
func extractKeywords(msg string) []string {
	words := strings.Fields(strings.ToLower(msg))
	var keywords []string
	for _, w := range words {
		// Strip common punctuation.
		w = strings.Trim(w, ".,!?;:'\"()[]{}/-")
		if len(w) > 3 && !stopWords[w] {
			keywords = append(keywords, w)
		}
	}
	return keywords
}

// matchesKeyword returns true if any keyword appears as a substring in content.
func matchesKeyword(content string, keywords []string) bool {
	lower := strings.ToLower(content)
	for _, kw := range keywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// turnText concatenates the text content of all messages in a turn.
// It attempts to unmarshal each message's content as a JSON string first,
// producing cleaner text for budget classification.
func turnText(t turn) string {
	var sb strings.Builder
	for _, m := range t.messages {
		var text string
		if json.Unmarshal(m.Content, &text) == nil {
			sb.WriteString(text)
		} else {
			sb.WriteString(string(m.Content))
		}
	}
	return sb.String()
}

// cacheHash computes the SHA256 hash used as the cache key.
func cacheHash(turnContent, currentMsg string) string {
	h := sha256.Sum256([]byte(turnContent + "||" + currentMsg))
	return fmt.Sprintf("%x", h)
}

// cacheGet looks up a classification in the budget_cache table.
func (bm *BudgetManager) cacheGet(hash string) (classification, summary string, found bool) {
	var createdAt time.Time
	err := bm.db.QueryRow(
		`SELECT classification, summary, created_at FROM budget_cache WHERE hash = ?`,
		hash,
	).Scan(&classification, &summary, &createdAt)
	if err != nil {
		return "", "", false
	}

	// Check TTL.
	if time.Since(createdAt) > bm.cacheTTL {
		return "", "", false
	}

	return classification, summary, true
}

// cacheSet writes a classification result to the budget_cache table.
func (bm *BudgetManager) cacheSet(hash, classification, summary string) {
	_, err := bm.db.Exec(
		`INSERT OR REPLACE INTO budget_cache (hash, classification, summary, created_at) VALUES (?, ?, ?, ?)`,
		hash, classification, summary, time.Now().UTC(),
	)
	if err != nil {
		slog.Error("budget: cache write failed", "err", err)
	}
}

// cleanupCache deletes expired entries from the budget_cache table.
func (bm *BudgetManager) cleanupCache() {
	threshold := time.Now().UTC().Add(-bm.cacheTTL)
	_, err := bm.db.Exec(`DELETE FROM budget_cache WHERE created_at < ?`, threshold)
	if err != nil {
		slog.Error("budget: cache cleanup failed", "err", err)
	}
}
