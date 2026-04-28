package skills

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jialuohu/curlycatclaw/config"
)

// newTimezoneTestDB returns an in-memory SQLite DB with the system_prefs
// schema applied. The real schema lives in internal/memory/store.go's
// migrate(); we mirror it here so skill tests don't pull in the memory
// package's NewStore (which writes to disk).
func newTimezoneTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db := newTestDB(t)
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS system_prefs (
			key        TEXT PRIMARY KEY,
			value      TEXT NOT NULL,
			updated_at INTEGER NOT NULL
		);
	`)
	if err != nil {
		t.Fatalf("create system_prefs: %v", err)
	}
	return db
}

func initTimezoneSkillsForTest(t *testing.T, cfg *config.Config, db *sql.DB) (map[string]*Skill, chan struct{}) {
	t.Helper()
	ch := make(chan struct{}, 1)
	skills := InitTimezoneSkills(cfg, db, ch)
	m := make(map[string]*Skill, len(skills))
	for _, s := range skills {
		m[s.Name] = s
	}
	return m, ch
}

func TestGetTimezone_ShowsConfigSource(t *testing.T) {
	db := newTimezoneTestDB(t)
	cfg := &config.Config{Timezone: "America/Los_Angeles"}
	skills, _ := initTimezoneSkillsForTest(t, cfg, db)

	out, err := skills["get_timezone"].Execute(context.Background(), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "America/Los_Angeles (source: config)") {
		t.Errorf("output missing config source line:\n%s", out)
	}
	if !strings.Contains(out, "Config default: America/Los_Angeles") {
		t.Errorf("output missing config default line:\n%s", out)
	}
	if !strings.Contains(out, "Current local time:") {
		t.Errorf("output missing local time:\n%s", out)
	}
}

func TestGetTimezone_ShowsOverrideSource(t *testing.T) {
	db := newTimezoneTestDB(t)
	cfg := &config.Config{Timezone: "America/Los_Angeles"}
	skills, ch := initTimezoneSkillsForTest(t, cfg, db)

	setIn, _ := json.Marshal(map[string]string{"timezone": "Asia/Tokyo"})
	if _, err := skills["set_timezone"].Execute(context.Background(), setIn); err != nil {
		t.Fatalf("set_timezone: %v", err)
	}
	<-ch // drain the wakeup signal so the channel doesn't leak across tests

	out, err := skills["get_timezone"].Execute(context.Background(), nil)
	if err != nil {
		t.Fatalf("get_timezone: %v", err)
	}
	if !strings.Contains(out, "Asia/Tokyo (source: override)") {
		t.Errorf("output missing override source line:\n%s", out)
	}
	if !strings.Contains(out, "Config default: America/Los_Angeles") {
		t.Errorf("output missing config default line for revert path:\n%s", out)
	}
}

func TestSetTimezone_PersistsAndSignals(t *testing.T) {
	db := newTimezoneTestDB(t)
	cfg := &config.Config{Timezone: "UTC"}
	skills, ch := initTimezoneSkillsForTest(t, cfg, db)

	in, _ := json.Marshal(map[string]string{"timezone": "Asia/Tokyo"})
	out, err := skills["set_timezone"].Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "Asia/Tokyo") || !strings.Contains(out, "reschedule") {
		t.Errorf("unexpected success message: %q", out)
	}

	// Signal should fire exactly once for a fresh override.
	select {
	case <-ch:
	default:
		t.Errorf("expected wakeup signal on tzChangeCh, got none")
	}

	// DB row should be set.
	var v string
	if err := db.QueryRow(`SELECT value FROM system_prefs WHERE key = 'timezone'`).Scan(&v); err != nil {
		t.Fatalf("DB read: %v", err)
	}
	if v != "Asia/Tokyo" {
		t.Errorf("DB value = %q, want %q", v, "Asia/Tokyo")
	}
}

func TestSetTimezone_TrimsWhitespace(t *testing.T) {
	db := newTimezoneTestDB(t)
	cfg := &config.Config{Timezone: "UTC"}
	skills, _ := initTimezoneSkillsForTest(t, cfg, db)

	in, _ := json.Marshal(map[string]string{"timezone": "  Asia/Tokyo\n"})
	out, err := skills["set_timezone"].Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "Asia/Tokyo.") {
		t.Errorf("trimmed value not in success message: %q", out)
	}

	var v string
	if err := db.QueryRow(`SELECT value FROM system_prefs WHERE key = 'timezone'`).Scan(&v); err != nil {
		t.Fatalf("DB read: %v", err)
	}
	if v != "Asia/Tokyo" {
		t.Errorf("DB stored %q, want trimmed %q", v, "Asia/Tokyo")
	}
}

func TestSetTimezone_RejectsEmpty(t *testing.T) {
	db := newTimezoneTestDB(t)
	cfg := &config.Config{Timezone: "UTC"}
	skills, _ := initTimezoneSkillsForTest(t, cfg, db)

	for _, val := range []string{"", "   ", "\n\t  "} {
		in, _ := json.Marshal(map[string]string{"timezone": val})
		_, err := skills["set_timezone"].Execute(context.Background(), in)
		if err == nil {
			t.Errorf("set_timezone(%q): want error, got nil", val)
		}
	}
}

func TestSetTimezone_RejectsInvalid(t *testing.T) {
	db := newTimezoneTestDB(t)
	cfg := &config.Config{Timezone: "UTC"}
	skills, _ := initTimezoneSkillsForTest(t, cfg, db)

	in, _ := json.Marshal(map[string]string{"timezone": "Not/A_Real_Zone"})
	_, err := skills["set_timezone"].Execute(context.Background(), in)
	if err == nil {
		t.Errorf("set_timezone(invalid): want error, got nil")
	}
	if err != nil && !strings.Contains(err.Error(), "invalid timezone") {
		t.Errorf("expected error to mention 'invalid timezone', got %v", err)
	}
}

func TestSetTimezone_NoOpOnSame(t *testing.T) {
	db := newTimezoneTestDB(t)
	cfg := &config.Config{Timezone: "Asia/Tokyo"}
	skills, ch := initTimezoneSkillsForTest(t, cfg, db)

	in, _ := json.Marshal(map[string]string{"timezone": "Asia/Tokyo"})
	out, err := skills["set_timezone"].Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "already") {
		t.Errorf("expected 'already' in no-op message, got %q", out)
	}

	// No signal should fire when the value is unchanged.
	select {
	case <-ch:
		t.Errorf("unexpected wakeup signal for no-op set_timezone")
	default:
	}

	// And no row should have been written.
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM system_prefs WHERE key = 'timezone'`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 rows for no-op, got %d", count)
	}
}

