package eval

import (
	"strings"
	"testing"
	"time"

	"github.com/jialuohu/curlycatclaw/internal/telegram"
)

func TestSendReport_BasicFormatting(t *testing.T) {
	ch := make(chan telegram.OutgoingMessage, 1)
	r := NewReporter(ch)

	run := EvalRun{ConversationsScanned: 5}
	scores := []EvalScore{
		{OverallScore: 0.8, CorrectionCount: 1, RetryCount: 0},
		{OverallScore: 0.4, CorrectionCount: 3, RetryCount: 2},
	}
	clusters := []FailureCluster{
		{ClusterType: "tool_error", Severity: 7, Description: "Tool X failed 3 times"},
	}

	r.SendReport(123, run, scores, clusters)

	msg := <-ch
	if msg.ChatID != 123 {
		t.Errorf("expected ChatID 123, got %d", msg.ChatID)
	}
	if !strings.Contains(msg.Text, "Conversations scanned: 5") {
		t.Error("expected conversation count in report")
	}
	if !strings.Contains(msg.Text, "Average quality: 0.60") {
		t.Errorf("expected average 0.60, got text: %s", msg.Text)
	}
	if !strings.Contains(msg.Text, "Failure patterns: 1") {
		t.Error("expected failure pattern count")
	}
	if !strings.Contains(msg.Text, "[tool_error]") {
		t.Error("expected tool_error cluster in report")
	}
}

func TestSendReport_WarnMarker(t *testing.T) {
	ch := make(chan telegram.OutgoingMessage, 1)
	r := NewReporter(ch)

	run := EvalRun{ConversationsScanned: 1}
	scores := []EvalScore{
		{OverallScore: 0.5, CorrectionCount: 2, RetryCount: 1},
	}

	r.SendReport(1, run, scores, nil)

	msg := <-ch
	if !strings.Contains(msg.Text, "[WARN]") {
		t.Error("expected WARN marker for score < 0.6")
	}
}

func TestSendReport_OKMarker(t *testing.T) {
	ch := make(chan telegram.OutgoingMessage, 1)
	r := NewReporter(ch)

	run := EvalRun{ConversationsScanned: 1}
	scores := []EvalScore{
		{OverallScore: 0.9, CorrectionCount: 0, RetryCount: 0},
	}

	r.SendReport(1, run, scores, nil)

	msg := <-ch
	if !strings.Contains(msg.Text, "[OK]") {
		t.Error("expected OK marker for score >= 0.6")
	}
}

func TestSendReport_TruncatesLongLists(t *testing.T) {
	ch := make(chan telegram.OutgoingMessage, 1)
	r := NewReporter(ch)

	run := EvalRun{ConversationsScanned: 15}
	scores := make([]EvalScore, 15)
	for i := range scores {
		scores[i] = EvalScore{OverallScore: 0.7}
	}
	clusters := make([]FailureCluster, 8)
	for i := range clusters {
		clusters[i] = FailureCluster{
			ClusterType: "test", Severity: 5, Description: "test cluster",
			CreatedAt: time.Now(),
		}
	}

	r.SendReport(1, run, scores, clusters)

	msg := <-ch
	if !strings.Contains(msg.Text, "and 3 more") {
		t.Error("expected truncation for clusters > 5")
	}
	if !strings.Contains(msg.Text, "and 5 more") {
		t.Error("expected truncation for scores > 10")
	}
}

func TestSendReport_EmptyScores(t *testing.T) {
	ch := make(chan telegram.OutgoingMessage, 1)
	r := NewReporter(ch)

	run := EvalRun{ConversationsScanned: 0}
	r.SendReport(1, run, nil, nil)

	msg := <-ch
	if !strings.Contains(msg.Text, "Conversations scanned: 0") {
		t.Error("expected zero conversation count")
	}
	if strings.Contains(msg.Text, "Average quality") {
		t.Error("should not show average with no scores")
	}
}

func TestSendReport_FullChannel(t *testing.T) {
	// Channel with no buffer - send should not block
	ch := make(chan telegram.OutgoingMessage)
	r := NewReporter(ch)

	run := EvalRun{ConversationsScanned: 1}
	// This should not block thanks to the select/default pattern
	r.SendReport(1, run, nil, nil)
	// If we reach here, it didn't block - test passes
}
