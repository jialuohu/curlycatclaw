package config

import (
	"fmt"
	"log/slog"
	"net/url"
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
	Ingest           IngestConfig           `toml:"ingest"`
	EmailIngest      EmailIngestConfig      `toml:"email_ingest"` // deprecated: backward compat
	Projects         []ProjectConfig        `toml:"projects"`
	SkillCollections []SkillCollectionConfig `toml:"skill_collections"`
	Eval             EvalConfig             `toml:"eval"`
	Update           UpdateConfig           `toml:"update"`
	GitHub           GitHubConfig           `toml:"github"`
	Personality      PersonalityConfig      `toml:"personality"`
}

// PersonalityConfig controls the agent's persona via an external markdown file.
type PersonalityConfig struct {
	File string `toml:"file"` // absolute path to personality markdown file
}

// GitHubConfig holds GitHub integration settings for issue creation from Telegram.
type GitHubConfig struct {
	Owner string `toml:"owner"` // repository owner, e.g. "jialuohu"
	Repo  string `toml:"repo"`  // repository name, e.g. "curlycatclaw"
}

// OwnerOrDefault returns the configured owner or the curlycatclaw default
// when unset. Used for agent self-reports (bugs about curlycatclaw itself).
func (g GitHubConfig) OwnerOrDefault() string {
	if g.Owner == "" {
		return "jialuohu"
	}
	return g.Owner
}

// RepoOrDefault returns the configured repo or the curlycatclaw default
// when unset. Used for agent self-reports (bugs about curlycatclaw itself).
func (g GitHubConfig) RepoOrDefault() string {
	if g.Repo == "" {
		return "curlycatclaw"
	}
	return g.Repo
}

// UpdateConfig controls the self-update system. Requires the curlycatclaw-updater sidecar.
type UpdateConfig struct {
	Enabled    bool   `toml:"enabled"`
	UpdaterURL string `toml:"updater_url"`
	AutoUpdate bool   `toml:"auto_update"`
	Schedule   string `toml:"schedule"`
}

// EvalConfig controls the self-evaluation pipeline.
type EvalConfig struct {
	Enabled              bool    `toml:"enabled"`
	Schedule             string  `toml:"schedule"`               // cron expression (5-field), default "0 3 * * *"
	LookbackHours        int     `toml:"lookback_hours"`         // hours of history per run, default 24
	ScoreThreshold       float64 `toml:"score_threshold"`        // below this triggers failure mining, default 0.6
	AutoCommit           bool    `toml:"auto_commit"`            // Phase 3: enable auto-commit for high-confidence candidates
	AutoCommitConfidence float64 `toml:"auto_commit_confidence"` // above this auto-commits, default 0.9
	MaxCandidatesPerRun  int     `toml:"max_candidates_per_run"` // prevent flooding memory, default 5
	CandidateExpiryDays  int     `toml:"candidate_expiry_days"`  // pending candidates expire after N days, default 7
	ReportChatID         int64   `toml:"report_chat_id"`         // Telegram chat ID for eval reports
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
	// Transport selects the MCP transport protocol: "" or "stdio" for local
	// subprocesses (default), "http" for remote Streamable HTTP servers.
	Transport string            `toml:"transport"`
	// URL is the remote MCP server endpoint (required when transport is "http").
	URL       string            `toml:"url"`
	// Headers are sent with every HTTP request (e.g. API keys). Values
	// support the encrypted:ref: prefix for credential decryption.
	Headers   map[string]string `toml:"headers"`
}

