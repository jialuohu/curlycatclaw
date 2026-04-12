package skills

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jialuohu/curlycatclaw/internal/personality"
)

// InitPersonalitySkills returns get_personality and set_personality skills.
// filePath is the configured personality file path (empty = no file configured).
func InitPersonalitySkills(filePath string) []*Skill {
	return []*Skill{
		{
			Name:        "get_personality",
			Description: "View the current agent personality configuration. Returns the full personality markdown content.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
			Execute:     makeGetPersonalityExecute(filePath),
		},
		{
			Name:        "set_personality",
			Description: "Update the agent personality. Takes the full personality content as markdown. The change takes effect on the next message.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"content":{"type":"string","description":"Full personality content in markdown format (max 20KB)"}},"required":["content"]}`),
			Execute:     makeSetPersonalityExecute(filePath),
		},
	}
}

func makeGetPersonalityExecute(filePath string) func(ctx context.Context, input json.RawMessage) (string, error) {
	return func(_ context.Context, _ json.RawMessage) (string, error) {
		if filePath == "" {
			p := personality.Default()
			return fmt.Sprintf("Using default personality (no file configured):\n\n%s", p.Content), nil
		}
		p, err := personality.Load(filePath)
		if err != nil {
			return "", fmt.Errorf("failed to load personality: %w", err)
		}
		return fmt.Sprintf("Personality file: %s\n\n%s", filePath, p.Content), nil
	}
}

type setPersonalityInput struct {
	Content string `json:"content"`
}

func makeSetPersonalityExecute(filePath string) func(ctx context.Context, input json.RawMessage) (string, error) {
	return func(_ context.Context, input json.RawMessage) (string, error) {
		if filePath == "" {
			return "", fmt.Errorf("no personality file configured — add [personality] file = \"/data/personality.md\" to config.toml")
		}
		var params setPersonalityInput
		if err := json.Unmarshal(input, &params); err != nil {
			return "", fmt.Errorf("invalid input: %w", err)
		}
		if params.Content == "" {
			return "", fmt.Errorf("content is required")
		}
		p, err := personality.Save(filePath, params.Content)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Personality updated (%d chars, hash=%s). Takes effect on next message.", len(p.Content), p.ContentHash[:12]), nil
	}
}
