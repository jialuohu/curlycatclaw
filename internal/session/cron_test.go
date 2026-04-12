package session

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jialuohu/curlycatclaw/config"
	"github.com/jialuohu/curlycatclaw/internal/claude"
	"github.com/jialuohu/curlycatclaw/internal/mcp"
	"github.com/jialuohu/curlycatclaw/internal/memory"
	"github.com/jialuohu/curlycatclaw/skills"
)

// mockToolRouter satisfies ToolRouter for cron tests.
type mockCronToolRouter struct {
	result string
	err    error
}

func (m *mockCronToolRouter) CallTool(_ context.Context, _ string, _ map[string]any, _, _ int64) (string, error) {
	return m.result, m.err
}

func (m *mockCronToolRouter) Tools() []mcp.ToolDef {
	return nil
}

// mockFactProvider returns canned facts for cron tests.
type mockCronFactProvider struct {
	facts []memory.Fact
}

func (m *mockCronFactProvider) GetFacts(_ int64) ([]memory.Fact, error) {
	return m.facts, nil
}

func (m *mockCronFactProvider) UpdateLastReferenced(_ []int64) error {
	return nil
}

func newTestCronExecutor(llm LLMClient, facts FactProvider) *CronExecutor {
	cfg := &config.Config{
		Timezone: "UTC",
		Claude:   config.ClaudeConfig{Model: "claude-test"},
		Memory:   config.MemoryConfig{Enabled: true},
	}
	return &CronExecutor{
		cfg:    cfg,
		claude: llm,
		mcp:    &mockCronToolRouter{},
		skills: skills.NewRegistry(),
		facts:  facts,
		sem:    make(chan struct{}, 3),
	}
}

func TestCronExecutor_SimplePrompt(t *testing.T) {
	llm := &mockLLM{
		responses: []*claude.Response{
			{TextContent: "Your morning summary: everything is on track."},
		},
	}

	ce := newTestCronExecutor(llm, &mockCronFactProvider{})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := ce.Execute(ctx, 1, 10, "Summarize my day", "", time.Now())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "morning summary") {
		t.Errorf("result = %q, want it to contain the response", result)
	}

	// Verify system prompt contains cron task instruction.
	if len(llm.calls) != 1 {
		t.Fatalf("expected 1 LLM call, got %d", len(llm.calls))
	}
	sp := llm.calls[0].SystemPrompt
	if !strings.Contains(sp, "scheduled task") {
		t.Errorf("system prompt should mention scheduled task, got: %s", sp)
	}
}

func TestCronExecutor_WithToolUse(t *testing.T) {
	toolInput, _ := json.Marshal(map[string]string{"query": "test"})
	llm := &mockLLM{
		responses: []*claude.Response{
			{
				TextContent: "Let me search for that.",
				ToolCalls: []claude.ToolCall{
					{ID: "call_1", Name: "web_search", Input: toolInput},
				},
			},
			{TextContent: "Based on my search: all good."},
		},
	}

	reg := skills.NewRegistry()
	reg.Register(&skills.Skill{
		Name:        "web_search",
		Description: "Search the web",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Execute: func(_ context.Context, _ json.RawMessage) (string, error) {
			return "search result: test passed", nil
		},
	})

	cfg := &config.Config{Timezone: "UTC", Claude: config.ClaudeConfig{Model: "test"}}
	ce := &CronExecutor{
		cfg:    cfg,
		claude: llm,
		mcp:    &mockCronToolRouter{},
		skills: reg,
		facts:  &mockCronFactProvider{},
		sem:    make(chan struct{}, 3),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := ce.Execute(ctx, 1, 10, "Search for test", "", time.Now())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "Based on my search") {
		t.Errorf("result = %q, want final response after tool use", result)
	}
	if len(llm.calls) != 2 {
		t.Errorf("expected 2 LLM calls (initial + after tool), got %d", len(llm.calls))
	}
}

