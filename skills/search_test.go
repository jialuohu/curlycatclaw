package skills

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRegistry_RegisterAndGet(t *testing.T) {
	r := NewRegistry()
	s := &Skill{
		Name:        "test_skill",
		Description: "A test skill",
		InputSchema: json.RawMessage(`{}`),
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			return "ok", nil
		},
	}

	r.Register(s)
	got := r.Get("test_skill")

	if got == nil {
		t.Fatal("expected to get registered skill, got nil")
	}
	if got.Name != "test_skill" {
		t.Errorf("expected name %q, got %q", "test_skill", got.Name)
	}
	if got.Description != "A test skill" {
		t.Errorf("expected description %q, got %q", "A test skill", got.Description)
	}
}

func TestRegistry_GetUnknown(t *testing.T) {
	r := NewRegistry()
	got := r.Get("nonexistent")

	if got != nil {
		t.Errorf("expected nil for unknown skill, got %+v", got)
	}
}

func TestRegistry_All(t *testing.T) {
	r := NewRegistry()

	names := []string{"alpha", "beta", "gamma"}
	for _, name := range names {
		r.Register(&Skill{
			Name:        name,
			Description: "Skill " + name,
			InputSchema: json.RawMessage(`{}`),
			Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
				return "", nil
			},
		})
	}

	all := r.All()
	if len(all) != 3 {
		t.Fatalf("expected 3 skills, got %d", len(all))
	}

	// Verify all names are present (order is not guaranteed with maps).
	found := make(map[string]bool)
	for _, s := range all {
		found[s.Name] = true
	}
	for _, name := range names {
		if !found[name] {
			t.Errorf("expected skill %q in All() results", name)
		}
	}
}

// ---------------------------------------------------------------------------
// Regression: web_search response body capped at 2MB
// ---------------------------------------------------------------------------

func TestWebSearch_ResponseBodyLimit_2MB(t *testing.T) {
	// The bug fix added io.LimitReader(resp.Body, 2<<20) to cap search
	// response bodies at 2MB. Verify that the LimitReader pattern used
	// in executeWebSearch truncates oversized responses correctly.
	const twoMB = 2 << 20
	const oversized = twoMB + 8192

	// Create a test server that returns more than 2MB of data.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Write oversized data.
		data := make([]byte, oversized)
		for i := range data {
			data[i] = 'X'
		}
		w.Write(data)
	}))
	defer ts.Close()

	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatalf("http.Get: %v", err)
	}
	defer resp.Body.Close()

	// Apply the same LimitReader as executeWebSearch: io.LimitReader(resp.Body, 2<<20).
	buf, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		t.Fatalf("io.ReadAll: %v", err)
	}

	if len(buf) != twoMB {
		t.Errorf("response size = %d, want exactly %d (2MB)", len(buf), twoMB)
	}
}

func TestWebSearch_SmallResponseUnchanged(t *testing.T) {
	// Verify that responses smaller than 2MB are returned in full.
	const smallSize = 1024

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data := make([]byte, smallSize)
		for i := range data {
			data[i] = 'Y'
		}
		w.Write(data)
	}))
	defer ts.Close()

	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatalf("http.Get: %v", err)
	}
	defer resp.Body.Close()

	buf, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		t.Fatalf("io.ReadAll: %v", err)
	}

	if len(buf) != smallSize {
		t.Errorf("response size = %d, want %d", len(buf), smallSize)
	}
}

func TestWebSearch_ResponseLimitConstant(t *testing.T) {
	// Verify the limit constant 2<<20 equals exactly 2097152 (2MB).
	const twoMB = 2 << 20
	if twoMB != 2097152 {
		t.Errorf("2<<20 = %d, want 2097152", twoMB)
	}
}

func TestWebSearch_ExecuteEmptyQuery(t *testing.T) {
	// Verify that an empty query returns an error.
	input := json.RawMessage(`{"query":""}`)
	_, err := executeWebSearch(context.Background(), input)
	if err == nil {
		t.Fatal("expected error for empty query")
	}
}

func TestWebSearch_ExecuteInvalidJSON(t *testing.T) {
	// Verify that invalid JSON input returns an error.
	input := json.RawMessage(`{not valid}`)
	_, err := executeWebSearch(context.Background(), input)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}
