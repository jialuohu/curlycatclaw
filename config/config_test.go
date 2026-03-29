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
# neither api_key nor auth_token

[telegram]
token = "tok"
`
	path := writeConfig(t, content)

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load without api_key or auth_token should return an error")
	}
	if !strings.Contains(err.Error(), "cli_path") && !strings.Contains(err.Error(), "api_key") {
		t.Errorf("error = %q, want it to mention auth options", err.Error())
	}
}

func TestLoad_AuthTokenOnly(t *testing.T) {
	content := `
[claude]
auth_token = "oauth-test-token"

[telegram]
token = "tok"
allow_all = true

[storage]
db_path = "/tmp/test.db"
`
	path := writeConfig(t, content)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load with auth_token should succeed: %v", err)
	}
	if cfg.Claude.AuthToken != "oauth-test-token" {
		t.Errorf("Claude.AuthToken = %q, want %q", cfg.Claude.AuthToken, "oauth-test-token")
	}
	if cfg.Claude.APIKey != "" {
		t.Errorf("Claude.APIKey = %q, want empty", cfg.Claude.APIKey)
	}
}

func TestLoad_BothAuthMethodsFails(t *testing.T) {
	content := `
[claude]
api_key = "sk-ant-test"
auth_token = "oauth-test"

[telegram]
token = "tok"
allow_all = true
`
	path := writeConfig(t, content)

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load with both api_key and auth_token should return an error")
	}
	if !strings.Contains(err.Error(), "cannot have both") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "cannot have both")
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

func TestValidate_BothAuthMethods(t *testing.T) {
	cfg := &Config{
		Timezone: "UTC",
		Claude:   ClaudeConfig{APIKey: "sk-key", AuthToken: "oauth-token"},
		Telegram: TGConfig{Token: "tok", AllowAll: true},
	}

	err := cfg.validate()
	if err == nil {
		t.Fatal("validate with both auth methods should return an error")
	}
	if !strings.Contains(err.Error(), "cannot have both") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "cannot have both")
	}
}

func TestValidate_AuthTokenOnly(t *testing.T) {
	cfg := &Config{
		Timezone: "UTC",
		Claude:   ClaudeConfig{AuthToken: "oauth-token"},
		Telegram: TGConfig{Token: "tok", AllowAll: true},
		Storage:  StorageConfig{DBPath: "/data/test.db"},
	}

	if err := cfg.validate(); err != nil {
		t.Fatalf("validate with auth_token only should succeed, got: %v", err)
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

func TestLoad_SandboxDefaults(t *testing.T) {
	path := writeConfig(t, validTOML)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Sandbox.Enabled {
		t.Error("Sandbox.Enabled should default to false")
	}
	if len(cfg.Sandbox.ExtraPaths) != 0 {
		t.Errorf("Sandbox.ExtraPaths = %v, want empty", cfg.Sandbox.ExtraPaths)
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

func TestValidate_BudgetEnabledMissingModel(t *testing.T) {
	cfg := &Config{
		Timezone: "UTC",
		Claude:   ClaudeConfig{APIKey: "sk-key"},
		Telegram: TGConfig{Token: "tok", AllowAll: true},
		Storage:  StorageConfig{DBPath: "/data/test.db"},
		Budget:   BudgetConfig{Enabled: true, Model: ""},
	}
	err := cfg.validate()
	if err == nil {
		t.Fatal("expected error for budget enabled without model")
	}
	if !strings.Contains(err.Error(), "budget.model") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "budget.model")
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
