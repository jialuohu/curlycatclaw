package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"path/filepath"
	"time"

	"encoding/hex"
	"net/http"

	"github.com/jialuohu/curlycatclaw/config"
	"github.com/jialuohu/curlycatclaw/internal/extension"
	"github.com/jialuohu/curlycatclaw/internal/memory"
	"github.com/jialuohu/curlycatclaw/internal/security"
	"github.com/jialuohu/curlycatclaw/internal/skillloader"
	"github.com/jialuohu/curlycatclaw/skills"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpProxySep is the separator for namespacing proxied MCP extension tools.
const mcpProxySep = "__"

// dangerousEnvPrefixes lists env var prefixes that should not be passed
// to MCP server subprocesses to prevent library injection attacks.
// Duplicated from internal/session (different package).
var dangerousEnvPrefixes = []string{"LD_PRELOAD", "LD_LIBRARY_PATH", "DYLD_"}

// baselineEnvAllowlist is the minimum set of environment variables that MCP
// child processes need to function (find binaries, set locale, etc.).
// Matches the allowlist in internal/mcp/manager.go.
var baselineEnvAllowlist = map[string]struct{}{
	"PATH": {}, "HOME": {}, "USER": {}, "LANG": {}, "LC_ALL": {},
	"SHELL": {}, "TMPDIR": {}, "TZ": {}, "XDG_RUNTIME_DIR": {},
	// Playwright (needed by scrapling-mcp browser tools).
	"PLAYWRIGHT_BROWSERS_PATH": {},
}

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
	// Note: web_search is NOT registered here because the CLI subprocess
	// already has a built-in WebSearch tool. It IS registered in main.go
	// for direct API mode where no CLI is available.
	reg := skills.NewRegistry()

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

	// Observation skills (requires memory + observations enabled).
	// VectorStore is connected later (line ~242); observation skills that need
	// vector search (search_observations) will fail gracefully if vs is nil.
	// Skills that only need SQLite/FTS5 (list, get, forget, search_entities) always work.
	var mcpObsAdapter *obsSkillAdapter
	if cfg.Memory.Enabled && cfg.Memory.Observations.Enabled {
		mcpObsAdapter = &obsSkillAdapter{store: store, vs: nil, cfg: cfg}
		entStore := &entitySkillAdapter{store: store}
		obsSkills, err := skills.InitObservationSkills(store.DB(), mcpObsAdapter, entStore)
		if err != nil {
			slog.Warn("mcp-server: observation skills init failed", "err", err)
		} else {
			for _, s := range obsSkills {
				reg.Register(s)
			}
		}
		reg.Register(skills.InitSupersedeSkill(mcpObsAdapter))
	}

	// send_file skill (queued mode — files delivered by session actor after tool loop).
	reg.Register(skills.NewSendFileSkill(&skills.QueuedDocumentSender{Queue: store}))

	// Plugin management skills (optional, requires CLI + isolated home).
	cliPath := os.Getenv("CURLYCATCLAW_CLI_PATH")
	isolatedHome := os.Getenv("CURLYCATCLAW_ISOLATED_HOME")
	if cliPath != "" && isolatedHome != "" {
		for _, s := range skills.InitPluginSkills(cliPath, isolatedHome) {
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

	// Credential store for encrypted extension env vars (optional).
	// Read master key from file (avoids /proc/PID/cmdline exposure),
	// falling back to env var for direct API mode.
	var credStore *security.CredentialStore
	mkHex := os.Getenv("CURLYCATCLAW_MASTER_KEY")
	if mkHex == "" {
		if mkFile := os.Getenv("CURLYCATCLAW_MASTER_KEY_FILE"); mkFile != "" {
			data, err := os.ReadFile(mkFile)
			if err != nil {
				slog.Warn("mcp-server: failed to read master key file", "err", err)
			} else {
				mkHex = strings.TrimSpace(string(data))
			}
		}
	}
	if mkHex != "" {
		masterKey, err := hex.DecodeString(mkHex)
		if err != nil {
			slog.Warn("mcp-server: invalid master key (not hex)", "err", err)
		} else {
			credPath := filepath.Join(filepath.Dir(dbPath), "credentials.enc")
			cs, err := security.NewCredentialStore(credPath, masterKey)
			if err != nil {
				slog.Warn("mcp-server: credential store init failed", "err", err)
			} else {
				credStore = cs
			}
		}
	}

	// Runtime extension registry (exec extensions + management skills).
	var server *mcp.Server
	var hotReloader *mcpHotReloader
	extRegistryPath := filepath.Join(filepath.Dir(dbPath), "extensions.json")
	extReg, err := extension.Load(extRegistryPath)
	if err != nil {
		slog.Warn("mcp-server: extension registry load failed", "err", err)
	} else {
		for _, ext := range extReg.ByType(extension.TypeExec) {
			adapter := skillloader.NewExecAdapter(ext.Command, ext.Args, "", ext.Env, 30*time.Second)
			registryName := extension.ExecSkillPrefix + ext.Name
			extCopy := ext // capture for closure
			reg.Register(&skills.Skill{
				Name:        registryName,
				Description: extCopy.Description,
				InputSchema: extCopy.InputSchema,
				Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
					user := skills.GetUser(ctx)
					return adapter.Execute(ctx, input, user)
				},
			})
		}

		extReloadFunc := func() {
			ih := os.Getenv("CURLYCATCLAW_ISOLATED_HOME")
			if ih != "" {
				path := filepath.Join(ih, ".curlycatclaw-reload-needed")
				os.WriteFile(path, []byte("1"), 0644) //nolint:errcheck
			}
		}

		// Create MCP server early so the hot-reloader can reference it.
		// Skills are registered on the server after all skill init is done.
		server = mcp.NewServer(
			&mcp.Implementation{Name: "curlycatclaw-skills", Version: version},
			nil,
		)

		// Create hot-reloader for dynamic MCP extension tool management.
		hotReloader = newMCPHotReloader(server, userID, chatID, credStore)

		// mcpMgr is nil in MCP server subprocess mode.
		var cfgServers []extension.ConfigMCPServer
		for _, srv := range cfg.MCP.Servers {
			cfgServers = append(cfgServers, extension.ConfigMCPServer{Name: srv.Name, Command: srv.Command})
		}
		for _, s := range extension.InitExtensionSkills(extReg, nil, reg, extReloadFunc, hotReloader, credStore, cfgServers) {
			reg.Register(s)
		}
	}

	// Semantic search (optional, requires Qdrant).
	if cfg.Vector.Enabled {
		embedder := newEmbedder(cfg.Vector)
		slog.Info("mcp-server: embedder configured", "name", embedder.Name(), "dim", embedder.Dimension())
		ctx := context.Background()
		vs, err := memory.NewVectorStore(ctx, cfg.Vector.QdrantAddr, embedder)
		if err != nil {
			slog.Warn("mcp-server: vector store init failed", "err", err)
		} else {
			defer vs.Close()
			reg.Register(skills.NewSemanticSearchSkill(vs))
			// Wire VectorStore into observation skill adapter for search_observations.
			if mcpObsAdapter != nil {
				mcpObsAdapter.vs = vs
			}
		}
	}

	// Create MCP server if not already created (no extension registry case).
	if server == nil {
		server = mcp.NewServer(
			&mcp.Implementation{Name: "curlycatclaw-skills", Version: version},
			nil,
		)
	}

	for _, skill := range reg.All() {
		registerSkillAsTool(server, skill, userID, chatID)
	}

	// Load existing MCP extensions via the hot-reloader (same path used
	// at runtime when add_extension is called). This unifies startup and
	// runtime extension loading.
	var proxyToolCount int
	if hotReloader != nil && extReg != nil {
		defer hotReloader.CloseAll()
		for _, ext := range extReg.ByType(extension.TypeMCP) {
			if _, _, err := hotReloader.ConnectAndRegister(context.Background(), ext); err != nil {
				slog.Warn("mcp-server: failed to connect MCP extension",
					"name", ext.Name, "err", err)
				continue
			}
			proxyToolCount += len(hotReloader.toolsFor(ext.Name))
			slog.Info("mcp-server: proxying MCP extension",
				"name", ext.Name, "tools", len(hotReloader.toolsFor(ext.Name)))
		}
	}

	// Proxy config-based MCP servers ([[mcp.servers]] in config.toml).
	// Stdio servers go through the extension hot-reloader (subprocess).
	// HTTP servers connect directly via Streamable HTTP transport.
	var configProxyCount int
	for _, srv := range cfg.MCP.Servers {
		if srv.Transport == "http" {
			// HTTP transport: connect directly, bypassing the stdio extension path.
			if hotReloader != nil {
				count, err := hotReloader.ConnectHTTPAndRegister(context.Background(), srv)
				if err != nil {
					slog.Warn("mcp-server: failed to proxy HTTP MCP server",
						"name", srv.Name, "url", srv.URL, "err", err)
					continue
				}
				configProxyCount += count
				slog.Info("mcp-server: proxying HTTP MCP server",
					"name", srv.Name, "url", srv.URL, "tools", count)
			}
			continue
		}
		// Stdio transport: wrap as extension and connect via subprocess.
		ext := &extension.Extension{
			Name:    srv.Name,
			Type:    extension.TypeMCP,
			Command: srv.Command,
			Args:    srv.Args,
			Env:     srv.Env,
		}
		if hotReloader != nil {
			if _, _, err := hotReloader.ConnectAndRegister(context.Background(), ext); err != nil {
				slog.Warn("mcp-server: failed to proxy config MCP server",
					"name", srv.Name, "err", err)
				continue
			}
			configProxyCount += len(hotReloader.toolsFor(srv.Name))
			slog.Info("mcp-server: proxying config MCP server",
				"name", srv.Name, "tools", len(hotReloader.toolsFor(srv.Name)))
		}
	}

	slog.Info("mcp-server: starting",
		"user_id", userID,
		"chat_id", chatID,
		"skills", len(reg.All()),
		"proxied_mcp_tools", proxyToolCount,
		"proxied_config_tools", configProxyCount)

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

// mcpHotReloader implements extension.MCPHotReloader for dynamic MCP
// extension tool registration without subprocess restart.
type mcpHotReloader struct {
	server    *mcp.Server
	userID    int64
	chatID    int64
	credStore *security.CredentialStore

	mu       sync.Mutex
	sessions map[string]*mcp.ClientSession
	tools    map[string][]string // ext name → namespaced tool names
}

func newMCPHotReloader(server *mcp.Server, userID, chatID int64, credStore *security.CredentialStore) *mcpHotReloader {
	return &mcpHotReloader{
		server:    server,
		userID:    userID,
		chatID:    chatID,
		credStore: credStore,
		sessions:  make(map[string]*mcp.ClientSession),
		tools:     make(map[string][]string),
	}
}

func (r *mcpHotReloader) ConnectAndRegister(ctx context.Context, ext *extension.Extension) ([]string, func(), error) {
	resolvedEnv := ext.Env
	if r.credStore != nil && len(ext.Env) > 0 {
		var err error
		resolvedEnv, err = r.credStore.ResolveEnv(ext.Env)
		if err != nil {
			return nil, nil, fmt.Errorf("resolve env: %w", err)
		}
	}

	session, tools, err := connectMCPExtension(ctx, ext, resolvedEnv)
	if err != nil {
		return nil, nil, err
	}
	if len(tools) == 0 {
		session.Close()
		return nil, nil, fmt.Errorf("MCP extension %q has no tools", ext.Name)
	}

	var toolNames []string
	var toolDescs []string
	for _, tool := range tools {
		namespacedName := ext.Name + mcpProxySep + tool.Name
		registerProxyTool(r.server, namespacedName, tool, session, r.userID, r.chatID, true)
		toolNames = append(toolNames, namespacedName)
		desc := tool.Name
		if tool.Description != "" {
			desc += " — " + tool.Description
		}
		toolDescs = append(toolDescs, desc)
	}

	// Swap session and tools, capturing the old session for the caller to close.
	// Remove any stale tools that existed in the old set but not the new one
	// (the extension's tool set may change across env updates or upgrades).
	r.mu.Lock()
	oldSession := r.sessions[ext.Name]
	oldToolNames := r.tools[ext.Name]
	r.sessions[ext.Name] = session
	r.tools[ext.Name] = toolNames
	r.mu.Unlock()

	// Diff old vs new tool names; remove any that disappeared.
	if len(oldToolNames) > 0 {
		newSet := make(map[string]struct{}, len(toolNames))
		for _, n := range toolNames {
			newSet[n] = struct{}{}
		}
		var stale []string
		for _, n := range oldToolNames {
			if _, ok := newSet[n]; !ok {
				stale = append(stale, n)
			}
		}
		if len(stale) > 0 {
			r.server.RemoveTools(stale...)
			slog.Info("mcp-server: removed stale proxy tools", "name", ext.Name, "removed", stale)
		}
	}

	var oldCloser func()
	if oldSession != nil {
		oldCloser = func() { oldSession.Close() }
	}

	slog.Info("mcp-server: hot-reloaded MCP extension", "name", ext.Name, "tools", len(tools))
	return toolDescs, oldCloser, nil
}

// ConnectHTTPAndRegister connects to a remote MCP server via Streamable HTTP
// and registers its tools. Used for config servers with transport = "http".
func (r *mcpHotReloader) ConnectHTTPAndRegister(ctx context.Context, srv config.MCPServerConfig) (int, error) {
	// Resolve encrypted header values.
	resolvedHeaders := make(map[string]string, len(srv.Headers))
	for k, v := range srv.Headers {
		resolved := v
		if r.credStore != nil && strings.HasPrefix(v, "encrypted:ref:") {
			got, err := r.credStore.Get(strings.TrimPrefix(v, "encrypted:ref:"))
			if err != nil {
				return 0, fmt.Errorf("resolve header %q: %w", k, err)
			}
			resolved = got
		}
		resolvedHeaders[k] = resolved
	}

	// Reserved headers that the SDK manages internally.
	reserved := map[string]struct{}{
		"content-type": {}, "accept": {}, "mcp-session-id": {},
	}

	httpClient := &http.Client{
		Transport: headerRoundTripper{resolvedHeaders, reserved},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Timeout: 60 * time.Second,
	}

	transport := &mcp.StreamableClientTransport{
		Endpoint:             srv.URL,
		HTTPClient:           httpClient,
		DisableStandaloneSSE: true,
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "curlycatclaw", Version: version}, nil)
	connectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	session, err := client.Connect(connectCtx, transport, nil)
	if err != nil {
		return 0, fmt.Errorf("connect http %q: %w", srv.URL, err)
	}

	var tools []*mcp.Tool
	for tool, err := range session.Tools(connectCtx, nil) {
		if err != nil {
			slog.Warn("mcp-server: error listing HTTP tools", "name", srv.Name, "error", err)
			break
		}
		tools = append(tools, tool)
	}
	if len(tools) == 0 {
		session.Close()
		return 0, fmt.Errorf("HTTP MCP server %q has no tools", srv.Name)
	}

	var toolNames []string
	for _, tool := range tools {
		namespacedName := srv.Name + mcpProxySep + tool.Name
		registerProxyTool(r.server, namespacedName, tool, session, r.userID, r.chatID, false)
		toolNames = append(toolNames, namespacedName)
	}

	r.mu.Lock()
	r.sessions[srv.Name] = session
	r.tools[srv.Name] = toolNames
	r.mu.Unlock()

	return len(tools), nil
}

