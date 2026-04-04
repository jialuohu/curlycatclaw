package voice

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTranscribe_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("method = %s, want POST", r.Method)
		}
		auth := r.Header.Get("Authorization")
		if auth != "Bearer test-key" {
			t.Errorf("auth = %q, want %q", auth, "Bearer test-key")
		}
		ct := r.Header.Get("Content-Type")
		if ct == "" {
			t.Error("Content-Type header is empty")
		}

		if err := r.ParseMultipartForm(10 << 20); err != nil {
			t.Fatalf("parse multipart: %v", err)
		}
		model := r.FormValue("model")
		if model != "whisper-1" {
			t.Errorf("model = %q, want %q", model, "whisper-1")
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			t.Fatalf("form file: %v", err)
		}
		defer file.Close()
		if header.Filename != "audio.ogg" {
			t.Errorf("filename = %q, want %q", header.Filename, "audio.ogg")
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"text": "hello world"}) //nolint:errcheck
	}))
	defer srv.Close()

	tr := NewOpenAITranscriber("test-key", "whisper-1")
	tr.client = &http.Client{Timeout: 5 * time.Second}
	// Override the endpoint by using a custom transport.
	origTransport := tr.client.Transport
	tr.client.Transport = rewriteTransport{base: origTransport, targetURL: srv.URL}

	text, err := tr.Transcribe(context.Background(), []byte("fake-ogg-data"), "ogg")
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if text != "hello world" {
		t.Errorf("text = %q, want %q", text, "hello world")
	}
}

func TestTranscribe_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"invalid api key"}}`)) //nolint:errcheck
	}))
	defer srv.Close()

	tr := NewOpenAITranscriber("bad-key", "whisper-1")
	tr.client = &http.Client{Timeout: 5 * time.Second}
	tr.client.Transport = rewriteTransport{base: tr.client.Transport, targetURL: srv.URL}

	_, err := tr.Transcribe(context.Background(), []byte("audio"), "ogg")
	if err == nil {
		t.Fatal("expected error for 401 response")
	}
	if got := err.Error(); !contains(got, "openai error 401") {
		t.Errorf("error = %q, want to contain %q", got, "openai error 401")
	}
}

func TestTranscribe_EmptyText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"text": ""}) //nolint:errcheck
	}))
	defer srv.Close()

	tr := NewOpenAITranscriber("key", "whisper-1")
	tr.client = &http.Client{Timeout: 5 * time.Second}
	tr.client.Transport = rewriteTransport{base: tr.client.Transport, targetURL: srv.URL}

	text, err := tr.Transcribe(context.Background(), []byte("silence"), "ogg")
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if text != "" {
		t.Errorf("text = %q, want empty", text)
	}
}

func TestTranscribe_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"text": "late"}) //nolint:errcheck
	}))
	defer srv.Close()

	tr := NewOpenAITranscriber("key", "whisper-1")
	tr.client = &http.Client{Timeout: 50 * time.Millisecond}
	tr.client.Transport = rewriteTransport{base: tr.client.Transport, targetURL: srv.URL}

	_, err := tr.Transcribe(context.Background(), []byte("audio"), "ogg")
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

// rewriteTransport rewrites requests to point at a test server URL
// while preserving the request path and body.
type rewriteTransport struct {
	base      http.RoundTripper
	targetURL string
}

func (rt rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = "http"
	req.URL.Host = rt.targetURL[len("http://"):]
	base := rt.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
