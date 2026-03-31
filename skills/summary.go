package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jialuohu/curlycatclaw/internal/memory"
)

// SummaryStore abstracts the summary operations needed by summary skills.
type SummaryStore interface {
	ListSummaries(userID int64) ([]memory.Summary, error)
	DeleteSummary(summaryID int64, userID int64) error
}

// InitSummarySkills returns the list_summaries and delete_summary skills.
func InitSummarySkills(store SummaryStore) []*Skill {
	return []*Skill{
		{
			Name:        "list_summaries",
			Description: "List all stored conversation summaries for the user. Shows summary ID, date, and a preview of each summary.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
			Execute:     makeListSummariesExecute(store),
		},
		{
			Name:        "delete_summary",
			Description: "Delete a conversation summary by its ID. Use if a summary contains incorrect information.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"summary_id":{"type":"integer","description":"ID of the summary to delete (from list_summaries)"}},"required":["summary_id"]}`),
			Execute:     makeDeleteSummaryExecute(store),
		},
	}
}

func makeListSummariesExecute(store SummaryStore) func(ctx context.Context, input json.RawMessage) (string, error) {
	return func(ctx context.Context, input json.RawMessage) (string, error) {
		user := GetUser(ctx)
		summaries, err := store.ListSummaries(user.UserID)
		if err != nil {
			return "", err
		}
		if len(summaries) == 0 {
			return "No conversation summaries stored yet.", nil
		}

		var sb strings.Builder
		fmt.Fprintf(&sb, "Found %d summaries:\n\n", len(summaries))
		for _, s := range summaries {
			date := s.CreatedAt
			if len(date) > 10 {
				date = date[:10]
			}
			preview := s.Summary
			runes := []rune(preview)
			if len(runes) > 120 {
				preview = string(runes[:120]) + "..."
			}
			fmt.Fprintf(&sb, "[id=%d] %s: %s\n", s.ID, date, preview)
		}
		return sb.String(), nil
	}
}

type deleteSummaryInput struct {
	SummaryID int64 `json:"summary_id"`
}

func makeDeleteSummaryExecute(store SummaryStore) func(ctx context.Context, input json.RawMessage) (string, error) {
	return func(ctx context.Context, input json.RawMessage) (string, error) {
		var params deleteSummaryInput
		if err := json.Unmarshal(input, &params); err != nil {
			return "", fmt.Errorf("invalid input: %w", err)
		}
		if params.SummaryID <= 0 {
			return "", fmt.Errorf("summary_id is required")
		}

		user := GetUser(ctx)
		if err := store.DeleteSummary(params.SummaryID, user.UserID); err != nil {
			return "", err
		}
		return fmt.Sprintf("Deleted summary %d.", params.SummaryID), nil
	}
}
