package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
)

// ToolRouter abstracts MCP tool invocation.
type ToolRouter interface {
	CallTool(ctx context.Context, name string, args map[string]any, userID, chatID int64) (string, error)
}

// GmailSource implements Source for Gmail via the GWS MCP server.
// It handles account discovery, date-based cursors, and Gmail-specific
// JSON parsing. Each Gmail account is a separate partition.
type GmailSource struct {
	name       string
	account    string // Gmail account name (partition), empty for single-account
	mcp        ToolRouter
	ownerUID   int64
	ownerCID   int64
	labels     []string
	skipSenders []string
}

// GmailSourceConfig holds Gmail-specific configuration.
type GmailSourceConfig struct {
	Name        string
	Account     string // partition name, empty for single-account
	MCP         ToolRouter
	OwnerUID    int64
	OwnerCID    int64
	Labels      []string
	SkipSenders []string
}

func NewGmailSource(cfg GmailSourceConfig) *GmailSource {
	return &GmailSource{
		name:        cfg.Name,
		account:     cfg.Account,
		mcp:         cfg.MCP,
		ownerUID:    cfg.OwnerUID,
		ownerCID:    cfg.OwnerCID,
		labels:      cfg.Labels,
		skipSenders: cfg.SkipSenders,
	}
}

func (s *GmailSource) Name() string { return s.name }

// Discover searches Gmail for recent messages. The cursor is a JSON string
// containing the last search query date (e.g., "2025/04/01").
// A nil cursor defaults to "newer_than:1d" for incremental sync.
func (s *GmailSource) Discover(ctx context.Context, cursor json.RawMessage) ([]ItemRef, json.RawMessage, error) {
	query := "newer_than:1d"
	if cursor != nil {
		var dateCursor string
		if err := json.Unmarshal(cursor, &dateCursor); err == nil && dateCursor != "" {
			query = fmt.Sprintf("after:%s", dateCursor)
		}
	}

	args := map[string]any{"query": query, "labels": true}
	if s.account != "" {
		args["account"] = s.account
	}

	result, err := s.mcp.CallTool(ctx, "gws__gws_gmail_triage", args, s.ownerUID, s.ownerCID)
	if err != nil {
		return nil, cursor, fmt.Errorf("gmail triage: %w", err)
	}

	preview := result
	if len(preview) > 500 { preview = preview[:500] }
	slog.Info("gmail triage raw response", "len", len(result), "preview", preview)
	refs := parseGmailSearchResult(result)
	var items []ItemRef
	for _, ref := range refs {
		items = append(items, ItemRef{
			ID:      ref.id,
			Title:   ref.subject,
			Snippet: ref.snippet,
			Metadata: map[string]string{
				"from":   ref.from,
				"labels": strings.Join(ref.labels, ","),
			},
		})
	}

	// Cursor stays the same for incremental — we rely on dedup.
	return items, cursor, nil
}

// Read fetches the full email message content.
func (s *GmailSource) Read(ctx context.Context, id string) (Content, error) {
	args := map[string]any{"id": id}
	if s.account != "" {
		args["account"] = s.account
	}

	result, err := s.mcp.CallTool(ctx, "gws__gws_gmail_read", args, s.ownerUID, s.ownerCID)
	if err != nil {
		return Content{}, fmt.Errorf("gmail read: %w", err)
	}

	msg := parseGmailReadResult(result, s.account, id)
	sourceID := "gmail"
	if s.account != "" {
		sourceID = fmt.Sprintf("gmail:%s", s.account)
	}

	return Content{
		ID:       msg.id,
		SourceID: sourceID,
		Title:    msg.subject,
		Body:     msg.body,
		Author:   msg.from,
		Date:     msg.date,
		Metadata: map[string]string{
			"thread_id": msg.threadID,
			"to":        msg.to,
			"account":   s.account,
			"labels":    strings.Join(msg.labels, ","),
		},
	}, nil
}

// Prefilter checks labels and sender patterns.
func (s *GmailSource) Prefilter(item ItemRef) bool {
	// Label check.
	if len(s.labels) > 0 {
		itemLabels := strings.Split(item.Metadata["labels"], ",")
		found := false
		for _, req := range s.labels {
			reqLower := strings.ToLower(req)
			for _, label := range itemLabels {
				if strings.ToLower(strings.TrimSpace(label)) == reqLower {
					found = true
					break
				}
			}
			if found {
				break
			}
		}
		if !found {
			return false
		}
	}

	// Sender check.
	fromLower := strings.ToLower(item.Metadata["from"])
	for _, pattern := range s.skipSenders {
		if strings.Contains(fromLower, strings.ToLower(pattern)) {
			return false
		}
	}

	return true
}

