package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/jialuohu/curlycatclaw/config"
)

func TestSetupLogging_DefaultStderr(t *testing.T) {
	cfg := config.LoggingConfig{
		Level:  "info",
		Format: "text",
	}
	if err := setupLogging(cfg); err != nil {
		t.Fatalf("setupLogging: %v", err)
	}
	// Verify the default logger is set (no panic on use).
	slog.Info("test log from default stderr handler")
}

func TestSetupLogging_JSONFormat(t *testing.T) {
	cfg := config.LoggingConfig{
		Level:  "info",
		Format: "json",
	}
	if err := setupLogging(cfg); err != nil {
		t.Fatalf("setupLogging: %v", err)
	}
	slog.Info("test log in json format")
}

func TestSetupLogging_LevelParsing(t *testing.T) {
	levels := []struct {
		input string
		want  slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"error", slog.LevelError},
		{"INFO", slog.LevelInfo},
		{"unknown", slog.LevelInfo},
	}
	for _, tc := range levels {
		cfg := config.LoggingConfig{Level: tc.input, Format: "text"}
		if err := setupLogging(cfg); err != nil {
			t.Fatalf("setupLogging(%q): %v", tc.input, err)
		}
		if !slog.Default().Enabled(context.Background(), tc.want) {
			t.Errorf("level %q: expected %v to be enabled", tc.input, tc.want)
		}
	}
}

func TestSetupLogging_FileHandler(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "logs", "test.log")
	cfg := config.LoggingConfig{
		Level:      "info",
		File:       logFile,
		MaxSize:    1,
		MaxAge:     1,
		MaxBackups: 1,
		Format:     "text",
	}
	if err := setupLogging(cfg); err != nil {
		t.Fatalf("setupLogging: %v", err)
	}

	slog.Info("file handler test message")

	// Verify the log directory was created.
	dir := filepath.Dir(logFile)
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("log dir not created: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("%s is not a directory", dir)
	}
}

func TestHealthHandler_Returns200(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := httptest.NewServer(newHealthHandler(ctx))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

func TestHealthHandler_Returns503OnShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	srv := httptest.NewServer(newHealthHandler(ctx))
	defer srv.Close()

	cancel()

	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("expected 503 during shutdown, got %d", resp.StatusCode)
	}
}

func TestEnsureIsolatedHome_CreatesStructure(t *testing.T) {
	home := filepath.Join(t.TempDir(), "isolated")

	if err := ensureIsolatedHome(home); err != nil {
		t.Fatalf("ensureIsolatedHome: %v", err)
	}

	// Check directories exist.
	for _, dir := range []string{
		home,
		filepath.Join(home, ".claude"),
		filepath.Join(home, ".claude", "plugins"),
		filepath.Join(home, ".ssh"),
	} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Errorf("expected dir %s to exist: %v", dir, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("expected %s to be a directory", dir)
		}
	}

	// Check settings.json exists.
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	if _, err := os.Stat(settingsPath); err != nil {
		t.Errorf("settings.json should exist: %v", err)
	}
}

func TestEnsureIsolatedHome_Idempotent(t *testing.T) {
	home := filepath.Join(t.TempDir(), "isolated")

	// Call twice; should not error on second call.
	if err := ensureIsolatedHome(home); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if err := ensureIsolatedHome(home); err != nil {
		t.Fatalf("second call (idempotent): %v", err)
	}
}

func TestEnsureIsolatedHome_CopiesGitconfig(t *testing.T) {
	// Create a real home dir with .gitconfig.
	realHome := t.TempDir()
	t.Setenv("HOME", realHome)

	gitconfig := filepath.Join(realHome, ".gitconfig")
	if err := os.WriteFile(gitconfig, []byte("[user]\n\tname = Test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	home := filepath.Join(t.TempDir(), "isolated")
	if err := ensureIsolatedHome(home); err != nil {
		t.Fatalf("ensureIsolatedHome: %v", err)
	}

	dst := filepath.Join(home, ".gitconfig")
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read copied .gitconfig: %v", err)
	}
	if string(data) != "[user]\n\tname = Test\n" {
		t.Errorf(".gitconfig content = %q, want original content", string(data))
	}

	// Verify it's a copy, not a symlink.
	info, err := os.Lstat(dst)
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Error(".gitconfig should be a copy, not a symlink")
	}
}

func TestEnsureIsolatedHome_SymlinksKnownHosts(t *testing.T) {
	realHome := t.TempDir()
	t.Setenv("HOME", realHome)

	// Create .ssh/known_hosts in real home.
	sshDir := filepath.Join(realHome, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		t.Fatal(err)
	}
	knownHosts := filepath.Join(sshDir, "known_hosts")
	if err := os.WriteFile(knownHosts, []byte("github.com ssh-rsa AAAA...\n"), 0644); err != nil {
		t.Fatal(err)
	}

	home := filepath.Join(t.TempDir(), "isolated")
	if err := ensureIsolatedHome(home); err != nil {
		t.Fatalf("ensureIsolatedHome: %v", err)
	}

	dst := filepath.Join(home, ".ssh", "known_hosts")
	info, err := os.Lstat(dst)
	if err != nil {
		t.Fatalf("known_hosts not created: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Error("known_hosts should be a symlink")
	}

	// Verify .ssh directory is a real directory, not a symlink.
	sshInfo, err := os.Lstat(filepath.Join(home, ".ssh"))
	if err != nil {
		t.Fatalf("lstat .ssh: %v", err)
	}
	if sshInfo.Mode()&os.ModeSymlink != 0 {
		t.Error(".ssh should be a real directory, not a symlink")
	}
}
