package skillloader

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jialuohu/curlycatclaw/config"
	"github.com/jialuohu/curlycatclaw/skills"
)

// writeFile creates a file with the given content inside dir, creating
// intermediate directories as needed.
func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

// writeScript creates an executable shell script in dir.
func writeScript(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0755); err != nil {
		t.Fatal(err)
	}
}

// echoScript returns a bash script that reads stdin JSON and echoes it
// back as {"result": <stdin>}.
const echoScript = `#!/bin/bash
input=$(cat)
echo "{\"result\": \"got: $input\"}"
`

func TestLoadValidCollection(t *testing.T) {
	root := t.TempDir()
	colDir := filepath.Join(root, "mycol")

	writeFile(t, colDir, "collection.toml", `
name = "testcol"
version = "1.0.0"
`)

	skillDir := filepath.Join(colDir, "echo")
	writeScript(t, skillDir, "run.sh", echoScript)
	writeFile(t, skillDir, "skill.toml", `
name = "echo"
description = "echoes input"
type = "exec"
[exec]
command = "run.sh"
`)

	reg := skills.NewRegistry()
	loader := New(reg)
	err := loader.LoadAll(context.Background(), []config.SkillCollectionConfig{
		{Path: colDir},
	})
	if err != nil {
		t.Fatal("LoadAll failed:", err)
	}

	skill := reg.Get("testcol__echo")
	if skill == nil {
		t.Fatal("expected skill testcol__echo to be registered")
	}
	if skill.Description != "echoes input" {
		t.Errorf("unexpected description: %s", skill.Description)
	}
}

func TestLoadMissingCollectionToml(t *testing.T) {
	root := t.TempDir()
	colDir := filepath.Join(root, "mytools")

	// No collection.toml — should use directory name as namespace.
	skillDir := filepath.Join(colDir, "greet")
	writeScript(t, skillDir, "run.sh", echoScript)
	writeFile(t, skillDir, "skill.toml", `
name = "greet"
description = "greets"
type = "exec"
[exec]
command = "run.sh"
`)

	reg := skills.NewRegistry()
	loader := New(reg)
	err := loader.LoadAll(context.Background(), []config.SkillCollectionConfig{
		{Path: colDir},
	})
	if err != nil {
		t.Fatal("LoadAll failed:", err)
	}

	// Namespace should be the directory name "mytools".
	skill := reg.Get("mytools__greet")
	if skill == nil {
		t.Fatal("expected skill mytools__greet to be registered")
	}
}

func TestLoadConfigNamespaceOverride(t *testing.T) {
	root := t.TempDir()
	colDir := filepath.Join(root, "somedir")

	writeFile(t, colDir, "collection.toml", `
name = "fromtoml"
`)

	skillDir := filepath.Join(colDir, "hello")
	writeScript(t, skillDir, "run.sh", echoScript)
	writeFile(t, skillDir, "skill.toml", `
name = "hello"
description = "hello"
type = "exec"
[exec]
command = "run.sh"
`)

	reg := skills.NewRegistry()
	loader := New(reg)
	// Config namespace takes priority over collection.toml.
	err := loader.LoadAll(context.Background(), []config.SkillCollectionConfig{
		{Path: colDir, Namespace: "override"},
	})
	if err != nil {
		t.Fatal("LoadAll failed:", err)
	}

	if reg.Get("override__hello") == nil {
		t.Fatal("expected namespace override to take priority")
	}
	if reg.Get("fromtoml__hello") != nil {
		t.Error("collection.toml namespace should not be used when config namespace is set")
	}
}

func TestLoadInvalidSkillType(t *testing.T) {
	root := t.TempDir()
	colDir := filepath.Join(root, "col")
	skillDir := filepath.Join(colDir, "bad")

	writeScript(t, skillDir, "run.sh", echoScript)
	writeFile(t, skillDir, "skill.toml", `
name = "bad"
description = "unsupported type"
type = "wasm"
[exec]
command = "run.sh"
`)

	reg := skills.NewRegistry()
	loader := New(reg)
	// LoadAll logs a warning but doesn't return an error for individual skill failures.
	err := loader.LoadAll(context.Background(), []config.SkillCollectionConfig{
		{Path: colDir},
	})
	if err != nil {
		t.Fatal("LoadAll should not fail for individual skill errors:", err)
	}

	if reg.Get("col__bad") != nil {
		t.Error("skill with unsupported type should not be registered")
	}
}

