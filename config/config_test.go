package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// validTOML is a minimal valid config with all required fields.
const validTOML = `
timezone = "America/New_York"

[claude]
api_key = "sk-test-key"
model   = "claude-sonnet-4-6-20250514"

[telegram]
token           = "123456:ABC-DEF"
allowed_user_ids = [42]

[storage]
db_path = "/tmp/test.db"
`

// writeConfig writes content to a temp file and returns its path.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return path
}

func TestLoad_ValidTOML(t *testing.T) {
	path := writeConfig(t, validTOML)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Timezone != "America/New_York" {
		t.Errorf("Timezone = %q, want %q", cfg.Timezone, "America/New_York")
	}
	if cfg.Claude.APIKey != "sk-test-key" {
		t.Errorf("Claude.APIKey = %q, want %q", cfg.Claude.APIKey, "sk-test-key")
	}
	if cfg.Claude.Model != "claude-sonnet-4-6-20250514" {
		t.Errorf("Claude.Model = %q, want %q", cfg.Claude.Model, "claude-sonnet-4-6-20250514")
	}
	if cfg.Telegram.Token != "123456:ABC-DEF" {
		t.Errorf("Telegram.Token = %q, want %q", cfg.Telegram.Token, "123456:ABC-DEF")
	}
	if len(cfg.Telegram.AllowedID) != 1 || cfg.Telegram.AllowedID[0] != 42 {
		t.Errorf("Telegram.AllowedID = %v, want [42]", cfg.Telegram.AllowedID)
	}
	if cfg.Storage.DBPath != "/tmp/test.db" {
		t.Errorf("Storage.DBPath = %q, want %q", cfg.Storage.DBPath, "/tmp/test.db")
	}
}

func TestLoad_Defaults(t *testing.T) {
	// Minimal config with only required fields; defaults should fill in the rest.
	minimal := `
[claude]
api_key = "sk-key"

[telegram]
token = "tok"
allow_all = true
`
	path := writeConfig(t, minimal)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Timezone != "UTC" {
		t.Errorf("default Timezone = %q, want %q", cfg.Timezone, "UTC")
	}
	if cfg.Claude.Model != "claude-sonnet-4-6-20250514" {
		t.Errorf("default Claude.Model = %q, want %q", cfg.Claude.Model, "claude-sonnet-4-6-20250514")
	}
}

func TestLoad_MissingFile(t *testing.T) {
	_, err := Load("/nonexistent/path/config.toml")
	if err == nil {
		t.Fatal("Load with missing file should return an error")
	}
	if !strings.Contains(err.Error(), "read config") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "read config")
	}
}

func TestLoad_InvalidTOML(t *testing.T) {
	path := writeConfig(t, `[[[broken toml`)

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load with invalid TOML should return an error")
	}
	if !strings.Contains(err.Error(), "parse config") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "parse config")
	}
}

func TestLoad_MissingAuth(t *testing.T) {
	content := `
[claude]
# no cli_path or api_key

[telegram]
token = "tok"
`
	path := writeConfig(t, content)

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load without cli_path or api_key should return an error")
	}
	if !strings.Contains(err.Error(), "cli_path") && !strings.Contains(err.Error(), "api_key") {
		t.Errorf("error = %q, want it to mention auth options", err.Error())
	}
}

func TestLoad_MissingToken(t *testing.T) {
	content := `
[claude]
api_key = "sk-key"

[telegram]
# token intentionally omitted
`
	path := writeConfig(t, content)

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load without telegram token should return an error")
	}
	if !strings.Contains(err.Error(), "token") {
		t.Errorf("error = %q, want it to mention %q", err.Error(), "token")
	}
}

func TestLoad_InvalidTimezone(t *testing.T) {
	content := `
timezone = "Not/A/Timezone"

[claude]
api_key = "sk-key"

[telegram]
token = "tok"
allow_all = true
`
	path := writeConfig(t, content)

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load with invalid timezone should return an error")
	}
	if !strings.Contains(err.Error(), "invalid timezone") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "invalid timezone")
	}
}

func TestLocation_ValidTimezone(t *testing.T) {
	cfg := &Config{Timezone: "Asia/Tokyo"}

	loc := cfg.Location()
	want, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatalf("time.LoadLocation(Asia/Tokyo): %v", err)
	}
	if loc.String() != want.String() {
		t.Errorf("Location() = %q, want %q", loc.String(), want.String())
	}
}

