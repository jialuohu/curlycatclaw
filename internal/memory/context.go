package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
)

const (
	defaultMaxTurns = 25
	defaultMaxChars = 600_000 // ~150K tokens at 4 chars/token
)

// ContextBuilder constructs a sliding-window message slice suitable for
// passing to the Claude API. It loads recent messages from the store and
// trims by turn count and estimated token budget.
type ContextBuilder struct {
	store    *Store
	maxTurns int
	maxChars int
	budget   *BudgetManager
}

// NewContextBuilder returns a ContextBuilder with default limits:
// 25 turn pairs and 600K characters (~150K tokens).
func NewContextBuilder(store *Store) *ContextBuilder {
	return &ContextBuilder{
		store:    store,
		maxTurns: defaultMaxTurns,
		maxChars: defaultMaxChars,
	}
}

// SetMaxTurns overrides the maximum number of turn pairs to retain.
func (cb *ContextBuilder) SetMaxTurns(n int) {
	cb.maxTurns = n
}

// SetMaxChars overrides the maximum character budget.
func (cb *ContextBuilder) SetMaxChars(n int) {
	cb.maxChars = n
}

// SetBudget attaches a BudgetManager for relevance-based context pruning.
func (cb *ContextBuilder) SetBudget(bm *BudgetManager) {
	cb.budget = bm
}

// turn represents one conversational turn: a user message followed by the
// assistant's response, including any interleaved tool_use/tool_result
// exchanges. A multi-step tool chain counts as a single turn.
type turn struct {
	messages []Message
	chars    int
}

// BuildContext loads recent messages for convID and returns a
// sliding-window slice trimmed to fit within the turn and character budgets.
// Messages are returned in chronological order.
func (cb *ContextBuilder) BuildContext(convID string) ([]Message, error) {
	// Load enough messages to fill the turn budget. A turn typically has
	// 2-4 messages (user + assistant + tool_result cycles), so *4 gives
	// headroom for multi-step tool chains without over-fetching.
	msgs, err := cb.store.GetMessages(convID, cb.maxTurns*4)
	if err != nil {
		return nil, fmt.Errorf("context: load messages: %w", err)
	}
	if len(msgs) == 0 {
		return nil, nil
	}

	turns := splitTurns(msgs)

	// Keep only the last maxTurns turns.
	if len(turns) > cb.maxTurns {
		turns = turns[len(turns)-cb.maxTurns:]
	}

	// Drop oldest turns until the total character count is within budget.
	for len(turns) > 1 && totalChars(turns) > cb.maxChars {
		turns = turns[1:]
	}

	// Flatten turns back into a message slice.
	var result []Message
	for _, t := range turns {
		result = append(result, t.messages...)
	}

	return result, nil
}

// BuildContextWithBudget loads recent messages for convID and applies
// budget-aware relevance classification when a BudgetManager is available.
// Turns classified as "full" are included verbatim, "summary" turns are
// replaced with a synthetic user message containing the summary, and "none"
// turns are dropped. Falls back to BuildContext on any error.
func (cb *ContextBuilder) BuildContextWithBudget(ctx context.Context, convID, currentMsg string) ([]Message, error) {
	// If no budget manager or it is disabled, use the standard path.
	if cb.budget == nil || !cb.budget.enabled {
		return cb.BuildContext(convID)
	}

	msgs, err := cb.store.GetMessages(convID, cb.maxTurns*4)
	if err != nil {
		return nil, fmt.Errorf("context: load messages: %w", err)
	}
	if len(msgs) == 0 {
		return nil, nil
	}

	turns := splitTurns(msgs)

	// Keep only the last maxTurns turns.
	if len(turns) > cb.maxTurns {
		turns = turns[len(turns)-cb.maxTurns:]
	}

	// Classify turns via the budget manager.
	classified, err := cb.budget.ClassifyTurns(ctx, currentMsg, turns)
	if err != nil {
		slog.Warn("budget classification failed, falling back to standard context", "err", err)
		return cb.BuildContext(convID)
	}

	// Build result from classified turns.
	var budgetTurns []turn
	for _, ct := range classified {
		switch ct.Classification {
		case "full":
			budgetTurns = append(budgetTurns, ct.Turn)
		case "summary":
			summary := ct.Summary
			if summary == "" {
				summary = "[earlier conversation turn]"
			}
			syntheticContent, _ := json.Marshal(summary)
			budgetTurns = append(budgetTurns, turn{
				messages: []Message{{Role: "user", Content: syntheticContent}},
				chars:    len(syntheticContent),
			})
		case "none":
			// Drop the turn entirely.
		default:
			// Unknown classification: include fully to be safe.
			budgetTurns = append(budgetTurns, ct.Turn)
		}
	}

	// Apply char budget trimming as fallback.
	for len(budgetTurns) > 1 && totalChars(budgetTurns) > cb.maxChars {
		budgetTurns = budgetTurns[1:]
	}

	// Flatten turns back into a message slice.
	var result []Message
	for _, t := range budgetTurns {
		result = append(result, t.messages...)
	}

	return result, nil
}

// splitTurns groups a chronologically ordered message slice into turns.
// A new turn begins at each "user" message. All assistant and tool_result
// messages following a user message belong to the same turn (so a multi-step
// tool chain is one turn).
func splitTurns(msgs []Message) []turn {
	var turns []turn
	var current *turn

	for _, m := range msgs {
		if m.Role == "user" {
			// Start a new turn.
			if current != nil {
				turns = append(turns, *current)
			}
			current = &turn{}
		}

		// If we haven't seen a user message yet (e.g. conversation starts
		// with an assistant message), start a turn anyway so nothing is lost.
		if current == nil {
			current = &turn{}
		}

		n := charCount(m.Content)
		current.messages = append(current.messages, m)
		current.chars += n
	}

	if current != nil && len(current.messages) > 0 {
		turns = append(turns, *current)
	}

	return turns
}

// totalChars sums the character counts across all turns.
func totalChars(turns []turn) int {
	total := 0
	for _, t := range turns {
		total += t.chars
	}
	return total
}

// charCount returns the number of characters in a JSON content block.
// It uses len() on the raw JSON bytes, which is a reasonable proxy for
// the 4-chars-per-token estimation used by the budget.
func charCount(content json.RawMessage) int {
	return len(content)
}
