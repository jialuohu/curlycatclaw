package wasm

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"

	"github.com/jialuohu/curlycatclaw/config"
	"github.com/jialuohu/curlycatclaw/internal/telegram"
	"github.com/jialuohu/curlycatclaw/skills"
)

const (
	httpTimeout    = 10 * time.Second
	dbQueryTimeout = 1 * time.Second
	maxQueryRows   = 100
)

// chatIDCtxKey is used to thread the invoking chat ID through Wasm execution
// context for send_message scope validation.
type chatIDCtxKey struct{}

// userIDCtxKey threads the invoking user ID through Wasm execution
// context for db_read user scoping.
type userIDCtxKey struct{}

// Manifest describes a wasm skill's metadata and permission grants.
// It is loaded from a JSON file alongside the .wasm binary.
type Manifest struct {
	Name         string   `json:"name"`
	Capabilities []string `json:"capabilities"`
	AllowedHosts []string `json:"allowed_hosts"`
}

// hasCapability reports whether the manifest grants the named capability.
func (m *Manifest) hasCapability(cap string) bool {
	for _, c := range m.Capabilities {
		if c == cap {
			return true
		}
	}
	return false
}

// WasmModule holds a compiled wazero module together with its manifest
// and instantiated module reference.
type WasmModule struct {
	manifest *Manifest
	compiled wazero.CompiledModule
	instance api.Module
}

// WasmRuntime manages the lifecycle of wasm skill modules. It compiles
// and instantiates .wasm files, exposes capability-gated host functions,
// and registers each module as a skill in the shared Registry.
type WasmRuntime struct {
	cfg      config.WasmConfig
	rt       wazero.Runtime
	modules  map[string]*WasmModule
	registry *skills.Registry
	db       *sql.DB
	tgInbox  chan<- telegram.OutgoingMessage
	mu       sync.RWMutex
}

// NewWasmRuntime creates a wasm runtime backed by wazero.
func NewWasmRuntime(
	cfg config.WasmConfig,
	registry *skills.Registry,
	db *sql.DB,
	tgInbox chan<- telegram.OutgoingMessage,
) (*WasmRuntime, error) {
	rt := wazero.NewRuntime(context.Background())

	return &WasmRuntime{
		cfg:      cfg,
		rt:       rt,
		modules:  make(map[string]*WasmModule),
		registry: registry,
		db:       db,
		tgInbox:  tgInbox,
	}, nil
}

// LoadAll scans the configured skills directory for .wasm files and loads
// each one. It returns an error only if the directory cannot be read;
// individual module failures are logged but do not stop the scan.
func (w *WasmRuntime) LoadAll(ctx context.Context) error {
	entries, err := os.ReadDir(w.cfg.SkillsDir)
	if err != nil {
		if os.IsNotExist(err) {
			slog.Info("wasm: skills directory does not exist, skipping", "dir", w.cfg.SkillsDir)
			return nil
		}
		return fmt.Errorf("wasm: read skills dir: %w", err)
	}

	loaded := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".wasm") {
			continue
		}
		wasmPath := filepath.Join(w.cfg.SkillsDir, entry.Name())
		if err := w.LoadModule(ctx, wasmPath); err != nil {
			slog.Warn("wasm: failed to load module", "path", wasmPath, "err", err)
			continue
		}
		loaded++
	}

	slog.Info("wasm: modules loaded", "count", loaded)
	return nil
}

