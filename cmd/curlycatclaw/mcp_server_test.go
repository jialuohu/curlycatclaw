package main

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/jialuohu/curlycatclaw/internal/extension"
)

func TestIsDangerousEnvKey(t *testing.T) {
	dangerous := []string{
		"LD_PRELOAD",
		"LD_LIBRARY_PATH",
		"DYLD_INSERT_LIBRARIES",
		"DYLD_FRAMEWORK_PATH",
		"ld_preload", // case insensitive
	}
	for _, k := range dangerous {
		if !isDangerousEnvKey(k) {
			t.Errorf("expected %q to be dangerous", k)
		}
	}

	safe := []string{
		"PATH",
		"HOME",
		"API_KEY",
		"BRAVE_API_KEY",
		"NORMAL_VAR",
	}
	for _, k := range safe {
		if isDangerousEnvKey(k) {
			t.Errorf("expected %q to be safe", k)
		}
	}
}

func TestBuildMCPExtEnv(t *testing.T) {
	env := buildMCPExtEnv(map[string]string{
		"API_KEY":     "secret",
		"LD_PRELOAD":  "/evil.so",
		"CUSTOM_FLAG": "1",
		"PATH":        "/evil/bin", // should not override baseline
	})

	has := func(key string) bool {
		for _, entry := range env {
			if len(entry) > len(key) && entry[:len(key)+1] == key+"=" {
				return true
			}
		}
		return false
	}

	val := func(key string) string {
		for _, entry := range env {
			if len(entry) > len(key) && entry[:len(key)+1] == key+"=" {
				return entry[len(key)+1:]
			}
		}
		return ""
	}

	// Extension env vars should pass through (minus dangerous ones).
	if !has("API_KEY") {
		t.Error("expected API_KEY in env")
	}
	if !has("CUSTOM_FLAG") {
		t.Error("expected CUSTOM_FLAG in env")
	}
	if has("LD_PRELOAD") {
		t.Error("LD_PRELOAD should be filtered out")
	}

	// Baseline vars should be present and NOT overridden by extension.
	if !has("PATH") {
		t.Error("expected PATH from baseline allowlist")
	}
	if val("PATH") == "/evil/bin" {
		t.Error("extension should not override baseline PATH")
	}
}

func TestFormatMCPResult_Nil(t *testing.T) {
	if got := formatMCPResult(nil); got != "" {
		t.Errorf("formatMCPResult(nil) = %q, want empty", got)
	}
}

// TestLoadProxyUpstreams_EmptyInputsFastReturn verifies the trivial empty
// case: no extensions, no config servers, must not block or panic.
func TestLoadProxyUpstreams_EmptyInputsFastReturn(t *testing.T) {
	hr := newMCPHotReloader(nil, 0, 0, nil)
	done := make(chan struct{})
	go func() {
		loadProxyUpstreams(nil, nil, hr, func(string, ...any) {})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loadProxyUpstreams did not return promptly with empty inputs")
	}
}

// TestLoadProxyUpstreams_PerUpstreamTimeoutIsBounded locks in the Apr-15
// fix: loadProxyUpstreams is synchronous-with-parallel-fanout, so a single
// slow upstream must not stall total wall time past perUpstreamTimeout.
// Without the parallelism, five slow upstreams would take 5×15s=75s,
// blasting Claude CLI's MCP initialize budget and resurrecting the exact
// silent-exit bug we already fixed once.
//
// This test uses fake stdio extensions that will fail to spawn (command
// doesn't exist), which exercises the connectExt path including the ctx
// timeout machinery. Even with failed connects, total wall time must stay
// well under N × perUpstreamTimeout.
func TestLoadProxyUpstreams_PerUpstreamTimeoutIsBounded(t *testing.T) {
	// Five bogus stdio extensions. Each ConnectAndRegister will hit
	// exec.CommandContext("does-not-exist") which fails fast, but even if
	// they hung, perUpstreamTimeout caps each at 15s. With parallel fanout
	// total time stays near the slowest single one, not the sum.
	regPath := filepath.Join(t.TempDir(), "extensions.json")
	reg, err := extension.Load(regPath)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		if err := reg.Add(extension.Extension{
			Name:    fmt.Sprintf("bogus-%d", i),
			Type:    extension.TypeMCP,
			Command: "/no/such/binary/anywhere",
			AddedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	hr := newMCPHotReloader(nil, 0, 0, nil)

	start := time.Now()
	loadProxyUpstreams(reg, nil, hr, func(string, ...any) {})
	elapsed := time.Since(start)

	// Parallel fanout: all five should error roughly simultaneously.
	// Give ourselves well under 2×perUpstreamTimeout as the safety margin.
	if elapsed > 2*perUpstreamTimeout {
		t.Fatalf("loadProxyUpstreams took %v for 5 fast-failing upstreams; parallel fanout broken (expected < %v)", elapsed, 2*perUpstreamTimeout)
	}
}
