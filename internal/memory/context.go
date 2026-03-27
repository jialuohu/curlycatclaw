package memory

import (
	"encoding/json"
	"fmt"
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
	// Load a generous number of messages — well beyond what the window
	// can hold — so we have enough to fill the budget.
	msgs, err := cb.store.GetMessages(convID, cb.maxTurns*20)
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
