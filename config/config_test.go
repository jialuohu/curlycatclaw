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

func TestLoad_MissingAPIKey(t *testing.T) {
	content := `
[claude]
# api_key intentionally omitted

[telegram]
token = "tok"
`
	path := writeConfig(t, content)

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load without api_key should return an error")
	}
	if !strings.Contains(err.Error(), "api_key") {
		t.Errorf("error = %q, want it to mention %q", err.Error(), "api_key")
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

func TestValidate_MissingAPIKey(t *testing.T) {
	cfg := &Config{
		Timezone: "UTC",
		Claude:   ClaudeConfig{APIKey: ""},
		Telegram: TGConfig{Token: "tok"},
	}

	err := cfg.validate()
	if err == nil {
		t.Fatal("validate with missing api_key should return an error")
	}
	if !strings.Contains(err.Error(), "api_key") {
		t.Errorf("error = %q, want it to mention %q", err.Error(), "api_key")
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
		Telegram: TGConfig{Token: "tok"},
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
		Telegram: TGConfig{Token: "tok"},
	}

	if err := cfg.validate(); err != nil {
		t.Fatalf("validate with valid config returned error: %v", err)
	}
}