func TestLocation_InvalidTimezone(t *testing.T) {
	cfg := &Config{Timezone: "Invalid/Zone"}

	loc := cfg.Location()
	if loc != time.UTC {
		t.Errorf("Location() with invalid timezone = %q, want UTC", loc.String())
	}
}

func TestValidate_MissingAuth(t *testing.T) {
	cfg := &Config{
		Timezone: "UTC",
		Claude:   ClaudeConfig{},
		Telegram: TGConfig{Token: "tok", AllowAll: true},
	}

	err := cfg.validate()
	if err == nil {
		t.Fatal("validate with no auth should return an error")
	}
	if !strings.Contains(err.Error(), "cli_path") && !strings.Contains(err.Error(), "api_key") {
		t.Errorf("error = %q, want it to mention auth options", err.Error())
	}
}

func TestValidate_CLIAndAPIKeyFails(t *testing.T) {
	cfg := &Config{
		Timezone: "UTC",
		Claude:   ClaudeConfig{CLIPath: "/usr/bin/claude", APIKey: "sk-key"},
		Telegram: TGConfig{Token: "tok", AllowAll: true},
	}

	err := cfg.validate()
	if err == nil {
		t.Fatal("validate with both cli_path and api_key should return an error")
	}
	if !strings.Contains(err.Error(), "cannot be combined") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "cannot be combined")
	}
}

func TestValidate_MissingToken(t *testing.T) {
	cfg := &Config{
		Timezone: "UTC",
		Claude:   ClaudeConfig{APIKey: "sk-key"},
		Telegram: TGConfig{Token: ""},
	}

	err := cfg.validate()
	if err == nil {
		t.Fatal("validate with missing token should return an error")
	}
	if !strings.Contains(err.Error(), "token") {
		t.Errorf("error = %q, want it to mention %q", err.Error(), "token")
	}
}

func TestValidate_InvalidTimezone(t *testing.T) {
	cfg := &Config{
		Timezone: "Bogus/TZ",
		Claude:   ClaudeConfig{APIKey: "sk-key"},
		Telegram: TGConfig{Token: "tok", AllowAll: true},
	}

	err := cfg.validate()
	if err == nil {
		t.Fatal("validate with invalid timezone should return an error")
	}
	if !strings.Contains(err.Error(), "invalid timezone") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "invalid timezone")
	}
}

func TestValidate_AllValid(t *testing.T) {
	cfg := &Config{
		Timezone: "UTC",
		Claude:   ClaudeConfig{APIKey: "sk-key"},
		Telegram: TGConfig{Token: "tok", AllowAll: true},
		Storage:  StorageConfig{DBPath: "/data/test.db"},
	}

	if err := cfg.validate(); err != nil {
		t.Fatalf("validate with valid config returned error: %v", err)
	}
}

func TestValidate_EmptyDBPath(t *testing.T) {
	cfg := &Config{
		Timezone: "UTC",
		Claude:   ClaudeConfig{APIKey: "sk-key"},
		Telegram: TGConfig{Token: "tok", AllowAll: true},
		Storage:  StorageConfig{DBPath: ""},
	}
	if err := cfg.validate(); err == nil {
		t.Fatal("expected error for empty db_path")
	}
}

func TestValidate_MCPServerMissingCommand(t *testing.T) {
	cfg := &Config{
		Timezone: "UTC",
		Claude:   ClaudeConfig{APIKey: "sk-key"},
		Telegram: TGConfig{Token: "tok", AllowAll: true},
		Storage:  StorageConfig{DBPath: "/data/test.db"},
		MCP:      MCPConfig{Servers: []MCPServerConfig{{Name: "test", Command: ""}}},
	}
	if err := cfg.validate(); err == nil {
		t.Fatal("expected error for empty mcp command")
	}
}

