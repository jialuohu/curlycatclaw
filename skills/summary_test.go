package skills

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jialuohu/curlycatclaw/internal/memory"
)

type mockSummaryStore struct {
	summaries []memory.Summary
	deletedID int64
	deleteErr error
}

func (m *mockSummaryStore) ListSummaries(userID int64) ([]memory.Summary, error) {
	var filtered []memory.Summary
	for _, s := range m.summaries {
		filtered = append(filtered, s)
	}
	return filtered, nil
}

func (m *mockSummaryStore) DeleteSummary(summaryID int64, userID int64) error {
	m.deletedID = summaryID
	return m.deleteErr
}

func TestListSummaries(t *testing.T) {
	store := &mockSummaryStore{
		summaries: []memory.Summary{
			{ID: 1, Summary: "User discussed Go testing patterns", CreatedAt: "2026-03-28T10:00:00Z"},
			{ID: 2, Summary: "User asked about Docker deployment", CreatedAt: "2026-03-29T14:00:00Z"},
		},
	}

	skills := InitSummarySkills(store)
	var listSkill *Skill
	for _, s := range skills {
		if s.Name == "list_summaries" {
			listSkill = s
			break
		}
	}
	if listSkill == nil {
		t.Fatal("list_summaries skill not found")
	}

	ctx := WithUser(context.Background(), UserInfo{UserID: 42, ChatID: 100})
	result, err := listSkill.Execute(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "2 summaries") {
		t.Errorf("expected '2 summaries', got %q", result)
	}
	if !strings.Contains(result, "[id=1]") || !strings.Contains(result, "[id=2]") {
		t.Errorf("expected both summary IDs in output, got %q", result)
	}
}

func TestListSummaries_Empty(t *testing.T) {
	store := &mockSummaryStore{}

	skills := InitSummarySkills(store)
	var listSkill *Skill
	for _, s := range skills {
		if s.Name == "list_summaries" {
			listSkill = s
			break
		}
	}

	ctx := WithUser(context.Background(), UserInfo{UserID: 42, ChatID: 100})
	result, err := listSkill.Execute(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "No conversation summaries") {
		t.Errorf("expected empty message, got %q", result)
	}
}

func TestDeleteSummary(t *testing.T) {
	store := &mockSummaryStore{}

	skills := InitSummarySkills(store)
	var deleteSkill *Skill
	for _, s := range skills {
		if s.Name == "delete_summary" {
			deleteSkill = s
			break
		}
	}

	ctx := WithUser(context.Background(), UserInfo{UserID: 42, ChatID: 100})
	result, err := deleteSkill.Execute(ctx, json.RawMessage(`{"summary_id": 5}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if store.deletedID != 5 {
		t.Errorf("expected delete call with ID 5, got %d", store.deletedID)
	}
	if !strings.Contains(result, "Deleted summary 5") {
		t.Errorf("expected confirmation, got %q", result)
	}
}

func TestDeleteSummary_InvalidID(t *testing.T) {
	store := &mockSummaryStore{}

	skills := InitSummarySkills(store)
	var deleteSkill *Skill
	for _, s := range skills {
		if s.Name == "delete_summary" {
			deleteSkill = s
			break
		}
	}

	ctx := WithUser(context.Background(), UserInfo{UserID: 42, ChatID: 100})
	_, err := deleteSkill.Execute(ctx, json.RawMessage(`{"summary_id": 0}`))
	if err == nil {
		t.Fatal("expected error for invalid summary_id")
	}
}
