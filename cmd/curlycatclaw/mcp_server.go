package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"

	"github.com/jialuohu/curlycatclaw/config"
	"github.com/jialuohu/curlycatclaw/internal/memory"
	"github.com/jialuohu/curlycatclaw/internal/skillloader"
	"github.com/jialuohu/curlycatclaw/skills"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// runMCPServer starts curlycatclaw as an MCP stdio server, exposing built-in
// skills as MCP tools. This is spawned by the claude CLI via --mcp-config.
//
// User scoping is passed via environment variables (not CLI args, to avoid
// leaking user IDs in /proc/PID/cmdline):
//
//	CURLYCATCLAW_USER_ID=123
//	CURLYCATCLAW_CHAT_ID=456
//	CURLYCATCLAW_DB_PATH=/path/to/data.db
//	CURLYCATCLAW_CONFIG=/path/to/config.toml
func runMCPServer() error {
	userID, err := strconv.ParseInt(os.Getenv("CURLYCATCLAW_USER_ID"), 10, 64)
	if err != nil {
		return fmt.Errorf("CURLYCATCLAW_USER_ID: %w", err)
	}
	chatID, err := strconv.ParseInt(os.Getenv("CURLYCATCLAW_CHAT_ID"), 10, 64)
	if err != nil {
		return fmt.Errorf("CURLYCATCLAW_CHAT_ID: %w", err)
	}

	configPath := os.Getenv("CURLYCATCLAW_CONFIG")
	if configPath == "" {
		configPath = defaultConfigPath()
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	dbPath := os.Getenv("CURLYCATCLAW_DB_PATH")
	if dbPath == "" {
		dbPath = cfg.Storage.DBPath
	}

	// Open SQLite (WAL mode for concurrent access with main process).
	store, err := memory.NewStore(dbPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer store.Close()

	// Build skill registry.
	reg := skills.NewRegistry()
	reg.Register(skills.NewWebSearchSkill())

	noteSkills, err := skills.InitNoteSkills(store.DB())
	if err != nil {
		slog.Warn("mcp-server: note skills init failed", "err", err)
	} else {
		for _, s := range noteSkills {
			reg.Register(s)
		}
	}

	// Remind skills need a signal channel but we don't process reminders in MCP mode.
	// Use a buffered channel; drain it in the background to avoid blocking.
	remindSignalCh := make(chan int64, 64)
	go func() {
		for range remindSignalCh {
		}
	}()
	remindSkills, err := skills.InitRemindSkills(store.DB(), remindSignalCh, cfg.Location())
	if err != nil {
		slog.Warn("mcp-server: remind skills init failed", "err", err)
	} else {
		for _, s := range remindSkills {
			reg.Register(s)
		}
	}

	// Fact skills.
	if cfg.Memory.Enabled {
		factStore := memory.NewFactStore(store.DB(), cfg.Memory.MaxFacts)
		for _, s := range skills.InitFactSkills(factStore) {
			reg.Register(s)
		}
	}

	// Plugin management skills (optional, requires CLI + isolated home).
	cliPath := os.Getenv("CURLYCATCLAW_CLI_PATH")
	isolatedHome := os.Getenv("CURLYCATCLAW_ISOLATED_HOME")
	if cliPath != "" && isolatedHome != "" {
		for _, s := range skills.InitPluginSkills(cliPath, isolatedHome, cfg.Claude.AllowedPlugins) {
			reg.Register(s)
		}
	}

	// External skill collections.
	if len(cfg.SkillCollections) > 0 {
		loader := skillloader.New(reg)
		if err := loader.LoadAll(context.Background(), cfg.SkillCollections); err != nil {
			slog.Warn("mcp-server: skill collections", "err", err)
		}
		// No hot-reload in MCP server subprocess (short-lived).
	}

	// Semantic search (optional, requires Qdrant).
	if cfg.Vector.Enabled {
		embedder := newEmbedder(cfg.Vector)
		ctx := context.Background()
		vs, err := memory.NewVectorStore(ctx, cfg.Vector.QdrantAddr, embedder)
		if err != nil {
			slog.Warn("mcp-server: vector store init failed", "err", err)
		} else {
			defer vs.Close()
			reg.Register(skills.NewSemanticSearchSkill(vs))
		}
	}

	// Create MCP server and register all skills as tools.
	server := mcp.NewServer(
		&mcp.Implementation{Name: "curlycatclaw-skills", Version: version},
		nil,
	)

	for _, skill := range reg.All() {
		registerSkillAsTool(server, skill, userID, chatID)
	}

	slog.Info("mcp-server: starting",
		"user_id", userID,
		"chat_id", chatID,
		"tools", len(reg.All()))

	// Run over stdio until the parent CLI process disconnects.
	return server.Run(context.Background(), &mcp.StdioTransport{})
}

// errResult creates an MCP tool error result.
func errResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
		IsError: true,
	}
}

// skillOutput is the structured output type for MCP tool results.
type skillOutput struct {
	Text string `json:"text"`
}

// registerSkillAsTool wraps a built-in Skill as an MCP tool on the server.
// Uses the generic mcp.AddTool with map[string]any input to handle arbitrary
// JSON schemas from each skill without needing typed structs.
func registerSkillAsTool(server *mcp.Server, skill *skills.Skill, userID, chatID int64) {
	tool := &mcp.Tool{
		Name:        skill.Name,
		Description: skill.Description,
	}

	skillRef := skill // capture for closure

	mcp.AddTool(server, tool, func(
		ctx context.Context,
		req *mcp.CallToolRequest,
		input map[string]any,
	) (*mcp.CallToolResult, skillOutput, error) {
		// Inject user identity into context.
		ctx = skills.WithUser(ctx, skills.UserInfo{
			UserID: userID,
			ChatID: chatID,
		})

		// Marshal the arguments back to JSON for the skill.
		inputJSON, err := json.Marshal(input)
		if err != nil {
			return errResult(fmt.Sprintf("invalid input: %v", err)), skillOutput{}, nil
		}

		result, execErr := skillRef.Execute(ctx, inputJSON)
		if execErr != nil {
			return errResult(execErr.Error()), skillOutput{}, nil
		}

		return nil, skillOutput{Text: result}, nil
	})
}
