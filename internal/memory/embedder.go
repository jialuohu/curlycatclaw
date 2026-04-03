package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

// Embedder converts text into vector embeddings.
type Embedder interface {
	// Embed returns a vector embedding for the given text.
	Embed(ctx context.Context, text string) ([]float32, error)
	// BatchEmbed returns vector embeddings for multiple texts.
	BatchEmbed(ctx context.Context, texts []string) ([][]float32, error)
	// Dimension returns the output vector dimension.
	Dimension() uint64
	// Name returns a stable identifier for this embedder configuration.
	Name() string
}

// --- FNVEmbedder ---

// FNVEmbedder produces bag-of-words vectors using FNV hashing.
// This is the offline fallback that requires no external services.
type FNVEmbedder struct{}

const fnvDim = 384

func (FNVEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	vec := make([]float32, fnvDim)
	words := strings.Fields(strings.ToLower(text))
	if len(words) == 0 {
		return vec, nil
	}

	for _, w := range words {
		h := fnv.New32a()
		h.Write([]byte(w))
		bucket := h.Sum32() % fnvDim
		vec[bucket] += 1.0
	}

	// L2 normalize.
	var norm float64
	for _, v := range vec {
		norm += float64(v) * float64(v)
	}
	if norm > 0 {
		norm = math.Sqrt(norm)
		for i := range vec {
			vec[i] = float32(float64(vec[i]) / norm)
		}
	}

	return vec, nil
}

func (e FNVEmbedder) BatchEmbed(ctx context.Context, texts []string) ([][]float32, error) {
	results := make([][]float32, len(texts))
	for i, t := range texts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		vec, err := e.Embed(ctx, t)
		if err != nil {
			return nil, fmt.Errorf("fnv: batch embed [%d]: %w", i, err)
		}
		results[i] = vec
	}
	return results, nil
}

func (FNVEmbedder) Dimension() uint64 { return fnvDim }
func (FNVEmbedder) Name() string      { return "fnv-384" }

// --- OllamaEmbedder ---

// OllamaEmbedder calls a local Ollama instance for real embeddings.
type OllamaEmbedder struct {
	baseURL string
	model   string
	dim     uint64
	client  *http.Client
}

// NewOllamaEmbedder creates an embedder that calls the Ollama API.
func NewOllamaEmbedder(baseURL, model string, dim uint64) *OllamaEmbedder {
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}
	if model == "" {
		model = "bge-m3"
	}
	if dim == 0 {
		dim = 1024
	}
	return &OllamaEmbedder{
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   model,
		dim:     dim,
		client:  &http.Client{Timeout: 120 * time.Second},
	}
}

