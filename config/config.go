package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// Config holds all application configuration.
type Config struct {
	Timezone string        `toml:"timezone"`
	Claude   ClaudeConfig  `toml:"claude"`
	Telegram TGConfig      `toml:"telegram"`
	Storage  StorageConfig `toml:"storage"`
	MCP      MCPConfig     `toml:"mcp"`
	Vector   VectorConfig  `toml:"vector"`
	Wasm     WasmConfig    `toml:"wasm"`
	Memory           MemoryConfig           `toml:"memory"`
	Logging          LoggingConfig          `toml:"logging"`
	Health           HealthConfig           `toml:"health"`
	Voice            VoiceConfig            `toml:"voice"`
	ConfirmTools     []string               `toml:"confirm_tools"`
	Projects         []ProjectConfig        `toml:"projects"`
	SkillCollections []SkillCollectionConfig `toml:"skill_collections"`
}

// SkillCollectionConfig defines a directory of external skills.
type SkillCollectionConfig struct {
	Path      string `toml:"path"`
	Namespace string `toml:"namespace"` // optional, defaults to collection name
}

// ProjectConfig defines a project that can be activated for CLI work.
type ProjectConfig struct {
	Name string `toml:"name"`
	Path string `toml:"path"`
}

// Effort controls reasoning depth for Claude requests.
type Effort string

const (
	EffortLow    Effort = "low"
	EffortMedium Effort = "medium"
	EffortHigh   Effort = "high"
	EffortMax    Effort = "max"
)

// ValidEffort returns true if e is a recognized effort level (including empty for default).
func ValidEffort(e Effort) bool {
	switch e {
	case "", EffortLow, EffortMedium, EffortHigh, EffortMax:
		return true
	}
	return false
}

type ClaudeConfig struct {
	CLIPath        string   `toml:"cli_path"`        // path to claude binary (CLI subprocess mode)
	APIKey         string   `toml:"api_key"`         // direct API mode
	OAuthToken     string   `toml:"oauth_token"`     // long-lived token from `claude setup-token` (CLI mode)
	Model          string   `toml:"model"`
	ThinkingEffort Effort   `toml:"thinking_effort"` // reasoning depth: low, medium, high, max
	IsolatedHome   string   `toml:"isolated_home"`   // path to isolated Claude home dir for project work
}

// UseCLI returns true if CLI subprocess mode is configured.
func (c *ClaudeConfig) UseCLI() bool {
	return c.CLIPath != ""
}

// AuthOption returns the SDK request option for the configured auth method.
// Call only after validation ensures APIKey is set. Not used in CLI mode.
func (c *ClaudeConfig) AuthOption() option.RequestOption {
	return option.WithAPIKey(c.APIKey)
}

type TGConfig struct {
	Token         string  `toml:"token"`
	AllowedID     []int64 `toml:"allowed_user_ids"`
	AllowAll      bool    `toml:"allow_all"`
	ShowToolCalls bool    `toml:"show_tool_calls"`
}

type StorageConfig struct {
	DBPath string `toml:"db_path"`
}

type MCPConfig struct {
	Servers []MCPServerConfig `toml:"servers"`
}

type MCPServerConfig struct {
	Name       string            `toml:"name"`
	Command    string            `toml:"command"`
	Args       []string          `toml:"args"`
	Env        map[string]string `toml:"env"`
	EnvInherit []string          `toml:"env_inherit"`
}