// headerRoundTripper injects static headers into HTTP requests for MCP servers.
// Clones the request to satisfy the http.RoundTripper contract.
type headerRoundTripper struct {
	headers  map[string]string
	reserved map[string]struct{}
}

func (h headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	for k, v := range h.headers {
		if _, ok := h.reserved[strings.ToLower(k)]; ok {
			continue
		}
		clone.Header.Set(k, v)
	}
	return http.DefaultTransport.RoundTrip(clone)
}

func (r *mcpHotReloader) DisconnectAndUnregister(name string) error {
	r.mu.Lock()
	session := r.sessions[name]
	toolNames := r.tools[name]
	delete(r.sessions, name)
	delete(r.tools, name)
	r.mu.Unlock()

	if len(toolNames) > 0 {
		r.server.RemoveTools(toolNames...)
	}
	if session != nil {
		session.Close()
	}

	slog.Info("mcp-server: hot-unloaded MCP extension", "name", name)
	return nil
}

// CloseAll closes all tracked MCP extension sessions. Called on shutdown.
func (r *mcpHotReloader) CloseAll() {
	r.mu.Lock()
	defer r.mu.Unlock()

	for name, session := range r.sessions {
		session.Close()
		delete(r.sessions, name)
		delete(r.tools, name)
	}
}

