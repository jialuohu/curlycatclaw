package memory

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jialuohu/curlycatclaw/config"
)

// systemPrefKeyTimezone is the system_prefs row key for the runtime timezone
// override. When present, EffectiveLocation prefers it over cfg.Timezone.
const systemPrefKeyTimezone = "timezone"

// EffectiveLocation returns the timezone the running daemon should treat as
// authoritative, plus a tag describing where it came from ("override" if the
// system_prefs row is set and parseable, "config" otherwise).
//
// Both callers of this function (the get_timezone skill and the startup log
// line) need both values, so returning them together avoids a second query.
//
// Failure modes are graceful: a missing row, a corrupted IANA value, or a DB
// query error all fall back to cfg.Location() and emit a single WARN log so
// drift shows up in `docker compose logs`.
func EffectiveLocation(cfg *config.Config, db *sql.DB) (*time.Location, string) {
	if db == nil {
		// Tests and bootstrap paths can construct an Actor or CronExecutor
		// before the storage layer is wired. Without a DB there's no override
		// to consult, so the config value is the only correct answer.
		return cfg.Location(), "config"
	}
	raw, err := getTimezoneOverride(db)
	if err != nil {
		slog.Warn("memory: timezone override read failed; falling back to config", "err", err)
		return cfg.Location(), "config"
	}
	if raw == "" {
		return cfg.Location(), "config"
	}
	loc, err := time.LoadLocation(raw)
	if err != nil {
		slog.Warn("memory: timezone override is corrupted; falling back to config", "value", raw, "err", err)
		return cfg.Location(), "config"
	}
	return loc, "override"
}

// SetTimezoneOverride persists a timezone override to system_prefs. The caller
// is responsible for validating tz via time.LoadLocation before calling this;
// SetTimezoneOverride trusts its input and stores it verbatim.
func SetTimezoneOverride(db *sql.DB, tz string) error {
	if strings.TrimSpace(tz) == "" {
		return errors.New("memory: timezone override cannot be empty")
	}
	_, err := db.Exec(
		`INSERT INTO system_prefs (key, value, updated_at)
		 VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		systemPrefKeyTimezone, tz, time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("memory: write timezone override: %w", err)
	}
	return nil
}

// getTimezoneOverride reads the raw stored timezone string from system_prefs.
// Returns ("", nil) when no override row exists. Bad rows surface as errors so
// EffectiveLocation can log and fall back.
func getTimezoneOverride(db *sql.DB) (string, error) {
	var v string
	err := db.QueryRow(`SELECT value FROM system_prefs WHERE key = ?`, systemPrefKeyTimezone).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("memory: read timezone override: %w", err)
	}
	return v, nil
}
