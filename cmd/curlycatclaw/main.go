package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
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
	"github.com/go-co-op/gocron/v2"
	"github.com/jialuohu/curlycatclaw/config"
	"github.com/jialuohu/curlycatclaw/internal/actor"
	"github.com/jialuohu/curlycatclaw/internal/claude"
	"github.com/jialuohu/curlycatclaw/internal/ingest"
	"github.com/jialuohu/curlycatclaw/internal/eval"
	"github.com/jialuohu/curlycatclaw/internal/extension"
	"github.com/jialuohu/curlycatclaw/internal/mcp"
	"github.com/jialuohu/curlycatclaw/internal/memory"
	"github.com/jialuohu/curlycatclaw/internal/security"
	"github.com/jialuohu/curlycatclaw/internal/session"
	"github.com/jialuohu/curlycatclaw/internal/skillloader"
	"github.com/jialuohu/curlycatclaw/internal/telegram"
	"github.com/jialuohu/curlycatclaw/internal/update"
	"github.com/jialuohu/curlycatclaw/internal/voice"
	"github.com/jialuohu/curlycatclaw/internal/wasm"
	"github.com/jialuohu/curlycatclaw/skills"
	"gopkg.in/natefinch/lumberjack.v2"
)

var version = "dev"

func main() {
	configPath := flag.String("config", defaultConfigPath(), "path to config.toml")
	versionFlag := flag.Bool("version", false, "print version and exit")
	mcpServerFlag := flag.Bool("mcp-server", false, "run as MCP stdio server (spawned by claude CLI)")
	migrateEmbedderFlag := flag.Bool("migrate-embedder", false, "wipe and rebuild vector collections with the configured embedder, then exit")
	migrateDryRun := flag.Bool("dry-run", false, "with --migrate-embedder: count texts only, do not modify collections")
	evalExportFlag := flag.Bool("eval-export", false, "export recent conversations for manual quality labeling, then exit")
	evalExportHours := flag.Int("eval-hours", 168, "with --eval-export: hours of history to export (default: 168 = 1 week)")
	evalSeedFlag := flag.Bool("eval-seed", false, "seed database with synthetic conversations for eval validation, then exit")
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

	// Eval export mode: dump recent conversations for manual labeling.
	if *evalSeedFlag {
		if err := runEvalSeed(*configPath); err != nil {
			slog.Error("eval-seed fatal", "err", err)
			os.Exit(1)
		}
		return
	}

	if *evalExportFlag {
		if err := runEvalExport(*configPath, *evalExportHours); err != nil {
			slog.Error("eval-export fatal", "err", err)
			os.Exit(1)
		}
		return
	}

	// Embedder migration mode: wipe and rebuild vector collections.
	if *migrateEmbedderFlag {
		if err := runMigrateEmbedder(*configPath, *migrateDryRun); err != nil {
			slog.Error("migrate-embedder fatal", "err", err)
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

	// Set up isolated home directory for CLI project work.
	if cfg.Claude.IsolatedHome != "" {
		if err := ensureIsolatedHome(cfg.Claude.IsolatedHome); err != nil {
			return fmt.Errorf("ensure isolated home: %w", err)
		}
		slog.Info("isolated home initialized", "path", cfg.Claude.IsolatedHome)

		// Pre-install standard plugins on first startup.
		if cfg.Claude.UseCLI() {
			skills.EnsureDefaultPlugins(cfg.Claude.CLIPath, cfg.Claude.IsolatedHome)
		}
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
		cliManager = claude.NewCLIManager(cfg.Claude.CLIPath, cfg.Claude.Model, string(cfg.Claude.ThinkingEffort), cfg.Claude.OAuthToken)
		slog.Info("claude CLI manager initialized", "cli", cfg.Claude.CLIPath, "model", cfg.Claude.Model, "effort", cfg.Claude.ThinkingEffort)
	} else {
		authOpt = cfg.Claude.AuthOption()
		claudeClient = claude.NewClient(authOpt, cfg.Claude.Model)
		slog.Info("claude client initialized", "model", cfg.Claude.Model, "effort", cfg.Claude.ThinkingEffort)
		if cfg.Claude.ThinkingEffort == config.EffortLow || cfg.Claude.ThinkingEffort == config.EffortMedium {
			slog.Info("note: low/medium effort uses standard reasoning in direct API mode (no extended thinking)")
		}
	}

	// Initialize Telegram channel.
	tg, err := telegram.NewChannel(cfg.Telegram)
	if err != nil {
		return fmt.Errorf("init telegram: %w", err)
	}
	slog.Info("telegram channel initialized")

	// Initialize update client (self-update system, optional).
	var updateClient *update.Client
	if cfg.Update.Enabled {
		secret := os.Getenv("UPDATER_SECRET")
		if secret == "" {
			slog.Warn("update enabled but UPDATER_SECRET not set; update commands will fail")
		}
		updateClient = update.NewClient(cfg.Update.UpdaterURL, secret)
		slog.Info("update client initialized", "url", cfg.Update.UpdaterURL)
	}

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
	skillReg.Register(skills.NewSendFileSkill(tg))
	slog.Info("skills registered", "count", len(skillReg.All()))

	// Load external skill collections (exec-based skills from disk).
	if len(cfg.SkillCollections) > 0 {
		sl := skillloader.New(skillReg)
		if err := sl.LoadAll(ctx, cfg.SkillCollections); err != nil {
			slog.Warn("skill collections", "err", err)
		}
		go func() {
			if err := sl.WatchForChanges(ctx); err != nil {
				slog.Warn("skillloader: file watcher stopped", "err", err)
			}
		}()
		defer func() { _ = sl.Shutdown() }()
	}

	// Load runtime extension registry (persisted MCP servers + exec skills).
	extRegistryPath := filepath.Join(dataDir, "extensions.json")
	extReg, err := extension.Load(extRegistryPath)
	if err != nil {
		slog.Warn("extension registry load failed, starting empty", "path", extRegistryPath, "err", err)
		extReg = extension.Empty(extRegistryPath)
	}

	// Pre-seed default extensions (e.g. Scrapling MCP + agent skill) on first startup.
	wrappersDir := filepath.Join(dataDir, "extension-wrappers")
	extension.EnsureDefaults(extReg, wrappersDir)
	for _, ext := range extReg.ByType(extension.TypeMCP) {
		mcpCfg := config.MCPServerConfig{
			Name:    ext.Name,
			Command: ext.Command,
			Args:    ext.Args,
			Env:     ext.Env,
		}
		if err := mcpMgr.AddServer(ctx, mcpCfg, nil); err != nil {
			slog.Warn("extension: failed to start MCP server", "name", ext.Name, "err", err)
		}
	}
	for _, ext := range extReg.ByType(extension.TypeExec) {
		adapter := skillloader.NewExecAdapter(ext.Command, ext.Args, "", ext.Env, 30*time.Second)
		registryName := extension.ExecSkillPrefix + ext.Name
		extCopy := ext // capture for closure
		skillReg.Register(&skills.Skill{
			Name:        registryName,
			Description: extCopy.Description,
			InputSchema: extCopy.InputSchema,
			Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
				user := skills.GetUser(ctx)
				return adapter.Execute(ctx, input, user)
			},
		})
		slog.Info("extension: exec skill registered", "name", registryName)
	}
	extReloadFunc := func() {
		if cfg.Claude.IsolatedHome != "" {
			path := filepath.Join(cfg.Claude.IsolatedHome, ".curlycatclaw-reload-needed")
			os.WriteFile(path, []byte("1"), 0644) //nolint:errcheck
		}
	}
	for _, s := range extension.InitExtensionSkills(extReg, mcpMgr, skillReg, extReloadFunc, nil, credStore, nil) {
		skillReg.Register(s)
	}
	slog.Info("extension registry loaded", "path", extRegistryPath, "count", len(extReg.All()))

	// Initialize vector store (optional).
	var vectorStore *memory.VectorStore
	if cfg.Vector.Enabled {
		embedder := newEmbedder(cfg.Vector)
		slog.Info("embedder configured", "name", embedder.Name(), "dim", embedder.Dimension())
		if cfg.Memory.Enabled && (cfg.Vector.Embedder == "" || cfg.Vector.Embedder == "fnv") {
			slog.Warn("FNV embedder provides word-overlap matching only, not semantic search. " +
				"Memory retrieval quality will be limited. Consider 'ollama' or 'voyage' for better results.")
		}

		// Check migration state and determine which embedder to use for serving.
		servingEmbedder := embedder
		var migrationMgr *memory.MigrationManager
		embState, err := store.GetEmbedderState()
		if err != nil {
			slog.Warn("failed to read embedder state", "err", err)
		}

		if embState == nil {
			// First boot: initialize state with current embedder.
			if err := store.InitEmbedderState(embedder.Name()); err != nil {
				slog.Warn("failed to init embedder state", "err", err)
			}
			// Persist config for future migration detection (A6).
			store.UpdateEmbedderConfig(embedder.Name(), cfg.Vector.Embedder, embedderModel(cfg.Vector), int(embedder.Dimension())) //nolint:errcheck
		} else if embState.MigrationStatus == "" && embState.ActiveEmbedder == embedder.Name() {
			// No change, no migration. Update stored config at steady state (A6).
			store.UpdateEmbedderConfig(embedder.Name(), cfg.Vector.Embedder, embedderModel(cfg.Vector), int(embedder.Dimension())) //nolint:errcheck
		} else if embState.MigrationStatus == "" && embState.ActiveEmbedder != embedder.Name() {
			// Embedder changed — start background migration.
			slog.Info("embedder config changed, starting background migration",
				"old", embState.ActiveEmbedder, "new", embedder.Name())
			oldEmb := reconstructEmbedder(embState, cfg.Vector)
			if oldEmb != nil {
				servingEmbedder = oldEmb
				newVersion := embState.ActiveVersion + 1
				if err := store.StartMigration(embedder.Name(), newVersion,
					embState.OldEmbedderType, embState.OldEmbedderModel, embState.OldEmbedderDim); err != nil {
					slog.Error("failed to start migration", "err", err)
				} else {
					st, _ := store.GetEmbedderState()
					migrationMgr = memory.NewMigrationManager(store, nil, oldEmb, embedder, st, cfg.Vector.Embedder, embedderModel(cfg.Vector), int(embedder.Dimension())) // vs set below
				}
			} else {
				slog.Warn("cannot reconstruct old embedder, using new embedder directly")
			}
		} else if embState.MigrationStatus == "running" || embState.MigrationStatus == "completing" {
			// Crash recovery — resume migration.
			slog.Info("resuming migration from crash", "status", embState.MigrationStatus)
			oldEmb := reconstructEmbedder(embState, cfg.Vector)
			if oldEmb != nil {
				servingEmbedder = oldEmb
				migrationMgr = memory.NewMigrationManager(store, nil, oldEmb, embedder, embState, cfg.Vector.Embedder, embedderModel(cfg.Vector), int(embedder.Dimension())) // vs set below
			} else {
				slog.Warn("cannot reconstruct old embedder for crash recovery, using new embedder")
			}
		} else if embState.MigrationStatus == "failed" {
			// Failed migration — serve with old embedder, warn user.
			slog.Warn("previous migration failed, using old embedder. Run --migrate-embedder to retry.",
				"old", embState.ActiveEmbedder, "new", embedder.Name())
			oldEmb := reconstructEmbedder(embState, cfg.Vector)
			if oldEmb != nil {
				servingEmbedder = oldEmb
			}
		}

		vs, err := memory.NewVectorStore(ctx, cfg.Vector.QdrantAddr, servingEmbedder)
		if err != nil {
			slog.Warn("vector store init failed, disabling", "err", err)
		} else {
			vectorStore = vs
			defer vectorStore.Close()
			skillReg.Register(skills.NewSemanticSearchSkill(vectorStore))
			slog.Info("vector store enabled", "addr", cfg.Vector.QdrantAddr)

			// Start background migration if needed.
			if migrationMgr != nil {
				migrationMgr.SetVectorStore(vectorStore)
				migrationMgr.Start(ctx)
				defer migrationMgr.Stop()
			}
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

	// Initialize hierarchical memory (optional).
	var factStore *memory.FactStore
	var summarizer *memory.ConversationSummarizer
	if cfg.Memory.Enabled {
		factStore = memory.NewFactStore(store.DB(), cfg.Memory.MaxFacts)
		for _, s := range skills.InitFactSkills(factStore) {
			skillReg.Register(s)
		}
		for _, s := range skills.InitSummarySkills(store) {
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
		} else if cliManager != nil {
			// CLI mode: use SpawnOneShot with summarize_model override (cheaper model for summarization).
			summarizeModel := cfg.Memory.SummarizeModel
			summarizer = memory.NewSummarizer(func(ctx context.Context, system, user string) (string, error) {
				proc, err := cliManager.SpawnOneShot(ctx, claude.SpawnParams{
					SystemPrompt: system,
					InitialMsg:   claude.BuildUserMessage(user),
					Model:        summarizeModel,
				})
				if err != nil {
					return "", fmt.Errorf("cli summarize: spawn: %w", err)
				}
				defer proc.Kill()

				var text strings.Builder
				events, err := proc.Send(ctx, nil, func(delta string) {
					text.WriteString(delta)
				}, nil)
				if err != nil {
					return "", fmt.Errorf("cli summarize: send: %w", err)
				}

				for _, ev := range events {
					if res, ok := ev.(claude.ResultEvent); ok {
						if res.IsError {
							errMsg := strings.Join(res.Errors, "; ")
							if errMsg == "" {
								errMsg = "unknown CLI error"
							}
							return "", fmt.Errorf("cli summarize: %s", errMsg)
						}
						if text.Len() == 0 {
							text.WriteString(res.Result)
						}
					}
				}

				return text.String(), nil
			})
			slog.Info("summarizer enabled via CLI subprocess")
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

	// Create generic ingest actor (background knowledge source processing).
	// Uses direct API client if available, falls back to CLI one-shot mode.
	var ingestActor *ingest.Actor
	var ingestLLM ingest.LLMClient
	if claudeClient != nil {
		ingestLLM = claudeClient
	} else if cliManager != nil && cfg.Claude.OAuthToken != "" {
		ingestModel := cfg.Memory.Observations.ExtractionModel
		if ingestModel == "" {
			ingestModel = string(cfg.Claude.Model)
		}
		ingestLLM = claude.NewPersistentCLISender(cliManager, ingestModel, 0)
		slog.Info("ingest: using persistent CLI mode for LLM extraction", "model", ingestModel)
	}
	if len(cfg.Ingest.Sources) > 0 && ingestLLM != nil && len(cfg.Telegram.AllowedID) > 0 {
		ownerUID := cfg.Telegram.AllowedID[0]
		var ingestVS ingest.VectorIndexer
		if vectorStore != nil {
			ingestVS = vectorStore
		}

		sources, err := buildIngestSources(ctx, cfg, mcpMgr, ownerUID)
		if err != nil {
			slog.Error("failed to build ingest sources", "err", err)
		} else if len(sources) > 0 {
			ingestActor = ingest.New(sources, ingestLLM, store, ingestVS, ownerUID, ownerUID)
			// Set extractors now that actor exists (needs LLM sender).
			llmExtractor := ingestActor.MakeLLMExtractor()
			passthrough := &ingest.PassthroughExtractor{}
			for i := range sources {
				switch sources[i].ExtractionMode {
				case ingest.ExtractionPassthrough:
					sources[i].Extractor = passthrough
				case ingest.ExtractionHybrid:
					sources[i].Extractor = &ingest.HybridExtractor{LLM: llmExtractor, Passthrough: passthrough}
				default:
					sources[i].Extractor = llmExtractor
				}
			}
			slog.Info("ingest actor enabled", "sources", len(sources))
		}
	}

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
	var transcriber voice.Transcriber
	if cfg.Voice.Enabled {
		transcriber = voice.NewOpenAITranscriber(cfg.Voice.OpenAIAPIKey, cfg.Voice.STTModel)
		slog.Info("voice transcription enabled", "model", cfg.Voice.STTModel)
	}

	// Create observation extractor if enabled.
	var observer *memory.ObservationExtractor
	var obsStore session.ObservationStore
	if cfg.Memory.Enabled && cfg.Memory.Observations.Enabled {
		if cfg.Claude.APIKey != "" {
			// Direct API mode: use a dedicated client for extraction.
			obsModel := cfg.Memory.Observations.ExtractionModel
			if obsModel == "" {
				obsModel = "claude-haiku-4-5"
			}
			obsClient := claude.NewClient(cfg.Claude.AuthOption(), obsModel)
			observer = memory.NewObservationExtractor(func(ctx context.Context, system, user string) (string, error) {
				resp, err := obsClient.Send(ctx, claude.SendParams{
					SystemPrompt: system,
					Messages:     []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(user))},
					MaxTokens:    1024,
				})
				if err != nil {
					return "", err
				}
				return resp.TextContent, nil
			}, store)
		} else if cliManager != nil {
			// CLI mode: use SpawnOneShot with extraction_model override (cheaper model for extraction).
			extractionModel := cfg.Memory.Observations.ExtractionModel
			observer = memory.NewObservationExtractor(func(ctx context.Context, system, user string) (string, error) {
				proc, err := cliManager.SpawnOneShot(ctx, claude.SpawnParams{
					SystemPrompt: system,
					InitialMsg:   claude.BuildUserMessage(user),
					Model:        extractionModel,
				})
				if err != nil {
					return "", fmt.Errorf("cli observe: spawn: %w", err)
				}
				defer proc.Kill()
				var text strings.Builder
				_, err = proc.Send(ctx, nil, func(delta string) {
					text.WriteString(delta)
				}, nil)
				if err != nil {
					return "", fmt.Errorf("cli observe: send: %w", err)
				}
				return text.String(), nil
			}, store)
		}
		if observer != nil {
			obsStore = store
			slog.Info("observation memory enabled")

			// Register observation skills.
			skillObsStore := &obsSkillAdapter{store: store, vs: vectorStore, cfg: cfg}
			entityStore := &entitySkillAdapter{store: store}
			obsSkills, err := skills.InitObservationSkills(store.DB(), skillObsStore, entityStore)
			if err != nil {
				slog.Warn("failed to init observation skills", "err", err)
			} else {
				for _, s := range obsSkills {
					skillReg.Register(s)
				}
			}
			// Register supersede_observation skill (requires richer store interface).
			skillReg.Register(skills.InitSupersedeSkill(skillObsStore))
		}
	}
	sess := session.New(cfg, claudeClient, sessionCLI, tg, mcpMgr, store, skillReg, vectorStore, factStore, sessionSummarizer, configPath, extReg, transcriber, observer, obsStore, updateClient)

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

	// Start eval actor if enabled.
	actors := []actor.Actor{tg, sess, reminderActor}
	if cfg.Eval.Enabled {
		reportChatID := cfg.Eval.ReportChatID
		if reportChatID == 0 && len(cfg.Telegram.AllowedID) > 0 {
			reportChatID = cfg.Telegram.AllowedID[0]
		}
		// Create LLM caller for eval: prefer API mode, fall back to CLI mode.
		var evalLLM eval.LLMCaller
		if claudeClient != nil {
			evalLLM = eval.NewAPILLMCaller(claudeClient)
			slog.Info("eval: using API mode for LLM-as-judge")
		} else if cliManager != nil {
			evalLLM = eval.NewCLILLMCaller(cliManager)
			slog.Info("eval: using CLI mode for LLM-as-judge")
		}
		evalActor, evalErr := eval.NewActor(eval.ActorConfig{
			DBPath:              cfg.Storage.DBPath,
			Schedule:            cfg.Eval.Schedule,
			LookbackHours:       cfg.Eval.LookbackHours,
			ScoreThreshold:      cfg.Eval.ScoreThreshold,
			ReportChatID:        reportChatID,
			MaxCandidatesPerRun: cfg.Eval.MaxCandidatesPerRun,
			AutoCommit:          cfg.Eval.AutoCommit,
			CandidateExpiryDays: cfg.Eval.CandidateExpiryDays,
		}, store, tg.Inbox(), evalLLM)
		if evalErr != nil {
			slog.Error("eval actor creation failed", "err", evalErr)
		} else {
			actors = append(actors, evalActor)
			slog.Info("eval actor enabled", "schedule", cfg.Eval.Schedule)
		}
	}

	slog.Info("curlycatclaw started")

	// Post-update notification: detect version change via file on shared volume.
	go checkVersionChange(version, tg, cfg.Telegram.AllowedID)

	// Auto-update cron: periodically check for updates and apply if configured.
	if cfg.Update.Enabled && cfg.Update.AutoUpdate && updateClient != nil {
		notifyChatID := int64(0)
		if len(cfg.Telegram.AllowedID) > 0 {
			notifyChatID = cfg.Telegram.AllowedID[0]
		}
		go runAutoUpdate(ctx, cfg.Update.Schedule, updateClient, store, tg, notifyChatID)
	}

	// Add ingest actor if enabled.
	if ingestActor != nil {
		actors = append(actors, ingestActor)
	}

	// Run actors under supervision.
	actor.SuperviseAll(ctx, 30*time.Second, actors...)
	close(shutdownComplete)

	slog.Info("curlycatclaw stopped")
	return nil
}

// checkVersionChange detects a post-update restart by comparing the running
// version against the last known version stored in /data/.last-version. If
// different, it sends a notification to all allowed Telegram users.
func checkVersionChange(ver string, tg *telegram.Channel, allowedIDs []int64) {
	// Give the Telegram channel time to connect before sending.
	time.Sleep(5 * time.Second)

	const versionFile = "/data/.last-version"
	old, err := os.ReadFile(versionFile)
	if err == nil && strings.TrimSpace(string(old)) != ver && ver != "dev" {
		msg := fmt.Sprintf("Updated to v%s -- all systems operational.", ver)
		for _, id := range allowedIDs {
			tg.Inbox() <- telegram.OutgoingMessage{ChatID: id, Text: msg}
		}
		slog.Info("post-update notification sent", "old", strings.TrimSpace(string(old)), "new", ver)
	}
	_ = os.WriteFile(versionFile, []byte(ver), 0o644)
}

// runAutoUpdate starts a cron-scheduled auto-update loop. It checks for available
// updates and applies them if no conversation is active (no message in last 5 minutes).
func runAutoUpdate(ctx context.Context, schedule string, client *update.Client, store *memory.Store, tg *telegram.Channel, notifyChatID int64) {
	sched, err := gocron.NewScheduler()
	if err != nil {
		slog.Error("auto-update: failed to create scheduler", "err", err)
		return
	}

	_, err = sched.NewJob(
		gocron.CronJob(schedule, false),
		gocron.NewTask(func() {
			slog.Info("auto-update: checking for updates")

			status, err := client.Check(ctx)
			if err != nil {
				slog.Warn("auto-update: check failed", "err", err)
				return
			}
			if !status.UpdateAvailable {
				slog.Info("auto-update: already up to date", "version", status.CurrentVersion)
				return
			}

			// Check for active conversations: skip if any user sent a message in the last 5 minutes.
			if hasRecentActivity(store) {
				slog.Info("auto-update: conversation active, deferring update")
				return
			}

			slog.Info("auto-update: applying update", "from", status.CurrentVersion, "to", status.LatestVersion)
			if notifyChatID != 0 {
				tg.Inbox() <- telegram.OutgoingMessage{
					ChatID: notifyChatID,
					Text:   fmt.Sprintf("Auto-updating to v%s... I'll be right back.", status.LatestVersion),
				}
			}

			if err := client.Update(ctx); err != nil {
				slog.Error("auto-update: update failed", "err", err)
				if notifyChatID != 0 {
					tg.Inbox() <- telegram.OutgoingMessage{
						ChatID: notifyChatID,
						Text:   fmt.Sprintf("Auto-update failed: %v", err),
					}
				}
			}
		}),
	)
	if err != nil {
		slog.Error("auto-update: failed to schedule job", "err", err)
		return
	}

	sched.Start()
	slog.Info("auto-update: scheduler started", "schedule", schedule)
	<-ctx.Done()
	_ = sched.Shutdown()
}

// hasRecentActivity checks if any user sent a message in the last 5 minutes.
func hasRecentActivity(store *memory.Store) bool {
	var count int
	err := store.DB().QueryRow(
		`SELECT COUNT(*) FROM messages WHERE role = 'user' AND created_at > datetime('now', '-5 minutes')`,
	).Scan(&count)
	if err != nil {
		slog.Warn("auto-update: failed to check recent activity", "err", err)
		return true // err on the side of caution
	}
	return count > 0
}

// embedderModel extracts the model name from a VectorConfig for the configured embedder type.
func embedderModel(cfg config.VectorConfig) string {
	switch cfg.Embedder {
	case "ollama":
		m := cfg.OllamaModel
		if m == "" {
			m = "bge-m3"
		}
		return m
	case "voyage":
		m := cfg.VoyageModel
		if m == "" {
			m = "voyage-3-lite"
		}
		return m
	default:
		return ""
	}
}

// buildIngestSources creates Source entries from the ingest config.
func buildIngestSources(ctx context.Context, cfg *config.Config, mcpRouter ingest.ToolRouter, ownerUID int64) ([]ingest.SourceEntry, error) {
	var entries []ingest.SourceEntry

	for _, src := range cfg.Ingest.Sources {
		if !src.Enabled {
			continue
		}

		trustLevel := ingest.TrustUntrusted
		if src.TrustLevel == "trusted" {
			trustLevel = ingest.TrustTrusted
		}

		extractionMode := ingest.ExtractionLLM
		switch src.Extraction {
		case "passthrough":
			extractionMode = ingest.ExtractionPassthrough
		case "hybrid":
			extractionMode = ingest.ExtractionHybrid
		}

		interval := time.Duration(src.IntervalMinutes) * time.Minute

		// Apply defaults for zero-value caps (prevents blocking all processing).
		maxDailyObs := src.MaxDailyObservations
		if maxDailyObs == 0 {
			maxDailyObs = 100
		}
		maxDailyLLM := src.MaxDailyLLMCalls
		if maxDailyLLM == 0 {
			maxDailyLLM = 200
		}
		maxBodyChars := src.MaxBodyChars
		if maxBodyChars == 0 {
			maxBodyChars = 4000
		}

		switch src.Type {
		case "gmail":
			// Discover Gmail accounts for multi-account support.
			accounts, err := ingest.DiscoverGmailAccounts(ctx, mcpRouter, ownerUID, ownerUID)
			if err != nil {
				slog.Warn("gmail account discovery failed, using single-account", "err", err)
				accounts = []string{""}
			}

			// Filter to configured accounts if specified.
			if len(src.Accounts) > 0 {
				allowed := make(map[string]bool, len(src.Accounts))
				for _, a := range src.Accounts {
					allowed[a] = true
				}
				var filtered []string
				for _, a := range accounts {
					if allowed[a] {
						filtered = append(filtered, a)
					}
				}
				slog.Info("gmail account filter applied", "discovered", len(accounts), "allowed", len(filtered), "accounts", filtered)
				accounts = filtered
			}

			for _, account := range accounts {
				gmailSrc := ingest.NewGmailSource(ingest.GmailSourceConfig{
					Name:        src.Name,
					Account:     account,
					MCP:         mcpRouter,
					OwnerUID:    ownerUID,
					OwnerCID:    ownerUID,
					Labels:      src.Prefilter.Labels,
					SkipSenders: src.Prefilter.SkipSenders,
				})
				entries = append(entries, ingest.SourceEntry{
					Source:         gmailSrc,
					TrustLevel:     trustLevel,
					ExtractionMode: extractionMode,
					ChatType:       "email",
					Partition:      account,
					Interval:       interval,
					BackfillDays:   src.BackfillDays,
					MaxDailyObs:    maxDailyObs,
					MaxDailyLLM:    maxDailyLLM,
					MinImportance:  src.MinImportance,
					MaxBodyChars:   maxBodyChars,
				})
			}

		case "file":
			fileSrc := ingest.NewFileSource(ingest.FileSourceConfig{
				Name:         src.Name,
				RootDir:      src.RootDir,
				Patterns:     src.Patterns,
				IncludePaths: src.Prefilter.IncludePaths,
				ExcludePaths: src.Prefilter.ExcludePaths,
			})
			entries = append(entries, ingest.SourceEntry{
				Source:         fileSrc,
				TrustLevel:     trustLevel,
				ExtractionMode: extractionMode,
				ChatType:       src.Name,
				Interval:       interval,
				BackfillDays:   src.BackfillDays,
				MaxDailyObs:    maxDailyObs,
				MaxDailyLLM:    maxDailyLLM,
				MinImportance:  src.MinImportance,
				MaxBodyChars:   maxBodyChars,
			})

		case "notion":
			notionSrc := ingest.NewNotionSource(ingest.NotionSourceConfig{
				Name:     src.Name,
				MCP:      mcpRouter,
				OwnerUID: ownerUID,
				OwnerCID: ownerUID,
			})
			entries = append(entries, ingest.SourceEntry{
				Source:         notionSrc,
				TrustLevel:     trustLevel,
				ExtractionMode: extractionMode,
				ChatType:       "notion",
				Interval:       interval,
				BackfillDays:   src.BackfillDays,
				MaxDailyObs:    maxDailyObs,
				MaxDailyLLM:    maxDailyLLM,
				MinImportance:  src.MinImportance,
				MaxBodyChars:   maxBodyChars,
			})
		}
	}

	// Extractors are set by the caller after actor creation (needs LLM sender).
	return entries, nil
}

// reconstructEmbedder recreates the old embedder from stored state and current config.
// Returns nil if reconstruction is not possible.
func reconstructEmbedder(state *memory.EmbedderState, cfg config.VectorConfig) memory.Embedder {
	switch state.OldEmbedderType {
	case "ollama":
		return memory.NewOllamaEmbedder(cfg.OllamaURL, state.OldEmbedderModel, uint64(state.OldEmbedderDim))
	case "voyage":
		if cfg.VoyageKey == "" {
			slog.Warn("cannot reconstruct voyage embedder: no API key in current config")
			return nil
		}
		return memory.NewVoyageEmbedder(cfg.VoyageKey, state.OldEmbedderModel, uint64(state.OldEmbedderDim))
	case "fnv", "":
		return memory.FNVEmbedder{}
	default:
		slog.Warn("unknown old embedder type", "type", state.OldEmbedderType)
		return nil
	}
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
// Binds to all interfaces (for Docker sidecar access) and sets conservative timeouts to prevent slowloris.
func startHealthServer(ctx context.Context, port int) {
	srv := &http.Server{
		Addr:              net.JoinHostPort("0.0.0.0", fmt.Sprintf("%d", port)),
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

// ensureIsolatedHome creates the isolated home directory structure for CLI
// project work. It only symlinks ~/.ssh/known_hosts (not the whole .ssh dir),
// copies ~/.gitconfig, and skips .gnupg entirely.
func ensureIsolatedHome(homePath string) error {
	// Create main directory and plugin dir.
	pluginDir := filepath.Join(homePath, ".claude", "plugins")
	if err := os.MkdirAll(pluginDir, 0700); err != nil {
		return fmt.Errorf("create plugin dir: %w", err)
	}

	// Set up .ssh directory: only symlink known_hosts.
	sshDir := filepath.Join(homePath, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		return fmt.Errorf("create .ssh dir: %w", err)
	}

	realHome, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("get user home: %w", err)
	}

	knownHostsSrc := filepath.Join(realHome, ".ssh", "known_hosts")
	knownHostsDst := filepath.Join(sshDir, "known_hosts")
	if _, err := os.Stat(knownHostsSrc); err == nil {
		// Only create symlink if it doesn't already exist.
		if _, err := os.Lstat(knownHostsDst); os.IsNotExist(err) {
			if err := os.Symlink(knownHostsSrc, knownHostsDst); err != nil {
				return fmt.Errorf("symlink known_hosts: %w", err)
			}
		}
	}

	// Copy .gitconfig (not symlink, so isolated env can diverge).
	gitconfigSrc := filepath.Join(realHome, ".gitconfig")
	gitconfigDst := filepath.Join(homePath, ".gitconfig")
	if _, err := os.Stat(gitconfigSrc); err == nil {
		if _, err := os.Stat(gitconfigDst); os.IsNotExist(err) {
			data, err := os.ReadFile(gitconfigSrc)
			if err != nil {
				return fmt.Errorf("read .gitconfig: %w", err)
			}
			if err := os.WriteFile(gitconfigDst, data, 0644); err != nil {
				return fmt.Errorf("write .gitconfig: %w", err)
			}
		}
	}

	// Create minimal .claude/settings.json if it doesn't exist.
	settingsPath := filepath.Join(homePath, ".claude", "settings.json")
	if _, err := os.Stat(settingsPath); os.IsNotExist(err) {
		settings := []byte(`{"permissions":{},"preferences":{}}` + "\n")
		if err := os.WriteFile(settingsPath, settings, 0644); err != nil {
			return fmt.Errorf("write settings.json: %w", err)
		}
	}

	return nil
}

// obsSkillAdapter bridges the skills.ObservationStore interface to the real
// memory.Store and memory.VectorStore implementations.
type obsSkillAdapter struct {
	store *memory.Store
	vs    *memory.VectorStore
	cfg   *config.Config
}

func (a *obsSkillAdapter) SearchObservations(ctx context.Context, query string, userID int64, obsType string, limit int) ([]skills.ObservationSearchResult, error) {
	if a.vs == nil {
		return nil, fmt.Errorf("vector store not configured")
	}
	threshold := float32(a.cfg.Memory.Observations.ScoreThreshold)
	if threshold <= 0 {
		threshold = 0.3
	}
	// When filtering by type, over-fetch from Qdrant since post-filtering
	// may discard results of other types.
	fetchLimit := limit
	if obsType != "" {
		fetchLimit = limit * 3
	}
	results, err := a.vs.SearchObservations(ctx, query, userID, 0, "private", fetchLimit, threshold)
	if err != nil {
		return nil, err
	}
	var out []skills.ObservationSearchResult
	for _, r := range results {
		if obsType != "" && r.Type != obsType {
			continue
		}
		out = append(out, skills.ObservationSearchResult{
			ID:        r.ID,
			Title:     r.Title,
			Type:      r.Type,
			Score:     r.Score,
			CreatedAt: r.CreatedAt,
		})
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (a *obsSkillAdapter) DeleteObservation(id string, userID int64) error {
	return a.store.DeleteObservation(id, userID)
}

func (a *obsSkillAdapter) DeleteObservationVector(ctx context.Context, id string) error {
	if a.vs == nil {
		return nil
	}
	return a.vs.DeleteObservationVector(ctx, id)
}

func (a *obsSkillAdapter) ArchiveObservation(id string, userID int64) error {
	return a.store.ArchiveObservation(id, userID)
}

func (a *obsSkillAdapter) RestoreObservation(id string, userID int64) error {
	return a.store.RestoreObservation(id, userID)
}

func (a *obsSkillAdapter) UpdateObservation(id string, userID int64, title, summary, obsType string, importance int) error {
	return a.store.UpdateObservation(id, userID, title, summary, obsType, importance)
}

func (a *obsSkillAdapter) SaveNewObservation(userID, chatID int64, chatType, obsType, title, summary, contentHash string, facts []string, importance int) (string, error) {
	obs := memory.Observation{
		UserID:      userID,
		ChatID:      chatID,
		ChatType:    chatType,
		Type:        obsType,
		Title:       title,
		Summary:     summary,
		Facts:       facts,
		Importance:  importance,
		ContentHash: contentHash,
	}
	if err := a.store.SaveObservation(&obs); err != nil {
		return "", err
	}
	// Best-effort Qdrant indexing.
	if a.vs != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := a.vs.IndexObservation(ctx, obs); err != nil {
			slog.Warn("supersede: vector index failed", "err", err, "obs", obs.ID)
		}
	}
	return obs.ID, nil
}

func (a *obsSkillAdapter) AddObservationRelation(sourceID, targetID, relationType string, confidence float64, userID int64) error {
	return a.store.AddObservationRelation(sourceID, targetID, relationType, confidence, userID)
}

func (a *obsSkillAdapter) ObservationExistsByHash(userID int64, hash string) (bool, error) {
	return a.store.ObservationExistsByHash(userID, hash)
}

// entitySkillAdapter bridges the skills.EntityStore interface to memory.Store.
type entitySkillAdapter struct {
	store *memory.Store
}

func (a *entitySkillAdapter) SearchEntitiesFTS(query string, entityType string, userID int64, limit int) ([]skills.EntitySearchResult, error) {
	results, err := a.store.SearchEntitiesFTS(query, entityType, userID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]skills.EntitySearchResult, len(results))
	for i, r := range results {
		out[i] = skills.EntitySearchResult{
			ObservationID: r.ObservationID,
			Name:          r.Name,
			EntityType:    r.EntityType,
		}
	}
	return out, nil
}