// LoadModule compiles and instantiates a single .wasm file. It reads a
// sibling .manifest.json for capability and host-allowlist configuration,
// calls the guest's skill_info export to obtain tool metadata, and
// registers the result in the skill registry.
func (w *WasmRuntime) LoadModule(ctx context.Context, wasmPath string) error {
	// Read manifest.
	manifestPath := wasmPathToManifest(wasmPath)
	manifest, err := loadManifest(manifestPath)
	if err != nil {
		return fmt.Errorf("load manifest %s: %w", manifestPath, err)
	}

	// Compile wasm bytes.
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		return fmt.Errorf("read wasm %s: %w", wasmPath, err)
	}

	compiled, err := w.rt.CompileModule(ctx, wasmBytes)
	if err != nil {
		return fmt.Errorf("compile wasm %s: %w", wasmPath, err)
	}

	// Define host module "catclaw" with capability-gated functions.
	hostBuilder := w.rt.NewHostModuleBuilder("catclaw")

	// Always available: logging.
	hostBuilder.NewFunctionBuilder().
		WithFunc(func(ctx context.Context, mod api.Module, ptr, length uint32) {
			msg := readString(mod, ptr, length)
			slog.Info("wasm: guest log", "module", manifest.Name, "msg", msg)
		}).
		Export("catclaw_log")

	// Capability: http
	if manifest.hasCapability("http") {
		hostBuilder.NewFunctionBuilder().
			WithFunc(func(ctx context.Context, mod api.Module, urlPtr, urlLen uint32) uint64 {
				return w.hostHTTPGet(ctx, mod, manifest, urlPtr, urlLen)
			}).
			Export("catclaw_http_get")
	}

	// Capability: db_read
	if manifest.hasCapability("db_read") {
		hostBuilder.NewFunctionBuilder().
			WithFunc(func(ctx context.Context, mod api.Module, queryPtr, queryLen uint32) uint64 {
				return w.hostDBQuery(ctx, mod, queryPtr, queryLen)
			}).
			Export("catclaw_db_query")
	}

	// Capability: send_message
	if manifest.hasCapability("send_message") {
		hostBuilder.NewFunctionBuilder().
			WithFunc(func(ctx context.Context, mod api.Module, ptr, length uint32) {
				w.hostSendMessage(ctx, mod, ptr, length)
			}).
			Export("catclaw_send_message")
	}

	if _, err := hostBuilder.Instantiate(ctx); err != nil {
		compiled.Close(ctx)
		return fmt.Errorf("instantiate host module for %s: %w", manifest.Name, err)
	}

	// Instantiate the guest module.
	instance, err := w.rt.InstantiateModule(ctx, compiled, wazero.NewModuleConfig().
		WithName(manifest.Name).
		WithStdout(os.Stdout).
		WithStderr(os.Stderr))
	if err != nil {
		compiled.Close(ctx)
		return fmt.Errorf("instantiate guest %s: %w", manifest.Name, err)
	}

	// Call skill_info to get metadata.
	skillInfo, err := callSkillInfo(ctx, instance)
	if err != nil {
		instance.Close(ctx)
		compiled.Close(ctx)
		return fmt.Errorf("skill_info for %s: %w", manifest.Name, err)
	}

	// Create the skill and register it.
	mod := &WasmModule{
		manifest: manifest,
		compiled: compiled,
		instance: instance,
	}

	skill := &skills.Skill{
		Name:        skillInfo.Name,
		Description: skillInfo.Description,
		InputSchema: skillInfo.InputSchema,
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			w.mu.RLock()
			defer w.mu.RUnlock()
			// Thread the invoking chat ID into the Wasm context so
			// hostSendMessage can validate message scope.
			user := skills.GetUser(ctx)
			if user.ChatID == 0 {
				return "", fmt.Errorf("wasm: no chat context for %s execution", manifest.Name)
			}
			execCtx := context.WithValue(ctx, chatIDCtxKey{}, user.ChatID)
			execCtx = context.WithValue(execCtx, userIDCtxKey{}, user.UserID)
			return callSkillExecute(execCtx, instance, input)
		},
	}

	w.mu.Lock()
	w.modules[manifest.Name] = mod
	w.mu.Unlock()

	w.registry.Register(skill)
	slog.Info("wasm: module loaded", "name", manifest.Name, "skill", skillInfo.Name)
	return nil
}

