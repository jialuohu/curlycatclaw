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
	e.dim = 3
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

func TestFNVEmbedder_BatchEmbed(t *testing.T) {
	e := FNVEmbedder{}
	ctx := context.Background()

	texts := []string{"hello world", "foo bar", "test embedding"}
	vecs, err := e.BatchEmbed(ctx, texts)
	if err != nil {
		t.Fatalf("BatchEmbed: %v", err)
	}
	if len(vecs) != 3 {
		t.Fatalf("expected 3 vectors, got %d", len(vecs))
	}

	// Each vector should match individual Embed results.
	for i, text := range texts {
		single, err := e.Embed(ctx, text)
		if err != nil {
			t.Fatalf("Embed(%q): %v", text, err)
		}
		if len(vecs[i]) != len(single) {
			t.Fatalf("vector %d: length mismatch: %d vs %d", i, len(vecs[i]), len(single))
		}
		for j := range single {
			if vecs[i][j] != single[j] {
				t.Fatalf("vector %d dim %d: %f vs %f", i, j, vecs[i][j], single[j])
			}
		}
	}
}

func TestFNVEmbedder_BatchEmbed_Empty(t *testing.T) {
	e := FNVEmbedder{}
	vecs, err := e.BatchEmbed(context.Background(), nil)
	if err != nil {
		t.Fatalf("BatchEmbed(nil): %v", err)
	}
	if len(vecs) != 0 {
		t.Fatalf("expected 0 vectors for nil input, got %d", len(vecs))
	}
}

func TestFNVEmbedder_BatchEmbed_ContextCancel(t *testing.T) {
	e := FNVEmbedder{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := e.BatchEmbed(ctx, []string{"hello"})
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

func TestOllamaEmbedder_BatchEmbed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		inputs, ok := req["input"].([]any)
		if !ok {
			t.Errorf("expected input to be an array, got %T", req["input"])
			http.Error(w, "bad request", 400)
			return
		}
		// Return one embedding per input.
		embeddings := make([][]float64, len(inputs))
		for i := range inputs {
			embeddings[i] = []float64{float64(i) * 0.1, float64(i) * 0.2}
		}
		json.NewEncoder(w).Encode(map[string]any{
			"embeddings": embeddings,
		})
	}))
	defer server.Close()

	e := NewOllamaEmbedder(server.URL, "nomic-embed-text", 2)
	vecs, err := e.BatchEmbed(context.Background(), []string{"hello", "world"})
	if err != nil {
		t.Fatalf("BatchEmbed: %v", err)
	}
	if len(vecs) != 2 {
		t.Fatalf("expected 2 vectors, got %d", len(vecs))
	}
	if vecs[1][0] != 0.1 || vecs[1][1] != 0.2 {
		t.Fatalf("unexpected values for vecs[1]: %v", vecs[1])
	}
}

func TestVoyageEmbedder_BatchEmbed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		inputs, ok := req["input"].([]any)
		if !ok {
			t.Errorf("expected input to be an array")
			http.Error(w, "bad request", 400)
			return
		}
		data := make([]map[string]any, len(inputs))
		for i := range inputs {
			data[i] = map[string]any{
				"embedding": []float64{float64(i) * 0.3, float64(i) * 0.4},
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer server.Close()

	e := newTestVoyageEmbedder(server.URL, "test-key")
	vecs, err := e.BatchEmbed(context.Background(), []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("BatchEmbed: %v", err)
	}
	if len(vecs) != 3 {
		t.Fatalf("expected 3 vectors, got %d", len(vecs))
	}
	// vecs[2] should be [0.6, 0.8]
	if vecs[2][0] != 0.6 || vecs[2][1] != 0.8 {
		t.Fatalf("unexpected values for vecs[2]: %v", vecs[2])
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

func (e *testVoyageEmbedder) BatchEmbed(ctx context.Context, texts []string) ([][]float32, error) {
	return e.batchEmbedWithURL(ctx, texts, e.baseURL+"/v1/embeddings")
}

func (e *testVoyageEmbedder) batchEmbedWithURL(ctx context.Context, texts []string, url string) ([][]float32, error) {
	body, _ := json.Marshal(map[string]any{
		"model":      e.model,
		"input":      texts,
		"input_type": "document",
	})

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