// toolsFor returns a copy of the tracked tool names for an extension (for logging).
func (r *mcpHotReloader) toolsFor(name string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.tools[name]...)
}

// connectMCPExtension starts an MCP client connection to a runtime MCP
// extension and discovers its tools. The caller must defer session.Close().
//
// A 30-second timeout covers both the initial handshake and tool discovery.
// If the extension hangs (e.g. package download), it is skipped instead of
// blocking the entire curlycatclaw-skills subprocess.
func connectMCPExtension(ctx context.Context, ext *extension.Extension, resolvedEnv map[string]string) (*mcp.ClientSession, []*mcp.Tool, error) {
	env := buildMCPExtEnv(resolvedEnv)

	// Command lifetime is independent of the connect timeout — the process
	// must stay alive for the entire server session. Cleanup is handled by
	// session.Close() (via defer in the caller).
	cmd := exec.CommandContext(context.Background(), ext.Command, ext.Args...)
	cmd.Env = env

	transport := &mcp.CommandTransport{Command: cmd}
	client := mcp.NewClient(
		&mcp.Implementation{Name: "curlycatclaw", Version: version},
		nil,
	)

	// Timeout covers handshake + tool discovery. If the extension is slow
	// to start, we skip it rather than blocking all tools.
	connectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	session, err := client.Connect(connectCtx, transport, nil)
	if err != nil {
		// Kill the subprocess if it was started but handshake failed.
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return nil, nil, fmt.Errorf("connect: %w", err)
	}

	var tools []*mcp.Tool
	for tool, err := range session.Tools(connectCtx, nil) {
		if err != nil {
			slog.Warn("mcp-server: error listing tools from extension",
				"name", ext.Name, "error", err)
			break
		}
		tools = append(tools, tool)
	}

	return session, tools, nil
}