func TestLoadMissingExecutable(t *testing.T) {
	root := t.TempDir()
	colDir := filepath.Join(root, "col")
	skillDir := filepath.Join(colDir, "missing")

	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, skillDir, "skill.toml", `
name = "missing"
description = "missing executable"
type = "exec"
[exec]
command = "nonexistent.sh"
`)

	reg := skills.NewRegistry()
	loader := New(reg)
	err := loader.LoadAll(context.Background(), []config.SkillCollectionConfig{
		{Path: colDir},
	})
	if err != nil {
		t.Fatal("LoadAll should not fail for individual skill errors:", err)
	}

	if reg.Get("col__missing") != nil {
		t.Error("skill with missing executable should not be registered")
	}
}

func TestExecAdapterExecute(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "echo.sh")
	script := `#!/bin/bash
input=$(cat)
echo "{\"result\": \"hello from exec\"}"
`
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	adapter := NewExecAdapter(scriptPath, nil, dir, nil, 10*time.Second)

	ctx := context.Background()
	input := json.RawMessage(`{"message":"test"}`)
	user := skills.UserInfo{UserID: 42, ChatID: 99}

	result, err := adapter.Execute(ctx, input, user)
	if err != nil {
		t.Fatal("Execute failed:", err)
	}
	if result != "hello from exec" {
		t.Errorf("unexpected result: %s", result)
	}
}

func TestExecAdapterTimeout(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "slow.sh")
	// Use exec to replace shell with sleep so SIGKILL works immediately.
	script := `#!/bin/bash
exec sleep 30
`
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	adapter := NewExecAdapter(scriptPath, nil, dir, nil, 200*time.Millisecond)

	ctx := context.Background()
	input := json.RawMessage(`{}`)
	user := skills.UserInfo{UserID: 1, ChatID: 1}

	start := time.Now()
	_, err := adapter.Execute(ctx, input, user)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("expected timeout in error, got: %s", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("timeout took too long: %s (expected ~200ms)", elapsed)
	}
}

func TestExecAdapterMinimalEnv(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "env.sh")
	// Script that checks for sensitive env vars and reports which are set.
	script := `#!/bin/bash
result=""
if [ -n "$CLAUDE_CODE_OAUTH_TOKEN" ]; then
  result="LEAKED:CLAUDE_CODE_OAUTH_TOKEN"
elif [ -n "$CURLYCATCLAW_MASTER_KEY" ]; then
  result="LEAKED:CURLYCATCLAW_MASTER_KEY"
elif [ -n "$PATH" ]; then
  result="clean"
else
  result="no_path"
fi
echo "{\"result\": \"$result\"}"
`
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	// Set sensitive env vars in the current process.
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "secret-token-value")
	t.Setenv("CURLYCATCLAW_MASTER_KEY", "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef")

	adapter := NewExecAdapter(scriptPath, nil, dir, nil, 10*time.Second)

	ctx := context.Background()
	input := json.RawMessage(`{}`)
	user := skills.UserInfo{UserID: 1, ChatID: 1}

	result, err := adapter.Execute(ctx, input, user)
	if err != nil {
		t.Fatal("Execute failed:", err)
	}
	if result != "clean" {
		t.Errorf("expected 'clean' (no leaked env vars), got: %s", result)
	}
}

func TestExecAdapterSkillEnv(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "env.sh")
	script := `#!/bin/bash
echo "{\"result\": \"$MY_SKILL_VAR\"}"
`
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	skillEnv := map[string]string{"MY_SKILL_VAR": "custom_value"}
	adapter := NewExecAdapter(scriptPath, nil, dir, skillEnv, 10*time.Second)

	ctx := context.Background()
	input := json.RawMessage(`{}`)
	user := skills.UserInfo{UserID: 1, ChatID: 1}

	result, err := adapter.Execute(ctx, input, user)
	if err != nil {
		t.Fatal("Execute failed:", err)
	}
	if result != "custom_value" {
		t.Errorf("expected skill env var to be passed, got: %s", result)
	}
}