// Gmail JSON parsing helpers.

type gmailMessageRef struct {
	id      string
	from    string
	subject string
	snippet string
	labels  []string
}

type gmailMessage struct {
	id       string
	threadID string
	from     string
	to       string
	subject  string
	date     string
	body     string
	labels   []string
}

func parseGmailSearchResult(result string) []gmailMessageRef {
	// MCP CallTool may wrap the response in {"text": "..."} — unwrap it.
	raw := result
	var textEnvelope struct{ Text string `json:"text"` }
	if json.Unmarshal([]byte(raw), &textEnvelope) == nil && textEnvelope.Text != "" {
		raw = textEnvelope.Text
	}

	type triageMsg struct {
		ID       string   `json:"id"`
		From     string   `json:"from"`
		Subject  string   `json:"subject"`
		Date     string   `json:"date"`
		Snippet  string   `json:"snippet"`
		Labels   []string `json:"labels"`   // gws +triage --labels output
		LabelIDs []string `json:"labelIds"` // fallback for raw API format
	}
	var messages []triageMsg
	if err := json.Unmarshal([]byte(raw), &messages); err != nil {
		var wrapper struct {
			Messages []triageMsg `json:"messages"`
		}
		if err := json.Unmarshal([]byte(raw), &wrapper); err != nil {
			slog.Warn("ingest: failed to parse gmail triage result", "err", err)
			return nil
		}
		messages = wrapper.Messages
	}

	var refs []gmailMessageRef
	for _, m := range messages {
		labels := m.Labels
		if len(labels) == 0 {
			labels = m.LabelIDs // fallback to raw API format
		}
		refs = append(refs, gmailMessageRef{
			id:      m.ID,
			from:    m.From,
			subject: m.Subject,
			snippet: m.Snippet,
			labels:  labels,
		})
	}
	return refs
}

func parseGmailReadResult(result, account, messageID string) gmailMessage {
	// Unwrap MCP {"text": "..."} envelope.
	raw := result
	var textEnvelope struct{ Text string `json:"text"` }
	if json.Unmarshal([]byte(raw), &textEnvelope) == nil && textEnvelope.Text != "" {
		raw = textEnvelope.Text
	}

	var msg struct {
		ID       string   `json:"id"`
		ThreadID string   `json:"threadId"`
		From     string   `json:"from"`
		To       string   `json:"to"`
		Subject  string   `json:"subject"`
		Date     string   `json:"date"`
		Body     string   `json:"body"`
		Labels   []string `json:"labelIds"`
	}
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		return gmailMessage{id: messageID, body: result}
	}

	return gmailMessage{
		id:       msg.ID,
		threadID: msg.ThreadID,
		from:     msg.From,
		to:       msg.To,
		subject:  msg.Subject,
		date:     msg.Date,
		body:     msg.Body,
		labels:   msg.Labels,
	}
}

// DiscoverGmailAccounts calls gws_list_accounts via MCP to find configured
// Gmail accounts. Returns [""] for single-account mode.
func DiscoverGmailAccounts(ctx context.Context, mcp ToolRouter, ownerUID, ownerCID int64) ([]string, error) {
	result, err := mcp.CallTool(ctx, "gws__gws_list_accounts", map[string]any{}, ownerUID, ownerCID)
	if err != nil {
		return []string{""}, nil
	}

	// Unwrap MCP {"text": "..."} envelope if present.
	raw := result
	var textEnvelope struct{ Text string `json:"text"` }
	if json.Unmarshal([]byte(result), &textEnvelope) == nil && textEnvelope.Text != "" {
		raw = textEnvelope.Text
	}

	var accounts []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(raw), &accounts); err != nil {
		return []string{""}, nil
	}

	var names []string
	for _, acc := range accounts {
		names = append(names, acc.Name)
	}
	if len(names) == 0 {
		return []string{""}, nil
	}
	return names, nil
}

// NotionSource implements Source for Notion via MCP.
type NotionSource struct {
	name     string
	mcp      ToolRouter
	ownerUID int64
	ownerCID int64
}

// NotionSourceConfig holds Notion-specific configuration.
type NotionSourceConfig struct {
	Name     string
	MCP      ToolRouter
	OwnerUID int64
	OwnerCID int64
}

