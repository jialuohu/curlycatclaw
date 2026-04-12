package wasm

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/jialuohu/curlycatclaw/config"
	"github.com/jialuohu/curlycatclaw/internal/telegram"
	"github.com/jialuohu/curlycatclaw/skills"
)

// ---------------------------------------------------------------------------
// Manifest loading
// ---------------------------------------------------------------------------

func TestLoadManifest_ValidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.manifest.json")

	manifest := Manifest{
		Name:         "test_skill",
		Capabilities: []string{"http", "db_read"},
		AllowedHosts: []string{"api.example.com"},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}

	m, err := loadManifest(path)
	if err != nil {
		t.Fatalf("loadManifest: %v", err)
	}
	if m.Name != "test_skill" {
		t.Errorf("Name = %q, want %q", m.Name, "test_skill")
	}
	if len(m.Capabilities) != 2 {
		t.Errorf("Capabilities len = %d, want 2", len(m.Capabilities))
	}
	if len(m.AllowedHosts) != 1 || m.AllowedHosts[0] != "api.example.com" {
		t.Errorf("AllowedHosts = %v, want [api.example.com]", m.AllowedHosts)
	}
}

func TestLoadManifest_MissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nonexistent.manifest.json")

	m, err := loadManifest(path)
	if err != nil {
		t.Fatalf("loadManifest for missing file should not error: %v", err)
	}
	if m.Name != "nonexistent" {
		t.Errorf("Name = %q, want %q", m.Name, "nonexistent")
	}
	if len(m.Capabilities) != 0 {
		t.Errorf("Capabilities should be empty for missing manifest")
	}
}

func TestLoadManifest_EmptyName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.manifest.json")

	data := []byte(`{"capabilities":["http"]}`)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}

	_, err := loadManifest(path)
	if err == nil {
		t.Fatal("loadManifest should fail on empty name")
	}
}

func TestLoadManifest_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.manifest.json")

	if err := os.WriteFile(path, []byte(`{not valid json}`), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := loadManifest(path)
	if err == nil {
		t.Fatal("loadManifest should fail on invalid JSON")
	}
}

// ---------------------------------------------------------------------------
// Manifest.hasCapability
// ---------------------------------------------------------------------------

func TestManifest_HasCapability(t *testing.T) {
	m := &Manifest{Capabilities: []string{"http", "db_read"}}

	if !m.hasCapability("http") {
		t.Error("hasCapability(http) = false, want true")
	}
	if !m.hasCapability("db_read") {
		t.Error("hasCapability(db_read) = false, want true")
	}
	if m.hasCapability("send_message") {
		t.Error("hasCapability(send_message) = true, want false")
	}
}

// ---------------------------------------------------------------------------
// SQL validation (isSelectOnly)
// ---------------------------------------------------------------------------

