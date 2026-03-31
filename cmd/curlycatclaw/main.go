package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/jialuohu/curlycatclaw/config"
	"github.com/jialuohu/curlycatclaw/internal/actor"
	"github.com/jialuohu/curlycatclaw/internal/claude"
	"github.com/jialuohu/curlycatclaw/internal/mcp"
	"github.com/jialuohu/curlycatclaw/internal/memory"
	"github.com/jialuohu/curlycatclaw/internal/security"
	"github.com/jialuohu/curlycatclaw/internal/session"
	"github.com/jialuohu/curlycatclaw/internal/telegram"
	"github.com/jialuohu/curlycatclaw/internal/wasm"
	"github.com/jialuohu/curlycatclaw/skills"
	"gopkg.in/natefinch/lumberjack.v2"
)

var version = "dev"

func main() {
	configPath := flag.String("config", defaultConfigPath(), "path to config.toml")
	versionFlag := flag.Bool("version", false, "print version and exit")
	mcpServerFlag := flag.Bool("mcp-server", false, "run as MCP stdio server (spawned by claude CLI)")
	flag.Parse()

	if *versionFlag {
		fmt.Println("curlycatclaw", version)
		return
	}

	// Set up structured logging (default, will be reconfigured after config load).
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	// MCP server mode: expose skills as MCP tools over stdio.
	if *mcpServerFlag {
		if err := runMCPServer(); err != nil {
			slog.Error("mcp-server fatal", "err", err)
			os.Exit(1)
		}
		return
	}

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

	// Reconfigure logging from config.
	if err := setupLogging(cfg.Logging); err != nil {
		return fmt.Errorf("setup logging: %w", err)
	}
	slog.Info("config loaded", "timezone", cfg.Timezone, "version", version)

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
	var credStore *security.CredentialStore
	if masterKeyHex := os.Getenv("CURLYCATCLAW_MASTER_KEY"); masterKeyHex != "" {
		masterKey, err := hex.DecodeString(masterKeyHex)
		if err != nil || len(masterKey) != 32 {
			slog.Warn("invalid CURLYCATCLAW_MASTER_KEY (need 64 hex chars for 32 bytes), credentials disabled")
		} else {
			credPath := filepath.Join(dataDir, "credentials.enc")
			credStore, err = security.NewCredentialStore(credPath, masterKey)
			if err != nil {
				slog.Warn("credential store init failed", "err", err)
			} else {
				slog.Info("credential store initialized")
			}
		}
	}

	// Initialize Claude client (direct API) or CLI manager (subprocess mode).
	var claudeClient *claude.Client
	var cliManager *claude.CLIManager
	var authOpt option.RequestOption
	if cfg.Claude.UseCLI() {
		cliManager = claude.NewCLIManager(cfg.Claude.CLIPath, cfg.Claude.Model, cfg.Claude.OAuthToken)
		slog.Info("claude CLI manager initialized", "cli", cfg.Claude.CLIPath, "model", cfg.Claude.Model)
	} else {
		authOpt = cfg.Claude.AuthOption()
		claudeClient = claude.NewClient(authOpt, cfg.Claude.Model)
		slog.Info("claude client initialized", "model", cfg.Claude.Model)
	}

	// Initialize Telegram channel.
	tg, err := telegram.NewChannel(cfg.Telegram)
	if err != nil {
		return fmt.Errorf("init telegram: %w", err)
	}
	slog.Info("telegram channel initialized")

	// Initialize MCP manager.
	mcpMgr := mcp.NewManager(version)

	// Set up context with cancellation on signal.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start MCP servers.
	envResolver := func(val string) (string, error) {
		const prefix = "encrypted:ref:"
		if !strings.HasPrefix(val, prefix) {
			return val, nil
		}
		if credStore == nil {
			return "", fmt.Errorf("credential store not available (set CURLYCATCLAW_MASTER_KEY)")
		}
		return credStore.Get(strings.TrimPrefix(val, prefix))
	}
	if err := mcpMgr.Start(ctx, cfg.MCP.Servers, envResolver); err != nil {
		slog.Warn("some MCP servers failed to start", "err", err)
	}
	defer mcpMgr.Shutdown()

	// Initialize built-in skills.
	skillReg := skills.NewRegistry()
	skillReg.Register(skills.NewWebSearchSkill())
	noteSkills, err := skills.InitNoteSkills(store.DB())
	if err != nil {
		slog.Warn("failed to initialize note skills", "err", err)
	} else {
		for _, s := range noteSkills {
			skillReg.Register(s)
		}
	}
	remindSignalCh := make(chan int64, 16)
	remindSkills, err := skills.InitRemindSkills(store.DB(), remindSignalCh, cfg.Location())
	if err != nil {
		slog.Warn("failed to initialize remind skills", "err", err)
	} else {
		for _, s := range remindSkills {
			skillReg.Register(s)
		}
	}
	slog.Info("skills registered", "count", len(skillReg.All()))

	// Initialize prompt budget manager (optional, requires direct API — not available in CLI mode).
	var budgetMgr *memory.BudgetManager
	if cfg.Budget.Enabled && authOpt != nil {
		haikuClient := claude.NewClient(authOpt, cfg.Budget.Model)
		var bmErr error
		budgetMgr, bmErr = memory.NewBudgetManager(store.DB(), haikuClient, true)
		if bmErr != nil {
			slog.Warn("budget manager init failed", "err", bmErr)
		} else {
			slog.Info("budget manager enabled", "model", cfg.Budget.Model)
		}
	} else if cfg.Budget.Enabled && cfg.Claude.UseCLI() {
		slog.Info("budget manager disabled in CLI mode (requires direct API)")
	}

	// Initialize vector store (optional).
	var vectorStore *memory.VectorStore
	if cfg.Vector.Enabled {
		embedder := newEmbedder(cfg.Vector)
		slog.Info("embedder configured", "name", embedder.Name(), "dim", embedder.Dimension())

		vs, err := memory.NewVectorStore(ctx, cfg.Vector.QdrantAddr, embedder)
		if err != nil {
			slog.Warn("vector store init failed, disabling", "err", err)
		} else {
			vectorStore = vs
			defer vectorStore.Close()
			skillReg.Register(skills.NewSemanticSearchSkill(vectorStore))
			slog.Info("vector store enabled", "addr", cfg.Vector.QdrantAddr)
		}
	}

	// Initialize wasm skill runtime (optional).
	if cfg.Wasm.Enabled {
		wasmRT, err := wasm.NewWasmRuntime(cfg.Wasm, skillReg, store.DB(), tg.Inbox())
		if err != nil {
			slog.Warn("wasm runtime init failed", "err", err)
		} else {
			if err := wasmRT.LoadAll(ctx); err != nil {
				slog.Warn("wasm: failed to load some modules", "err", err)
			}
			go func() {
				if err := wasmRT.WatchForChanges(ctx); err != nil {
					slog.Warn("wasm: file watcher stopped", "err", err)
				}
			}()
			defer wasmRT.Close()
			slog.Info("wasm runtime enabled", "dir", cfg.Wasm.SkillsDir)
		}
	}

	// Apply filesystem sandbox (Linux-only, no-op on other platforms).
	if cfg.Sandbox.Enabled {
		var logDir string
		if cfg.Logging.File != "" {
			logDir = filepath.Dir(cfg.Logging.File)
		}
		if err := security.ApplySandbox(security.SandboxParams{
			DataDir:      dataDir,
			ConfigPath:   configPath,
			LogDir:       logDir,
			ExtraPaths:   cfg.Sandbox.ExtraPaths,
			ExtraPathsRW: cfg.Sandbox.ExtraPathsRW,
		}); err != nil {
			slog.Warn("sandbox: failed to apply", "err", err)
		}
	}

	// Initialize hierarchical memory (optional).
	var factStore *memory.FactStore
	var summarizer *memory.ConversationSummarizer
	if cfg.Memory.Enabled {
		factStore = memory.NewFactStore(store.DB(), cfg.Memory.MaxFacts)
		for _, s := range skills.InitFactSkills(factStore) {
			skillReg.Register(s)
		}

		// Create a dedicated client for summarization (requires direct API).
		if authOpt != nil {
			sumModel := cfg.Claude.Model
			if cfg.Memory.SummarizeModel != "" {
				sumModel = cfg.Memory.SummarizeModel
			}
			sumClient := claude.NewClient(authOpt, sumModel)
			summarizer = memory.NewSummarizer(func(ctx context.Context, system, user string) (string, error) {
				resp, err := sumClient.Send(ctx, claude.SendParams{
					SystemPrompt: system,
					Messages:     []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(user))},
					MaxTokens:    512,
				})
				if err != nil {
					return "", err
				}
				return resp.TextContent, nil
			})
		} else {
			slog.Info("summarizer disabled in CLI mode (requires direct API)")
		}

		slog.Info("hierarchical memory enabled", "max_facts", cfg.Memory.MaxFacts)
	}

	// Create cron executor for Claude-powered scheduled tasks.
	// Uses the same Claude client, MCP manager, and skills as the session actor
	// but runs with a clean context (facts only, no conversation history).
	var cronRunner skills.CronRunner
	if claudeClient != nil || cliManager != nil {
		var sessionFacts session.FactProvider
		if factStore != nil {
			sessionFacts = factStore
		}
		cronRunner = session.NewCronExecutor(cfg, claudeClient, cliManager, mcpMgr, skillReg, sessionFacts)
	}

	// Create reminder actor.
	reminderActor := skills.NewReminderActor(store.DB(), tg.Inbox(), cfg.Location(), remindSignalCh, cronRunner)

	// Create session actor.
	// Pass explicit nil interfaces (not typed nil pointers) when components
	// are disabled, so that session.Actor's nil checks work correctly.
	// A nil *T passed to an interface becomes non-nil (Go nil-interface trap).
	var sessionCLI session.CLIClient
	if cliManager != nil {
		sessionCLI = cliManager
	}
	var sessionSummarizer session.Summarizer
	if summarizer != nil {
		sessionSummarizer = summarizer
	}
	sess := session.New(cfg, claudeClient, sessionCLI, tg, mcpMgr, store, skillReg, budgetMgr, vectorStore, factStore, sessionSummarizer, configPath)

	// Handle shutdown signals. First signal triggers graceful shutdown;
	// second signal forces immediate exit.
	shutdownComplete := make(chan struct{})
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		slog.Info("received signal, shutting down gracefully", "signal", sig)
		cancel()
		select {
		case sig = <-sigCh:
			slog.Error("received second signal, forcing exit", "signal", sig)
			os.Exit(1)
		case <-shutdownComplete:
			return
		}
	}()

	// Start health server if enabled.
	if cfg.Health.Enabled {
		startHealthServer(ctx, cfg.Health.Port)
	}

	// CLI manager lifecycle: periodic idle cleanup + graceful shutdown.
	// The cleanup goroutine must exit before Shutdown runs to avoid races.
	if cliManager != nil {
		cleanupDone := make(chan struct{})
		go func() {
			defer close(cleanupDone)
			ticker := time.NewTicker(5 * time.Minute)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					cliManager.Cleanup(4 * time.Hour) // match conversation expiry
				}
			}
		}()
		defer func() {
			<-cleanupDone
			cliManager.Shutdown(30 * time.Second)
		}()
	}

	slog.Info("curlycatclaw started")

	// Run actors under supervision.
	actor.SuperviseAll(ctx, 30*time.Second, tg, sess, reminderActor)
	close(shutdownComplete)

	slog.Info("curlycatclaw stopped")
	return nil
}

