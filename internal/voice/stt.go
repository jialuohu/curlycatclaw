package voice

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"time"
)

// Transcriber converts audio bytes into text.
type Transcriber interface {
	Transcribe(ctx context.Context, audio []byte, format string) (string, error)
}

// OpenAITranscriber transcribes audio using the OpenAI Whisper API.
type OpenAITranscriber struct {
	apiKey string
	model  string
	client *http.Client
}

// NewOpenAITranscriber creates a Transcriber that uses the OpenAI Whisper API.
func NewOpenAITranscriber(apiKey, model string) *OpenAITranscriber {
	return &OpenAITranscriber{
		apiKey: apiKey,
		model:  model,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

// Transcribe sends audio bytes to the OpenAI transcription endpoint and returns the text.
// The format parameter is used as the file extension (e.g. "ogg", "mp3").
func (t *OpenAITranscriber) Transcribe(ctx context.Context, audio []byte, format string) (string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	part, err := w.CreateFormFile("file", "audio."+format)
	if err != nil {
		return "", fmt.Errorf("create form file: %w", err)
	}
	if _, err := part.Write(audio); err != nil {
		return "", fmt.Errorf("write audio: %w", err)
	}
	if err := w.WriteField("model", t.model); err != nil {
		return "", fmt.Errorf("write model field: %w", err)
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("close multipart writer: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.openai.com/v1/audio/transcriptions", &buf)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+t.apiKey)
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := t.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("openai request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("openai error %d: %s", resp.StatusCode, body)
	}

	var result struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}
	return result.Text, nil
}
