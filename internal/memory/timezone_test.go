package memory

import (
	"testing"
	"time"

	"github.com/jialuohu/curlycatclaw/config"
)

func TestEffectiveLocation_FallbackToConfig(t *testing.T) {
	s := newTestStore(t)
	cfg := &config.Config{Timezone: "America/Los_Angeles"}

	loc, source := EffectiveLocation(cfg, s.DB())
	if source != "config" {
		t.Errorf("source = %q, want %q", source, "config")
	}
	if loc.String() != "America/Los_Angeles" {
		t.Errorf("loc = %q, want %q", loc.String(), "America/Los_Angeles")
	}
}

func TestEffectiveLocation_OverrideWins(t *testing.T) {
	s := newTestStore(t)
	cfg := &config.Config{Timezone: "America/Los_Angeles"}

	if err := SetTimezoneOverride(s.DB(), "Asia/Tokyo"); err != nil {
		t.Fatalf("SetTimezoneOverride: %v", err)
	}

	loc, source := EffectiveLocation(cfg, s.DB())
	if source != "override" {
		t.Errorf("source = %q, want %q", source, "override")
	}
	if loc.String() != "Asia/Tokyo" {
		t.Errorf("loc = %q, want %q", loc.String(), "Asia/Tokyo")
	}
}

func TestEffectiveLocation_OverrideRoundTripPreservesValue(t *testing.T) {
	s := newTestStore(t)
	cfg := &config.Config{Timezone: "UTC"}

	for _, tz := range []string{"Asia/Tokyo", "America/Los_Angeles", "Europe/Berlin", "UTC"} {
		if err := SetTimezoneOverride(s.DB(), tz); err != nil {
			t.Fatalf("SetTimezoneOverride(%q): %v", tz, err)
		}
		loc, source := EffectiveLocation(cfg, s.DB())
		if loc.String() != tz {
			t.Errorf("after Set(%q), loc = %q", tz, loc.String())
		}
		if source != "override" {
			t.Errorf("after Set(%q), source = %q", tz, source)
		}
	}
}

func TestEffectiveLocation_CorruptedOverrideFallsBack(t *testing.T) {
	s := newTestStore(t)
	cfg := &config.Config{Timezone: "America/Los_Angeles"}

	// Bypass validation: write a bogus IANA name straight to the DB.
	_, err := s.DB().Exec(
		`INSERT INTO system_prefs (key, value, updated_at) VALUES (?, ?, ?)`,
		systemPrefKeyTimezone, "Not/A_Real/Zone", time.Now().Unix(),
	)
	if err != nil {
		t.Fatalf("seed corrupted row: %v", err)
	}

	loc, source := EffectiveLocation(cfg, s.DB())
	if source != "config" {
		t.Errorf("corrupted override should fall back, got source=%q", source)
	}
	if loc.String() != "America/Los_Angeles" {
		t.Errorf("loc = %q, want config fallback %q", loc.String(), "America/Los_Angeles")
	}
}

func TestEffectiveLocation_NilDBFallsBack(t *testing.T) {
	// Integration tests and bootstrap paths construct Actor/CronExecutor
	// before the storage layer is wired. EffectiveLocation must not panic
	// with a nil DB; it falls back to cfg.Location().
	cfg := &config.Config{Timezone: "America/Los_Angeles"}
	loc, source := EffectiveLocation(cfg, nil)
	if source != "config" {
		t.Errorf("source = %q, want %q", source, "config")
	}
	if loc.String() != "America/Los_Angeles" {
		t.Errorf("loc = %q, want %q", loc.String(), "America/Los_Angeles")
	}
}

func TestEffectiveLocation_EmptyConfigDefaultsUTC(t *testing.T) {
	s := newTestStore(t)
	// Config.Location() returns time.UTC when Timezone is empty/invalid.
	cfg := &config.Config{Timezone: ""}

	loc, source := EffectiveLocation(cfg, s.DB())
	if source != "config" {
		t.Errorf("source = %q, want %q", source, "config")
	}
	if loc.String() != "UTC" {
		t.Errorf("loc = %q, want UTC", loc.String())
	}
}

func TestSetTimezoneOverride_RejectsEmpty(t *testing.T) {
	s := newTestStore(t)

	if err := SetTimezoneOverride(s.DB(), ""); err == nil {
		t.Errorf("SetTimezoneOverride(\"\") = nil, want error")
	}
	if err := SetTimezoneOverride(s.DB(), "   "); err == nil {
		t.Errorf("SetTimezoneOverride(whitespace) = nil, want error")
	}
}

func TestSetTimezoneOverride_OverwritesExistingRow(t *testing.T) {
	s := newTestStore(t)
	cfg := &config.Config{Timezone: "UTC"}

	if err := SetTimezoneOverride(s.DB(), "Asia/Tokyo"); err != nil {
		t.Fatalf("first SetTimezoneOverride: %v", err)
	}
	if err := SetTimezoneOverride(s.DB(), "America/Los_Angeles"); err != nil {
		t.Fatalf("second SetTimezoneOverride: %v", err)
	}

	loc, source := EffectiveLocation(cfg, s.DB())
	if source != "override" {
		t.Errorf("source = %q, want %q", source, "override")
	}
	if loc.String() != "America/Los_Angeles" {
		t.Errorf("loc = %q, want second value", loc.String())
	}

	// Confirm only one row exists (PRIMARY KEY upsert worked).
	var count int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM system_prefs WHERE key = ?`, systemPrefKeyTimezone).Scan(&count); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count != 1 {
		t.Errorf("row count = %d, want 1", count)
	}
}