func TestValidate_MCPServerTransport(t *testing.T) {
	base := Config{
		Timezone: "UTC",
		Claude:   ClaudeConfig{APIKey: "sk-key"},
		Telegram: TGConfig{Token: "tok", AllowAll: true},
		Storage:  StorageConfig{DBPath: "/data/test.db"},
	}

	tests := []struct {
		name    string
		server  MCPServerConfig
		wantErr bool
	}{
		{
			name:    "stdio valid",
			server:  MCPServerConfig{Name: "s", Command: "echo"},
			wantErr: false,
		},
		{
			name:    "stdio explicit valid",
			server:  MCPServerConfig{Name: "s", Transport: "stdio", Command: "echo"},
			wantErr: false,
		},
		{
			name:    "http valid",
			server:  MCPServerConfig{Name: "s", Transport: "http", URL: "https://example.com/mcp"},
			wantErr: false,
		},
		{
			name:    "http missing url",
			server:  MCPServerConfig{Name: "s", Transport: "http"},
			wantErr: true,
		},
		{
			name:    "http with command rejected",
			server:  MCPServerConfig{Name: "s", Transport: "http", URL: "https://x.com/mcp", Command: "echo"},
			wantErr: true,
		},
		{
			name:    "stdio with url rejected",
			server:  MCPServerConfig{Name: "s", Command: "echo", URL: "https://x.com/mcp"},
			wantErr: true,
		},
		{
			name:    "unknown transport rejected",
			server:  MCPServerConfig{Name: "s", Transport: "grpc", Command: "echo"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base
			cfg.MCP = MCPConfig{Servers: []MCPServerConfig{tt.server}}
			err := cfg.validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidate_VectorEnabledMissingAddr(t *testing.T) {
	cfg := &Config{
		Timezone: "UTC",
		Claude:   ClaudeConfig{APIKey: "sk-key"},
		Telegram: TGConfig{Token: "tok", AllowAll: true},
		Storage:  StorageConfig{DBPath: "/data/test.db"},
		Vector:   VectorConfig{Enabled: true, QdrantAddr: ""},
	}
	if err := cfg.validate(); err == nil {
		t.Fatal("expected error for vector enabled without qdrant_addr")
	}
}

func TestValidate_WasmEnabledMissingDir(t *testing.T) {
	cfg := &Config{
		Timezone: "UTC",
		Claude:   ClaudeConfig{APIKey: "sk-key"},
		Telegram: TGConfig{Token: "tok", AllowAll: true},
		Storage:  StorageConfig{DBPath: "/data/test.db"},
		Wasm:     WasmConfig{Enabled: true, SkillsDir: ""},
	}
	if err := cfg.validate(); err == nil {
		t.Fatal("expected error for wasm enabled without skills_dir")
	}
}

func TestLoad_LoggingDefaults(t *testing.T) {
	path := writeConfig(t, validTOML)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Logging.Level != "info" {
		t.Errorf("Logging.Level = %q, want %q", cfg.Logging.Level, "info")
	}
	if cfg.Logging.MaxSize != 50 {
		t.Errorf("Logging.MaxSize = %d, want 50", cfg.Logging.MaxSize)
	}
	if cfg.Logging.MaxAge != 14 {
		t.Errorf("Logging.MaxAge = %d, want 14", cfg.Logging.MaxAge)
	}
	if cfg.Logging.MaxBackups != 3 {
		t.Errorf("Logging.MaxBackups = %d, want 3", cfg.Logging.MaxBackups)
	}
	if cfg.Logging.Format != "text" {
		t.Errorf("Logging.Format = %q, want %q", cfg.Logging.Format, "text")
	}
	if cfg.Logging.File != "" {
		t.Errorf("Logging.File = %q, want empty", cfg.Logging.File)
	}
}

func TestLoad_LoggingFromTOML(t *testing.T) {
	content := validTOML + `
[logging]
level = "debug"
file = "/var/log/test.log"
max_size = 100
format = "json"
`
	path := writeConfig(t, content)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Logging.Level != "debug" {
		t.Errorf("Logging.Level = %q, want %q", cfg.Logging.Level, "debug")
	}
	if cfg.Logging.File != "/var/log/test.log" {
		t.Errorf("Logging.File = %q, want %q", cfg.Logging.File, "/var/log/test.log")
	}
	if cfg.Logging.MaxSize != 100 {
		t.Errorf("Logging.MaxSize = %d, want 100", cfg.Logging.MaxSize)
	}
	if cfg.Logging.Format != "json" {
		t.Errorf("Logging.Format = %q, want %q", cfg.Logging.Format, "json")
	}
}

func TestValidate_EmptyAllowlistNoAllowAll(t *testing.T) {
	cfg := &Config{
		Timezone: "UTC",
		Claude:   ClaudeConfig{APIKey: "sk-key"},
		Telegram: TGConfig{Token: "tok", AllowedID: nil, AllowAll: false},
	}

	err := cfg.validate()
	if err == nil {
		t.Fatal("validate with empty allowlist and no allow_all should return an error")
	}
	if !strings.Contains(err.Error(), "allowed_user_ids") {
		t.Errorf("error = %q, want it to mention %q", err.Error(), "allowed_user_ids")
	}
}

func TestValidate_EmptyAllowlistWithAllowAll(t *testing.T) {
	cfg := &Config{
		Timezone: "UTC",
		Claude:   ClaudeConfig{APIKey: "sk-key"},
		Telegram: TGConfig{Token: "tok", AllowedID: nil, AllowAll: true},
		Storage:  StorageConfig{DBPath: "/data/test.db"},
	}

	if err := cfg.validate(); err != nil {
		t.Fatalf("validate with allow_all=true should succeed, got: %v", err)
	}
}

func TestValidate_PopulatedAllowlist(t *testing.T) {
	cfg := &Config{
		Timezone: "UTC",
		Claude:   ClaudeConfig{APIKey: "sk-key"},
		Telegram: TGConfig{Token: "tok", AllowedID: []int64{42}},
		Storage:  StorageConfig{DBPath: "/data/test.db"},
	}

	if err := cfg.validate(); err != nil {
		t.Fatalf("validate with populated allowlist should succeed, got: %v", err)
	}
}

func TestLoad_ShowToolCallsDefault(t *testing.T) {
	path := writeConfig(t, validTOML)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if !cfg.Telegram.ShowToolCalls {
		t.Error("ShowToolCalls should default to true")
	}
}

func TestValidate_VectorVoyageMissingKey(t *testing.T) {
	cfg := &Config{
		Timezone: "UTC",
		Claude:   ClaudeConfig{APIKey: "sk-key"},
		Telegram: TGConfig{Token: "tok", AllowAll: true},
		Storage:  StorageConfig{DBPath: "/data/test.db"},
		Vector:   VectorConfig{Enabled: true, QdrantAddr: "localhost:6334", Embedder: "voyage"},
	}
	err := cfg.validate()
	if err == nil {
		t.Fatal("expected error for voyage embedder without voyage_api_key")
	}
	if !strings.Contains(err.Error(), "voyage_api_key") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "voyage_api_key")
	}
}

func TestValidate_VectorUnknownEmbedder(t *testing.T) {
	cfg := &Config{
		Timezone: "UTC",
		Claude:   ClaudeConfig{APIKey: "sk-key"},
		Telegram: TGConfig{Token: "tok", AllowAll: true},
		Storage:  StorageConfig{DBPath: "/data/test.db"},
		Vector:   VectorConfig{Enabled: true, QdrantAddr: "localhost:6334", Embedder: "openai"},
	}
	err := cfg.validate()
	if err == nil {
		t.Fatal("expected error for unknown embedder type")
	}
	if !strings.Contains(err.Error(), `got "openai"`) {
		t.Errorf("error = %q, want it to contain the unknown embedder name", err.Error())
	}
}

func TestValidate_VectorVoyageValid(t *testing.T) {
	cfg := &Config{
		Timezone: "UTC",
		Claude:   ClaudeConfig{APIKey: "sk-key"},
		Telegram: TGConfig{Token: "tok", AllowAll: true},
		Storage:  StorageConfig{DBPath: "/data/test.db"},
		Vector:   VectorConfig{Enabled: true, QdrantAddr: "localhost:6334", Embedder: "voyage", VoyageKey: "vk-test"},
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate with voyage + api key should succeed, got: %v", err)
	}
}

func TestValidate_VectorFNVExplicit(t *testing.T) {
	cfg := &Config{
		Timezone: "UTC",
		Claude:   ClaudeConfig{APIKey: "sk-key"},
		Telegram: TGConfig{Token: "tok", AllowAll: true},
		Storage:  StorageConfig{DBPath: "/data/test.db"},
		Vector:   VectorConfig{Enabled: true, QdrantAddr: "localhost:6334", Embedder: "fnv"},
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("fnv embedder with vector enabled should succeed: %v", err)
	}
}

func TestValidate_VectorOllamaValid(t *testing.T) {
	cfg := &Config{
		Timezone: "UTC",
		Claude:   ClaudeConfig{APIKey: "sk-key"},
		Telegram: TGConfig{Token: "tok", AllowAll: true},
		Storage:  StorageConfig{DBPath: "/data/test.db"},
		Vector:   VectorConfig{Enabled: true, QdrantAddr: "localhost:6334", Embedder: "ollama"},
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("ollama embedder should succeed: %v", err)
	}
}

func TestLoad_EnvOverrides(t *testing.T) {
	path := writeConfig(t, validTOML)

	t.Setenv("CURLYCATCLAW_DB_PATH", "/data/override.db")
	t.Setenv("CURLYCATCLAW_QDRANT_ADDR", "qdrant:6334")
	t.Setenv("CURLYCATCLAW_MODEL", "claude-opus-4-6")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Storage.DBPath != "/data/override.db" {
		t.Errorf("Storage.DBPath = %q, want %q", cfg.Storage.DBPath, "/data/override.db")
	}
	if cfg.Vector.QdrantAddr != "qdrant:6334" {
		t.Errorf("Vector.QdrantAddr = %q, want %q", cfg.Vector.QdrantAddr, "qdrant:6334")
	}
	if cfg.Claude.Model != "claude-opus-4-6" {
		t.Errorf("Claude.Model = %q, want %q", cfg.Claude.Model, "claude-opus-4-6")
	}
}

func TestValidate_IsolatedHomeParentMissing(t *testing.T) {
	cfg := &Config{
		Timezone: "UTC",
		Claude:   ClaudeConfig{APIKey: "sk-key", IsolatedHome: "/nonexistent/parent/claude-home"},
		Telegram: TGConfig{Token: "tok", AllowAll: true},
		Storage:  StorageConfig{DBPath: "/data/test.db"},
	}
	err := cfg.validate()
	if err == nil {
		t.Fatal("expected error for nonexistent isolated_home parent")
	}
	if !strings.Contains(err.Error(), "isolated_home parent") {
		t.Errorf("error = %q, want it to mention isolated_home parent", err.Error())
	}
}

func TestValidate_IsolatedHomeParentExists(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		Timezone: "UTC",
		Claude:   ClaudeConfig{APIKey: "sk-key", IsolatedHome: filepath.Join(dir, "claude-home")},
		Telegram: TGConfig{Token: "tok", AllowAll: true},
		Storage:  StorageConfig{DBPath: "/data/test.db"},
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate should succeed when parent exists: %v", err)
	}
}

func TestValidate_ProjectPathMissing(t *testing.T) {
	cfg := &Config{
		Timezone: "UTC",
		Claude:   ClaudeConfig{APIKey: "sk-key"},
		Telegram: TGConfig{Token: "tok", AllowAll: true},
		Storage:  StorageConfig{DBPath: "/data/test.db"},
		Projects: []ProjectConfig{{Name: "test", Path: "/nonexistent/path"}},
	}
	err := cfg.validate()
	if err == nil {
		t.Fatal("expected error for nonexistent project path")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("error = %q, want it to mention does not exist", err.Error())
	}
}

func TestValidate_ProjectPathNotDir(t *testing.T) {
	f := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(f, []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		Timezone: "UTC",
		Claude:   ClaudeConfig{APIKey: "sk-key"},
		Telegram: TGConfig{Token: "tok", AllowAll: true},
		Storage:  StorageConfig{DBPath: "/data/test.db"},
		Projects: []ProjectConfig{{Name: "test", Path: f}},
	}
	err := cfg.validate()
	if err == nil {
		t.Fatal("expected error for project path that is not a directory")
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("error = %q, want it to mention not a directory", err.Error())
	}
}

func TestValidate_ProjectValid(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		Timezone: "UTC",
		Claude:   ClaudeConfig{APIKey: "sk-key"},
		Telegram: TGConfig{Token: "tok", AllowAll: true},
		Storage:  StorageConfig{DBPath: "/data/test.db"},
		Projects: []ProjectConfig{{Name: "test", Path: dir}},
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate should succeed for valid project: %v", err)
	}
}

