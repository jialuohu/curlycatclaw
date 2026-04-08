package config

import (
	"testing"
)

func TestMigrateEmailIngest_Enabled(t *testing.T) {
	cfg := &Config{
		EmailIngest: EmailIngestConfig{
			Enabled:              true,
			IntervalMinutes:      15,
			BackfillDays:         30,
			MaxDailyObservations: 100,
			MaxDailyLLMCalls:     200,
			MinImportance:        3,
			Labels:               []string{"INBOX"},
			SkipSenders:          []string{"noreply@"},
		},
	}

	cfg.MigrateEmailIngest()

	if len(cfg.Ingest.Sources) != 1 {
		t.Fatalf("expected 1 source, got %d", len(cfg.Ingest.Sources))
	}

	src := cfg.Ingest.Sources[0]
	if src.Name != "gmail" {
		t.Errorf("expected name gmail, got %s", src.Name)
	}
	if src.Type != "gmail" {
		t.Errorf("expected type gmail, got %s", src.Type)
	}
	if !src.Enabled {
		t.Error("expected enabled")
	}
	if src.IntervalMinutes != 15 {
		t.Errorf("expected interval 15, got %d", src.IntervalMinutes)
	}
	if src.TrustLevel != "untrusted" {
		t.Errorf("expected untrusted, got %s", src.TrustLevel)
	}
	if src.MaxDailyObservations != 100 {
		t.Errorf("expected max_daily_observations 100, got %d", src.MaxDailyObservations)
	}
	if len(src.Prefilter.Labels) != 1 || src.Prefilter.Labels[0] != "INBOX" {
		t.Errorf("unexpected labels: %v", src.Prefilter.Labels)
	}
}

func TestMigrateEmailIngest_Disabled(t *testing.T) {
	cfg := &Config{EmailIngest: EmailIngestConfig{Enabled: false}}
	cfg.MigrateEmailIngest()
	if len(cfg.Ingest.Sources) != 0 {
		t.Fatalf("expected 0 sources for disabled email_ingest, got %d", len(cfg.Ingest.Sources))
	}
}

func TestMigrateEmailIngest_NoDoubleAdd(t *testing.T) {
	cfg := &Config{
		EmailIngest: EmailIngestConfig{Enabled: true, IntervalMinutes: 15, MinImportance: 3},
		Ingest: IngestConfig{
			Sources: []SourceConfig{
				{Name: "gmail", Type: "gmail", Enabled: true, IntervalMinutes: 30, MinImportance: 3},
			},
		},
	}
	cfg.MigrateEmailIngest()
	if len(cfg.Ingest.Sources) != 1 {
		t.Fatalf("expected 1 source (no double-add), got %d", len(cfg.Ingest.Sources))
	}
	if cfg.Ingest.Sources[0].IntervalMinutes != 30 {
		t.Errorf("expected user's interval 30, got %d", cfg.Ingest.Sources[0].IntervalMinutes)
	}
}

// Config validation tests for ingest sources.

func TestValidate_IngestSourceMissingName(t *testing.T) {
	cfg := minimalConfig()
	cfg.Ingest.Sources = []SourceConfig{
		{Type: "gmail", Enabled: true, IntervalMinutes: 15, MinImportance: 3},
	}
	if err := cfg.validate(); err == nil {
		t.Fatal("expected error for missing source name")
	}
}

func TestValidate_IngestSourceInvalidType(t *testing.T) {
	cfg := minimalConfig()
	cfg.Ingest.Sources = []SourceConfig{
		{Name: "test", Type: "invalid", Enabled: true, IntervalMinutes: 15, MinImportance: 3},
	}
	if err := cfg.validate(); err == nil {
		t.Fatal("expected error for invalid source type")
	}
}

func TestValidate_IngestSourceIntervalZero(t *testing.T) {
	cfg := minimalConfig()
	cfg.Ingest.Sources = []SourceConfig{
		{Name: "test", Type: "gmail", Enabled: true, IntervalMinutes: 0, MinImportance: 3},
	}
	if err := cfg.validate(); err == nil {
		t.Fatal("expected error for interval_minutes = 0")
	}
}

