package extension

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "extensions.json")
	reg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reg.All()) != 0 {
		t.Fatal("expected empty registry")
	}
}

func TestAddAndPersistRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "extensions.json")
	reg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	ext := Extension{
		Name:    "brave-search",
		Type:    TypeMCP,
		Command: "npx",
		Args:    []string{"-y", "@anthropic/mcp-server-brave-search"},
		Env:     map[string]string{"BRAVE_API_KEY": "test-key"},
	}
	if err := reg.Add(ext); err != nil {
		t.Fatal(err)
	}

	// Verify in memory.
	got := reg.Get("brave-search")
	if got == nil {
		t.Fatal("expected to find extension")
	}
	if got.Command != "npx" {
		t.Fatalf("expected command npx, got %s", got.Command)
	}
	if got.Env["BRAVE_API_KEY"] != "test-key" {
		t.Fatal("expected env var to be preserved")
	}

	// Reload from disk.
	reg2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got2 := reg2.Get("brave-search")
	if got2 == nil {
		t.Fatal("expected extension to persist")
	}
	if got2.Command != "npx" {
		t.Fatalf("expected command npx after reload, got %s", got2.Command)
	}
}

func TestAddDuplicate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "extensions.json")
	reg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	ext := Extension{Name: "test", Type: TypeMCP, Command: "echo"}
	if err := reg.Add(ext); err != nil {
		t.Fatal(err)
	}
	if err := reg.Add(ext); err == nil {
		t.Fatal("expected duplicate error")
	}
}

func TestRemove(t *testing.T) {
	path := filepath.Join(t.TempDir(), "extensions.json")
	reg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := reg.Add(Extension{Name: "test", Type: TypeMCP, Command: "echo"}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Remove("test"); err != nil {
		t.Fatal(err)
	}
	if reg.Get("test") != nil {
		t.Fatal("expected extension to be removed")
	}

	// Verify persistence.
	reg2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if reg2.Get("test") != nil {
		t.Fatal("expected removal to persist")
	}
}

func TestRemoveNotFound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "extensions.json")
	reg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Remove("nonexistent"); err == nil {
		t.Fatal("expected not-found error")
	}
}

func TestValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "extensions.json")
	reg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		ext  Extension
	}{
		{"empty name", Extension{Type: TypeMCP, Command: "echo"}},
		{"bad name chars", Extension{Name: "a b", Type: TypeMCP, Command: "echo"}},
		{"invalid type", Extension{Name: "test", Type: "invalid", Command: "echo"}},
		{"empty command", Extension{Name: "test", Type: TypeMCP}},
		{"exec missing description", Extension{Name: "test", Type: TypeExec, Command: "echo"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := reg.Add(tc.ext); err == nil {
				t.Error("expected validation error")
			}
		})
	}
}

func TestByType(t *testing.T) {
	path := filepath.Join(t.TempDir(), "extensions.json")
	reg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := reg.Add(Extension{Name: "mcp1", Type: TypeMCP, Command: "echo"}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Add(Extension{Name: "exec1", Type: TypeExec, Command: "echo", Description: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Add(Extension{Name: "mcp2", Type: TypeMCP, Command: "echo"}); err != nil {
		t.Fatal(err)
	}

	mcps := reg.ByType(TypeMCP)
	if len(mcps) != 2 {
		t.Fatalf("expected 2 MCP extensions, got %d", len(mcps))
	}

	execs := reg.ByType(TypeExec)
	if len(execs) != 1 {
		t.Fatalf("expected 1 exec extension, got %d", len(execs))
	}
}

func TestAllSortedByAddedAt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "extensions.json")
	reg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	ext1 := Extension{Name: "second", Type: TypeMCP, Command: "echo", AddedAt: now.Add(time.Second)}
	ext2 := Extension{Name: "first", Type: TypeMCP, Command: "echo", AddedAt: now}

	if err := reg.Add(ext1); err != nil {
		t.Fatal(err)
	}
	if err := reg.Add(ext2); err != nil {
		t.Fatal(err)
	}

	all := reg.All()
	if len(all) != 2 {
		t.Fatalf("expected 2 extensions, got %d", len(all))
	}
	if all[0].Name != "first" {
		t.Fatalf("expected first to be sorted first, got %s", all[0].Name)
	}
}

func TestGetReturnsCopy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "extensions.json")
	reg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := reg.Add(Extension{Name: "test", Type: TypeMCP, Command: "echo"}); err != nil {
		t.Fatal(err)
	}

	got := reg.Get("test")
	got.Command = "mutated"

	original := reg.Get("test")
	if original.Command != "echo" {
		t.Fatal("Get should return a copy, not a reference")
	}
}

func TestEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "extensions.json")
	reg := Empty(path)
	if len(reg.All()) != 0 {
		t.Fatal("expected empty registry")
	}
	// Should be able to add and persist.
	if err := reg.Add(Extension{Name: "test", Type: TypeMCP, Command: "echo"}); err != nil {
		t.Fatal(err)
	}
	if reg.Get("test") == nil {
		t.Fatal("expected extension after add")
	}
}

func TestNameLengthLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "extensions.json")
	reg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	longName := string(make([]byte, 200))
	for i := range longName {
		longName = longName[:i] + "a" + longName[i+1:]
	}
	err = reg.Add(Extension{Name: longName, Type: TypeMCP, Command: "echo"})
	if err == nil {
		t.Fatal("expected error for long name")
	}
}

func TestUpdate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "extensions.json")
	reg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := reg.Add(Extension{
		Name:    "test-mcp",
		Type:    TypeMCP,
		Command: "echo",
		Env:     map[string]string{"KEY": "old"},
	}); err != nil {
		t.Fatal(err)
	}

	// Update env var.
	if err := reg.Update("test-mcp", func(ext *Extension) {
		ext.Env["KEY"] = "new"
		ext.Env["EXTRA"] = "added"
	}); err != nil {
		t.Fatal(err)
	}

	got := reg.Get("test-mcp")
	if got.Env["KEY"] != "new" {
		t.Errorf("env KEY = %q, want new", got.Env["KEY"])
	}
	if got.Env["EXTRA"] != "added" {
		t.Error("expected EXTRA env var to be added")
	}

	// Verify persistence.
	reg2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got2 := reg2.Get("test-mcp")
	if got2.Env["KEY"] != "new" {
		t.Error("update not persisted")
	}
}

func TestUpdateNotFound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "extensions.json")
	reg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	err = reg.Update("nonexistent", func(ext *Extension) {
		ext.Env["KEY"] = "val"
	})
	if err == nil {
		t.Fatal("expected not-found error")
	}
}

func TestPromptTypeValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "extensions.json")
	reg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	// Prompt without description should fail.
	err = reg.Add(Extension{Name: "test", Type: TypePrompt, Command: "/tmp"})
	if err == nil {
		t.Fatal("expected error for prompt without description")
	}

	// Prompt with description but non-existent directory should fail.
	err = reg.Add(Extension{Name: "test", Type: TypePrompt, Command: "/nonexistent", Description: "test"})
	if err == nil {
		t.Fatal("expected error for prompt with missing SKILL.md")
	}

	// Prompt with valid SKILL.md should succeed.
	skillDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("# Test Skill"), 0644); err != nil {
		t.Fatal(err)
	}
	err = reg.Add(Extension{Name: "test-prompt", Type: TypePrompt, Command: skillDir, Description: "A test prompt skill"})
	if err != nil {
		t.Fatal(err)
	}
	got := reg.Get("test-prompt")
	if got == nil || got.Type != TypePrompt {
		t.Fatal("expected prompt extension to be stored")
	}
}

func TestLoadCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "extensions.json")
	if err := os.WriteFile(path, []byte("not json"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestExecWithInputSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "extensions.json")
	reg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	schema := json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}}}`)
	ext := Extension{
		Name:        "my-tool",
		Type:        TypeExec,
		Command:     "/usr/bin/my-tool",
		Description: "A test tool",
		InputSchema: schema,
	}
	if err := reg.Add(ext); err != nil {
		t.Fatal(err)
	}

	// Reload and verify schema preserved.
	reg2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got := reg2.Get("my-tool")
	if got == nil {
		t.Fatal("expected extension to persist")
	}
	var expected, actual any
	if err := json.Unmarshal(schema, &expected); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(got.InputSchema, &actual); err != nil {
		t.Fatal(err)
	}
	e, _ := json.Marshal(expected)
	a, _ := json.Marshal(actual)
	if string(e) != string(a) {
		t.Fatalf("expected schema %s, got %s", e, a)
	}
}
