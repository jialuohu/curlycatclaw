package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/jialuohu/curlycatclaw/config"
	"github.com/jialuohu/curlycatclaw/internal/memory"
)

// runEvalSeed seeds the database with synthetic conversations that have varied
// quality signals for Phase 0C validation. Each conversation has known signal
// levels so the user can label them and check correlation.
func runEvalSeed(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	store, err := memory.NewStore(cfg.Storage.DBPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer store.Close()

	seeds := []seedConv{
		{
			label:       "Perfect simple Q&A",
			messages:    []msg{{"user", "What's 2+2?"}, {"assistant", "4."}},
			toolErrors:  0, toolTotal: 0, corrections: 0, retries: 0, efforts: 0,
			expectedScore: "9-10",
		},
		{
			label:       "Clean tool use",
			messages:    []msg{{"user", "Search my notes for project updates"}, {"assistant", "Found 3 notes about your dashboard project."}},
			toolErrors:  0, toolTotal: 2, corrections: 0, retries: 0, efforts: 0,
			expectedScore: "8-9",
		},
		{
			label:       "Good but verbose",
			messages:    []msg{{"user", "How does Go garbage collection work?"}, {"assistant", "Go uses a concurrent tri-color mark-and-sweep GC..."}},
			toolErrors:  0, toolTotal: 0, corrections: 0, retries: 0, efforts: 0,
			expectedScore: "7-8",
		},
		{
			label:       "One minor correction",
			messages:    []msg{{"user", "What's the capital of Canada?"}, {"assistant", "The capital of Canada is Toronto."}, {"user", "No, it's Ottawa."}, {"assistant", "You're right, Ottawa is the capital."}},
			toolErrors:  0, toolTotal: 0, corrections: 1, retries: 0, efforts: 0,
			expectedScore: "6-7",
		},
		{
			label:       "Needed effort bump",
			messages:    []msg{{"user", "Explain quantum entanglement"}, {"assistant", "Quantum entanglement is when particles are linked..."}},
			toolErrors:  0, toolTotal: 0, corrections: 0, retries: 0, efforts: 1,
			expectedScore: "6-7",
		},
		{
			label:       "Tool error + retry",
			messages:    []msg{{"user", "Search GitHub for my open PRs"}, {"assistant", "I got an error accessing GitHub."}, {"user", "Try again"}, {"assistant", "Found 3 open PRs."}},
			toolErrors:  1, toolTotal: 2, corrections: 0, retries: 1, efforts: 0,
			expectedScore: "5-6",
		},
		{
			label:       "Multiple corrections needed",
			messages:    []msg{{"user", "List all planets"}, {"assistant", "Mercury, Venus, Earth, Mars, Jupiter, Saturn, Neptune."}, {"user", "Wrong, you forgot Uranus."}, {"assistant", "Adding Uranus."}, {"user", "Actually, that's still not right. What about Pluto?"}, {"assistant", "Pluto is a dwarf planet."}},
			toolErrors:  0, toolTotal: 0, corrections: 2, retries: 0, efforts: 0,
			expectedScore: "4-5",
		},
		{
			label:       "Effort override + correction + retry",
			messages:    []msg{{"user", "Write me a haiku about Rust"}, {"assistant", "Memory is safe / Borrow checker guards the gate / No garbage collected"}, {"user", "That's not a haiku. Wrong syllable count."}, {"assistant", "Let me try again..."}},
			toolErrors:  0, toolTotal: 0, corrections: 1, retries: 1, efforts: 1,
			expectedScore: "3-5",
		},
		{
			label:       "Tool failures + corrections",
			messages:    []msg{{"user", "Send an email to my team about the meeting"}, {"assistant", "I encountered errors sending the email."}, {"user", "No, that's wrong. Try my work account."}, {"assistant", "Still having issues."}, {"user", "Actually, I meant use Gmail not Outlook."}},
			toolErrors:  2, toolTotal: 3, corrections: 2, retries: 0, efforts: 0,
			expectedScore: "2-4",
		},
		{
			label:       "Everything went wrong",
			messages:    []msg{{"user", "Book a meeting with Sarah for tomorrow"}, {"assistant", "I don't have calendar access."}, {"user", "Wrong, you do have calendar access."}, {"assistant", "Let me check again..."}, {"user", "No, that's still not right. Try again."}, {"assistant", "I'm having trouble."}},
			toolErrors:  3, toolTotal: 4, corrections: 3, retries: 2, efforts: 1,
			expectedScore: "1-2",
		},
		// More variance in the middle range
		{
			label:       "Decent with one retry",
			messages:    []msg{{"user", "Translate hello to French, German, Japanese"}, {"assistant", "Bonjour, Hallo, Konnichiwa"}},
			toolErrors:  0, toolTotal: 0, corrections: 0, retries: 1, efforts: 0,
			expectedScore: "7-8",
		},
		{
			label:       "Good but needed effort",
			messages:    []msg{{"user", "Explain the CAP theorem with examples"}, {"assistant", "The CAP theorem states that a distributed system can only guarantee two of three: Consistency, Availability, Partition tolerance..."}},
			toolErrors:  0, toolTotal: 1, corrections: 0, retries: 0, efforts: 1,
			expectedScore: "6-7",
		},
		{
			label:       "Tool heavy, one error",
			messages:    []msg{{"user", "Check my unread emails and summarize the important ones"}, {"assistant", "Found 15 unread. 3 are flagged important: ..."}},
			toolErrors:  1, toolTotal: 5, corrections: 0, retries: 0, efforts: 0,
			expectedScore: "6-7",
		},
		{
			label:       "Bad misunderstanding",
			messages:    []msg{{"user", "Set a reminder for my dentist appointment"}, {"assistant", "I've set a reminder for your doctor appointment."}, {"user", "No, I said dentist not doctor."}, {"assistant", "Updated to dentist."}, {"user", "Wrong time. I said 3pm not 3am."}},
			toolErrors:  0, toolTotal: 2, corrections: 2, retries: 0, efforts: 0,
			expectedScore: "3-4",
		},
		{
			label:       "Painful multi-retry",
			messages:    []msg{{"user", "Generate a SQL query to find duplicate emails"}, {"assistant", "SELECT email FROM users GROUP BY email HAVING COUNT(*) > 1"}, {"user", "No, that's not what I need. I need to find rows where the email column has duplicates AND show the user names."}, {"assistant", "Let me try again..."}},
			toolErrors:  0, toolTotal: 0, corrections: 1, retries: 2, efforts: 1,
			expectedScore: "3-4",
		},
	}

	fmt.Fprintf(os.Stderr, "Seeding %d synthetic conversations...\n", len(seeds))

	baseTime := time.Now().UTC().Add(-2 * time.Hour)

	for i, s := range seeds {
		convTime := baseTime.Add(time.Duration(i*8) * time.Minute)
		convID := newSeedID()

		// Create conversation.
		_, err := store.DB().Exec(
			`INSERT INTO conversations (id, user_id, chat_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
			convID, 2069204235, 2069204235, convTime, convTime.Add(time.Duration(len(s.messages))*time.Minute),
		)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  SKIP %s: %v\n", s.label, err)
			continue
		}

		// Insert messages.
		for j, m := range s.messages {
			msgTime := convTime.Add(time.Duration(j) * time.Minute)
			content, _ := json.Marshal(m.text)
			store.DB().Exec(
				`INSERT INTO messages (conversation_id, role, content, created_at) VALUES (?, ?, ?, ?)`,
				convID, m.role, string(content), msgTime,
			)
		}

		// Insert tool calls.
		for j := 0; j < s.toolTotal; j++ {
			isErr := j < s.toolErrors
			output := "success"
			if isErr {
				output = "error: operation failed"
			}
			store.DB().Exec(
				`INSERT INTO tool_calls (id, conversation_id, name, input, output, is_error, created_at) VALUES (?, ?, ?, '{}', ?, ?, ?)`,
				newSeedID(), convID, "test_tool", output, isErr, convTime.Add(time.Duration(j)*30*time.Second),
			)
		}

		// Insert interaction events.
		for j := 0; j < s.retries; j++ {
			store.LogInteractionEvent(convID, 2069204235, 2069204235, "retry", "")
		}
		for j := 0; j < s.efforts; j++ {
			store.LogInteractionEvent(convID, 2069204235, 2069204235, "effort_override", "max")
		}

		fmt.Fprintf(os.Stderr, "  [%2d] %-40s signals: tools=%d/%d corr=%d retry=%d effort=%d  suggested=%s\n",
			i+1, s.label, s.toolErrors, s.toolTotal, s.corrections, s.retries, s.efforts, s.expectedScore)
	}

	fmt.Fprintf(os.Stderr, "\nDone. Now run:\n")
	fmt.Fprintf(os.Stderr, "  curlycatclaw --eval-export --eval-hours 3 --config /data/config.toml\n")
	fmt.Fprintf(os.Stderr, "\nLabel each conversation 0-10. The 'suggested' scores above are hints, but use your judgment.\n")
	return nil
}

type msg struct {
	role, text string
}

type seedConv struct {
	label         string
	messages      []msg
	toolErrors    int
	toolTotal     int
	corrections   int // detected by heuristic from messages
	retries       int
	efforts       int
	expectedScore string
}

func newSeedID() string {
	b := make([]byte, 16)
	rand.Read(b) //nolint:errcheck
	return hex.EncodeToString(b)
}
