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

// --- Unit tests for textToVector (no Qdrant needed) ---

func TestTextToVector_Dimensions(t *testing.T) {
	vec := textToVector("hello world")
	if len(vec) != vectorDim {
		t.Fatalf("expected %d dimensions, got %d", vectorDim, len(vec))
	}
}

func TestTextToVector_Deterministic(t *testing.T) {
	a := textToVector("the quick brown fox")
	b := textToVector("the quick brown fox")
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("vectors differ at dim %d: %f vs %f", i, a[i], b[i])
		}
	}
}

func TestTextToVector_DifferentTexts(t *testing.T) {
	a := textToVector("machine learning algorithms")
	b := textToVector("chocolate cake recipe")
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

func TestTextToVector_EmptyText(t *testing.T) {
	vec := textToVector("")
	for i, v := range vec {
		if v != 0 {
			t.Fatalf("expected zero vector for empty text, got non-zero at dim %d: %f", i, v)
		}
	}
}

func TestTextToVector_Normalized(t *testing.T) {
	vec := textToVector("some interesting words here for testing normalization")
	var sumSq float64
	for _, v := range vec {
		sumSq += float64(v) * float64(v)
	}
	norm := math.Sqrt(sumSq)
	if math.Abs(norm-1.0) > 1e-5 {
		t.Fatalf("expected unit norm, got %f", norm)
	}
}

// --- Integration tests (require running Qdrant) ---

func TestNewVectorStore(t *testing.T) {
	skipIfNoQdrant(t)
	ctx := context.Background()

	vs, err := NewVectorStore(ctx, "localhost:6334")
	if err != nil {
		t.Fatalf("NewVectorStore failed: %v", err)
	}
	defer vs.Close()
}

func TestVectorStore_IndexAndSearch(t *testing.T) {
	skipIfNoQdrant(t)
	ctx := context.Background()

	vs, err := NewVectorStore(ctx, "localhost:6334")
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

	vs, err := NewVectorStore(ctx, "localhost:6334")
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

	vs, err := NewVectorStore(ctx, "localhost:6334")
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

	vs, err := NewVectorStore(ctx, "localhost:6334")
	if err != nil {
		t.Fatalf("NewVectorStore failed: %v", err)
	}
	if err := vs.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
}
