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
		cfg:        cfg,
		configPath: "/tmp/test-config.toml",
		claude:     llm,
		mcp:        &mockCronToolRouter{},
		skills:     skills.NewRegistry(),
		facts:      facts,
		sem:        make(chan struct{}, 3),
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

	result, err := ce.Execute(ctx, 1, 10, "Summarize my day", "", "", time.Now())
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

	result, err := ce.Execute(ctx, 1, 10, "Search for test", "", "", time.Now())
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

	result, err := ce.Execute(ctx, 1, 10, "Use broken tool", "", "", time.Now())
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

	_, err := ce.Execute(ctx, 42, 10, "check user context", "", "", time.Now())
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

	_, err := ce.Execute(ctx, 1, 10, "blocked", "", "", time.Now())
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

// TestBuildSystemPrompt_IncludesMCPNamingHint is the regression guard for
// the Apr 17 incident where the cron-fired paper digest claimed
// `search_papers`/`search_arxiv`/`search_semantic` were "not loaded in
// the current environment" and fell back to WebSearch. Root cause: the
// MCP subprocess loaded all 60 paper-search tools correctly, but they
// were exposed under namespaced names (`mcp__curlycatclaw-skills__paper
// -search-mcp__search_papers`), and the reminder prompt referenced the
// bare upstream names. The cron agent got a minimal system prompt that
// didn't explain MCP naming, looked for literal `search_papers`, didn't
// find it, and gave up. The fix: nudge the cron agent to search by
// suffix when the task prompt uses bare upstream names.
func TestBuildSystemPrompt_IncludesMCPNamingHint(t *testing.T) {
	ce := newTestCronExecutor(
		&mockLLM{responses: []*claude.Response{{TextContent: "ok"}}},
		&mockCronFactProvider{},
	)

	prompt := ce.buildSystemPrompt(1, time.Now())
	if !strings.Contains(prompt, "mcp__") {
		t.Errorf("prompt must show the MCP naming format (mcp__<server>__<tool>), got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "namespaced") {
		t.Errorf("prompt must explain namespacing so the agent can map bare tool names, got:\n%s", prompt)
	}
	// The hint must explicitly warn against declaring tools missing
	// without searching the inventory — that's the exact failure mode
	// the Apr 17 incident exhibited.
	if !strings.Contains(prompt, "Do NOT declare") && !strings.Contains(prompt, "do not declare") {
		t.Errorf("prompt must warn against declaring tools missing without searching first, got:\n%s", prompt)
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

// TestCronExecutor_SpawnParamsIncludeMCPConfig is the regression guard for
// the Apr 15 incident where the cron-fired paper digest reported that
// `search_papers`/`search_arxiv`/`search_semantic` were "not registered"
// and fell back to WebSearch/WebFetch. Root cause: CronExecutor.executeWithCLI
// built SpawnParams without an MCPConfig, so the one-shot CLI subprocess
// spawned with zero MCP servers — losing access to every runtime MCP
// extension the interactive session can use.
//
// This test asserts buildSpawnParams populates MCPConfig with at least the
// curlycatclaw-skills server, which is what proxies every runtime extension
// through to the CLI subprocess.
func TestCronExecutor_SpawnParamsIncludeMCPConfig(t *testing.T) {
	ce := newTestCronExecutor(&mockLLM{}, &mockCronFactProvider{})

	// Pass a non-empty effort so the assertion below can verify that
	// per-reminder effort flows through buildSpawnParams to SpawnParams.Effort.
	// A regression that drops effort at the CronExecutor layer would be
	// silent today — fireCronTask still reads r.Effort and passes it into
	// CronRunner.Execute, but if CronExecutor.buildSpawnParams failed to
	// copy it into SpawnParams, the Claude CLI would spawn at the config
	// default. This guard lives next to the MCPConfig guard because both
	// are "field silently dropped between the reminder row and the CLI
	// spawn" class bugs (same class as the Apr 15 MCPConfig regression).
	params := ce.buildSpawnParams(42, 100, "hi", "", "xhigh", time.Now())

	if params.MCPConfig == "" {
		t.Fatal("cron SpawnParams.MCPConfig is empty; cron CLI subprocess will spawn with zero MCP servers, blocking access to every runtime extension (paper-search-mcp, scrapling, etc.). This is the v0.36.7 regression.")
	}
	if params.Effort != "xhigh" {
		t.Errorf("cron SpawnParams.Effort = %q, want %q; per-reminder effort override must flow through to the CLI spawn (--effort flag) or the reminder's stored effort is silently ignored", params.Effort, "xhigh")
	}
	if !strings.Contains(params.MCPConfig, "curlycatclaw-skills") {
		t.Errorf("cron SpawnParams.MCPConfig must register the curlycatclaw-skills proxy so runtime extensions reach the CLI subprocess; got:\n%s", params.MCPConfig)
	}

	// User/chat scoping must reach the MCP subprocess via the JSON env map
	// so the subprocess queries the right conversation.
	var parsed struct {
		MCPServers map[string]struct {
			Env map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(params.MCPConfig), &parsed); err != nil {
		t.Fatalf("MCPConfig is not valid JSON: %v\n%s", err, params.MCPConfig)
	}
	skills, ok := parsed.MCPServers["curlycatclaw-skills"]
	if !ok {
		t.Fatalf("MCPConfig missing curlycatclaw-skills server entry: %s", params.MCPConfig)
	}
	if skills.Env["CURLYCATCLAW_USER_ID"] != "42" {
		t.Errorf("MCPConfig env[CURLYCATCLAW_USER_ID] = %q, want %q", skills.Env["CURLYCATCLAW_USER_ID"], "42")
	}
	if skills.Env["CURLYCATCLAW_CHAT_ID"] != "100" {
		t.Errorf("MCPConfig env[CURLYCATCLAW_CHAT_ID] = %q, want %q", skills.Env["CURLYCATCLAW_CHAT_ID"], "100")
	}
	if skills.Env["CURLYCATCLAW_CONFIG"] != "/tmp/test-config.toml" {
		t.Errorf("MCPConfig env[CURLYCATCLAW_CONFIG] = %q, want %q (cron's configPath must propagate so the MCP subprocess loads the same config as interactive)", skills.Env["CURLYCATCLAW_CONFIG"], "/tmp/test-config.toml")
	}
}