type VectorConfig struct {
	Enabled    bool   `toml:"enabled"`
	QdrantAddr string `toml:"qdrant_addr"`
	Embedder   string `toml:"embedder"`    // "ollama" (default), "fnv", "voyage"
	OllamaURL  string `toml:"ollama_url"`  // default "http://localhost:11434"
	OllamaModel string `toml:"ollama_model"` // default "bge-m3"
	OllamaDim  uint64 `toml:"ollama_dim"`  // default 1024
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

// IngestConfig controls the generic knowledge source ingest pipeline.
type IngestConfig struct {
	Sources []SourceConfig `toml:"sources"`
}

// SourceConfig defines a single knowledge source for the ingest pipeline.
type SourceConfig struct {
	Name                 string          `toml:"name"`
	Type                 string          `toml:"type"` // "gmail", "file", "notion"
	Enabled              bool            `toml:"enabled"`
	IntervalMinutes      int             `toml:"interval_minutes"`
	BackfillDays         int             `toml:"backfill_days"`
	TrustLevel           string          `toml:"trust_level"` // "trusted" or "untrusted"
	Extraction           string          `toml:"extraction"`  // "llm", "passthrough", "hybrid"
	MaxDailyObservations int             `toml:"max_daily_observations"`
	MaxDailyLLMCalls     int             `toml:"max_daily_llm_calls"`
	MinImportance        int             `toml:"min_importance"`
	MaxBodyChars         int             `toml:"max_body_chars"`
	Prefilter            PrefilterConfig `toml:"prefilter"`

	// Gmail-specific (type = "gmail").
	Accounts []string `toml:"accounts"` // if set, only ingest from these accounts

	// File-specific (type = "file").
	RootDir  string   `toml:"root_dir"`
	Patterns []string `toml:"patterns"`
}

// PrefilterConfig holds source-specific prefilter settings.
type PrefilterConfig struct {
	Labels       []string `toml:"labels"`
	SkipSenders  []string `toml:"skip_senders"`
	IncludePaths []string `toml:"include_paths"`
	ExcludePaths []string `toml:"exclude_paths"`
}

// EmailIngestConfig is the deprecated config format for backward compatibility.
type EmailIngestConfig struct {
	Enabled              bool     `toml:"enabled"`
	IntervalMinutes      int      `toml:"interval_minutes"`
	BackfillDays         int      `toml:"backfill_days"`
	BatchSize            int      `toml:"batch_size"`
	MaxDailyObservations int      `toml:"max_daily_observations"`
	MaxDailyLLMCalls     int      `toml:"max_daily_llm_calls"`
	MinImportance        int      `toml:"min_importance"`
	Labels               []string `toml:"labels"`
	SkipSenders          []string `toml:"skip_senders"`
}

// MigrateEmailIngest converts the deprecated [email_ingest] config into the
// first entry of the new [[ingest.sources]] array for backward compatibility.
func (c *Config) MigrateEmailIngest() {
	if !c.EmailIngest.Enabled {
		return
	}
	// Don't add if user already has a gmail source configured.
	for _, src := range c.Ingest.Sources {
		if src.Type == "gmail" {
			return
		}
	}
	c.Ingest.Sources = append([]SourceConfig{{
		Name:                 "gmail",
		Type:                 "gmail",
		Enabled:              true,
		IntervalMinutes:      c.EmailIngest.IntervalMinutes,
		BackfillDays:         c.EmailIngest.BackfillDays,
		TrustLevel:           "untrusted",
		Extraction:           "llm",
		MaxDailyObservations: c.EmailIngest.MaxDailyObservations,
		MaxDailyLLMCalls:     c.EmailIngest.MaxDailyLLMCalls,
		MinImportance:        c.EmailIngest.MinImportance,
		MaxBodyChars:         4000,
		Prefilter: PrefilterConfig{
			Labels:      c.EmailIngest.Labels,
			SkipSenders: c.EmailIngest.SkipSenders,
		},
	}}, c.Ingest.Sources...)
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
			Port:    18080,
		},
		Voice: VoiceConfig{
			Enabled:  false,
			STTModel: "whisper-1",
		},
		EmailIngest: EmailIngestConfig{
			Enabled:              false,
			IntervalMinutes:      15,
			BackfillDays:         30,
			BatchSize:            20,
			MaxDailyObservations: 100,
			MaxDailyLLMCalls:     200,
			MinImportance:        3,
			Labels:               []string{"INBOX"},
			SkipSenders:          []string{"noreply@", "no-reply@", "notifications@", "mailer-daemon@"},
		},
		Eval: EvalConfig{
			Enabled:              false,
			Schedule:             "0 3 * * *",
			LookbackHours:        24,
			ScoreThreshold:       0.6,
			AutoCommit:           false,
			AutoCommitConfidence: 0.9,
			MaxCandidatesPerRun:  5,
			CandidateExpiryDays:  7,
		},
		Update: UpdateConfig{
			Enabled:    false,
			UpdaterURL: "http://curlycatclaw-updater:8081",
			Schedule:   "0 3 * * 0",
		},
	}

	if err := toml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	// Backward compat: migrate deprecated [email_ingest] to [[ingest.sources]].
	cfg.MigrateEmailIngest()

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
		switch srv.Transport {
		case "", "stdio":
			if srv.Command == "" {
				return fmt.Errorf("config: mcp.servers[%d].command is required for stdio transport", i)
			}
			if srv.URL != "" {
				return fmt.Errorf("config: mcp.servers[%d].url is not allowed for stdio transport", i)
			}
		case "http":
			if srv.URL == "" {
				return fmt.Errorf("config: mcp.servers[%d].url is required for http transport", i)
			}
			u, err := url.Parse(srv.URL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
				return fmt.Errorf("config: mcp.servers[%d].url must be http:// or https://, got %q", i, srv.URL)
			}
			if srv.Command != "" {
				return fmt.Errorf("config: mcp.servers[%d].command is not allowed for http transport", i)
			}
		default:
			return fmt.Errorf("config: mcp.servers[%d].transport must be \"\", \"stdio\", or \"http\", got %q", i, srv.Transport)
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
	if c.Personality.File != "" {
		if !filepath.IsAbs(c.Personality.File) {
			return fmt.Errorf("config: personality.file must be an absolute path, got %q (e.g. /data/personality.md)", c.Personality.File)
		}
		info, err := os.Stat(c.Personality.File)
		if err != nil {
			return fmt.Errorf("config: personality.file %q does not exist or is not readable: %w", c.Personality.File, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("config: personality.file %q is not a regular file", c.Personality.File)
		}
		if info.Size() > 20*1024 {
			return fmt.Errorf("config: personality.file %q is %d bytes, max allowed is 20KB", c.Personality.File, info.Size())
		}
	}
	for i, src := range c.Ingest.Sources {
		if !src.Enabled {
			continue
		}
		if src.Name == "" {
			return fmt.Errorf("config: ingest.sources[%d].name is required", i)
		}
		switch src.Type {
		case "gmail", "file", "notion":
		default:
			return fmt.Errorf("config: ingest.sources[%d].type must be gmail, file, or notion; got %q", i, src.Type)
		}
		if src.IntervalMinutes < 1 {
			return fmt.Errorf("config: ingest.sources[%d].interval_minutes must be >= 1", i)
		}
		if src.BackfillDays < -1 {
			return fmt.Errorf("config: ingest.sources[%d].backfill_days must be >= 0 (or -1 for unlimited)", i)
		}
		if src.MinImportance < 1 || src.MinImportance > 10 {
			return fmt.Errorf("config: ingest.sources[%d].min_importance must be 1-10", i)
		}
		if src.Type == "file" && src.RootDir == "" {
			return fmt.Errorf("config: ingest.sources[%d].root_dir is required for file sources", i)
		}
		switch src.TrustLevel {
		case "", "trusted", "untrusted":
		default:
			return fmt.Errorf("config: ingest.sources[%d].trust_level must be trusted or untrusted; got %q", i, src.TrustLevel)
		}
		switch src.Extraction {
		case "", "llm", "passthrough", "hybrid":
		default:
			return fmt.Errorf("config: ingest.sources[%d].extraction must be llm, passthrough, or hybrid; got %q", i, src.Extraction)
		}
	}
	if c.Eval.Enabled {
		if c.Eval.LookbackHours < 1 {
			return fmt.Errorf("config: eval.lookback_hours must be >= 1")
		}
		if c.Eval.ScoreThreshold < 0 || c.Eval.ScoreThreshold > 1.0 {
			return fmt.Errorf("config: eval.score_threshold must be 0.0-1.0")
		}
		if c.Eval.MaxCandidatesPerRun < 1 {
			return fmt.Errorf("config: eval.max_candidates_per_run must be >= 1")
		}
	}
	if c.Update.Enabled && c.Update.UpdaterURL == "" {
		return fmt.Errorf("config: update.updater_url is required when update is enabled")
	}
	if c.Update.Enabled && c.Update.AutoUpdate && c.Update.Schedule == "" {
		return fmt.Errorf("config: update.schedule is required when auto_update is enabled")
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
