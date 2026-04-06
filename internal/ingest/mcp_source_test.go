package ingest

import (
	"context"
	"encoding/json"
	"testing"
)

func TestParseGmailSearchResult_Array(t *testing.T) {
	input := `[{"id":"msg1","from":"alice@test.com","subject":"Hello","snippet":"Hey there","labelIds":["INBOX"]}]`
	refs := parseGmailSearchResult(input)
	if len(refs) != 1 {
		t.Fatalf("expected 1 ref, got %d", len(refs))
	}
	if refs[0].id != "msg1" {
		t.Errorf("expected id msg1, got %s", refs[0].id)
	}
	if refs[0].from != "alice@test.com" {
		t.Errorf("expected from alice@test.com, got %s", refs[0].from)
	}
}

func TestParseGmailSearchResult_Wrapper(t *testing.T) {
	input := `{"messages":[{"id":"msg2","from":"bob@test.com","subject":"Test","snippet":"Preview","labelIds":["INBOX","IMPORTANT"]}]}`
	refs := parseGmailSearchResult(input)
	if len(refs) != 1 {
		t.Fatalf("expected 1 ref, got %d", len(refs))
	}
	if refs[0].subject != "Test" {
		t.Errorf("expected subject Test, got %s", refs[0].subject)
	}
}

func TestParseGmailSearchResult_Invalid(t *testing.T) {
	refs := parseGmailSearchResult("not json")
	if len(refs) != 0 {
		t.Errorf("expected 0 refs for invalid JSON, got %d", len(refs))
	}
}

func TestParseGmailReadResult(t *testing.T) {
	input := `{"id":"msg1","threadId":"thread1","from":"alice@test.com","to":"me@test.com","subject":"Hello","date":"2025-04-01","body":"Message body","labelIds":["INBOX"]}`
	msg := parseGmailReadResult(input, "personal", "msg1")
	if msg.id != "msg1" {
		t.Errorf("expected id msg1, got %s", msg.id)
	}
	if msg.threadID != "thread1" {
		t.Errorf("expected threadID thread1, got %s", msg.threadID)
	}
	if msg.body != "Message body" {
		t.Errorf("unexpected body: %s", msg.body)
	}
}

func TestParseGmailReadResult_InvalidJSON(t *testing.T) {
	msg := parseGmailReadResult("raw text fallback", "acc", "msg1")
	if msg.body != "raw text fallback" {
		t.Errorf("expected raw text as body, got %s", msg.body)
	}
	if msg.id != "msg1" {
		t.Errorf("expected id msg1, got %s", msg.id)
	}
}

func TestGmailSource_Prefilter_Labels(t *testing.T) {
	src := NewGmailSource(GmailSourceConfig{
		Name:        "gmail",
		Labels:      []string{"INBOX"},
		SkipSenders: []string{"noreply@"},
	})

	tests := []struct {
		name   string
		item   ItemRef
		want   bool
	}{
		{
			name: "passes with INBOX label",
			item: ItemRef{
				Metadata: map[string]string{
					"from":   "alice@company.com",
					"labels": "INBOX",
				},
			},
			want: true,
		},
		{
			name: "fails without required label",
			item: ItemRef{
				Metadata: map[string]string{
					"from":   "alice@company.com",
					"labels": "SPAM",
				},
			},
			want: false,
		},
		{
			name: "fails with skip sender",
			item: ItemRef{
				Metadata: map[string]string{
					"from":   "noreply@github.com",
					"labels": "INBOX",
				},
			},
			want: false,
		},
		{
			name: "case insensitive labels",
			item: ItemRef{
				Metadata: map[string]string{
					"from":   "alice@company.com",
					"labels": "inbox",
				},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := src.Prefilter(tt.item)
			if got != tt.want {
				t.Errorf("Prefilter() = %v, want %v", got, tt.want)
			}
		})
	}
}

type mockToolRouter struct {
	results map[string]string
	err     error
}

func (m *mockToolRouter) CallTool(_ context.Context, name string, _ map[string]any, _, _ int64) (string, error) {
	if m.err != nil {
		return "", m.err
	}
	return m.results[name], nil
}

func TestGmailSource_Discover(t *testing.T) {
	mock := &mockToolRouter{
		results: map[string]string{
			"gws__gws_gmail_search": `[{"id":"msg1","from":"alice@test.com","subject":"Test","snippet":"Preview","labelIds":["INBOX"]}]`,
		},
	}

	src := NewGmailSource(GmailSourceConfig{
		Name:     "gmail",
		MCP:      mock,
		OwnerUID: 1,
		OwnerCID: 1,
	})

	items, _, err := src.Discover(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].ID != "msg1" {
		t.Errorf("expected id msg1, got %s", items[0].ID)
	}
}

func TestGmailSource_Discover_WithCursor(t *testing.T) {
	mock := &mockToolRouter{
		results: map[string]string{
			"gws__gws_gmail_search": `[]`,
		},
	}

	src := NewGmailSource(GmailSourceConfig{
		Name:     "gmail",
		MCP:      mock,
		OwnerUID: 1,
		OwnerCID: 1,
	})

	cursor, _ := json.Marshal("2025/04/01")
	items, _, err := src.Discover(context.Background(), cursor)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Errorf("expected 0 items, got %d", len(items))
	}
}

func TestNotionSource_Prefilter(t *testing.T) {
	src := NewNotionSource(NotionSourceConfig{Name: "notion"})
	// Notion always passes prefilter.
	if !src.Prefilter(ItemRef{}) {
		t.Error("expected Notion prefilter to always pass")
	}
}

func TestDiscoverGmailAccounts_Success(t *testing.T) {
	mock := &mockToolRouter{
		results: map[string]string{
			"gws__gws_list_accounts": `[{"name":"personal"},{"name":"work"}]`,
		},
	}

	accounts, err := DiscoverGmailAccounts(context.Background(), mock, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 2 {
		t.Fatalf("expected 2 accounts, got %d", len(accounts))
	}
	if accounts[0] != "personal" || accounts[1] != "work" {
		t.Errorf("unexpected accounts: %v", accounts)
	}
}

func TestDiscoverGmailAccounts_Fallback(t *testing.T) {
	mock := &mockToolRouter{
		results: map[string]string{
			"gws__gws_list_accounts": "not json",
		},
	}

	accounts, err := DiscoverGmailAccounts(context.Background(), mock, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 || accounts[0] != "" {
		t.Errorf("expected single empty-string account fallback, got %v", accounts)
	}
}