// UnloadModule removes a loaded wasm module by manifest name, unregisters
// its skill, and closes the wazero module instance.
func (w *WasmRuntime) UnloadModule(name string) {
	w.mu.Lock()
	mod, ok := w.modules[name]
	if ok {
		delete(w.modules, name)
	}
	w.mu.Unlock()

	if !ok {
		return
	}

	w.registry.Unregister(name)
	if mod.instance != nil {
		mod.instance.Close(context.Background())
	}
	slog.Info("wasm: module unloaded", "name", name)
}

// WatchForChanges uses fsnotify to watch the skills directory for .wasm
// file changes. Created or modified files trigger LoadModule; removed
// files trigger UnloadModule. It blocks until ctx is cancelled.
func (w *WasmRuntime) WatchForChanges(ctx context.Context) error {
	// Ensure the directory exists before watching.
	if err := os.MkdirAll(w.cfg.SkillsDir, 0750); err != nil {
		return fmt.Errorf("wasm: create skills dir: %w", err)
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("wasm: create watcher: %w", err)
	}
	defer watcher.Close()

	if err := watcher.Add(w.cfg.SkillsDir); err != nil {
		return fmt.Errorf("wasm: watch dir: %w", err)
	}

	slog.Info("wasm: watching for changes", "dir", w.cfg.SkillsDir)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			if !strings.HasSuffix(event.Name, ".wasm") {
				continue
			}

			name := wasmPathToName(event.Name)

			if event.Op&(fsnotify.Create|fsnotify.Write) != 0 {
				slog.Info("wasm: detected change", "file", event.Name, "op", event.Op)
				// Unload existing version first if present.
				w.UnloadModule(name)
				if err := w.LoadModule(ctx, event.Name); err != nil {
					slog.Warn("wasm: reload failed", "file", event.Name, "err", err)
				}
			}
			if event.Op&fsnotify.Remove != 0 {
				slog.Info("wasm: detected removal", "file", event.Name)
				w.UnloadModule(name)
			}

		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			slog.Warn("wasm: watcher error", "err", err)
		}
	}
}

// Close shuts down the wazero runtime and all loaded modules.
func (w *WasmRuntime) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	for name, mod := range w.modules {
		if mod.instance != nil {
			mod.instance.Close(context.Background())
		}
		w.registry.Unregister(name)
	}
	w.modules = make(map[string]*WasmModule)

	return w.rt.Close(context.Background())
}

// ---------------------------------------------------------------------------
// Host function implementations
// ---------------------------------------------------------------------------

// hostHTTPGet performs an HTTP GET on behalf of the guest module. The URL
// is validated against the manifest's AllowedHosts list.
func (w *WasmRuntime) hostHTTPGet(ctx context.Context, mod api.Module, manifest *Manifest, urlPtr, urlLen uint32) uint64 {
	rawURL := readString(mod, urlPtr, urlLen)

	if !isHostAllowed(rawURL, manifest.AllowedHosts) {
		ptr, length := writeString(mod, marshalError("host not allowed: "+rawURL))
		return packPtrLen(ptr, length)
	}

	client := &http.Client{
		Timeout: httpTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if !isHostAllowed(req.URL.String(), manifest.AllowedHosts) {
				return fmt.Errorf("redirect to disallowed host: %s", req.URL.Host)
			}
			if len(via) >= 10 {
				return fmt.Errorf("too many redirects")
			}
			return nil
		},
	}
	resp, err := client.Get(rawURL)
	if err != nil {
		ptr, length := writeString(mod, marshalError(err.Error()))
		return packPtrLen(ptr, length)
	}
	defer resp.Body.Close()

	// Read up to 1MB.
	buf := make([]byte, 0, 1024)
	limit := 1 << 20 // 1MB
	for len(buf) < limit {
		tmp := make([]byte, 4096)
		n, err := resp.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}

	ptr, length := writeString(mod, string(buf))
	return packPtrLen(ptr, length)
}

