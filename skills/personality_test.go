package skills

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGetPersonality_NoFile(t *testing.T) {
	ss := InitPersonalitySkills("")
	get := ss[0]
	result, err := get.Execute(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "default personality") {
		t.Fatalf("expected default message, got %q", result)
	}
}

func TestGetPersonality_WithFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "personality.md")
	if err := os.WriteFile(f, []byte("You are a pirate."), 0o644); err != nil {
		t.Fatal(err)
	}

	ss := InitPersonalitySkills(f)
	get := ss[0]
	result, err := get.Execute(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "You are a pirate.") {
		t.Fatalf("expected personality content, got %q", result)
	}
}

func TestSetPersonality_NoFile(t *testing.T) {
	ss := InitPersonalitySkills("")
	set := ss[1]
	input, _ := json.Marshal(map[string]string{"content": "test"})
	_, err := set.Execute(context.Background(), input)
	if err == nil {
		t.Fatal("expected error when no file configured")
	}
	if !strings.Contains(err.Error(), "no personality file configured") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSetPersonality_HappyPath(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "personality.md")
	if err := os.WriteFile(f, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	ss := InitPersonalitySkills(f)
	set := ss[1]
	input, _ := json.Marshal(map[string]string{"content": "You are a space explorer."})
	result, err := set.Execute(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "Personality updated") {
		t.Fatalf("expected success message, got %q", result)
	}

	// Verify file was updated.
	data, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != "You are a space explorer." {
		t.Fatalf("file not updated: %q", string(data))
	}
}

func TestSetPersonality_EmptyContent(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "personality.md")
	if err := os.WriteFile(f, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	ss := InitPersonalitySkills(f)
	set := ss[1]
	input, _ := json.Marshal(map[string]string{"content": ""})
	_, err := set.Execute(context.Background(), input)
	if err == nil {
		t.Fatal("expected error for empty content")
	}
}

func TestSetPersonality_Roundtrip(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "personality.md")
	if err := os.WriteFile(f, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}

	ss := InitPersonalitySkills(f)
	get, set := ss[0], ss[1]

	// Set new personality.
	input, _ := json.Marshal(map[string]string{"content": "You are a helpful cat."})
	_, err := set.Execute(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}

	// Get should reflect the change.
	result, err := get.Execute(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "You are a helpful cat.") {
		t.Fatalf("get didn't reflect set: %q", result)
	}
}
