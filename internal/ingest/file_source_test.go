package ingest

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFileSource_Discover(t *testing.T) {
	// Create temp vault.
	dir := t.TempDir()
	writeFile(t, dir, "note1.md", "# Note 1\nSome content here.")
	writeFile(t, dir, "note2.txt", "Not markdown")
	writeFile(t, dir, "subdir/note3.md", "# Sub Note")

	src := NewFileSource(FileSourceConfig{
		Name:    "test-vault",
		RootDir: dir,
	})

	items, _, err := src.Discover(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}

	// Should find .md files only (default pattern).
	mdCount := 0
	for _, item := range items {
		if filepath.Ext(item.ID) == ".md" {
			mdCount++
		}
	}
	if mdCount != 2 {
		t.Errorf("expected 2 .md files, got %d (total items: %d)", mdCount, len(items))
	}
}

func TestFileSource_Discover_WithCursor(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "old.md", "Old content")

	// Set old file's mtime to the past.
	oldPath := filepath.Join(dir, "old.md")
	past := time.Now().Add(-2 * time.Hour)
	os.Chtimes(oldPath, past, past)

	// Cursor = 1 hour ago (after old.md's mtime).
	cursor := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	cursorJSON := []byte(`"` + cursor + `"`)

	// Write a new file (mtime = now, after cursor).
	writeFile(t, dir, "new.md", "New content")

	src := NewFileSource(FileSourceConfig{
		Name:    "test-vault",
		RootDir: dir,
	})

	items, _, err := src.Discover(context.Background(), cursorJSON)
	if err != nil {
		t.Fatal(err)
	}

	if len(items) != 1 {
		t.Fatalf("expected 1 item (only new.md), got %d", len(items))
	}
	if items[0].ID != "new.md" {
		t.Errorf("expected new.md, got %s", items[0].ID)
	}
}

func TestFileSource_Read_WithFrontMatter(t *testing.T) {
	dir := t.TempDir()
	content := "---\ntitle: My Note\ntype: decision\ntags: [project-x, planning]\n---\n\nActual content here."
	writeFile(t, dir, "note.md", content)

	src := NewFileSource(FileSourceConfig{
		Name:    "test-vault",
		RootDir: dir,
	})

	c, err := src.Read(context.Background(), "note.md")
	if err != nil {
		t.Fatal(err)
	}

	if c.Title != "My Note" {
		t.Errorf("expected title 'My Note', got %q", c.Title)
	}
	if c.Metadata["type"] != "decision" {
		t.Errorf("expected type 'decision', got %q", c.Metadata["type"])
	}
	if c.Metadata["has_front_matter"] != "true" {
		t.Error("expected has_front_matter=true")
	}
	if c.Metadata["tags"] != "project-x, planning" {
		t.Errorf("unexpected tags: %q", c.Metadata["tags"])
	}
	if c.ContentFingerprint == "" {
		t.Error("expected non-empty content fingerprint")
	}
}

func TestFileSource_Read_NoFrontMatter(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "plain.md", "# Just a heading\n\nSome plain markdown.")

	src := NewFileSource(FileSourceConfig{
		Name:    "test-vault",
		RootDir: dir,
	})

	c, err := src.Read(context.Background(), "plain.md")
	if err != nil {
		t.Fatal(err)
	}

	if c.Metadata["has_front_matter"] == "true" {
		t.Error("should not have front matter")
	}
	// Title falls back to filename.
	if c.Title != "plain" {
		t.Errorf("expected title 'plain', got %q", c.Title)
	}
}

func TestFileSource_Prefilter_IncludeExclude(t *testing.T) {
	src := NewFileSource(FileSourceConfig{
		Name:         "vault",
		RootDir:      "/tmp",
		IncludePaths: []string{"daily/", "projects/"},
		ExcludePaths: []string{".obsidian/", ".trash/"},
	})

	tests := []struct {
		path string
		want bool
	}{
		{"daily/2025-04-01.md", true},
		{"projects/alpha/notes.md", true},
		{"random/note.md", false},          // not in include paths
		{".obsidian/plugins/foo.json", false}, // in exclude paths
		{".trash/old.md", false},
	}

	for _, tt := range tests {
		item := ItemRef{ID: tt.path, Metadata: map[string]string{"path": tt.path}}
		got := src.Prefilter(item)
		if got != tt.want {
			t.Errorf("Prefilter(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestFileSource_Prefilter_NoFilters(t *testing.T) {
	src := NewFileSource(FileSourceConfig{
		Name:    "vault",
		RootDir: "/tmp",
	})

	item := ItemRef{ID: "anything.md", Metadata: map[string]string{"path": "anything.md"}}
	if !src.Prefilter(item) {
		t.Error("expected pass with no filters configured")
	}
}

func TestFileSource_SymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	writeFile(t, outside, "secret.md", "secret data")

	// Create symlink escaping the vault.
	os.Symlink(outside, filepath.Join(dir, "escape"))

	src := NewFileSource(FileSourceConfig{
		Name:    "vault",
		RootDir: dir,
	})

	// Discover should skip the symlinked directory.
	items, _, err := src.Discover(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}

	for _, item := range items {
		if item.ID == "escape/secret.md" {
			t.Error("symlink escape: discovered file outside vault boundary")
		}
	}

	// Read should also reject.
	_, err = src.Read(context.Background(), "escape/secret.md")
	if err == nil {
		t.Error("expected error reading file via symlink escape")
	}
}

func TestParseFrontMatter(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		hasFM   bool
		wantKey string
		wantVal string
	}{
		{
			name:    "standard front matter",
			input:   "---\ntitle: Hello\n---\nBody",
			hasFM:   true,
			wantKey: "title",
			wantVal: "Hello",
		},
		{
			name:  "no front matter",
			input: "# Just a heading\nBody text",
			hasFM: false,
		},
		{
			name:    "quoted values",
			input:   "---\ntitle: \"Quoted Title\"\n---\nBody",
			hasFM:   true,
			wantKey: "title",
			wantVal: "Quoted Title",
		},
		{
			name:    "yaml array",
			input:   "---\ntags: [a, b, c]\n---\nBody",
			hasFM:   true,
			wantKey: "tags",
			wantVal: "a, b, c",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fm, _, hasFM := parseFrontMatter(tt.input)
			if hasFM != tt.hasFM {
				t.Fatalf("hasFM = %v, want %v", hasFM, tt.hasFM)
			}
			if hasFM && fm[tt.wantKey] != tt.wantVal {
				t.Errorf("fm[%q] = %q, want %q", tt.wantKey, fm[tt.wantKey], tt.wantVal)
			}
		})
	}
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