func TestExecAdapterErrorResponse(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "err.sh")
	script := `#!/bin/bash
echo '{"error": "something went wrong"}'
`
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	adapter := NewExecAdapter(scriptPath, nil, dir, nil, 10*time.Second)

	ctx := context.Background()
	input := json.RawMessage(`{}`)
	user := skills.UserInfo{UserID: 1, ChatID: 1}

	_, err := adapter.Execute(ctx, input, user)
	if err == nil {
		t.Fatal("expected error from skill error response")
	}
	if !strings.Contains(err.Error(), "something went wrong") {
		t.Errorf("expected skill error message in error, got: %s", err)
	}
}

func TestExecAdapterStdinPayload(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "passthrough.sh")
	// Script that reads stdin and echoes it back wrapped in result.
	// Uses jq-free approach: just pass the raw stdin as the result string.
	script := `#!/bin/bash
input=$(cat)
# Escape the input for JSON string: replace backslash, then double-quote.
escaped=$(echo -n "$input" | sed 's/\\/\\\\/g; s/"/\\"/g')
echo "{\"result\": \"$escaped\"}"
`
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	adapter := NewExecAdapter(scriptPath, nil, dir, nil, 10*time.Second)

	ctx := context.Background()
	input := json.RawMessage(`{"key":"value"}`)
	user := skills.UserInfo{UserID: 42, ChatID: 99}

	result, err := adapter.Execute(ctx, input, user)
	if err != nil {
		t.Fatal("Execute failed:", err)
	}
	// The result should contain the JSON payload we sent.
	if !strings.Contains(result, `user_id`) || !strings.Contains(result, "42") {
		t.Errorf("expected stdin payload with user context in result, got: %s", result)
	}
}

func TestShutdown(t *testing.T) {
	root := t.TempDir()
	colDir := filepath.Join(root, "col")
	skillDir := filepath.Join(colDir, "test")

	writeScript(t, skillDir, "run.sh", echoScript)
	writeFile(t, skillDir, "skill.toml", `
name = "test"
description = "test skill"
type = "exec"
[exec]
command = "run.sh"
`)

	reg := skills.NewRegistry()
	loader := New(reg)
	err := loader.LoadAll(context.Background(), []config.SkillCollectionConfig{
		{Path: colDir},
	})
	if err != nil {
		t.Fatal("LoadAll failed:", err)
	}

	if reg.Get("col__test") == nil {
		t.Fatal("skill should be registered before shutdown")
	}

	if err := loader.Shutdown(); err != nil {
		t.Fatal("Shutdown failed:", err)
	}

	if reg.Get("col__test") != nil {
		t.Error("skill should be unregistered after shutdown")
	}
}

func TestLoadInputSchema(t *testing.T) {
	root := t.TempDir()
	colDir := filepath.Join(root, "col")
	skillDir := filepath.Join(colDir, "typed")

	writeScript(t, skillDir, "run.sh", echoScript)
	writeFile(t, skillDir, "skill.toml", `
name = "typed"
description = "typed skill"
type = "exec"
input_schema = '{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}'
[exec]
command = "run.sh"
`)

	reg := skills.NewRegistry()
	loader := New(reg)
	err := loader.LoadAll(context.Background(), []config.SkillCollectionConfig{
		{Path: colDir},
	})
	if err != nil {
		t.Fatal("LoadAll failed:", err)
	}

	skill := reg.Get("col__typed")
	if skill == nil {
		t.Fatal("expected skill to be registered")
	}
	if !strings.Contains(string(skill.InputSchema), "query") {
		t.Errorf("expected input schema to contain 'query', got: %s", skill.InputSchema)
	}
}

func TestLoadCustomTimeout(t *testing.T) {
	root := t.TempDir()
	colDir := filepath.Join(root, "col")
	skillDir := filepath.Join(colDir, "fast")

	writeScript(t, skillDir, "run.sh", `#!/bin/bash
exec sleep 30
`)
	writeFile(t, skillDir, "skill.toml", `
name = "fast"
description = "fast timeout"
type = "exec"
timeout = "200ms"
[exec]
command = "run.sh"
`)

	reg := skills.NewRegistry()
	loader := New(reg)
	err := loader.LoadAll(context.Background(), []config.SkillCollectionConfig{
		{Path: colDir},
	})
	if err != nil {
		t.Fatal("LoadAll failed:", err)
	}

	skill := reg.Get("col__fast")
	if skill == nil {
		t.Fatal("expected skill to be registered")
	}

	ctx := skills.WithUser(context.Background(), skills.UserInfo{UserID: 1, ChatID: 1})
	_, err = skill.Execute(ctx, json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("expected timeout in error, got: %s", err)
	}
}
