package eval

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/jialuohu/curlycatclaw/internal/telegram"
)

// Reporter sends eval results to Telegram as plain text messages.
type Reporter struct {
	tg chan<- telegram.OutgoingMessage
}

// NewReporter creates a Reporter that sends messages through the given Telegram inbox.
func NewReporter(tg chan<- telegram.OutgoingMessage) *Reporter {
	return &Reporter{tg: tg}
}

// SendReport sends a summary of an eval run to the specified chat.
func (r *Reporter) SendReport(chatID int64, run EvalRun, scores []EvalScore, clusters []FailureCluster) {
	var b strings.Builder

	b.WriteString("Eval Report\n\n")
	fmt.Fprintf(&b, "Conversations scanned: %d\n", run.ConversationsScanned)

	if len(scores) > 0 {
		var total float64
		for _, s := range scores {
			total += s.OverallScore
		}
		avg := total / float64(len(scores))
		fmt.Fprintf(&b, "Average quality: %.2f/1.0\n", avg)
	}

	fmt.Fprintf(&b, "Failure patterns: %d\n", len(clusters))

	if len(clusters) > 0 {
		b.WriteString("\nFailures:\n")
		for i, c := range clusters {
			if i >= 5 {
				fmt.Fprintf(&b, "  ... and %d more\n", len(clusters)-5)
				break
			}
			fmt.Fprintf(&b, "  [%s] (severity %d) %s\n", c.ClusterType, c.Severity, c.Description)
		}
	}

	if len(scores) > 0 {
		b.WriteString("\nPer-conversation:\n")
		for i, s := range scores {
			if i >= 10 {
				fmt.Fprintf(&b, "  ... and %d more\n", len(scores)-10)
				break
			}
			marker := "OK"
			if s.OverallScore < 0.6 {
				marker = "WARN"
			}
			fmt.Fprintf(&b, "  [%s] %.2f  (corrections: %d, retries: %d)\n",
				marker, s.OverallScore, s.CorrectionCount, s.RetryCount)
		}
	}

	select {
	case r.tg <- telegram.OutgoingMessage{ChatID: chatID, Text: b.String()}:
	default:
		slog.Warn("eval: report dropped, telegram inbox full", "chat_id", chatID)
	}
}
