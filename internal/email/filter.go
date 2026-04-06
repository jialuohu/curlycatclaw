package email

import (
	"strings"
)

// PrefilterResult indicates why a message was filtered or passed.
type PrefilterResult int

const (
	PrefilterPass      PrefilterResult = iota
	PrefilterLabel                     // missing required label
	PrefilterSender                    // matched skip-sender pattern
	PrefilterBodyEmpty                 // body too short after stripping
)

// Prefilter applies cheap, non-LLM checks to an email message.
// Returns PrefilterPass if the message should proceed to LLM triage.
func Prefilter(msg EmailMessage, requiredLabels, skipSenders []string) PrefilterResult {
	// Label check: at least one required label must be present.
	if len(requiredLabels) > 0 {
		found := false
		for _, req := range requiredLabels {
			reqLower := strings.ToLower(req)
			for _, label := range msg.Labels {
				if strings.ToLower(label) == reqLower {
					found = true
					break
				}
			}
			if found {
				break
			}
		}
		if !found {
			return PrefilterLabel
		}
	}

	// Sender check: skip messages from automated/noreply senders.
	fromLower := strings.ToLower(msg.From)
	for _, pattern := range skipSenders {
		if strings.Contains(fromLower, strings.ToLower(pattern)) {
			return PrefilterSender
		}
	}

	// Body length check after stripping quoted replies.
	stripped := StripQuotedReplies(msg.Body, 5000)
	if len(strings.TrimSpace(stripped)) < 20 {
		return PrefilterBodyEmpty
	}

	return PrefilterPass
}
