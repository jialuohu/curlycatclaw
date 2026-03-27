package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/jialuohu/curlycatclaw/config"
	"github.com/jialuohu/curlycatclaw/internal/actor"
	"github.com/jialuohu/curlycatclaw/internal/claude"
	"github.com/jialuohu/curlycatclaw/internal/mcp"
	"github.com/jialuohu/curlycatclaw/internal/memory"
	"github.com/jialuohu/curlycatclaw/internal/security"
	"github.com/jialuohu/curlycatclaw/internal/session"
	"github.com/jialuohu/curlycatclaw/internal/telegram"
)

func main() {
	configPath := flag.String("config", defaultConfigPath(), "path to config.toml")
	flag.Parse()

	// Set up structured logging.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	if err := run(*configPath); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(configPath string) error {
	// Load config.
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	slog.Info("config loaded", "timezone", cfg.Timezone)

	// Ensure data directory exists.
	dataDir := filepath.Dir(cfg.Storage.DBPath)
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}

	// Initialize storage.
	store, err := memory.NewStore(cfg.Storage.DBPath)
	if err != nil {
		return fmt.Errorf("init storage: %w", err)
	}
	defer store.Close()
	slog.Info("storage initialized", "path", cfg.Storage.DBPath)

	// Initialize credential store (Phase 1: optional, skip if no master key).
	credPath := filepath.Join(dataDir, "credentials.enc")
	credStore, err := security.NewCredentialStore(credPath, nil)
	if err != nil {
		slog.Warn("credential store unavailable, MCP env refs won't resolve", "err", err)
	}

	// Initialize Claude client.
	claudeClient := claude.NewClient(cfg.Claude.APIKey, cfg.Claude.Model)
	slog.Info("claude client initialized", "model", cfg.Claude.Model)

	// Initialize Telegram channel.
	tg, err := telegram.NewChannel(cfg.Telegram)
	if err != nil {
		return fmt.Errorf("init telegram: %w", err)
	}
	slog.Info("telegram channel initialized")

	// Initialize MCP manager.
	mcpMgr := mcp.NewManager()

	// Set up context with cancellation on signal.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start MCP servers.
	envResolver := func(ref string) (string, error) {
		if credStore == nil {
			return "", fmt.Errorf("credential store not available")
		}
		return credStore.Get(ref)
	}
	if err := mcpMgr.Start(ctx, cfg.MCP.Servers, envResolver); err != nil {
		slog.Warn("some MCP servers failed to start", "err", err)
	}
	defer mcpMgr.Shutdown()

	// Create session actor.
	sess := session.New(cfg, claudeClient, tg, mcpMgr, store)

	// Handle shutdown signals.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		slog.Info("received signal, shutting down", "signal", sig)
		cancel()
	}()

	slog.Info("curlycatclaw started")

	// Run actors under supervision.
	actor.SuperviseAll(ctx, tg, sess)

	slog.Info("curlycatclaw stopped")
	return nil
}

func defaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "config.toml"
	}
	return filepath.Join(home, ".curlycatclaw", "config.toml")
}
