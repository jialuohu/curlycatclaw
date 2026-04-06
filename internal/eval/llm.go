package eval

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/jialuohu/curlycatclaw/internal/claude"
)

// APILLMCaller implements LLMCaller using the Claude direct API.
// Read-only: no tools are passed, so the model cannot take actions.
type APILLMCaller struct {
	client *claude.Client
}

// NewAPILLMCaller creates an LLMCaller that uses the direct Claude API.
func NewAPILLMCaller(client *claude.Client) *APILLMCaller {
	return &APILLMCaller{client: client}
}

// EvalCall sends a read-only (no tools) request to Claude and returns the text response.
func (a *APILLMCaller) EvalCall(ctx context.Context, system string, messages []anthropic.MessageParam) (string, error) {
	resp, err := a.client.Send(ctx, claude.SendParams{
		SystemPrompt: system,
		Messages:     messages,
		MaxTokens:    2048,
	})
	if err != nil {
		return "", fmt.Errorf("eval api call: %w", err)
	}
	return resp.TextContent, nil
}

// CLILLMCaller implements LLMCaller using the Claude CLI subprocess.
// Read-only: spawns with no MCP config, so no tools are available.
type CLILLMCaller struct {
	cliMgr *claude.CLIManager
}

// NewCLILLMCaller creates an LLMCaller that uses the Claude CLI subprocess.
func NewCLILLMCaller(cliMgr *claude.CLIManager) *CLILLMCaller {
	return &CLILLMCaller{cliMgr: cliMgr}
}

// EvalCall spawns a one-shot CLI process without tools and returns the text response.
func (c *CLILLMCaller) EvalCall(ctx context.Context, system string, messages []anthropic.MessageParam) (string, error) {
	// Extract user message text from the first user message.
	var userText string
	for _, msg := range messages {
		if msg.Role == anthropic.MessageParamRoleUser {
			for _, block := range msg.Content {
				if block.OfText != nil {
					userText = block.OfText.Text
					break
				}
			}
			break
		}
	}
	if userText == "" {
		return "", fmt.Errorf("eval cli call: no user message text found")
	}

	// Spawn a one-shot subprocess with NO MCP config (no tools).
	proc, err := c.cliMgr.SpawnOneShot(ctx, claude.SpawnParams{
		SystemPrompt: system,
		// No MCPConfig = no tools. The subprocess is read-only.
	})
	if err != nil {
		return "", fmt.Errorf("eval cli spawn: %w", err)
	}
	defer proc.Kill()

	msgJSON, _ := json.Marshal(userText)
	var text string
	events, err := proc.Send(ctx, msgJSON, func(delta string) {
		text += delta
	}, nil)
	if err != nil {
		return "", fmt.Errorf("eval cli send: %w", err)
	}

	// If no streaming text was accumulated, check events for result text.
	if text == "" {
		for _, ev := range events {
			if re, ok := ev.(claude.ResultEvent); ok {
				text = re.Result
				break
			}
		}
	}

	return text, nil
}