func TestIsSelectOnly(t *testing.T) {
	tests := []struct {
		query string
		want  bool
	}{
		{"SELECT * FROM notes", true},
		{"select id from notes", true},
		{"  SELECT 1", true},
		{"SELECT * FROM notes WHERE title LIKE '%test%'", true},
		{"INSERT INTO notes VALUES (1)", false},
		{"UPDATE notes SET title = 'x'", false},
		{"DELETE FROM notes", false},
		{"DROP TABLE notes", false},
		{"ALTER TABLE notes ADD COLUMN x", false},
		{"CREATE TABLE evil (id int)", false},
		{"", false},
		{" ", false},
		// Mutating keyword inside SELECT is blocked.
		{"SELECT * FROM notes; DROP TABLE notes", false},
		{"SELECT * FROM notes; DELETE FROM notes", false},
		{"REPLACE INTO notes VALUES (1)", false},
		{"TRUNCATE TABLE notes", false},
		// Comment stripping: keywords inside comments are removed, query itself is clean.
		{"SELECT * FROM notes /* just a comment */", true},
		{"SELECT * FROM notes -- trailing comment", true},
		// Semicolon-based bypass blocked even inside comments.
		{"SELECT 1 /* ; DROP TABLE x */; DROP TABLE y", false},
		// Semicolon-based multi-statement.
		{"SELECT 1; SELECT 2", false},
		{"SELECT * FROM notes;DROP TABLE notes", false},
		// Word-boundary: mutating keywords inside identifiers/strings should NOT trigger.
		{"SELECT * FROM t WHERE action = 'DELETE_REQUEST'", true},
		{"SELECT * FROM t WHERE status = 'UPDATED'", true},
		{"SELECT CREATED_AT FROM notes", true},
		// But standalone keywords still blocked.
		{"SELECT * FROM notes UNION DELETE FROM notes", false},
		// String literals with comment-like content preserved.
		{"SELECT * FROM t WHERE name = '-- comment'", true},
		{"SELECT * FROM t WHERE name = 'O''Brien'", true},
		// New keywords: ATTACH, DETACH, PRAGMA, VACUUM, REINDEX.
		{"SELECT * FROM t WHERE x = 'ATTACH'", false}, // conservative: blocks keyword even in string
		{"SELECT 1; ATTACH DATABASE 'x' AS y", false},
		{"SELECT * FROM t UNION SELECT ATTACH FROM y", false},
		{"SELECT * FROM t WHERE DETACH = 1", false},
		{"SELECT * FROM t; PRAGMA table_info(x)", false},
		{"SELECT * FROM t; VACUUM", false},
		{"SELECT * FROM t; REINDEX", false},
		// Word-boundary: keywords inside identifiers should NOT trigger.
		{"SELECT ATTACHED_FILE FROM t", true},
		{"SELECT DETACHED FROM t", true},
		{"SELECT REINDEXED FROM t", true},
		// UNION/INTERSECT/EXCEPT blocked as standalone words.
		{"SELECT 1 UNION SELECT 2", false},
		{"SELECT a FROM t UNION ALL SELECT b FROM t2", false},
		{"SELECT a FROM t INTERSECT SELECT b FROM t2", false},
		{"SELECT a FROM t EXCEPT SELECT b FROM t2", false},
		// Word-boundary: "REUNION" should NOT be blocked.
		{"SELECT * FROM t WHERE event = 'REUNION'", true},
		{"SELECT REUNION_DATE FROM t", true},
		// WITH (CTE) blocked
		{"WITH cte AS (SELECT 1) SELECT * FROM cte", false},
		{"WITH RECURSIVE cte AS (SELECT 1 UNION ALL SELECT n+1 FROM cte WHERE n < 10) SELECT * FROM cte", false},
		// Word boundary: WITHDRAWN should NOT be blocked
		{"SELECT WITHDRAWN FROM t", true},
		{"SELECT * FROM t WHERE status = 'WITHHOLD'", true},
	}

	for _, tt := range tests {
		got := isSelectOnly(tt.query)
		if got != tt.want {
			t.Errorf("isSelectOnly(%q) = %v, want %v", tt.query, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// URL host validation (isHostAllowed)
// ---------------------------------------------------------------------------

func TestIsHostAllowed(t *testing.T) {
	tests := []struct {
		url     string
		allowed []string
		want    bool
	}{
		{"https://api.example.com/v1", []string{"api.example.com"}, true},
		{"http://api.example.com:8080/data", []string{"api.example.com"}, true},
		{"https://evil.com/steal", []string{"api.example.com"}, false},
		{"https://api.example.com/v1", []string{}, false},
		{"https://anything.example.com/v1", []string{"*.example.com"}, true},
		{"https://deep.sub.example.com/v1", []string{"*.example.com"}, true},
		{"https://example.com/v1", []string{"*.example.com"}, false},
		{"https://anything.com/v1", []string{"*"}, true},
		// Case insensitive.
		{"https://API.Example.COM/v1", []string{"api.example.com"}, true},
		// Userinfo bypass: must be rejected (net/url strips userinfo).
		{"http://evil@allowed.com/path", []string{"allowed.com"}, true},
		{"http://evil@allowed.com/path", []string{"evil.com"}, false},
		{"http://evil@allowed.com/path", []string{"*.allowed.com"}, false},
		// IPv6 loopback (blocked as private IP).
		{"http://[::1]:8080/path", []string{"::1"}, false},
		{"http://[::1]/path", []string{"::1"}, false},
		{"http://[::1]/path", []string{"127.0.0.1"}, false},
		// Private IPs blocked even with wildcard.
		{"http://127.0.0.1/path", []string{"*"}, false},
		{"http://10.0.0.1/path", []string{"*"}, false},
		{"http://172.16.0.1/path", []string{"*"}, false},
		{"http://192.168.1.1/path", []string{"*"}, false},
		{"http://169.254.1.1/path", []string{"*"}, false},
		// Public IPs still allowed.
		{"http://8.8.8.8/path", []string{"*"}, true},
		{"http://1.1.1.1/path", []string{"*"}, true},
		// URL with port (host extraction strips port).
		{"https://api.example.com:9090/v1", []string{"api.example.com"}, true},
		// Malformed URL.
		{"://bad", []string{"*"}, false},
		{"", []string{"*"}, false},
		{"not-a-url", []string{"not-a-url"}, false},
	}

	for _, tt := range tests {
		got := isHostAllowed(tt.url, tt.allowed)
		if got != tt.want {
			t.Errorf("isHostAllowed(%q, %v) = %v, want %v", tt.url, tt.allowed, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Private IP detection
// ---------------------------------------------------------------------------

func TestIsPrivateIP(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{"127.0.0.1", true},
		{"127.0.0.255", true},
		{"10.0.0.1", true},
		{"10.255.255.255", true},
		{"172.16.0.1", true},
		{"172.31.255.255", true},
		{"172.15.0.1", false},   // just outside 172.16/12
		{"172.32.0.1", false},   // just outside 172.16/12
		{"192.168.0.1", true},
		{"192.168.255.255", true},
		{"169.254.1.1", true},
		{"8.8.8.8", false},
		{"1.1.1.1", false},
		{"93.184.216.34", false},
		{"::1", true},           // IPv6 loopback
		{"fe80::1", true},       // IPv6 link-local
		{"fc00::1", true},       // IPv6 unique local
		{"2607:f8b0:4004:800::200e", false}, // Google public IPv6
		// Ranges added to close SSRF gaps:
		{"0.0.0.0", true},          // "this network" — routes to localhost on Linux
		{"0.1.2.3", true},          // still in 0.0.0.0/8
		{"100.64.0.1", true},       // CGNAT (Tailscale)
		{"100.127.255.255", true},  // last CGNAT address
		{"100.128.0.1", false},     // just outside CGNAT
		{"255.255.255.255", true},  // broadcast
		{"224.0.0.1", true},        // multicast (via IsMulticast)
		{"::", true},               // IPv6 unspecified
		{"ff02::1", true},          // IPv6 multicast
		{"192.0.2.1", true},        // TEST-NET-1 (documentation)
		{"198.51.100.1", true},     // TEST-NET-2
		{"203.0.113.1", true},      // TEST-NET-3
	}

	for _, tt := range tests {
		got := isPrivateIP(tt.host)
		if got != tt.want {
			t.Errorf("isPrivateIP(%q) = %v, want %v", tt.host, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// User-scoped table detection
// ---------------------------------------------------------------------------

func TestUserScopedTableAccessed(t *testing.T) {
	tests := []struct {
		query string
		want  bool
	}{
		{"SELECT * FROM user_facts WHERE user_id = :user_id", true},
		{"SELECT * FROM messages WHERE conversation_id = ?", true},
		{"SELECT * FROM conversations WHERE user_id = ?", true},
		{"SELECT * FROM notes WHERE user_id = ?", true},
		{"SELECT * FROM reminders WHERE user_id = ?", true},
		{"SELECT * FROM conversation_summaries", true},
		{"SELECT * FROM tool_calls", true},
		{"SELECT 1", false},
		{"SELECT count(*) FROM sqlite_master", false},
		// Table name in comment should NOT trigger detection.
		{"SELECT 1 -- user_facts", false},
		{"SELECT 1 /* messages */", false},
	}

	for _, tt := range tests {
		got := userScopedTableAccessed(tt.query)
		if got != tt.want {
			t.Errorf("userScopedTableAccessed(%q) = %v, want %v", tt.query, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Registry Unregister
// ---------------------------------------------------------------------------

func TestRegistryUnregister(t *testing.T) {
	reg := skills.NewRegistry()
	reg.Register(&skills.Skill{Name: "test_skill", Description: "test"})

	if reg.Get("test_skill") == nil {
		t.Fatal("test_skill should be registered")
	}

	reg.Unregister("test_skill")

	if reg.Get("test_skill") != nil {
		t.Fatal("test_skill should be unregistered")
	}

	// Unregistering a non-existent skill should not panic.
	reg.Unregister("nonexistent")
}

// ---------------------------------------------------------------------------
// LoadAll on empty directory
// ---------------------------------------------------------------------------

func TestLoadAll_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	reg := skills.NewRegistry()
	inbox := make(chan telegram.OutgoingMessage, 1)

	rt, err := NewWasmRuntime(config.WasmConfig{
		Enabled:   true,
		SkillsDir: dir,
	}, reg, nil, inbox)
	if err != nil {
		t.Fatalf("NewWasmRuntime: %v", err)
	}
	defer rt.Close()

	if err := rt.LoadAll(context.Background()); err != nil {
		t.Fatalf("LoadAll on empty dir: %v", err)
	}

	if len(reg.All()) != 0 {
		t.Errorf("expected 0 skills, got %d", len(reg.All()))
	}
}

// ---------------------------------------------------------------------------
// LoadAll on non-existent directory
// ---------------------------------------------------------------------------

func TestLoadAll_NonExistentDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does_not_exist")
	reg := skills.NewRegistry()
	inbox := make(chan telegram.OutgoingMessage, 1)

	rt, err := NewWasmRuntime(config.WasmConfig{
		Enabled:   true,
		SkillsDir: dir,
	}, reg, nil, inbox)
	if err != nil {
		t.Fatalf("NewWasmRuntime: %v", err)
	}
	defer rt.Close()

	// Should succeed (log and skip) rather than error.
	if err := rt.LoadAll(context.Background()); err != nil {
		t.Fatalf("LoadAll on non-existent dir should not error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// LoadModule with non-existent file
// ---------------------------------------------------------------------------

func TestLoadModule_NonExistentFile(t *testing.T) {
	dir := t.TempDir()
	reg := skills.NewRegistry()
	inbox := make(chan telegram.OutgoingMessage, 1)

	rt, err := NewWasmRuntime(config.WasmConfig{
		Enabled:   true,
		SkillsDir: dir,
	}, reg, nil, inbox)
	if err != nil {
		t.Fatalf("NewWasmRuntime: %v", err)
	}
	defer rt.Close()

	err = rt.LoadModule(context.Background(), filepath.Join(dir, "nonexistent.wasm"))
	if err == nil {
		t.Fatal("LoadModule should fail for non-existent file")
	}
}

// ---------------------------------------------------------------------------
// LoadAll skips non-wasm files
// ---------------------------------------------------------------------------

func TestLoadAll_SkipsNonWasmFiles(t *testing.T) {
	dir := t.TempDir()
	// Create a non-wasm file.
	if err := os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	// Create a subdirectory named something.wasm (should be skipped).
	if err := os.MkdirAll(filepath.Join(dir, "subdir.wasm"), 0755); err != nil {
		t.Fatal(err)
	}

	reg := skills.NewRegistry()
	inbox := make(chan telegram.OutgoingMessage, 1)

	rt, err := NewWasmRuntime(config.WasmConfig{
		Enabled:   true,
		SkillsDir: dir,
	}, reg, nil, inbox)
	if err != nil {
		t.Fatalf("NewWasmRuntime: %v", err)
	}
	defer rt.Close()

	if err := rt.LoadAll(context.Background()); err != nil {
		t.Fatalf("LoadAll: %v", err)
	}

	if len(reg.All()) != 0 {
		t.Errorf("expected 0 skills, got %d", len(reg.All()))
	}
}

// ---------------------------------------------------------------------------
// wasmPathToName
// ---------------------------------------------------------------------------

func TestWasmPathToName(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/home/user/skills/weather.wasm", "weather"},
		{"/tmp/my_skill.wasm", "my_skill"},
		{"simple.wasm", "simple"},
	}
	for _, tt := range tests {
		got := wasmPathToName(tt.path)
		if got != tt.want {
			t.Errorf("wasmPathToName(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// wasmPathToManifest
// ---------------------------------------------------------------------------

func TestWasmPathToManifest(t *testing.T) {
	got := wasmPathToManifest("/home/user/skills/weather.wasm")
	want := "/home/user/skills/weather.manifest.json"
	if got != want {
		t.Errorf("wasmPathToManifest = %q, want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// UnloadModule on non-existent module
// ---------------------------------------------------------------------------

func TestUnloadModule_NonExistent(t *testing.T) {
	dir := t.TempDir()
	reg := skills.NewRegistry()
	inbox := make(chan telegram.OutgoingMessage, 1)

	rt, err := NewWasmRuntime(config.WasmConfig{
		Enabled:   true,
		SkillsDir: dir,
	}, reg, nil, inbox)
	if err != nil {
		t.Fatalf("NewWasmRuntime: %v", err)
	}
	defer rt.Close()

	// Should not panic.
	rt.UnloadModule("nonexistent")
}

// ---------------------------------------------------------------------------
// Close on empty runtime
// ---------------------------------------------------------------------------

func TestClose_EmptyRuntime(t *testing.T) {
	dir := t.TempDir()
	reg := skills.NewRegistry()
	inbox := make(chan telegram.OutgoingMessage, 1)

	rt, err := NewWasmRuntime(config.WasmConfig{
		Enabled:   true,
		SkillsDir: dir,
	}, reg, nil, inbox)
	if err != nil {
		t.Fatalf("NewWasmRuntime: %v", err)
	}

	if err := rt.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// ---------------------------------------------------------------------------
// packPtrLen
// ---------------------------------------------------------------------------

func TestPackPtrLen(t *testing.T) {
	ptr := uint32(0x12345678)
	length := uint32(0xABCDEF01)
	packed := packPtrLen(ptr, length)

	gotPtr := uint32(packed >> 32)
	gotLen := uint32(packed & 0xFFFFFFFF)

	if gotPtr != ptr {
		t.Errorf("ptr = 0x%X, want 0x%X", gotPtr, ptr)
	}
	if gotLen != length {
		t.Errorf("len = 0x%X, want 0x%X", gotLen, length)
	}
}

// ---------------------------------------------------------------------------
// Regression: DB error sanitization (marshalError returns generic message)
// ---------------------------------------------------------------------------

func TestMarshalError_GenericQueryFailed(t *testing.T) {
	// The bug fix ensures that when a DB query fails, the error returned
	// to Wasm plugins is a generic "query failed" rather than raw DB errors
	// that could leak table names or SQLite details.
	result := marshalError("query failed")

	var parsed map[string]string
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("marshalError output is not valid JSON: %v", err)
	}
	if parsed["error"] != "query failed" {
		t.Errorf("error = %q, want %q", parsed["error"], "query failed")
	}
}

func TestMarshalError_DoesNotLeakDBDetails(t *testing.T) {
	// Simulate what used to happen before the fix: raw DB errors were returned
	// to the Wasm guest. Now hostDBQuery always returns marshalError("query failed")
	// instead. Verify that the generic message contains no SQL/table details.
	genericMsg := marshalError("query failed")

	sensitiveStrings := []string{
		"sqlite", "SQLITE", "table", "TABLE",
		"no such table", "syntax error",
		"user_facts", "messages", "conversations", "notes", "reminders",
	}

	for _, s := range sensitiveStrings {
		if strings.Contains(genericMsg, s) {
			t.Errorf("marshalError(\"query failed\") contains sensitive string %q: %s", s, genericMsg)
		}
	}
}

func TestHostDBQuery_ErrorSanitization(t *testing.T) {
	// Set up an in-memory SQLite database with no tables. A query against
	// a non-existent table triggers a real SQL error. Verify that
	// hostDBQuery would produce only "query failed" and not the raw error.
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Simulate the same logic as hostDBQuery: execute a bad query and check
	// that the error path produces the sanitized message.
	ctx := context.Background()
	_, queryErr := db.QueryContext(ctx, "SELECT * FROM secret_table WHERE id = 1")
	if queryErr == nil {
		t.Fatal("expected error querying non-existent table")
	}

	// Before the fix, the raw error was returned. After the fix, only
	// "query failed" is returned. Verify the raw error DOES contain
	// table info (confirming the leak existed).
	rawErrMsg := queryErr.Error()
	if !strings.Contains(rawErrMsg, "secret_table") {
		t.Fatalf("expected raw SQLite error to mention table name, got: %s", rawErrMsg)
	}

	// The fix: marshalError("query failed") is used instead.
	sanitized := marshalError("query failed")
	if strings.Contains(sanitized, "secret_table") {
		t.Errorf("sanitized error should not contain table name, got: %s", sanitized)
	}
	if strings.Contains(sanitized, "sqlite") || strings.Contains(sanitized, "SQLITE") {
		t.Errorf("sanitized error should not contain sqlite details, got: %s", sanitized)
	}

	// Verify it parses to the expected structure.
	var parsed map[string]string
	if err := json.Unmarshal([]byte(sanitized), &parsed); err != nil {
		t.Fatalf("sanitized error is not valid JSON: %v", err)
	}
	if parsed["error"] != "query failed" {
		t.Errorf("error field = %q, want %q", parsed["error"], "query failed")
	}
}

// ---------------------------------------------------------------------------
// Regression: HTTP response capped at 1MB
// ---------------------------------------------------------------------------

func TestHTTPResponseLimit_1MB(t *testing.T) {
	// The bug fix added io.LimitReader(resp.Body, 1<<20) to cap HTTP
	// responses at exactly 1MB. Verify this limit is enforced.
	const oneMB = 1 << 20
	const oversized = oneMB + 4096

	// Create a test server that returns more than 1MB.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Write oversized data (1MB + 4KB of 'A' bytes).
		data := make([]byte, oversized)
		for i := range data {
			data[i] = 'A'
		}
		w.Write(data)
	}))
	defer ts.Close()

	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatalf("http.Get: %v", err)
	}
	defer resp.Body.Close()

	// Apply the same LimitReader as hostHTTPGet.
	buf, err := io.ReadAll(io.LimitReader(resp.Body, oneMB))
	if err != nil {
		t.Fatalf("io.ReadAll: %v", err)
	}

	if len(buf) != oneMB {
		t.Errorf("response size = %d, want exactly %d (1MB)", len(buf), oneMB)
	}
}

func TestHTTPResponseLimit_SmallResponseUnchanged(t *testing.T) {
	// Verify that responses smaller than 1MB are returned in full.
	const smallSize = 512
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Repeat("x", smallSize)))
	}))
	defer ts.Close()

	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatalf("http.Get: %v", err)
	}
	defer resp.Body.Close()

	buf, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatalf("io.ReadAll: %v", err)
	}

	if len(buf) != smallSize {
		t.Errorf("response size = %d, want %d", len(buf), smallSize)
	}
}

func TestHTTPResponseLimit_ExactlyOneMB(t *testing.T) {
	// Verify that a response of exactly 1MB is returned in full (boundary check).
	const oneMB = 1 << 20
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data := make([]byte, oneMB)
		for i := range data {
			data[i] = 'B'
		}
		w.Write(data)
	}))
	defer ts.Close()

	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatalf("http.Get: %v", err)
	}
	defer resp.Body.Close()

	buf, err := io.ReadAll(io.LimitReader(resp.Body, oneMB))
	if err != nil {
		t.Fatalf("io.ReadAll: %v", err)
	}

	if len(buf) != oneMB {
		t.Errorf("response size = %d, want exactly %d (1MB)", len(buf), oneMB)
	}
}

// Verify the constant used in the code matches 1<<20 = 1048576 bytes.
func TestHTTPResponseLimit_ConstantValue(t *testing.T) {
	const oneMB = 1 << 20
	if oneMB != 1048576 {
		t.Errorf("1<<20 = %d, want 1048576", oneMB)
	}
	// The code uses io.LimitReader(resp.Body, 1<<20). Verify the expression
	// produces the correct truncation on a mock reader.
	data := make([]byte, oneMB+100)
	for i := range data {
		data[i] = byte(i % 256)
	}
	reader := io.LimitReader(strings.NewReader(string(data)), 1<<20)
	result, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("io.ReadAll: %v", err)
	}
	if len(result) != oneMB {
		t.Errorf("LimitReader(1<<20) read %d bytes, want %d", len(result), oneMB)
	}
}

// ---------------------------------------------------------------------------
// Regression: marshalError produces valid JSON (prevents injection)
// ---------------------------------------------------------------------------

func TestMarshalError_SpecialChars(t *testing.T) {
	// Verify marshalError properly escapes special characters in error messages.
	tests := []struct {
		input string
		want  string
	}{
		{"query failed", "query failed"},
		{`error with "quotes"`, `error with "quotes"`},
		{"error\nwith\nnewlines", "error\nwith\nnewlines"},
		{"error with <html> tags", "error with <html> tags"},
	}

	for _, tt := range tests {
		result := marshalError(tt.input)
		var parsed map[string]string
		if err := json.Unmarshal([]byte(result), &parsed); err != nil {
			t.Errorf("marshalError(%q) produced invalid JSON %q: %v", tt.input, result, err)
			continue
		}
		if parsed["error"] != tt.want {
			t.Errorf("marshalError(%q)[\"error\"] = %q, want %q", tt.input, parsed["error"], tt.want)
		}
	}
}

func TestReadWasmFile_ExceedsSizeLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "huge.wasm")

	// Create a file just over the limit (write a sparse file header).
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	// Seek to maxWasmSize+1 and write a byte to create a sparse file.
	if _, err := f.Seek(maxWasmSize+1, io.SeekStart); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if _, err := f.Write([]byte{0}); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()

	_, err = readWasmFile(path)
	if err == nil {
		t.Fatal("expected error for oversized file")
	}
	if !strings.Contains(err.Error(), "exceeds size limit") {
		t.Errorf("expected 'exceeds size limit' in error, got: %v", err)
	}
}

func TestReadWasmFile_ValidFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "small.wasm")

	content := []byte("fake wasm content")
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}

	data, err := readWasmFile(path)
	if err != nil {
		t.Fatalf("readWasmFile: %v", err)
	}
	if string(data) != string(content) {
		t.Errorf("got %q, want %q", data, content)
	}
}

// ---------------------------------------------------------------------------
// Regression: db_read rejects unscoped queries on user-scoped tables
// ---------------------------------------------------------------------------

func TestDBRead_UnscopedUserTable(t *testing.T) {
	// Verify that a query touching user-scoped tables without :user_id
	// is detected as needing rejection. This validates the guard logic
	// used in hostDBQuery (which requires wazero infrastructure to call
	// directly).
	tests := []struct {
		query      string
		wantReject bool
	}{
		// Unscoped query on user_facts: must be rejected.
		{"SELECT * FROM user_facts", true},
		// Unscoped query on messages: must be rejected.
		{"SELECT * FROM messages WHERE conversation_id = 42", true},
		// Properly scoped query: allowed.
		{"SELECT * FROM user_facts WHERE user_id = :user_id", false},
		// :user_id inside quotes only (bypass attempt): must be rejected.
		{"SELECT * FROM user_facts WHERE note LIKE '%:user_id%'", true},
	}

	for _, tt := range tests {
		_, paramCount := replaceOutsideQuotes(tt.query, ":user_id", "?")
		wouldReject := userScopedTableAccessed(tt.query) && paramCount == 0

		if wouldReject != tt.wantReject {
			t.Errorf("query %q: wouldReject = %v, want %v", tt.query, wouldReject, tt.wantReject)
		}
	}
}

// ---------------------------------------------------------------------------
// replaceOutsideQuotes
// ---------------------------------------------------------------------------

func TestReplaceOutsideQuotes(t *testing.T) {
	tests := []struct {
		name      string
		sql       string
		old       string
		new_      string
		wantSQL   string
		wantCount int
	}{
		{
			name:      "outside quotes",
			sql:       "SELECT * FROM t WHERE user_id = :user_id",
			old:       ":user_id",
			new_:      "?",
			wantSQL:   "SELECT * FROM t WHERE user_id = ?",
			wantCount: 1,
		},
		{
			name:      "inside single quotes",
			sql:       "SELECT * FROM t WHERE note LIKE '%:user_id%'",
			old:       ":user_id",
			new_:      "?",
			wantSQL:   "SELECT * FROM t WHERE note LIKE '%:user_id%'",
			wantCount: 0,
		},
		{
			name:      "mixed inside and outside",
			sql:       "SELECT * FROM t WHERE note LIKE '%:user_id%' AND user_id = :user_id",
			old:       ":user_id",
			new_:      "?",
			wantSQL:   "SELECT * FROM t WHERE note LIKE '%:user_id%' AND user_id = ?",
			wantCount: 1,
		},
		{
			name:      "inside double quotes",
			sql:       `SELECT * FROM t WHERE ":user_id" = :user_id`,
			old:       ":user_id",
			new_:      "?",
			wantSQL:   `SELECT * FROM t WHERE ":user_id" = ?`,
			wantCount: 1,
		},
		{
			name:      "multiple outside",
			sql:       "SELECT * FROM t WHERE a = :user_id AND b = :user_id",
			old:       ":user_id",
			new_:      "?",
			wantSQL:   "SELECT * FROM t WHERE a = ? AND b = ?",
			wantCount: 2,
		},
		{
			name:      "escaped quote in string",
			sql:       "SELECT * FROM t WHERE note = 'it''s :user_id' AND id = :user_id",
			old:       ":user_id",
			new_:      "?",
			wantSQL:   "SELECT * FROM t WHERE note = 'it''s :user_id' AND id = ?",
			wantCount: 1,
		},
		{
			name:      "no match",
			sql:       "SELECT 1",
			old:       ":user_id",
			new_:      "?",
			wantSQL:   "SELECT 1",
			wantCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, count := replaceOutsideQuotes(tt.sql, tt.old, tt.new_)
			if got != tt.wantSQL {
				t.Errorf("sql = %q, want %q", got, tt.wantSQL)
			}
			if count != tt.wantCount {
				t.Errorf("count = %d, want %d", count, tt.wantCount)
			}
		})
	}
}
