package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
)

// Config holds all application configuration.
type Config struct {
	Timezone string        `toml:"timezone"`
	Claude   ClaudeConfig  `toml:"claude"`
	Telegram TGConfig      `toml:"telegram"`
	Storage  StorageConfig `toml:"storage"`
	MCP      MCPConfig     `toml:"mcp"`
	Budget   BudgetConfig  `toml:"budget"`
	Vector   VectorConfig  `toml:"vector"`
	Wasm     WasmConfig    `toml:"wasm"`
	Logging  LoggingConfig `toml:"logging"`
	Sandbox  SandboxConfig `toml:"sandbox"`
}

type ClaudeConfig struct {
	APIKey string `toml:"api_key"`
	Model  string `toml:"model"`
}

type TGConfig struct {
	Token     string  `toml:"token"`
	AllowedID []int64 `toml:"allowed_user_ids"`
}

type StorageConfig struct {
	DBPath string `toml:"db_path"`
}

type MCPConfig struct {
	Servers []MCPServerConfig `toml:"servers"`
}

type MCPServerConfig struct {
	Name    string            `toml:"name"`
	Command string            `toml:"command"`
	Args    []string          `toml:"args"`
	Env     map[string]string `toml:"env"`
}

type BudgetConfig struct {
	Enabled bool   `toml:"enabled"`
	Model   string `toml:"model"`
}

type VectorConfig struct {
	Enabled    bool   `toml:"enabled"`
	QdrantAddr string `toml:"qdrant_addr"`
}

type WasmConfig struct {
	Enabled   bool   `toml:"enabled"`
	SkillsDir string `toml:"skills_dir"`
}

type LoggingConfig struct {
	Level      string `toml:"level"`
	File       string `toml:"file"`
	MaxSize    int    `toml:"max_size"`
	MaxAge     int    `toml:"max_age"`
	MaxBackups int    `toml:"max_backups"`
	Format     string `toml:"format"`
}

type SandboxConfig struct {
	Enabled      bool     `toml:"enabled"`
	ExtraPaths   []string `toml:"extra_paths"`
	ExtraPathsRW []string `toml:"extra_paths_rw"`
}

// Location returns the parsed timezone location.
func (c *Config) Location() *time.Location {
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return time.UTC
	}
	return loc
}

// Load reads config from the given TOML file path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	cfg := &Config{
		Timezone: "UTC",
		Claude: ClaudeConfig{
			Model: "claude-sonnet-4-6-20250514",
		},
		Storage: StorageConfig{
			DBPath: filepath.Join(defaultDataDir(), "curlycatclaw.db"),
		},
		Budget: BudgetConfig{
			Enabled: false,
			Model:   "claude-haiku-4-5-20251001",
		},
		Vector: VectorConfig{
			Enabled:    false,
			QdrantAddr: "localhost:6334",
		},
		Wasm: WasmConfig{
			Enabled:   false,
			SkillsDir: filepath.Join(defaultDataDir(), "skills"),
		},
		Logging: LoggingConfig{
			Level:      "info",
			MaxSize:    50,
			MaxAge:     14,
			MaxBackups: 3,
			Format:     "text",
		},
		Sandbox: SandboxConfig{
			Enabled: false,
		},
	}

	if err := toml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

func (c *Config) validate() error {
	if c.Claude.APIKey == "" {
		return fmt.Errorf("config: claude.api_key is required")
	}
	if c.Telegram.Token == "" {
		return fmt.Errorf("config: telegram.token is required")
	}
	if _, err := time.LoadLocation(c.Timezone); err != nil {
		return fmt.Errorf("config: invalid timezone %q: %w", c.Timezone, err)
	}
	return nil
}

func defaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".curlycatclaw"
	}
	return filepath.Join(home, ".curlycatclaw")
}
