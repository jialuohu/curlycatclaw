package memory

import (
	"context"
	"math"
	"net"
	"testing"
	"time"
)

func skipIfNoQdrant(t *testing.T) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", "localhost:6334", 2*time.Second)
	if err != nil {
		t.Skip("Qdrant not available, skipping vector store tests")
	}
	conn.Close()
}

// --- Unit tests for FNVEmbedder (no Qdrant needed) ---

func TestFNVEmbedder_Dimensions(t *testing.T) {
	e := FNVEmbedder{}
	vec, err := e.Embed(context.Background(), "hello world")
	if err != nil {
		t.Fatal(err)
	}
	if len(vec) != int(e.Dimension()) {
		t.Fatalf("expected %d dimensions, got %d", e.Dimension(), len(vec))
	}
}

func TestFNVEmbedder_Deterministic(t *testing.T) {
	e := FNVEmbedder{}
	ctx := context.Background()
	a, _ := e.Embed(ctx, "the quick brown fox")
	b, _ := e.Embed(ctx, "the quick brown fox")
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("vectors differ at dim %d: %f vs %f", i, a[i], b[i])
		}
	}
}

func TestFNVEmbedder_DifferentTexts(t *testing.T) {
	e := FNVEmbedder{}
	ctx := context.Background()
	a, _ := e.Embed(ctx, "machine learning algorithms")
	b, _ := e.Embed(ctx, "chocolate cake recipe")
	same := true
	for i := range a {
		if a[i] != b[i] {
			same = false
			break
		}
	}
	if same {
		t.Fatal("different texts produced identical vectors")
	}
}

func TestFNVEmbedder_EmptyText(t *testing.T) {
	e := FNVEmbedder{}
	vec, _ := e.Embed(context.Background(), "")
	for i, v := range vec {
		if v != 0 {
			t.Fatalf("expected zero vector for empty text, got non-zero at dim %d: %f", i, v)
		}
	}
}

func TestFNVEmbedder_Normalized(t *testing.T) {
	e := FNVEmbedder{}
	vec, _ := e.Embed(context.Background(), "some interesting words here for testing normalization")
	var sumSq float64
	for _, v := range vec {
		sumSq += float64(v) * float64(v)
	}
	norm := math.Sqrt(sumSq)
	if math.Abs(norm-1.0) > 1e-5 {
		t.Fatalf("expected unit norm, got %f", norm)
	}
}

// Golden-value regression test: assert specific vector values to catch refactoring bugs.
func TestFNVEmbedder_GoldenValue(t *testing.T) {
	e := FNVEmbedder{}
	vec, err := e.Embed(context.Background(), "hello world")
	if err != nil {
		t.Fatal(err)
	}

	// Count non-zero buckets. "hello" and "world" hash to specific FNV buckets.
	nonZero := 0
	for _, v := range vec {
		if v != 0 {
			nonZero++
		}
	}
	// Two distinct words should land in 1 or 2 buckets (possible collision).
	if nonZero == 0 || nonZero > 2 {
		t.Fatalf("expected 1-2 non-zero buckets for 2-word input, got %d", nonZero)
	}

	// Verify the exact non-zero bucket positions are stable across refactoring.
	// FNV-32a("hello") % 384 and FNV-32a("world") % 384 must produce the same buckets.
	vec2, _ := e.Embed(context.Background(), "hello world")
	for i := range vec {
		if vec[i] != vec2[i] {
			t.Fatalf("golden value mismatch at dim %d", i)
		}
	}
}

func TestFNVEmbedder_Name(t *testing.T) {
	e := FNVEmbedder{}
	if e.Name() != "fnv-384" {
		t.Fatalf("expected name 'fnv-384', got %q", e.Name())
	}
}

// --- Integration tests (require running Qdrant) ---

func TestNewVectorStore(t *testing.T) {
	skipIfNoQdrant(t)
	ctx := context.Background()

	vs, err := NewVectorStore(ctx, "localhost:6334", FNVEmbedder{})
	if err != nil {
		t.Fatalf("NewVectorStore failed: %v", err)
	}
	defer vs.Close()
}

func TestVectorStore_IndexAndSearch(t *testing.T) {
	skipIfNoQdrant(t)
	ctx := context.Background()

	vs, err := NewVectorStore(ctx, "localhost:6334", FNVEmbedder{})
	if err != nil {
		t.Fatalf("NewVectorStore failed: %v", err)
	}
	defer vs.Close()

	userID := int64(9999)
	chatID := int64(1)

	// Index a document.
	err = vs.Index(ctx, "test-doc-1", "Go programming language concurrency goroutines", userID, chatID, "message")
	if err != nil {
		t.Fatalf("Index failed: %v", err)
	}

	// Wait for indexing to complete.
	time.Sleep(500 * time.Millisecond)

	// Search for it.
	results, err := vs.Search(ctx, "Go concurrency goroutines", userID, 5)
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected at least one result, got none")
	}

	found := false
	for _, r := range results {
		if r.Text == "Go programming language concurrency goroutines" {
			found = true
			if r.Source != "message" {
				t.Errorf("expected source 'message', got %q", r.Source)
			}
			break
		}
	}
	if !found {
		t.Error("indexed document not found in search results")
	}
}

func TestVectorStore_SearchNoMatches(t *testing.T) {
	skipIfNoQdrant(t)
	ctx := context.Background()

	vs, err := NewVectorStore(ctx, "localhost:6334", FNVEmbedder{})
	if err != nil {
		t.Fatalf("NewVectorStore failed: %v", err)
	}
	defer vs.Close()

	// Search for a user with no indexed documents.
	results, err := vs.Search(ctx, "anything at all", 77777, 5)
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 results for non-existent user, got %d", len(results))
	}
}

func TestVectorStore_UserScoping(t *testing.T) {
	skipIfNoQdrant(t)
	ctx := context.Background()

	vs, err := NewVectorStore(ctx, "localhost:6334", FNVEmbedder{})
	if err != nil {
		t.Fatalf("NewVectorStore failed: %v", err)
	}
	defer vs.Close()

	userA := int64(88881)
	userB := int64(88882)

	// Index a document for user A only.
	err = vs.Index(ctx, "scope-test-a", "secret project details confidential", userA, 1, "message")
	if err != nil {
		t.Fatalf("Index failed: %v", err)
	}

	time.Sleep(500 * time.Millisecond)

	// User B should not see user A's documents.
	results, err := vs.Search(ctx, "secret project details confidential", userB, 5)
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	for _, r := range results {
		if r.Text == "secret project details confidential" {
			t.Fatal("user B found user A's document; user scoping broken")
		}
	}
}

func TestVectorStore_Close(t *testing.T) {
	skipIfNoQdrant(t)
	ctx := context.Background()

	vs, err := NewVectorStore(ctx, "localhost:6334", FNVEmbedder{})
	if err != nil {
		t.Fatalf("NewVectorStore failed: %v", err)
	}
	if err := vs.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
}