type VectorConfig struct {
	Enabled    bool   `toml:"enabled"`
	QdrantAddr string `toml:"qdrant_addr"`
	Embedder   string `toml:"embedder"`    // "fnv" (default), "ollama", "voyage"
	OllamaURL  string `toml:"ollama_url"`  // default "http://localhost:11434"
	OllamaModel string `toml:"ollama_model"` // default "nomic-embed-text"
	OllamaDim  uint64 `toml:"ollama_dim"`  // default 768
	VoyageKey  string `toml:"voyage_api_key"`
	VoyageModel string `toml:"voyage_model"` // default "voyage-3-lite"
	VoyageDim  uint64 `toml:"voyage_dim"`  // default 512
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

type MemoryConfig struct {
	Enabled              bool          `toml:"enabled"`
	MaxFacts             int           `toml:"max_facts"`
	SummaryRelevanceLimit int           `toml:"summary_relevance_limit"`
	SummaryScoreThreshold float64       `toml:"summary_score_threshold"`
	SummarizeModel       string        `toml:"summarize_model"`
	MinMsgToSummarize    int           `toml:"min_messages_to_summarize"`
	VectorSearchTimeoutSec int `toml:"vector_search_timeout_seconds"`
	Observations         ObservationsConfig `toml:"observations"`
}

// ObservationsConfig controls automatic observation extraction and retrieval.
type ObservationsConfig struct {
	Enabled             bool    `toml:"enabled"`
	ExtractionInterval  int     `toml:"extraction_interval"`
	ExtractionModel     string  `toml:"extraction_model"`
	MaxPerConversation  int     `toml:"max_observations_per_conversation"`
	MaxTranscriptChars  int     `toml:"max_transcript_chars"`
	CooldownSeconds     int     `toml:"cooldown_seconds"`
	RetrievalLimit      int     `toml:"retrieval_limit"`
	ScoreThreshold      float64 `toml:"retrieval_score_threshold"`
	// Phase 2 additions.
	HybridSearch          bool    `toml:"hybrid_search"`
	SupersessionThreshold float64 `toml:"supersession_threshold"`
	ProgressiveRetrieval  bool    `toml:"progressive_retrieval"`
	CompactLimit          int     `toml:"compact_limit"`
	ExpandedLimit         int     `toml:"expanded_limit"`
}

type HealthConfig struct {
	Enabled bool `toml:"enabled"`
	Port    int  `toml:"port"`
}

// VoiceConfig controls speech-to-text transcription for voice messages.
type VoiceConfig struct {
	Enabled      bool   `toml:"enabled"`
	OpenAIAPIKey string `toml:"openai_api_key"`
	STTModel     string `toml:"stt_model"`
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
		Telegram: TGConfig{
			ShowToolCalls: true,
		},
		Claude: ClaudeConfig{
			Model: "claude-sonnet-4-6-20250514",
		},
		Storage: StorageConfig{
			DBPath: filepath.Join(defaultDataDir(), "curlycatclaw.db"),
		},
		Vector: VectorConfig{
			Enabled:    false,
			QdrantAddr: "localhost:6334",
			Embedder:   "ollama",
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
		Memory: MemoryConfig{
			Enabled:              false,
			MaxFacts:             50,
			SummaryRelevanceLimit: 3,
			SummaryScoreThreshold: 0.3,
			MinMsgToSummarize:    4,
			VectorSearchTimeoutSec: 5,
			Observations: ObservationsConfig{
				Enabled:               false,
				ExtractionInterval:    3,
				ExtractionModel:       "claude-haiku-4-5",
				MaxPerConversation:    50,
				MaxTranscriptChars:    4000,
				CooldownSeconds:       60,
				RetrievalLimit:        8,
				ScoreThreshold:        0.3,
				HybridSearch:          false,
				SupersessionThreshold: 0.8,
				ProgressiveRetrieval:  false,
				CompactLimit:          15,
				ExpandedLimit:         3,
			},
		},
		Health: HealthConfig{
			Enabled: true,
			Port:    8080,
		},
		Voice: VoiceConfig{
			Enabled:  false,
			STTModel: "whisper-1",
		},
	}

	if err := toml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	// Allow environment variables to override path-related fields so a
	// single config.toml can be shared between local and Docker runs.
	cfg.applyEnvOverrides()

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// applyEnvOverrides lets environment variables override config fields that
// typically differ between local and containerized deployments, so one
// config file can be shared across both.
func (c *Config) applyEnvOverrides() {
	if v := os.Getenv("CURLYCATCLAW_DB_PATH"); v != "" {
		c.Storage.DBPath = v
	}
	if v := os.Getenv("CURLYCATCLAW_QDRANT_ADDR"); v != "" {
		c.Vector.QdrantAddr = v
	}
	if v := os.Getenv("CURLYCATCLAW_MODEL"); v != "" {
		c.Claude.Model = v
	}
	if v := os.Getenv("CURLYCATCLAW_CLI_PATH"); v != "" {
		c.Claude.CLIPath = v
	}
	if v := os.Getenv("CURLYCATCLAW_ISOLATED_HOME"); v != "" {
		c.Claude.IsolatedHome = v
	}
	if v := os.Getenv("CURLYCATCLAW_THINKING_EFFORT"); v != "" {
		c.Claude.ThinkingEffort = Effort(v)
	}
	if v := os.Getenv("CURLYCATCLAW_EMBEDDER"); v != "" {
		c.Vector.Embedder = v
	}
	if v := os.Getenv("CURLYCATCLAW_OLLAMA_URL"); v != "" {
		c.Vector.OllamaURL = v
	}
}

func (c *Config) validate() error {
	hasCLI := c.Claude.CLIPath != ""
	hasAPIKey := c.Claude.APIKey != ""
	if !hasCLI && !hasAPIKey {
		return fmt.Errorf("config: claude section requires cli_path or api_key")
	}
	if hasCLI && hasAPIKey {
		return fmt.Errorf("config: claude.cli_path cannot be combined with api_key")
	}
	if !ValidEffort(c.Claude.ThinkingEffort) {
		return fmt.Errorf("config: claude.thinking_effort must be one of low, medium, high, max; got %q", c.Claude.ThinkingEffort)
	}
	if c.Telegram.Token == "" {
		return fmt.Errorf("config: telegram.token is required")
	}
	if len(c.Telegram.AllowedID) == 0 && !c.Telegram.AllowAll {
		return fmt.Errorf("config: telegram.allowed_user_ids is empty; " +
			"set your Telegram user ID(s) or set telegram.allow_all = true to allow everyone")
	}
	if _, err := time.LoadLocation(c.Timezone); err != nil {
		return fmt.Errorf("config: invalid timezone %q: %w", c.Timezone, err)
	}
	if c.Storage.DBPath == "" {
		return fmt.Errorf("config: storage.db_path is required")
	}
	for i, srv := range c.MCP.Servers {
		if srv.Name == "" {
			return fmt.Errorf("config: mcp.servers[%d].name is required", i)
		}
		if srv.Command == "" {
			return fmt.Errorf("config: mcp.servers[%d].command is required", i)
		}
	}
	if c.Vector.Enabled && c.Vector.QdrantAddr == "" {
		return fmt.Errorf("config: vector.qdrant_addr is required when vector is enabled")
	}
	if c.Vector.Enabled {
		switch c.Vector.Embedder {
		case "fnv", "":
			// fnv is the default, no external config needed
		case "ollama":
			// ollama_url has a default, no required field
		case "voyage":
			if c.Vector.VoyageKey == "" {
				return fmt.Errorf("config: vector.voyage_api_key is required when embedder is \"voyage\"")
			}
		default:
			return fmt.Errorf("config: vector.embedder must be \"fnv\", \"ollama\", or \"voyage\", got %q", c.Vector.Embedder)
		}
	}
	if c.Wasm.Enabled && c.Wasm.SkillsDir == "" {
		return fmt.Errorf("config: wasm.skills_dir is required when wasm is enabled")
	}
	if c.Health.Enabled && (c.Health.Port < 1 || c.Health.Port > 65535) {
		return fmt.Errorf("config: health.port must be between 1 and 65535")
	}
	if c.Claude.IsolatedHome != "" {
		parent := filepath.Dir(c.Claude.IsolatedHome)
		info, err := os.Stat(parent)
		if err != nil {
			return fmt.Errorf("config: claude.isolated_home parent %q does not exist: %w", parent, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("config: claude.isolated_home parent %q is not a directory", parent)
		}
	}
	for i, p := range c.Projects {
		if p.Name == "" {
			return fmt.Errorf("config: projects[%d].name is required", i)
		}
		if p.Path == "" {
			return fmt.Errorf("config: projects[%d].path is required", i)
		}
		info, err := os.Stat(p.Path)
		if err != nil {
			return fmt.Errorf("config: projects[%d].path %q does not exist: %w", i, p.Path, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("config: projects[%d].path %q is not a directory", i, p.Path)
		}
	}
	if c.Voice.Enabled && c.Voice.OpenAIAPIKey == "" {
		return fmt.Errorf("config: voice.openai_api_key is required when voice is enabled")
	}
	for i, sc := range c.SkillCollections {
		if sc.Path == "" {
			return fmt.Errorf("config: skill_collections[%d].path is required", i)
		}
		info, err := os.Stat(sc.Path)
		if err != nil {
			return fmt.Errorf("config: skill_collections[%d].path %q does not exist: %w", i, sc.Path, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("config: skill_collections[%d].path %q is not a directory", i, sc.Path)
		}
	}
	return nil
}

func defaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		slog.Warn("HOME not set, using relative data directory", "fallback", ".curlycatclaw")
		return ".curlycatclaw"
	}
	return filepath.Join(home, ".curlycatclaw")
}
