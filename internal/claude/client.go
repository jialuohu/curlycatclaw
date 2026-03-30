// Package claude provides a streaming Claude API client wrapping the official
// Anthropic Go SDK. It handles a single request-response cycle (which may
// stream), returning accumulated text and any tool_use blocks for the caller
// to act on.
package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// defaultMaxTokens is used when the caller does not specify a limit.
const defaultMaxTokens = 8192

// ToolCall represents a single tool invocation requested by the model.
type ToolCall struct {
	ID    string          // tool_use block ID (needed for tool_result)
	Name  string          // tool name
	Input json.RawMessage // raw JSON arguments
}

// Response holds the result of one streaming request-response cycle.
type Response struct {
	// TextContent is the concatenated text from all text blocks.
	TextContent string
	// ToolCalls contains any tool_use blocks the model returned.
	ToolCalls []ToolCall
	// StopReason is the reason the model stopped generating.
	StopReason string
}

// SendParams configures a single SendStreaming call.
type SendParams struct {
	Messages     []anthropic.MessageParam
	SystemPrompt string
	Tools        []anthropic.ToolUnionParam
	MaxTokens    int64

	// OnPartialText is called with each text delta as it arrives from the
	// stream. This lets the caller push partial text to the user (e.g.
	// streaming to Telegram) before the full response is assembled.
	// It may be nil.
	OnPartialText func(delta string)
}

// Client wraps the Anthropic SDK client and exposes a streaming-first API.
type Client struct {
	sdk   anthropic.Client
	model string
	opts  []option.RequestOption
}

// NewClient creates a new Claude client. The authOpt should be
// option.WithAPIKey; model is the model identifier
// (e.g. "claude-sonnet-4-6-20250514"). Extra SDK options can be supplied
// for testing (e.g. option.WithBaseURL).
func NewClient(authOpt option.RequestOption, model string, extraOpts ...option.RequestOption) *Client {
	opts := []option.RequestOption{authOpt}
	opts = append(opts, extraOpts...)

	return &Client{
		sdk:   anthropic.NewClient(opts...),
		model: model,
		opts:  opts,
	}
}

// RateLimitError wraps an API error that has HTTP 429 status, so callers can
// detect it and back off.
type RateLimitError struct {
	Err *anthropic.Error
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("claude: rate limited (429): %s", e.Err.Error())
}

func (e *RateLimitError) Unwrap() error {
	return e.Err
}

// SendStreaming performs one streaming request-response cycle against the
// Claude API. It accumulates the full response and invokes OnPartialText for
// each text delta along the way. It does NOT loop on tool_use — the caller
// (session actor) is responsible for feeding tool results and calling again.
func (c *Client) SendStreaming(ctx context.Context, params SendParams) (*Response, error) {
	maxTokens := params.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}

	reqParams := anthropic.MessageNewParams{
		Model:     c.model,
		MaxTokens: maxTokens,
		Messages:  params.Messages,
	}

	if params.SystemPrompt != "" {
		reqParams.System = []anthropic.TextBlockParam{
			{Text: params.SystemPrompt},
		}
	}

	if len(params.Tools) > 0 {
		reqParams.Tools = params.Tools
	}

	stream := c.sdk.Messages.NewStreaming(ctx, reqParams)
	defer stream.Close()

	var msg anthropic.Message

	for stream.Next() {
		event := stream.Current()
		if err := msg.Accumulate(event); err != nil {
			return nil, fmt.Errorf("claude: accumulate stream event: %w", err)
		}

		// Fire the partial-text callback for text deltas.
		if params.OnPartialText != nil && event.Type == "content_block_delta" {
			if event.Delta.Type == "text_delta" && event.Delta.Text != "" {
				params.OnPartialText(event.Delta.Text)
			}
		}
	}

	if err := stream.Err(); err != nil {
		return nil, wrapAPIError(err)
	}

	return buildResponse(&msg), nil
}

// Send performs a single non-streaming request-response cycle against the
// Claude API. Used for short tasks like summarization where streaming is
// unnecessary. It does NOT loop on tool_use.
func (c *Client) Send(ctx context.Context, params SendParams) (*Response, error) {
	maxTokens := params.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}

	reqParams := anthropic.MessageNewParams{
		Model:     c.model,
		MaxTokens: maxTokens,
		Messages:  params.Messages,
	}

	if params.SystemPrompt != "" {
		reqParams.System = []anthropic.TextBlockParam{
			{Text: params.SystemPrompt},
		}
	}

	if len(params.Tools) > 0 {
		reqParams.Tools = params.Tools
	}

	msg, err := c.sdk.Messages.New(ctx, reqParams)
	if err != nil {
		return nil, wrapAPIError(err)
	}

	return buildResponse(msg), nil
}

// buildResponse converts the accumulated SDK Message into our Response type.
func buildResponse(msg *anthropic.Message) *Response {
	resp := &Response{
		StopReason: string(msg.StopReason),
	}

	for _, block := range msg.Content {
		switch block.Type {
		case "text":
			if resp.TextContent != "" {
				resp.TextContent += "\n"
			}
			resp.TextContent += block.Text
		case "tool_use":
			resp.ToolCalls = append(resp.ToolCalls, ToolCall{
				ID:    block.ID,
				Name:  block.Name,
				Input: block.Input,
			})
		}
	}

	return resp
}

// wrapAPIError inspects an error from the SDK and wraps rate-limit (429)
// errors in RateLimitError so callers can detect and back off. All other API
// errors are wrapped with context.
func wrapAPIError(err error) error {
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		if apiErr.StatusCode == 429 {
			return &RateLimitError{Err: apiErr}
		}
		return fmt.Errorf("claude: api error (status %d): %w", apiErr.StatusCode, err)
	}
	return fmt.Errorf("claude: %w", err)
}
