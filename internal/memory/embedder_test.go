package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestOllamaEmbedder_HappyPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" {
			t.Errorf("expected /api/embed, got %s", r.URL.Path)
		}
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if req["model"] != "nomic-embed-text" {
			t.Errorf("expected model nomic-embed-text, got %v", req["model"])
		}
		json.NewEncoder(w).Encode(map[string]any{
			"embeddings": [][]float64{{0.1, 0.2, 0.3}},
		})
	}))
	defer server.Close()

	e := NewOllamaEmbedder(server.URL, "nomic-embed-text", 3)
	vec, err := e.Embed(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	if len(vec) != 3 {
		t.Fatalf("expected 3 dims, got %d", len(vec))
	}
	if vec[0] != 0.1 || vec[1] != 0.2 || vec[2] != 0.3 {
		t.Fatalf("unexpected values: %v", vec)
	}
}

func TestOllamaEmbedder_ConnectionRefused(t *testing.T) {
	e := NewOllamaEmbedder("http://127.0.0.1:1", "nomic-embed-text", 768)
	_, err := e.Embed(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected error for connection refused")
	}
}

func TestOllamaEmbedder_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"model 'bad-model' not found"}`))
	}))
	defer server.Close()

	e := NewOllamaEmbedder(server.URL, "bad-model", 768)
	_, err := e.Embed(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected error for 404")
	}
}

func TestOllamaEmbedder_MalformedJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer server.Close()

	e := NewOllamaEmbedder(server.URL, "nomic-embed-text", 768)
	_, err := e.Embed(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

func TestOllamaEmbedder_EmptyEmbedding(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"embeddings": [][]float64{},
		})
	}))
	defer server.Close()

	e := NewOllamaEmbedder(server.URL, "nomic-embed-text", 768)
	_, err := e.Embed(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected error for empty embedding")
	}
}

func TestVoyageEmbedder_HappyPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("expected Bearer test-key, got %s", r.Header.Get("Authorization"))
		}
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if req["input_type"] != "document" {
			t.Errorf("expected input_type document, got %v", req["input_type"])
		}
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"embedding": []float64{0.4, 0.5, 0.6}},
			},
		})
	}))
	defer server.Close()

	e := newTestVoyageEmbedder(server.URL, "test-key")
	e.VoyageEmbedder.dim = 3
	vec, err := e.Embed(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	if len(vec) != 3 {
		t.Fatalf("expected 3 dims, got %d", len(vec))
	}
	if vec[0] != 0.4 || vec[1] != 0.5 || vec[2] != 0.6 {
		t.Fatalf("unexpected values: %v", vec)
	}
	if e.Dimension() != 3 {
		t.Fatalf("expected dimension 3, got %d", e.Dimension())
	}
}

func TestVoyageEmbedder_AuthFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid api key"}`))
	}))
	defer server.Close()

	e := newTestVoyageEmbedder(server.URL, "bad-key")
	_, err := e.Embed(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected error for 401")
	}
}

func TestVoyageEmbedder_RateLimit(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls <= 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":"rate limited"}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"embedding": []float64{0.1, 0.2}},
			},
		})
	}))
	defer server.Close()

	e := newTestVoyageEmbedder(server.URL, "test-key")
	vec, err := e.Embed(context.Background(), "hello")
	if err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if len(vec) != 2 {
		t.Fatalf("expected 2 dims, got %d", len(vec))
	}
	if calls != 3 {
		t.Fatalf("expected 3 calls (2 retries + 1 success), got %d", calls)
	}
}

func TestVoyageEmbedder_EmptyEmbedding(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{},
		})
	}))
	defer server.Close()

	e := newTestVoyageEmbedder(server.URL, "test-key")
	_, err := e.Embed(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected error for empty embedding")
	}
}

// newTestVoyageEmbedder creates a VoyageEmbedder that talks to a test server.
func newTestVoyageEmbedder(baseURL, apiKey string) *testVoyageEmbedder {
	return &testVoyageEmbedder{
		VoyageEmbedder: *NewVoyageEmbedder(apiKey, "voyage-3-lite", 512),
		baseURL:        baseURL,
	}
}

// testVoyageEmbedder wraps VoyageEmbedder to override the API URL for testing.
type testVoyageEmbedder struct {
	VoyageEmbedder
	baseURL string
}

func (e *testVoyageEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	return e.embedWithURL(ctx, text, "document", e.baseURL+"/v1/embeddings")
}

func (e *testVoyageEmbedder) embedWithURL(ctx context.Context, text, inputType, url string) ([]float32, error) {
	body, _ := json.Marshal(map[string]any{
		"model":      e.model,
		"input":      []string{text},
		"input_type": inputType,
	})

	var lastErr error
	for attempt := range 3 {
		req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+e.apiKey)

		resp, err := e.client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusTooManyRequests {
			lastErr = fmt.Errorf("voyage: status 429")
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(1<<attempt) * time.Millisecond):
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			return nil, fmt.Errorf("voyage: status %d: %s", resp.StatusCode, string(respBody))
		}

		var result struct {
			Data []struct {
				Embedding []float64 `json:"embedding"`
			} `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return nil, err
		}
		if len(result.Data) == 0 || len(result.Data[0].Embedding) == 0 {
			return nil, fmt.Errorf("voyage: empty embedding")
		}
		vec := make([]float32, len(result.Data[0].Embedding))
		for i, v := range result.Data[0].Embedding {
			vec[i] = float32(v)
		}
		return vec, nil
	}
	return nil, lastErr
}
