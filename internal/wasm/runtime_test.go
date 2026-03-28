package wasm

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

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
		{"SELECT * FROM budget_cache WHERE hash = ?", false},
		{"SELECT 1", false},
		{"SELECT count(*) FROM sqlite_master", false},
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
