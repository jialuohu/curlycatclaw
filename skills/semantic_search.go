package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jialuohu/curlycatclaw/internal/memory"
)

// NewSemanticSearchSkill returns a skill that performs vector similarity search
// across conversation history and notes.
func NewSemanticSearchSkill(vs *memory.VectorStore) *Skill {
	return &Skill{
		Name:        "semantic_search",
		Description: "Search conversation history and notes by meaning. Use when the user asks about something they mentioned before.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"Natural language query to search for"},"limit":{"type":"integer","description":"Maximum number of results to return","default":5}},"required":["query"]}`),
		Execute:     makeSemanticSearchExecute(vs),
	}
}

type semanticSearchInput struct {
	Query string `json:"query"`
	Limit int    `json:"limit"`
}

func makeSemanticSearchExecute(vs *memory.VectorStore) func(ctx context.Context, input json.RawMessage) (string, error) {
	return func(ctx context.Context, input json.RawMessage) (string, error) {
		var params semanticSearchInput
		if err := json.Unmarshal(input, &params); err != nil {
			return "", fmt.Errorf("invalid input: %w", err)
		}
		if params.Query == "" {
			return "", fmt.Errorf("query is required")
		}
		if params.Limit <= 0 {
			params.Limit = 5
		}

		user := GetUser(ctx)
		results, err := vs.Search(ctx, params.Query, user.UserID, params.Limit)
		if err != nil {
			return "", fmt.Errorf("semantic search: %w", err)
		}

		if len(results) == 0 {
			return fmt.Sprintf("No results found for: %s", params.Query), nil
		}

		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("Found %d results for: %s\n\n", len(results), params.Query))
		for i, r := range results {
			sb.WriteString(fmt.Sprintf("%d. [%s] (score: %.2f)\n", i+1, r.Source, r.Score))
			if r.CreatedAt != "" {
				sb.WriteString(fmt.Sprintf("   Time: %s\n", r.CreatedAt))
			}
			sb.WriteString(fmt.Sprintf("   %s\n\n", r.Text))
		}
		return strings.TrimSpace(sb.String()), nil
	}
}