func newEmbedder(cfg config.VectorConfig) memory.Embedder {
	switch cfg.Embedder {
	case "ollama":
		return memory.NewOllamaEmbedder(cfg.OllamaURL, cfg.OllamaModel, cfg.OllamaDim)
	case "voyage":
		return memory.NewVoyageEmbedder(cfg.VoyageKey, cfg.VoyageModel, cfg.VoyageDim)
	default:
		return memory.FNVEmbedder{}
	}
}

func defaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "config.toml"
	}
	return filepath.Join(home, ".curlycatclaw", "config.toml")
}

func setupLogging(cfg config.LoggingConfig) error {
	var level slog.Level
	switch strings.ToLower(cfg.Level) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	var w io.Writer = os.Stderr
	if cfg.File != "" {
		if err := os.MkdirAll(filepath.Dir(cfg.File), 0750); err != nil {
			return fmt.Errorf("create log dir: %w", err)
		}
		w = &lumberjack.Logger{
			Filename:   cfg.File,
			MaxSize:    cfg.MaxSize,
			MaxAge:     cfg.MaxAge,
			MaxBackups: cfg.MaxBackups,
		}
	}

	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	if cfg.Format == "json" {
		handler = slog.NewJSONHandler(w, opts)
	} else {
		handler = slog.NewTextHandler(w, opts)
	}

	slog.SetDefault(slog.New(handler))
	return nil
}

// newHealthHandler returns an HTTP handler that checks process liveness
// (context not cancelled). Does not probe SQLite to avoid blocking the
// single-connection pool during slow writes.
func newHealthHandler(ctx context.Context) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		if ctx.Err() != nil {
			http.Error(w, "shutting down", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})
	return mux
}

// startHealthServer runs an HTTP health endpoint in a background goroutine.
// Binds to localhost only and sets conservative timeouts to prevent slowloris.
func startHealthServer(ctx context.Context, port int) {
	srv := &http.Server{
		Addr:              net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", port)),
		Handler:           newHealthHandler(ctx),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}

	go func() {
		slog.Info("health server started", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("health server failed", "err", err)
		}
	}()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx) //nolint:errcheck
	}()
}
