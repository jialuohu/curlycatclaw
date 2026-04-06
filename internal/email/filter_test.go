package email

import (
	"testing"
)

func TestPrefilter_PassesNormalEmail(t *testing.T) {
	msg := EmailMessage{
		From:   "alice@company.com",
		Labels: []string{"INBOX"},
		Body:   "Hey, can we sync on the project updates? I have some questions about the timeline.",
	}
	result := Prefilter(msg, []string{"INBOX"}, []string{"noreply@"})
	if result != PrefilterPass {
		t.Errorf("expected PrefilterPass, got %d", result)
	}
}

func TestPrefilter_SkipsMissingLabel(t *testing.T) {
	msg := EmailMessage{
		From:   "alice@company.com",
		Labels: []string{"SPAM"},
		Body:   "Normal email body with enough content to pass.",
	}
	result := Prefilter(msg, []string{"INBOX"}, nil)
	if result != PrefilterLabel {
		t.Errorf("expected PrefilterLabel, got %d", result)
	}
}

func TestPrefilter_SkipsNoreply(t *testing.T) {
	msg := EmailMessage{
		From:   "noreply@github.com",
		Labels: []string{"INBOX"},
		Body:   "Your pull request was merged. Congratulations on shipping this feature!",
	}
	result := Prefilter(msg, []string{"INBOX"}, []string{"noreply@"})
	if result != PrefilterSender {
		t.Errorf("expected PrefilterSender, got %d", result)
	}
}

func TestPrefilter_SkipsNoReplyVariant(t *testing.T) {
	msg := EmailMessage{
		From:   "no-reply@service.com",
		Labels: []string{"INBOX"},
		Body:   "Your order has been confirmed and will arrive in 3-5 business days.",
	}
	result := Prefilter(msg, []string{"INBOX"}, []string{"no-reply@"})
	if result != PrefilterSender {
		t.Errorf("expected PrefilterSender, got %d", result)
	}
}

func TestPrefilter_SkipsShortBody(t *testing.T) {
	msg := EmailMessage{
		From:   "alice@company.com",
		Labels: []string{"INBOX"},
		Body:   "Ok",
	}
	result := Prefilter(msg, []string{"INBOX"}, nil)
	if result != PrefilterBodyEmpty {
		t.Errorf("expected PrefilterBodyEmpty, got %d", result)
	}
}

func TestPrefilter_SkipsQuotedOnlyBody(t *testing.T) {
	msg := EmailMessage{
		From:   "alice@company.com",
		Labels: []string{"INBOX"},
		Body:   "> Previous message\n> Another line\n> Third line",
	}
	result := Prefilter(msg, []string{"INBOX"}, nil)
	if result != PrefilterBodyEmpty {
		t.Errorf("expected PrefilterBodyEmpty, got %d", result)
	}
}

func TestPrefilter_CaseInsensitiveLabels(t *testing.T) {
	msg := EmailMessage{
		From:   "alice@company.com",
		Labels: []string{"inbox"},
		Body:   "Normal email body with enough content to pass the filter check.",
	}
	result := Prefilter(msg, []string{"INBOX"}, nil)
	if result != PrefilterPass {
		t.Errorf("expected PrefilterPass, got %d", result)
	}
}

func TestPrefilter_NoLabelsRequired(t *testing.T) {
	msg := EmailMessage{
		From: "alice@company.com",
		Body: "Normal email body with enough content to pass the filter check.",
	}
	result := Prefilter(msg, nil, nil)
	if result != PrefilterPass {
		t.Errorf("expected PrefilterPass with no label requirements, got %d", result)
	}
}

func TestPrefilter_SenderPatternSubstring(t *testing.T) {
	msg := EmailMessage{
		From:   "notifications@slack.com",
		Labels: []string{"INBOX"},
		Body:   "You have a new message in #general channel from your teammate.",
	}
	result := Prefilter(msg, []string{"INBOX"}, []string{"notifications@"})
	if result != PrefilterSender {
		t.Errorf("expected PrefilterSender, got %d", result)
	}
}
