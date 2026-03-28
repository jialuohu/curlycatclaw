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