func (e *OllamaEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	body, err := json.Marshal(map[string]any{
		"model": e.model,
		"input": text,
	})
	if err != nil {
		return nil, fmt.Errorf("ollama: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", e.baseURL+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("ollama: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("ollama: status %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Embeddings [][]float64 `json:"embeddings"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 10<<20)).Decode(&result); err != nil {
		return nil, fmt.Errorf("ollama: decode response: %w", err)
	}
	if len(result.Embeddings) == 0 || len(result.Embeddings[0]) == 0 {
		return nil, fmt.Errorf("ollama: empty embedding in response")
	}

	vec := make([]float32, len(result.Embeddings[0]))
	for i, v := range result.Embeddings[0] {
		vec[i] = float32(v)
	}
	return vec, nil
}

func (e *OllamaEmbedder) BatchEmbed(ctx context.Context, texts []string) ([][]float32, error) {
	body, err := json.Marshal(map[string]any{
		"model": e.model,
		"input": texts,
	})
	if err != nil {
		return nil, fmt.Errorf("ollama: marshal batch request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", e.baseURL+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("ollama: create batch request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama: batch request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("ollama: batch status %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Embeddings [][]float64 `json:"embeddings"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 50<<20)).Decode(&result); err != nil {
		return nil, fmt.Errorf("ollama: decode batch response: %w", err)
	}
	if len(result.Embeddings) != len(texts) {
		return nil, fmt.Errorf("ollama: expected %d embeddings, got %d", len(texts), len(result.Embeddings))
	}

	vecs := make([][]float32, len(result.Embeddings))
	for i, emb := range result.Embeddings {
		vec := make([]float32, len(emb))
		for j, v := range emb {
			vec[j] = float32(v)
		}
		vecs[i] = vec
	}
	return vecs, nil
}

func (e *OllamaEmbedder) Dimension() uint64 { return e.dim }
func (e *OllamaEmbedder) Name() string      { return fmt.Sprintf("ollama-%s-%d", e.model, e.dim) }

// --- VoyageEmbedder ---

// VoyageEmbedder calls the Voyage AI API for high-quality embeddings.
type VoyageEmbedder struct {
	apiKey string
	model  string
	dim    uint64
	client *http.Client
}

// NewVoyageEmbedder creates an embedder that calls the Voyage AI API.
func NewVoyageEmbedder(apiKey, model string, dim uint64) *VoyageEmbedder {
	if model == "" {
		model = "voyage-3-lite"
	}
	if dim == 0 {
		dim = 512
	}
	return &VoyageEmbedder{
		apiKey: apiKey,
		model:  model,
		dim:    dim,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

func (e *VoyageEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	return e.embed(ctx, text, "document")
}

func (e *VoyageEmbedder) embed(ctx context.Context, text, inputType string) ([]float32, error) {
	body, err := json.Marshal(map[string]any{
		"model":      e.model,
		"input":      []string{text},
		"input_type": inputType,
	})
	if err != nil {
		return nil, fmt.Errorf("voyage: marshal request: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		vec, err := e.doRequest(ctx, body)
		if err == nil {
			return vec, nil
		}
		lastErr = err
		// Retry only on rate limit (429).
		if !strings.Contains(err.Error(), "status 429") {
			return nil, err
		}
		// Exponential backoff: 1s, 2s, 4s.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(1<<attempt) * time.Second):
		}
	}
	return nil, lastErr
}

func (e *VoyageEmbedder) doRequest(ctx context.Context, body []byte) ([]float32, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.voyageai.com/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("voyage: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.apiKey)

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("voyage: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("voyage: status %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 10<<20)).Decode(&result); err != nil {
		return nil, fmt.Errorf("voyage: decode response: %w", err)
	}
	if len(result.Data) == 0 || len(result.Data[0].Embedding) == 0 {
		return nil, fmt.Errorf("voyage: empty embedding in response")
	}

	vec := make([]float32, len(result.Data[0].Embedding))
	for i, v := range result.Data[0].Embedding {
		vec[i] = float32(v)
	}
	return vec, nil
}

const voyageBatchSize = 128

func (e *VoyageEmbedder) BatchEmbed(ctx context.Context, texts []string) ([][]float32, error) {
	var allVecs [][]float32
	for i := 0; i < len(texts); i += voyageBatchSize {
		end := i + voyageBatchSize
		if end > len(texts) {
			end = len(texts)
		}
		batch := texts[i:end]

		vecs, err := e.batchEmbedChunk(ctx, batch)
		if err != nil {
			return nil, fmt.Errorf("voyage: batch [%d:%d]: %w", i, end, err)
		}
		allVecs = append(allVecs, vecs...)
	}
	return allVecs, nil
}

func (e *VoyageEmbedder) batchEmbedChunk(ctx context.Context, texts []string) ([][]float32, error) {
	body, err := json.Marshal(map[string]any{
		"model":      e.model,
		"input":      texts,
		"input_type": "document",
	})
	if err != nil {
		return nil, fmt.Errorf("voyage: marshal batch request: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		vecs, err := e.doBatchRequest(ctx, body, len(texts))
		if err == nil {
			return vecs, nil
		}
		lastErr = err
		if !strings.Contains(err.Error(), "status 429") {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(1<<attempt) * time.Second):
		}
	}
	return nil, lastErr
}

func (e *VoyageEmbedder) doBatchRequest(ctx context.Context, body []byte, expectedCount int) ([][]float32, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.voyageai.com/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("voyage: create batch request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.apiKey)

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("voyage: batch request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("voyage: status %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 50<<20)).Decode(&result); err != nil {
		return nil, fmt.Errorf("voyage: decode batch response: %w", err)
	}
	if len(result.Data) != expectedCount {
		return nil, fmt.Errorf("voyage: expected %d embeddings, got %d", expectedCount, len(result.Data))
	}

	vecs := make([][]float32, len(result.Data))
	for i, d := range result.Data {
		vec := make([]float32, len(d.Embedding))
		for j, v := range d.Embedding {
			vec[j] = float32(v)
		}
		vecs[i] = vec
	}
	return vecs, nil
}

func (e *VoyageEmbedder) Dimension() uint64 { return e.dim }
func (e *VoyageEmbedder) Name() string      { return fmt.Sprintf("voyage-%s-%d", e.model, e.dim) }