func TestSetTimezone_ChannelFullDoesNotBlock(t *testing.T) {
	db := newTimezoneTestDB(t)
	cfg := &config.Config{Timezone: "UTC"}
	// Use an unbuffered receiver-less channel to force the non-blocking send to drop.
	fullCh := make(chan struct{}, 1)
	fullCh <- struct{}{} // pre-fill so the next send must drop

	skills := InitTimezoneSkills(cfg, db, fullCh)
	m := make(map[string]*Skill, len(skills))
	for _, s := range skills {
		m[s.Name] = s
	}

	in, _ := json.Marshal(map[string]string{"timezone": "Asia/Tokyo"})
	done := make(chan struct{})
	go func() {
		_, _ = m["set_timezone"].Execute(context.Background(), in)
		close(done)
	}()
	// If the send blocks, this test will time out (test timeout is 10 min by default,
	// but the goroutine should return immediately).
	select {
	case <-done:
		// Good: the skill returned without blocking on the full channel.
	case <-context.Background().Done():
		t.Fatal("set_timezone blocked on full tzChangeCh")
	}
}

func TestSetTimezone_NilChannelIsSafe(t *testing.T) {
	db := newTimezoneTestDB(t)
	cfg := &config.Config{Timezone: "UTC"}

	skills := InitTimezoneSkills(cfg, db, nil)
	m := make(map[string]*Skill, len(skills))
	for _, s := range skills {
		m[s.Name] = s
	}

	in, _ := json.Marshal(map[string]string{"timezone": "Asia/Tokyo"})
	if _, err := m["set_timezone"].Execute(context.Background(), in); err != nil {
		t.Fatalf("Execute with nil tzChangeCh: %v", err)
	}
}