func TestValidate_ProjectMissingName(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		Timezone: "UTC",
		Claude:   ClaudeConfig{APIKey: "sk-key"},
		Telegram: TGConfig{Token: "tok", AllowAll: true},
		Storage:  StorageConfig{DBPath: "/data/test.db"},
		Projects: []ProjectConfig{{Name: "", Path: dir}},
	}
	err := cfg.validate()
	if err == nil {
		t.Fatal("expected error for empty project name")
	}
	if !strings.Contains(err.Error(), "name is required") {
		t.Errorf("error = %q, want it to mention name is required", err.Error())
	}
}

func TestLoad_ProjectsFromTOML(t *testing.T) {
	dir := t.TempDir()
	content := validTOML + `
[[projects]]
name = "myapp"
path = "` + dir + `"
`
	path := writeConfig(t, content)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Projects) != 1 {
		t.Fatalf("Projects = %d, want 1", len(cfg.Projects))
	}
	if cfg.Projects[0].Name != "myapp" {
		t.Errorf("Project name = %q, want %q", cfg.Projects[0].Name, "myapp")
	}
	if cfg.Projects[0].Path != dir {
		t.Errorf("Project path = %q, want %q", cfg.Projects[0].Path, dir)
	}
}

func TestLoad_EnvOverridesNotSet(t *testing.T) {
	path := writeConfig(t, validTOML)

	// Ensure env vars are NOT set.
	os.Unsetenv("CURLYCATCLAW_DB_PATH")
	os.Unsetenv("CURLYCATCLAW_QDRANT_ADDR")
	os.Unsetenv("CURLYCATCLAW_MODEL")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Should keep TOML values when env vars are absent.
	if cfg.Storage.DBPath != "/tmp/test.db" {
		t.Errorf("Storage.DBPath = %q, want %q (from TOML)", cfg.Storage.DBPath, "/tmp/test.db")
	}
	if cfg.Claude.Model != "claude-sonnet-4-6-20250514" {
		t.Errorf("Claude.Model = %q, want %q (from TOML)", cfg.Claude.Model, "claude-sonnet-4-6-20250514")
	}
}