func NewNotionSource(cfg NotionSourceConfig) *NotionSource {
	return &NotionSource{
		name:     cfg.Name,
		mcp:      cfg.MCP,
		ownerUID: cfg.OwnerUID,
		ownerCID: cfg.OwnerCID,
	}
}

func (s *NotionSource) Name() string { return s.name }

// Discover searches Notion for recently modified pages.
// Cursor is a JSON string containing a pagination token.
func (s *NotionSource) Discover(ctx context.Context, cursor json.RawMessage) ([]ItemRef, json.RawMessage, error) {
	args := map[string]any{
		"query": "",
		"sort":  map[string]any{"direction": "descending", "timestamp": "last_edited_time"},
	}
	if cursor != nil {
		var token string
		if err := json.Unmarshal(cursor, &token); err == nil && token != "" {
			args["start_cursor"] = token
		}
	}

	result, err := s.mcp.CallTool(ctx, "notion__notion-search", args, s.ownerUID, s.ownerCID)
	if err != nil {
		return nil, cursor, fmt.Errorf("notion search: %w", err)
	}

	items, nextCursor := parseNotionSearchResult(result)
	var newCursor json.RawMessage
	if nextCursor != "" {
		newCursor, _ = json.Marshal(nextCursor)
	}
	return items, newCursor, nil
}

// Read fetches the full page content from Notion.
func (s *NotionSource) Read(ctx context.Context, id string) (Content, error) {
	result, err := s.mcp.CallTool(ctx, "notion__notion-fetch", map[string]any{"pageId": id}, s.ownerUID, s.ownerCID)
	if err != nil {
		return Content{}, fmt.Errorf("notion read: %w", err)
	}

	return parseNotionPage(result, id), nil
}

// Prefilter always passes for Notion (filtering done server-side via search query).
func (s *NotionSource) Prefilter(_ ItemRef) bool { return true }

// Notion JSON parsing helpers.

func parseNotionSearchResult(result string) ([]ItemRef, string) {
	// Unwrap MCP {"text": "..."} envelope if present.
	var searchEnvelope struct{ Text string `json:"text"` }
	if json.Unmarshal([]byte(result), &searchEnvelope) == nil && searchEnvelope.Text != "" {
		result = searchEnvelope.Text
	}

	var resp struct {
		Results []struct {
			ID         string `json:"id"`
			Properties struct {
				Title struct {
					Title []struct {
						PlainText string `json:"plain_text"`
					} `json:"title"`
				} `json:"title"`
				Name struct {
					Title []struct {
						PlainText string `json:"plain_text"`
					} `json:"title"`
				} `json:"Name"`
			} `json:"properties"`
			LastEdited string `json:"last_edited_time"`
		} `json:"results"`
		NextCursor string `json:"next_cursor"`
		HasMore    bool   `json:"has_more"`
	}

	if err := json.Unmarshal([]byte(result), &resp); err != nil {
		slog.Warn("ingest: failed to parse notion search result", "err", err)
		return nil, ""
	}

	var items []ItemRef
	for _, r := range resp.Results {
		title := ""
		if len(r.Properties.Title.Title) > 0 {
			title = r.Properties.Title.Title[0].PlainText
		} else if len(r.Properties.Name.Title) > 0 {
			title = r.Properties.Name.Title[0].PlainText
		}

		items = append(items, ItemRef{
			ID:    r.ID,
			Title: title,
			Metadata: map[string]string{
				"last_edited": r.LastEdited,
			},
		})
	}

	nextCursor := ""
	if resp.HasMore {
		nextCursor = resp.NextCursor
	}
	return items, nextCursor
}

func parseNotionPage(result, pageID string) Content {
	// Unwrap MCP {"text": "..."} envelope if present.
	var pageEnvelope struct{ Text string `json:"text"` }
	if json.Unmarshal([]byte(result), &pageEnvelope) == nil && pageEnvelope.Text != "" {
		result = pageEnvelope.Text
	}

	var page struct {
		Title   string `json:"title"`
		Content string `json:"content"`
		Author  string `json:"created_by"`
		Date    string `json:"last_edited_time"`
	}
	if err := json.Unmarshal([]byte(result), &page); err != nil {
		// If parsing fails, treat raw result as body.
		return Content{
			ID:       pageID,
			SourceID: "notion",
			Body:     result,
		}
	}

	return Content{
		ID:       pageID,
		SourceID: "notion",
		Title:    page.Title,
		Body:     page.Content,
		Author:   page.Author,
		Date:     page.Date,
	}
}