func TestValidate_IngestSourceMinImportanceOutOfRange(t *testing.T) {
	cfg := minimalConfig()
	cfg.Ingest.Sources = []SourceConfig{
		{Name: "test", Type: "gmail", Enabled: true, IntervalMinutes: 15, MinImportance: 0},
	}
	if err := cfg.validate(); err == nil {
		t.Fatal("expected error for min_importance = 0")
	}
}

func TestValidate_IngestSourceFileNoRootDir(t *testing.T) {
	cfg := minimalConfig()
	cfg.Ingest.Sources = []SourceConfig{
		{Name: "vault", Type: "file", Enabled: true, IntervalMinutes: 60, MinImportance: 3},
	}
	if err := cfg.validate(); err == nil {
		t.Fatal("expected error for file source without root_dir")
	}
}

func TestValidate_IngestSourceInvalidTrustLevel(t *testing.T) {
	cfg := minimalConfig()
	cfg.Ingest.Sources = []SourceConfig{
		{Name: "test", Type: "gmail", Enabled: true, IntervalMinutes: 15, MinImportance: 3, TrustLevel: "invalid"},
	}
	if err := cfg.validate(); err == nil {
		t.Fatal("expected error for invalid trust_level")
	}
}

func TestValidate_IngestSourceInvalidExtraction(t *testing.T) {
	cfg := minimalConfig()
	cfg.Ingest.Sources = []SourceConfig{
		{Name: "test", Type: "gmail", Enabled: true, IntervalMinutes: 15, MinImportance: 3, Extraction: "invalid"},
	}
	if err := cfg.validate(); err == nil {
		t.Fatal("expected error for invalid extraction mode")
	}
}

func TestValidate_IngestSourceValid(t *testing.T) {
	cfg := minimalConfig()
	cfg.Ingest.Sources = []SourceConfig{
		{Name: "gmail", Type: "gmail", Enabled: true, IntervalMinutes: 15, MinImportance: 3, TrustLevel: "untrusted", Extraction: "llm"},
		{Name: "vault", Type: "file", Enabled: true, IntervalMinutes: 60, MinImportance: 3, TrustLevel: "trusted", Extraction: "hybrid", RootDir: "/tmp"},
		{Name: "notion", Type: "notion", Enabled: true, IntervalMinutes: 30, MinImportance: 3},
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("expected valid config, got: %v", err)
	}
}

func TestValidate_IngestSourceBackfillUnlimited(t *testing.T) {
	cfg := minimalConfig()
	cfg.Ingest.Sources = []SourceConfig{
		{Name: "gmail", Type: "gmail", Enabled: true, IntervalMinutes: 15, MinImportance: 3, BackfillDays: -1},
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("backfill_days=-1 (unlimited) should be valid, got: %v", err)
	}
}

func TestValidate_IngestSourceBackfillInvalid(t *testing.T) {
	cfg := minimalConfig()
	cfg.Ingest.Sources = []SourceConfig{
		{Name: "gmail", Type: "gmail", Enabled: true, IntervalMinutes: 15, MinImportance: 3, BackfillDays: -2},
	}
	if err := cfg.validate(); err == nil {
		t.Fatal("expected error for backfill_days = -2")
	}
}

func TestValidate_IngestDisabledSourcesSkipped(t *testing.T) {
	cfg := minimalConfig()
	cfg.Ingest.Sources = []SourceConfig{
		{Name: "", Type: "invalid", Enabled: false}, // invalid but disabled = skipped
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("expected disabled source to be skipped, got: %v", err)
	}
}

// minimalConfig returns a Config that passes all non-ingest validation.
func minimalConfig() *Config {
	return &Config{
		Timezone: "UTC",
		Claude:   ClaudeConfig{APIKey: "test-key", Model: "test"},
		Telegram: TGConfig{Token: "test", AllowedID: []int64{1}},
		Storage:  StorageConfig{DBPath: "/tmp/test.db"},
		Health:   HealthConfig{Enabled: true, Port: 18080},
	}
}