func TestValidEffort(t *testing.T) {
	valid := []Effort{"", EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax}
	for _, e := range valid {
		if !ValidEffort(e) {
			t.Errorf("ValidEffort(%q) = false, want true", e)
		}
	}

	invalid := []Effort{"turbo", "HIGH", "Max", "none", "auto"}
	for _, e := range invalid {
		if ValidEffort(e) {
			t.Errorf("ValidEffort(%q) = true, want false", e)
		}
	}
}

func TestValidate_InvalidThinkingEffort(t *testing.T) {
	cfg := &Config{
		Timezone: "UTC",
		Claude:   ClaudeConfig{APIKey: "sk-key", ThinkingEffort: "turbo"},
		Telegram: TGConfig{Token: "tok", AllowAll: true},
		Storage:  StorageConfig{DBPath: "/data/test.db"},
	}
	err := cfg.validate()
	if err == nil {
		t.Fatal("validate with invalid thinking_effort should return an error")
	}
	if !strings.Contains(err.Error(), "thinking_effort") {
		t.Errorf("error = %q, want it to mention thinking_effort", err.Error())
	}
}

func TestValidate_ValidThinkingEffort(t *testing.T) {
	for _, e := range []Effort{"", "low", "medium", "high", "xhigh", "max"} {
		cfg := &Config{
			Timezone: "UTC",
			Claude:   ClaudeConfig{APIKey: "sk-key", ThinkingEffort: e},
			Telegram: TGConfig{Token: "tok", AllowAll: true},
			Storage:  StorageConfig{DBPath: "/data/test.db"},
		}
		if err := cfg.validate(); err != nil {
			t.Errorf("validate with thinking_effort=%q returned error: %v", e, err)
		}
	}
}

func TestLoad_ThinkingEffortFromTOML(t *testing.T) {
	standalone := `
timezone = "America/New_York"

[claude]
api_key = "sk-test-key"
model   = "claude-sonnet-4-6-20250514"
thinking_effort = "high"

[telegram]
token           = "123456:ABC-DEF"
allowed_user_ids = [42]

[storage]
db_path = "/tmp/test.db"
`
	path := writeConfig(t, standalone)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Claude.ThinkingEffort != EffortHigh {
		t.Errorf("ThinkingEffort = %q, want %q", cfg.Claude.ThinkingEffort, EffortHigh)
	}
}

func TestEnvOverride_ThinkingEffort(t *testing.T) {
	path := writeConfig(t, validTOML)

	t.Setenv("CURLYCATCLAW_THINKING_EFFORT", "max")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Claude.ThinkingEffort != EffortMax {
		t.Errorf("ThinkingEffort = %q, want %q (from env)", cfg.Claude.ThinkingEffort, EffortMax)
	}
}
