package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jialuohu/curlycatclaw/internal/memory"
)

// InitFactSkills returns the remember_fact, forget_fact, and list_facts skills.
func InitFactSkills(factStore *memory.FactStore) []*Skill {
	return []*Skill{
		{
			Name:        "remember_fact",
			Description: "Save a persistent fact about the user that will be remembered across all future conversations. Use proactively when you learn something lasting about the user.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"fact":{"type":"string","description":"The fact to remember (max 200 chars)"},"category":{"type":"string","enum":["preference","identity","project","general"],"default":"general","description":"Category of the fact"}},"required":["fact"]}`),
			Execute:     makeRememberFactExecute(factStore),
		},
		{
			Name:        "forget_fact",
			Description: "Remove a previously saved fact about the user by its ID.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"fact_id":{"type":"integer","description":"ID of the fact to remove (from list_facts)"}},"required":["fact_id"]}`),
			Execute:     makeForgetFactExecute(factStore),
		},
		{
			Name:        "list_facts",
			Description: "List all persistent facts remembered about the user.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
			Execute:     makeListFactsExecute(factStore),
		},
	}
}

type rememberFactInput struct {
	Fact     string `json:"fact"`
	Category string `json:"category"`
}

func makeRememberFactExecute(fs *memory.FactStore) func(ctx context.Context, input json.RawMessage) (string, error) {
	return func(ctx context.Context, input json.RawMessage) (string, error) {
		var params rememberFactInput
		if err := json.Unmarshal(input, &params); err != nil {
			return "", fmt.Errorf("invalid input: %w", err)
		}
		if params.Fact == "" {
			return "", fmt.Errorf("fact is required")
		}
		if params.Category == "" {
			params.Category = "general"
		}

		user := GetUser(ctx)
		id, err := fs.AddFact(user.UserID, params.Fact, params.Category, "proactive")
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Remembered (id=%d, category=%s): %s", id, params.Category, params.Fact), nil
	}
}

type forgetFactInput struct {
	FactID int64 `json:"fact_id"`
}

func makeForgetFactExecute(fs *memory.FactStore) func(ctx context.Context, input json.RawMessage) (string, error) {
	return func(ctx context.Context, input json.RawMessage) (string, error) {
		var params forgetFactInput
		if err := json.Unmarshal(input, &params); err != nil {
			return "", fmt.Errorf("invalid input: %w", err)
		}
		if params.FactID <= 0 {
			return "", fmt.Errorf("fact_id must be a positive integer")
		}

		user := GetUser(ctx)
		if err := fs.DeleteFact(params.FactID, user.UserID); err != nil {
			return "", err
		}
		return fmt.Sprintf("Forgot fact %d.", params.FactID), nil
	}
}

func makeListFactsExecute(fs *memory.FactStore) func(ctx context.Context, input json.RawMessage) (string, error) {
	return func(ctx context.Context, _ json.RawMessage) (string, error) {
		user := GetUser(ctx)
		facts, err := fs.GetFacts(user.UserID)
		if err != nil {
			return "", err
		}
		if len(facts) == 0 {
			return "No facts saved yet.", nil
		}

		// Group by category.
		grouped := make(map[string][]memory.Fact)
		for _, f := range facts {
			grouped[f.Category] = append(grouped[f.Category], f)
		}

		var sb strings.Builder
		fmt.Fprintf(&sb, "Saved facts (%d total):\n", len(facts))
		for _, cat := range []string{"identity", "preference", "project", "general"} {
			items, ok := grouped[cat]
			if !ok {
				continue
			}
			fmt.Fprintf(&sb, "\n**%s**\n", cat)
			for _, f := range items {
				fmt.Fprintf(&sb, "  [id=%d] %s\n", f.ID, f.Fact)
			}
		}
		return sb.String(), nil
	}
}
