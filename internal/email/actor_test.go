package email

import "testing"

func TestParseGmailSearchResult_JSONArray(t *testing.T) {
	input := `[{"id":"msg1","from":"alice@test.com","subject":"Hello","snippet":"Hi there","labelIds":["INBOX"]}]`
	refs := parseGmailSearchResult(input)
	if len(refs) != 1 {
		t.Fatalf("expected 1 ref, got %d", len(refs))
	}
	if refs[0].ID != "msg1" {
		t.Errorf("expected ID msg1, got %s", refs[0].ID)
	}
	if refs[0].From != "alice@test.com" {
		t.Errorf("expected From alice@test.com, got %s", refs[0].From)
	}
	if refs[0].Subject != "Hello" {
		t.Errorf("expected Subject Hello, got %s", refs[0].Subject)
	}
	if len(refs[0].Labels) != 1 || refs[0].Labels[0] != "INBOX" {
		t.Errorf("expected Labels [INBOX], got %v", refs[0].Labels)
	}
}

func TestParseGmailSearchResult_WrappedObject(t *testing.T) {
	input := `{"messages":[{"id":"msg2","from":"bob@test.com","subject":"Update","snippet":"FYI","labelIds":["INBOX","IMPORTANT"]}]}`
	refs := parseGmailSearchResult(input)
	if len(refs) != 1 {
		t.Fatalf("expected 1 ref, got %d", len(refs))
	}
	if refs[0].ID != "msg2" {
		t.Errorf("expected ID msg2, got %s", refs[0].ID)
	}
	if len(refs[0].Labels) != 2 {
		t.Errorf("expected 2 labels, got %d", len(refs[0].Labels))
	}
}

func TestParseGmailSearchResult_InvalidJSON(t *testing.T) {
	refs := parseGmailSearchResult("not json at all")
	if refs != nil {
		t.Errorf("expected nil for invalid JSON, got %v", refs)
	}
}

func TestParseGmailSearchResult_EmptyArray(t *testing.T) {
	refs := parseGmailSearchResult("[]")
	if len(refs) != 0 {
		t.Errorf("expected 0 refs, got %d", len(refs))
	}
}

func TestParseGmailSearchResult_MultipleMessages(t *testing.T) {
	input := `[{"id":"a","from":"x@y.com","subject":"A","snippet":"","labelIds":[]},{"id":"b","from":"z@y.com","subject":"B","snippet":"","labelIds":[]}]`
	refs := parseGmailSearchResult(input)
	if len(refs) != 2 {
		t.Fatalf("expected 2 refs, got %d", len(refs))
	}
	if refs[0].ID != "a" || refs[1].ID != "b" {
		t.Errorf("unexpected IDs: %s, %s", refs[0].ID, refs[1].ID)
	}
}

func TestParseGmailReadResult_ValidJSON(t *testing.T) {
	input := `{"id":"msg1","threadId":"t1","from":"alice@test.com","to":"me@test.com","subject":"Hello","date":"2026-01-01","body":"Email body here","labelIds":["INBOX"]}`
	msg := parseGmailReadResult(input, "work", "msg1")
	if msg.MessageID != "msg1" {
		t.Errorf("expected MessageID msg1, got %s", msg.MessageID)
	}
	if msg.ThreadID != "t1" {
		t.Errorf("expected ThreadID t1, got %s", msg.ThreadID)
	}
	if msg.Account != "work" {
		t.Errorf("expected Account work, got %s", msg.Account)
	}
	if msg.From != "alice@test.com" {
		t.Errorf("expected From alice@test.com, got %s", msg.From)
	}
	if msg.Subject != "Hello" {
		t.Errorf("expected Subject Hello, got %s", msg.Subject)
	}
	if msg.To != "me@test.com" {
		t.Errorf("expected To me@test.com, got %s", msg.To)
	}
	if msg.Date != "2026-01-01" {
		t.Errorf("expected Date 2026-01-01, got %s", msg.Date)
	}
	if msg.Body != "Email body here" {
		t.Errorf("expected body 'Email body here', got %s", msg.Body)
	}
	if len(msg.Labels) != 1 || msg.Labels[0] != "INBOX" {
		t.Errorf("expected Labels [INBOX], got %v", msg.Labels)
	}
}

func TestParseGmailReadResult_InvalidJSON_FallsBackToRawBody(t *testing.T) {
	raw := "This is just plain text, not JSON"
	msg := parseGmailReadResult(raw, "personal", "fallback-id")
	if msg.MessageID != "fallback-id" {
		t.Errorf("expected MessageID fallback-id, got %s", msg.MessageID)
	}
	if msg.Account != "personal" {
		t.Errorf("expected Account personal, got %s", msg.Account)
	}
	if msg.Body != raw {
		t.Errorf("expected Body to be raw input, got %s", msg.Body)
	}
	if msg.From != "" {
		t.Errorf("expected empty From on fallback, got %s", msg.From)
	}
}

func TestParseGmailReadResult_EmptyAccount(t *testing.T) {
	input := `{"id":"msg1","body":"test"}`
	msg := parseGmailReadResult(input, "", "msg1")
	if msg.Account != "" {
		t.Errorf("expected empty Account, got %s", msg.Account)
	}
}