// buildMCPExtEnv returns a safe environment for spawning an MCP extension
// subprocess. Starts from a baseline allowlist of the current process env,
// then adds the extension's own env vars with dangerous prefixes filtered.
func buildMCPExtEnv(extEnv map[string]string) []string {
	var env []string
	for _, entry := range os.Environ() {
		if k, _, ok := strings.Cut(entry, "="); ok {
			if _, pass := baselineEnvAllowlist[k]; pass {
				env = append(env, entry)
			}
		}
	}
	for k, v := range extEnv {
		if isDangerousEnvKey(k) {
			continue
		}
		// Don't let extension env override baseline vars (e.g. PATH, HOME).
		if _, baseline := baselineEnvAllowlist[k]; baseline {
			continue
		}
		env = append(env, k+"="+v)
	}
	return env
}

// isDangerousEnvKey returns true if the key matches a dangerous env prefix.
func isDangerousEnvKey(key string) bool {
	upper := strings.ToUpper(key)
	for _, prefix := range dangerousEnvPrefixes {
		if strings.HasPrefix(upper, strings.ToUpper(prefix)) {
			return true
		}
	}
	return false
}

// registerProxyTool registers a proxied MCP extension tool on the server.
// The tool's original InputSchema is preserved so Claude sees the correct
// parameter definitions. Calls are forwarded to the extension's MCP session.
//
// injectUserCtx controls whether _user_context is added to tool arguments.
// Internal MCP servers (GWS, GitHub) expect it for per-user access control.
// External/remote servers (Google Maps) reject unknown fields, so it must be
// disabled for HTTP transport servers.
func registerProxyTool(server *mcp.Server, namespacedName string, tool *mcp.Tool,
	session *mcp.ClientSession, userID, chatID int64, injectUserCtx bool) {

	proxyTool := &mcp.Tool{
		Name:        namespacedName,
		Description: tool.Description,
		InputSchema: tool.InputSchema,
	}

	rawName := tool.Name
	sess := session

	mcp.AddTool(server, proxyTool, func(
		ctx context.Context,
		req *mcp.CallToolRequest,
		input map[string]any,
	) (*mcp.CallToolResult, skillOutput, error) {
		if injectUserCtx && userID != 0 {
			input["_user_context"] = map[string]any{
				"user_id": userID,
				"chat_id": chatID,
			}
		}

		// Gate destructive GitHub operations: require explicit user confirmation.
		// When Claude calls create_issue without confirmed=true, return the draft
		// content and ask Claude to show it to the user first.
		if rawName == "create_issue" || rawName == "issue_write" {
			if confirmed, _ := input["confirmed"].(bool); !confirmed {
				title, _ := input["title"].(string)
				body, _ := input["body"].(string)
				preview := fmt.Sprintf("ISSUE DRAFT (not yet submitted):\n\nTitle: %s\n\nBody:\n%s\n\n"+
					"Show this draft to the user and ask for their approval. "+
					"If they approve, call create_issue again with the same parameters plus confirmed=true. "+
					"If they want changes, revise and show the updated draft.", title, body)
				return nil, skillOutput{Text: preview}, nil
			}
			delete(input, "confirmed") // strip before forwarding to GitHub
		}

		result, err := sess.CallTool(ctx, &mcp.CallToolParams{
			Name:      rawName,
			Arguments: input,
		})
		if err != nil {
			slog.Warn("mcp-server: proxy tool call failed",
				"tool", rawName, "namespace", namespacedName, "err", err)
			return errResult(fmt.Sprintf("proxy call %q: %v", rawName, err)), skillOutput{}, nil
		}

		text := formatMCPResult(result)
		if result.IsError {
			slog.Warn("mcp-server: proxy tool returned error",
				"tool", rawName, "namespace", namespacedName, "error_text", text[:min(200, len(text))])
			return errResult(text), skillOutput{}, nil
		}
		return nil, skillOutput{Text: text}, nil
	})
}

// formatMCPResult converts a CallToolResult into a single string.
// Duplicated from internal/mcp (unexported).
func formatMCPResult(result *mcp.CallToolResult) string {
	if result == nil {
		return ""
	}
	var parts []string
	for _, c := range result.Content {
		switch v := c.(type) {
		case *mcp.TextContent:
			parts = append(parts, v.Text)
		default:
			data, err := json.Marshal(v)
			if err != nil {
				parts = append(parts, fmt.Sprintf("[unserializable content: %T]", v))
			} else {
				parts = append(parts, string(data))
			}
		}
	}
	return strings.Join(parts, "\n")
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