// hostDBQuery executes a read-only SQL query on behalf of the guest.
// Only SELECT statements are allowed, with a 1-second timeout and
// a maximum of 100 rows. The placeholder :user_id is automatically
// bound to the invoking user's ID for user-scoped queries.
func (w *WasmRuntime) hostDBQuery(ctx context.Context, mod api.Module, queryPtr, queryLen uint32) uint64 {
	query := readString(mod, queryPtr, queryLen)

	if !isSelectOnly(query) {
		ptr, length := writeString(mod, marshalError("only SELECT statements are allowed"))
		return packPtrLen(ptr, length)
	}

	// Extract user ID from execution context for user-scoped queries.
	userID, _ := ctx.Value(userIDCtxKey{}).(int64)

	// Warn if query touches user-scoped tables without :user_id placeholder.
	if userScopedTableAccessed(query) && !strings.Contains(query, ":user_id") {
		slog.Warn("wasm: db_read query accesses user-scoped table without :user_id filter",
			"query", query, "user_id", userID)
	}

	queryCtx, cancel := context.WithTimeout(ctx, dbQueryTimeout)
	defer cancel()

	// Bind :user_id placeholder if present.
	var rows *sql.Rows
	var err error
	if strings.Contains(query, ":user_id") {
		bound := strings.ReplaceAll(query, ":user_id", "?")
		rows, err = w.db.QueryContext(queryCtx, bound, userID)
	} else {
		rows, err = w.db.QueryContext(queryCtx, query)
	}
	if err != nil {
		ptr, length := writeString(mod, marshalError(err.Error()))
		return packPtrLen(ptr, length)
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		ptr, length := writeString(mod, marshalError(err.Error()))
		return packPtrLen(ptr, length)
	}

	var results []map[string]any
	count := 0
	for rows.Next() && count < maxQueryRows {
		values := make([]any, len(columns))
		valuePtrs := make([]any, len(columns))
		for i := range values {
			valuePtrs[i] = &values[i]
		}
		if err := rows.Scan(valuePtrs...); err != nil {
			continue
		}
		row := make(map[string]any, len(columns))
		for i, col := range columns {
			row[col] = values[i]
		}
		results = append(results, row)
		count++
	}

	data, _ := json.Marshal(results)
	ptr, length := writeString(mod, string(data))
	return packPtrLen(ptr, length)
}

// sendMessagePayload is the JSON schema for the catclaw_send_message host function.
type sendMessagePayload struct {
	ChatID int64  `json:"chat_id"`
	Text   string `json:"text"`
}

// hostSendMessage sends a Telegram message on behalf of the guest module.
// The context must carry the allowed chat ID (via chatIDCtxKey) so that
// plugins can only reply to the chat that invoked them.
func (w *WasmRuntime) hostSendMessage(ctx context.Context, mod api.Module, ptr, length uint32) {
	data := readString(mod, ptr, length)

	var payload sendMessagePayload
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		slog.Warn("wasm: invalid send_message payload", "err", err)
		return
	}

	if payload.ChatID == 0 || payload.Text == "" {
		slog.Warn("wasm: send_message missing chat_id or text")
		return
	}

	// Validate chat scope: plugins may only send to the chat that invoked them.
	// Default-deny: if context is missing or zero, block the message.
	allowed, ok := ctx.Value(chatIDCtxKey{}).(int64)
	if !ok || allowed == 0 {
		slog.Warn("wasm: send_message blocked, no chat scope in context")
		return
	}
	if payload.ChatID != allowed {
		slog.Warn("wasm: send_message blocked, chat_id mismatch",
			"requested", payload.ChatID, "allowed", allowed)
		return
	}

	select {
	case w.tgInbox <- telegram.OutgoingMessage{
		ChatID: payload.ChatID,
		Text:   payload.Text,
	}:
	default:
		slog.Warn("wasm: telegram inbox full, dropping message", "chat_id", payload.ChatID)
	}
}

// marshalError produces a properly escaped JSON error string for host function
// error responses. This avoids injection via unescaped characters in error messages.
func marshalError(msg string) string {
	data, _ := json.Marshal(map[string]string{"error": msg})
	return string(data)
}

