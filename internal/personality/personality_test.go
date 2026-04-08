package personality

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad(t *testing.T) {
	t.Run("happy path", func(t *testing.T) {
		f := writeTemp(t, "You are a pirate captain.")
		p, err := Load(f)
		if err != nil {
			t.Fatal(err)
		}
		if p.Content != "You are a pirate captain." {
			t.Fatalf("got content %q", p.Content)
		}
		if p.ContentHash == "" {
			t.Fatal("expected non-empty hash")
		}
		if p.FilePath != f {
			t.Fatalf("got path %q, want %q", p.FilePath, f)
		}
	})

	t.Run("trims whitespace", func(t *testing.T) {
		f := writeTemp(t, "  \n  Hello world  \n  ")
		p, err := Load(f)
		if err != nil {
			t.Fatal(err)
		}
		if p.Content != "Hello world" {
			t.Fatalf("got %q, want trimmed", p.Content)
		}
	})

	t.Run("file not found", func(t *testing.T) {
		_, err := Load("/nonexistent/personality.md")
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("empty file", func(t *testing.T) {
		f := writeTemp(t, "")
		_, err := Load(f)
		if err == nil {
			t.Fatal("expected error for empty file")
		}
		if !strings.Contains(err.Error(), "empty") {
			t.Fatalf("expected 'empty' in error, got %q", err)
		}
	})

	t.Run("whitespace only", func(t *testing.T) {
		f := writeTemp(t, "   \n\t  \n  ")
		_, err := Load(f)
		if err == nil {
			t.Fatal("expected error for whitespace-only file")
		}
		if !strings.Contains(err.Error(), "empty") {
			t.Fatalf("expected 'empty' in error, got %q", err)
		}
	})

	t.Run("invalid UTF-8", func(t *testing.T) {
		dir := t.TempDir()
		f := filepath.Join(dir, "bad.md")
		if err := os.WriteFile(f, []byte{0xff, 0xfe, 0x80}, 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := Load(f)
		if err == nil {
			t.Fatal("expected error for invalid UTF-8")
		}
		if !strings.Contains(err.Error(), "UTF-8") {
			t.Fatalf("expected 'UTF-8' in error, got %q", err)
		}
	})

	t.Run("oversized file", func(t *testing.T) {
		dir := t.TempDir()
		f := filepath.Join(dir, "big.md")
		data := make([]byte, maxFileSize+1)
		for i := range data {
			data[i] = 'A'
		}
		if err := os.WriteFile(f, data, 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := Load(f)
		if err == nil {
			t.Fatal("expected error for oversized file")
		}
		if !strings.Contains(err.Error(), "max allowed") {
			t.Fatalf("expected size error, got %q", err)
		}
	})
}

func TestDefault(t *testing.T) {
	p := Default()
	if p.Content != defaultPersonality {
		t.Fatalf("got %q, want %q", p.Content, defaultPersonality)
	}
	if p.ContentHash != "" {
		t.Fatal("default should have empty hash")
	}
	if p.FilePath != "" {
		t.Fatal("default should have empty path")
	}
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	f := filepath.Join(dir, "personality.md")
	if err := os.WriteFile(f, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}
