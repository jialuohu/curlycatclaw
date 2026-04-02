package main

import "testing"

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