// ---------------------------------------------------------------------------
// Guest memory helpers
// ---------------------------------------------------------------------------

// readString reads a UTF-8 string from guest memory at the given pointer
// and length.
func readString(mod api.Module, ptr, length uint32) string {
	if length == 0 {
		return ""
	}
	data, ok := mod.Memory().Read(ptr, length)
	if !ok {
		return ""
	}
	return string(data)
}

// writeString writes a string into guest memory by calling the guest's
// exported malloc function. Returns the pointer and length, or (0,0) on
// failure.
func writeString(mod api.Module, s string) (ptr, length uint32) {
	malloc := mod.ExportedFunction("malloc")
	if malloc == nil {
		slog.Warn("wasm: guest does not export malloc")
		return 0, 0
	}

	size := uint64(len(s))
	results, err := malloc.Call(context.Background(), size)
	if err != nil || len(results) == 0 {
		slog.Warn("wasm: malloc call failed", "err", err)
		return 0, 0
	}

	ptr = uint32(results[0])
	if !mod.Memory().Write(ptr, []byte(s)) {
		slog.Warn("wasm: failed to write to guest memory")
		return 0, 0
	}
	return ptr, uint32(len(s))
}

// packPtrLen combines a pointer and length into a single uint64 return
// value (ptr in the upper 32 bits, length in the lower 32).
func packPtrLen(ptr, length uint32) uint64 {
	return (uint64(ptr) << 32) | uint64(length)
}

// ---------------------------------------------------------------------------
// Guest function callers
// ---------------------------------------------------------------------------

// skillInfoResult is the JSON structure returned by the guest skill_info export.
type skillInfoResult struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// callSkillInfo calls the guest's skill_info export and parses the result.
func callSkillInfo(ctx context.Context, instance api.Module) (*skillInfoResult, error) {
	fn := instance.ExportedFunction("skill_info")
	if fn == nil {
		return nil, fmt.Errorf("module does not export skill_info")
	}

	results, err := fn.Call(ctx)
	if err != nil {
		return nil, fmt.Errorf("call skill_info: %w", err)
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("skill_info returned no results")
	}

	// skill_info returns a packed ptr|len.
	packed := results[0]
	ptr := uint32(packed >> 32)
	length := uint32(packed & 0xFFFFFFFF)

	data := readString(instance, ptr, length)
	if data == "" {
		return nil, fmt.Errorf("skill_info returned empty data")
	}

	var info skillInfoResult
	if err := json.Unmarshal([]byte(data), &info); err != nil {
		return nil, fmt.Errorf("parse skill_info: %w", err)
	}
	return &info, nil
}

// callSkillExecute calls the guest's skill_execute export with JSON input.
func callSkillExecute(ctx context.Context, instance api.Module, input json.RawMessage) (string, error) {
	fn := instance.ExportedFunction("skill_execute")
	if fn == nil {
		return "", fmt.Errorf("module does not export skill_execute")
	}

	ptr, length := writeString(instance, string(input))
	if length == 0 {
		return "", fmt.Errorf("failed to write input to guest memory")
	}

	results, err := fn.Call(ctx, uint64(ptr), uint64(length))
	if err != nil {
		return "", fmt.Errorf("call skill_execute: %w", err)
	}
	if len(results) == 0 {
		return "", fmt.Errorf("skill_execute returned no results")
	}

	packed := results[0]
	rPtr := uint32(packed >> 32)
	rLen := uint32(packed & 0xFFFFFFFF)

	return readString(instance, rPtr, rLen), nil
}

// ---------------------------------------------------------------------------
// Validation helpers
// ---------------------------------------------------------------------------

