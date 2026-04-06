package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jialuohu/curlycatclaw/config"
	"github.com/jialuohu/curlycatclaw/internal/memory"
)

// runEvalExport exports recent conversations to stdout in a reviewable format
// for manual quality labeling (Phase 0C of the eval pipeline).
func runEvalExport(configPath string, hours int) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	store, err := memory.NewStore(cfg.Storage.DBPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer store.Close()

	since := time.Now().UTC().Add(-time.Duration(hours) * time.Hour)
	convs, err := store.GetConversationsSince(since)
	if err != nil {
		return fmt.Errorf("get conversations: %w", err)
	}

	if len(convs) == 0 {
		fmt.Fprintf(os.Stderr, "No conversations found in the last %d hours.\n", hours)
		return nil
	}

	fmt.Fprintf(os.Stderr, "Exporting %d conversations from the last %d hours...\n", len(convs), hours)

	for i, conv := range convs {
		msgs, err := store.GetMessages(conv.ID, 200)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: skipping conversation %s: %v\n", conv.ID, err)
			continue
		}
		if len(msgs) == 0 {
			continue
		}

		// Get tool call and interaction event counts for scoring context.
		// Query both by conversation_id AND by (user_id, chat_id) for events
		// logged with empty conversation_id (before conversation lookup).
		events, _ := store.GetInteractionEvents(conv.ID)
		globalEvents, _ := store.GetInteractionEventsByUser(conv.UserID, conv.ChatID)

		var retries, effortChanges int
		for _, e := range events {
			switch e.EventType {
			case "retry":
				retries++
			case "effort_override":
				effortChanges++
			}
		}
		for _, e := range globalEvents {
			switch e.EventType {
			case "retry":
				retries++
			case "effort_override":
				effortChanges++
			}
		}

		// Count tool errors from tool_calls table.
		var tcTotal, tcErrors int
		store.DB().QueryRow(
			`SELECT COUNT(*), COALESCE(SUM(CASE WHEN is_error THEN 1 ELSE 0 END), 0) FROM tool_calls WHERE conversation_id = ?`,
			conv.ID,
		).Scan(&tcTotal, &tcErrors) //nolint:errcheck

		fmt.Printf("═══════════════════════════════════════════════════════════\n")
		fmt.Printf("CONVERSATION %d/%d  |  ID: %s\n", i+1, len(convs), conv.ID)
		fmt.Printf("User: %d  |  Chat: %d  |  Messages: %d\n", conv.UserID, conv.ChatID, len(msgs))
		fmt.Printf("Tool calls: %d (%d errors)  |  Retries: %d  |  Effort changes: %d\n", tcTotal, tcErrors, retries, effortChanges)
		fmt.Printf("───────────────────────────────────────────────────────────\n")

		for _, msg := range msgs {
			role := strings.ToUpper(msg.Role)
			text := extractText(msg.Content)
			if text == "" {
				text = "[non-text content]"
			}
			// Truncate very long messages for readability (rune-safe).
			if r := []rune(text); len(r) > 500 {
				text = string(r[:500]) + "... [truncated]"
			}
			fmt.Printf("[%s] %s\n", role, text)
		}

		fmt.Printf("───────────────────────────────────────────────────────────\n")
		fmt.Printf("QUALITY SCORE (0-10): ___\n")
		fmt.Printf("NOTES: \n\n")
	}

	fmt.Fprintf(os.Stderr, "Done. Label each conversation 0-10 and save the output.\n")
	fmt.Fprintf(os.Stderr, "Usage: curlycatclaw --eval-export --config ~/.curlycatclaw/config.toml > labels.txt\n")
	return nil
}

// extractText pulls plain text from a message content JSON blob.
func extractText(raw json.RawMessage) string {
	// Try as a simple string first (user messages).
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}

	// Try as an array of content blocks (assistant messages).
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}

	return ""
}
