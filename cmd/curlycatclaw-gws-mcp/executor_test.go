package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestFlagsToArgs(t *testing.T) {
	tests := []struct {
		name  string
		flags map[string]any
		want  map[string]bool // expected flag strings present
	}{
		{
			name:  "string values",
			flags: map[string]any{"to": "alice@example.com", "subject": "Hello"},
			want:  map[string]bool{"--to": true, "--subject": true},
		},
		{
			name:  "bool true",
			flags: map[string]any{"html": true},
			want:  map[string]bool{"--html": true},
		},
		{
			name:  "bool false omitted",
			flags: map[string]any{"html": false},
			want:  map[string]bool{},
		},
		{
			name:  "nil omitted",
			flags: map[string]any{"cc": nil},
			want:  map[string]bool{},
		},
		{
			name:  "slice repeats flag",
			flags: map[string]any{"attach": []any{"a.pdf", "b.csv"}},
			want:  map[string]bool{"--attach": true},
		},
		{
			name:  "reserved flag stripped",
			flags: map[string]any{"format": "table", "to": "alice@example.com"},
			want:  map[string]bool{"--to": true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := flagsToArgs(tt.flags)
			argSet := make(map[string]bool)
			for _, a := range args {
				argSet[a] = true
			}
			for k := range tt.want {
				if !argSet[k] {
					t.Errorf("expected arg %q not found in %v", k, args)
				}
			}
		})
	}
}

func TestValidArg(t *testing.T) {
	valid := []struct{ name, value string }{
		{"service", "gmail"},
		{"service", "admin-reports"},
		{"helper", "+send"},
		{"resource", "files"},
		{"method", "list"},
		{"service", "drive"},
	}
	for _, tt := range valid {
		if err := validArg(tt.name, tt.value); err != nil {
			t.Errorf("validArg(%q, %q) = %v, want nil", tt.name, tt.value, err)
		}
	}

	invalid := []struct{ name, value string }{
		{"service", ""},
		{"service", "--config"},
		{"method", "--format table"},
		{"service", "a b"},
		{"service", "foo;bar"},
		{"service", "../etc"},
		{"resource", "foo bar"},
	}
	for _, tt := range invalid {
		if err := validArg(tt.name, tt.value); err == nil {
			t.Errorf("validArg(%q, %q) = nil, want error", tt.name, tt.value)
		}
	}
}

func TestExecuteHelper_ArgumentInjection(t *testing.T) {
	e := &Executor{GWSPath: "gws", Timeout: time.Second}

	_, err := e.ExecuteHelper(context.Background(), "--config", "+send", nil)
	if err == nil {
		t.Error("expected error for injected service")
	}

	_, err = e.ExecuteHelper(context.Background(), "gmail", "--format json", nil)
	if err == nil {
		t.Error("expected error for injected helper")
	}
}

func TestExecuteAPI_ArgumentInjection(t *testing.T) {
	e := &Executor{GWSPath: "gws", Timeout: time.Second}

	_, err := e.ExecuteAPI(context.Background(), "--config", "", "list", nil, nil)
	if err == nil {
		t.Error("expected error for injected service")
	}

	_, err = e.ExecuteAPI(context.Background(), "drive", "files", "--format table", nil, nil)
	if err == nil {
		t.Error("expected error for injected method")
	}
}

func TestFlagsToArgsReservedStripped(t *testing.T) {
	flags := map[string]any{
		"format":  "table",
		"config":  "/evil/path",
		"profile": "admin",
		"help":    true,
		"to":      "alice@example.com",
	}
	args := flagsToArgs(flags)
	for _, a := range args {
		switch a {
		case "--format", "--config", "--profile", "--help":
			t.Errorf("reserved flag %s should be stripped", a)
		}
	}
	found := false
	for _, a := range args {
		if a == "--to" {
			found = true
		}
	}
	if !found {
		t.Error("non-reserved flag --to should be kept")
	}
}

func TestFlagsToArgsSliceRepeats(t *testing.T) {
	flags := map[string]any{"attach": []any{"a.pdf", "b.csv"}}
	args := flagsToArgs(flags)

	count := 0
	for _, a := range args {
		if a == "--attach" {
			count++
		}
	}
	if count != 2 {
		t.Errorf("expected --attach to appear 2 times, got %d in %v", count, args)
	}
}

func TestExecutorRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test not supported on Windows")
	}

	// Create a mock gws script.
	dir := t.TempDir()
	script := filepath.Join(dir, "gws")
	err := os.WriteFile(script, []byte(`#!/bin/sh
echo '{"result": "ok"}'
`), 0755)
	if err != nil {
		t.Fatal(err)
	}

	e := &Executor{GWSPath: script, Timeout: 5 * time.Second}

	result, err := e.ExecuteHelper(context.Background(), "gmail", "+send", map[string]any{
		"to":      "alice@example.com",
		"subject": "Test",
		"body":    "Hello",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, `"result"`) {
		t.Errorf("result = %q, expected JSON with result", result)
	}
}

func TestExecutorTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test not supported on Windows")
	}

	dir := t.TempDir()
	script := filepath.Join(dir, "gws")
	err := os.WriteFile(script, []byte(`#!/bin/sh
sleep 10
`), 0755)
	if err != nil {
		t.Fatal(err)
	}

	e := &Executor{GWSPath: script, Timeout: 100 * time.Millisecond}

	_, err = e.ExecuteHelper(context.Background(), "test", "+slow", nil)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %q, expected timeout message", err)
	}
}

func TestExecutorError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test not supported on Windows")
	}

	dir := t.TempDir()
	script := filepath.Join(dir, "gws")
	err := os.WriteFile(script, []byte(`#!/bin/sh
echo "something went wrong" >&2
exit 1
`), 0755)
	if err != nil {
		t.Fatal(err)
	}

	e := &Executor{GWSPath: script, Timeout: 5 * time.Second}

	_, err = e.ExecuteHelper(context.Background(), "test", "+fail", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "something went wrong") {
		t.Errorf("error = %q, expected stderr message", err)
	}
}

func TestExecuteAPI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test not supported on Windows")
	}

	dir := t.TempDir()
	script := filepath.Join(dir, "gws")
	// Echo all args so we can verify command construction.
	err := os.WriteFile(script, []byte(`#!/bin/sh
echo "$@"
`), 0755)
	if err != nil {
		t.Fatal(err)
	}

	e := &Executor{GWSPath: script, Timeout: 5 * time.Second}

	result, err := e.ExecuteAPI(context.Background(), "drive", "files", "list",
		map[string]any{"pageSize": float64(10)}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(result, "drive") {
		t.Errorf("result should contain service name, got: %q", result)
	}
	if !strings.Contains(result, "files") {
		t.Errorf("result should contain resource name, got: %q", result)
	}
	if !strings.Contains(result, "--format") {
		t.Errorf("result should contain --format flag, got: %q", result)
	}
}