// isSelectOnly returns true if the trimmed, uppercased query begins with
// SELECT and does not contain dangerous keywords as standalone words.
func isSelectOnly(query string) bool {
	// Strip SQL comments (both -- line and /* block */ styles) to prevent
	// hiding mutating keywords inside comments.
	cleaned := stripSQLComments(query)
	upper := strings.ToUpper(strings.TrimSpace(cleaned))
	if !strings.HasPrefix(upper, "SELECT") {
		return false
	}
	// Reject multi-statement queries (semicolons enable chaining).
	if strings.Contains(upper, ";") {
		return false
	}
	// Reject statements that contain mutating keywords as standalone words.
	// This avoids false positives like WHERE action = 'DELETE_REQUEST'.
	for _, kw := range []string{"INSERT", "UPDATE", "DELETE", "DROP", "ALTER", "CREATE", "REPLACE", "TRUNCATE", "ATTACH", "DETACH", "PRAGMA", "VACUUM", "REINDEX"} {
		if containsWord(upper, kw) {
			return false
		}
	}
	return true
}

// containsWord checks if word appears as a standalone word in s (not as a substring
// of a larger identifier). Word boundaries are non-alphanumeric/underscore characters.
func containsWord(s, word string) bool {
	for i := 0; i <= len(s)-len(word); i++ {
		if s[i:i+len(word)] != word {
			continue
		}
		// Check left boundary.
		if i > 0 && isWordChar(s[i-1]) {
			continue
		}
		// Check right boundary.
		end := i + len(word)
		if end < len(s) && isWordChar(s[end]) {
			continue
		}
		return true
	}
	return false
}

func isWordChar(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_'
}

// stripSQLComments removes -- line comments and /* block */ comments from SQL,
// while preserving comment-like sequences inside string literals.
func stripSQLComments(sql string) string {
	var b strings.Builder
	b.Grow(len(sql))
	i := 0
	for i < len(sql) {
		ch := sql[i]

		// Single-quoted string literal: consume until closing quote.
		// Handles escaped quotes ('') by treating '' as two characters.
		if ch == '\'' {
			b.WriteByte(ch)
			i++
			for i < len(sql) {
				if sql[i] == '\'' {
					b.WriteByte(sql[i])
					i++
					// '' is an escaped quote, stay in string.
					if i < len(sql) && sql[i] == '\'' {
						b.WriteByte(sql[i])
						i++
						continue
					}
					break // End of string literal.
				}
				b.WriteByte(sql[i])
				i++
			}
			continue
		}

		// Double-quoted identifier: consume until closing quote.
		if ch == '"' {
			b.WriteByte(ch)
			i++
			for i < len(sql) {
				if sql[i] == '"' {
					b.WriteByte(sql[i])
					i++
					if i < len(sql) && sql[i] == '"' {
						b.WriteByte(sql[i])
						i++
						continue
					}
					break
				}
				b.WriteByte(sql[i])
				i++
			}
			continue
		}

		// Line comment: skip until end of line.
		if i+1 < len(sql) && ch == '-' && sql[i+1] == '-' {
			for i < len(sql) && sql[i] != '\n' {
				i++
			}
			continue
		}

		// Block comment: skip until closing */.
		if i+1 < len(sql) && ch == '/' && sql[i+1] == '*' {
			i += 2
			for i+1 < len(sql) && !(sql[i] == '*' && sql[i+1] == '/') {
				i++
			}
			if i+1 < len(sql) {
				i += 2
			}
			continue
		}

		b.WriteByte(ch)
		i++
	}
	return b.String()
}

// userScopedTableAccessed returns true if the query references tables that
// contain per-user data (conversations, messages, notes, user_facts, reminders).
func userScopedTableAccessed(query string) bool {
	q := strings.ToLower(query)
	for _, table := range []string{"conversations", "messages", "notes", "user_facts", "reminders", "conversation_summaries", "tool_calls"} {
		if strings.Contains(q, table) {
			return true
		}
	}
	return false
}