func TestCronExecutor_ToolError(t *testing.T) {
	toolInput, _ := json.Marshal(map[string]string{"query": "fail"})
	llm := &mockLLM{
		responses: []*claude.Response{
			{
				ToolCalls: []claude.ToolCall{
					{ID: "call_1", Name: "broken_tool", Input: toolInput},
				},
			},
			{TextContent: "The tool failed, but here's what I know."},
		},
	}

	reg := skills.NewRegistry()
	reg.Register(&skills.Skill{
		Name:        "broken_tool",
		Description: "Always fails",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Execute: func(_ context.Context, _ json.RawMessage) (string, error) {
			return "", fmt.Errorf("connection refused")
		},
	})

	cfg := &config.Config{Timezone: "UTC", Claude: config.ClaudeConfig{Model: "test"}}
	ce := &CronExecutor{
		cfg:    cfg,
		claude: llm,
		mcp:    &mockCronToolRouter{},
		skills: reg,
		facts:  &mockCronFactProvider{},
		sem:    make(chan struct{}, 3),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := ce.Execute(ctx, 1, 10, "Use broken tool", "", time.Now())
	if err != nil {
		t.Fatalf("Execute should succeed (tool error fed back to Claude): %v", err)
	}
	if !strings.Contains(result, "tool failed") {
		t.Errorf("result = %q, want Claude's response after tool error", result)
	}
}

func TestCronExecutor_UserContext(t *testing.T) {
	var capturedUserID int64
	reg := skills.NewRegistry()
	reg.Register(&skills.Skill{
		Name:        "check_user",
		Description: "Captures user context",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Execute: func(ctx context.Context, _ json.RawMessage) (string, error) {
			user := skills.GetUser(ctx)
			capturedUserID = user.UserID
			return "ok", nil
		},
	})

	toolInput, _ := json.Marshal(map[string]string{})
	llm := &mockLLM{
		responses: []*claude.Response{
			{ToolCalls: []claude.ToolCall{{ID: "call_1", Name: "check_user", Input: toolInput}}},
			{TextContent: "done"},
		},
	}

	cfg := &config.Config{Timezone: "UTC", Claude: config.ClaudeConfig{Model: "test"}}
	ce := &CronExecutor{
		cfg:    cfg,
		claude: llm,
		mcp:    &mockCronToolRouter{},
		skills: reg,
		facts:  &mockCronFactProvider{},
		sem:    make(chan struct{}, 3),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := ce.Execute(ctx, 42, 10, "check user context", "", time.Now())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if capturedUserID != 42 {
		t.Errorf("captured userID = %d, want 42", capturedUserID)
	}
}

func TestCronExecutor_Semaphore(t *testing.T) {
	// Create executor with semaphore of 1.
	slowLLM := &mockLLM{
		responses: []*claude.Response{
			{TextContent: "slow response"},
			{TextContent: "should not run"},
		},
	}

	cfg := &config.Config{Timezone: "UTC", Claude: config.ClaudeConfig{Model: "test"}}
	ce := &CronExecutor{
		cfg:    cfg,
		claude: slowLLM,
		mcp:    &mockCronToolRouter{},
		skills: skills.NewRegistry(),
		facts:  &mockCronFactProvider{},
		sem:    make(chan struct{}, 1), // only 1 slot
	}

	// Fill the semaphore.
	ce.sem <- struct{}{}

	// Try to execute with a short timeout — should fail waiting for semaphore.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := ce.Execute(ctx, 1, 10, "blocked", "", time.Now())
	if err == nil {
		t.Fatal("expected error when semaphore is full and context times out")
	}
	if !strings.Contains(err.Error(), "semaphore") {
		t.Errorf("error = %q, want it to mention semaphore", err.Error())
	}

	// Drain the semaphore.
	<-ce.sem
}

func TestCronExecutor_SystemPromptIncludesFacts(t *testing.T) {
	facts := &mockCronFactProvider{
		facts: []memory.Fact{
			{ID: 1, Fact: "User works at Acme Corp", Category: "identity"},
		},
	}

	ce := newTestCronExecutor(
		&mockLLM{responses: []*claude.Response{{TextContent: "ok"}}},
		facts,
	)

	prompt := ce.buildSystemPrompt(1, time.Now())
	if !strings.Contains(prompt, "Acme Corp") {
		t.Errorf("system prompt should include user facts, got: %s", prompt)
	}
	if !strings.Contains(prompt, "scheduled task") {
		t.Errorf("system prompt should mention scheduled task, got: %s", prompt)
	}
}

// TestBuildSystemPrompt_IncludesScheduledAt guards against regression of the
// cron time-drift bug: the prompt must state the scheduled fire time (not just
// the wall time at execution) so Claude quotes the intended time in its reply.
func TestBuildSystemPrompt_IncludesScheduledAt(t *testing.T) {
	ce := newTestCronExecutor(
		&mockLLM{responses: []*claude.Response{{TextContent: "ok"}}},
		&mockCronFactProvider{},
	)

	// Pick a fixed UTC time so the rendered local-tz string is deterministic for
	// the config's default timezone. Using the configured location for rendering.
	loc := ce.cfg.Location()
	scheduled := time.Date(2026, 4, 12, 15, 0, 0, 0, time.UTC)
	wantScheduled := scheduled.In(loc).Format("2006-01-02 15:04 MST")

	prompt := ce.buildSystemPrompt(1, scheduled)
	if !strings.Contains(prompt, wantScheduled) {
		t.Errorf("prompt must include scheduled fire time %q, got:\n%s", wantScheduled, prompt)
	}
	if !strings.Contains(prompt, "SCHEDULED") {
		t.Errorf("prompt must instruct Claude to use the SCHEDULED time, got:\n%s", prompt)
	}
}