// privateIPNets contains CIDR ranges for private/internal networks.
// Requests to these ranges are blocked to prevent SSRF attacks from Wasm plugins.
var privateIPNets = func() []*net.IPNet {
	cidrs := []string{
		"127.0.0.0/8",     // loopback
		"10.0.0.0/8",      // RFC 1918
		"172.16.0.0/12",   // RFC 1918
		"192.168.0.0/16",  // RFC 1918
		"169.254.0.0/16",  // link-local
		"::1/128",         // IPv6 loopback
		"fc00::/7",        // IPv6 unique local
		"fe80::/10",       // IPv6 link-local
	}
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, cidr := range cidrs {
		_, n, _ := net.ParseCIDR(cidr)
		nets = append(nets, n)
	}
	return nets
}()

// isPrivateIP returns true if the given hostname resolves to a private/internal IP.
func isPrivateIP(hostname string) bool {
	// Direct IP check (no DNS lookup needed).
	if ip := net.ParseIP(hostname); ip != nil {
		for _, n := range privateIPNets {
			if n.Contains(ip) {
				return true
			}
		}
		return false
	}
	// Hostname: resolve and check all IPs.
	addrs, err := net.LookupHost(hostname)
	if err != nil {
		return false // can't resolve, let the HTTP call fail naturally
	}
	for _, addr := range addrs {
		ip := net.ParseIP(addr)
		if ip == nil {
			continue
		}
		for _, n := range privateIPNets {
			if n.Contains(ip) {
				return true
			}
		}
	}
	return false
}

// isHostAllowed checks whether the given URL matches any entry in the
// allowed hosts list using net/url.Parse for correct host extraction.
// Private/internal IP addresses are always blocked regardless of the allowlist.
func isHostAllowed(rawURL string, allowed []string) bool {
	if len(allowed) == 0 {
		return false
	}

	u, err := neturl.Parse(rawURL)
	if err != nil || u.Host == "" {
		return false
	}
	host := u.Hostname() // strips port, handles userinfo correctly

	// Block private/internal IPs to prevent SSRF.
	if isPrivateIP(host) {
		return false
	}

	for _, h := range allowed {
		if h == "*" {
			return true
		}
		if strings.EqualFold(host, h) {
			return true
		}
		// Support wildcard subdomains: *.example.com matches foo.example.com.
		if strings.HasPrefix(h, "*.") {
			suffix := h[1:] // ".example.com"
			if strings.HasSuffix(strings.ToLower(host), strings.ToLower(suffix)) {
				return true
			}
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// File path helpers
// ---------------------------------------------------------------------------

// wasmPathToManifest converts a .wasm file path to its sibling .manifest.json path.
func wasmPathToManifest(wasmPath string) string {
	base := strings.TrimSuffix(wasmPath, ".wasm")
	return base + ".manifest.json"
}

// wasmPathToName extracts the module name from a .wasm file path
// (the filename without extension).
func wasmPathToName(wasmPath string) string {
	return strings.TrimSuffix(filepath.Base(wasmPath), ".wasm")
}

// loadManifest reads and parses a manifest JSON file.
func loadManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// If no manifest file, use the filename as the name with no capabilities.
			name := strings.TrimSuffix(filepath.Base(path), ".manifest.json")
			return &Manifest{Name: name}, nil
		}
		return nil, fmt.Errorf("read manifest: %w", err)
	}

	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if m.Name == "" {
		return nil, fmt.Errorf("manifest missing required 'name' field")
	}
	return &m, nil
}

// ExportedForTesting exposes internal helpers for unit testing. This is
// only used in test files.
var ExportedForTesting = struct {
	IsSelectOnly   func(string) bool
	IsHostAllowed  func(string, []string) bool
	LoadManifest   func(string) (*Manifest, error)
	ReadString     func(api.Module, uint32, uint32) string
	WriteString    func(api.Module, string) (uint32, uint32)
	WasmPathToName func(string) string
}{
	IsSelectOnly:   isSelectOnly,
	IsHostAllowed:  isHostAllowed,
	LoadManifest:   loadManifest,
	ReadString:     readString,
	WriteString:    writeString,
	WasmPathToName: wasmPathToName,
}

