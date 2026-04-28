package skills

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jialuohu/curlycatclaw/config"
	"github.com/jialuohu/curlycatclaw/internal/memory"
)

// InitTimezoneSkills returns get_timezone and set_timezone skills.
//
// tzChangeCh is the buffered (size 1) signal channel that wakes the
// ReminderActor so it can rebuild its gocron scheduler in the new location.
// The send is non-blocking: a full buffer means another set_timezone call is
// already in flight, and the actor's poll path catches missed signals within
// 10 seconds anyway, so dropping is safe.
//
// Pass nil for tzChangeCh in tests that don't exercise the actor wakeup.
func InitTimezoneSkills(cfg *config.Config, db *sql.DB, tzChangeCh chan<- struct{}) []*Skill {
	return []*Skill{
		{
			Name:        "get_timezone",
			Description: "Show the timezone the daemon is currently using, plus where it came from (override = set via set_timezone, config = from config.toml). Useful before scheduling reminders that depend on local time.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
			Execute:     makeGetTimezoneExecute(cfg, db),
		},
		{
			Name:        "set_timezone",
			Description: "Set the daemon's effective timezone, overriding the config.toml default. Persists across restarts. Pending cron reminders will reschedule shortly (within ~10 seconds in CLI mode). Use a valid IANA name like \"America/Los_Angeles\" or \"Asia/Tokyo\". To revert to the config.toml default, call set_timezone with the value get_timezone reports under \"Config default\".",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"timezone":{"type":"string","description":"IANA timezone name (e.g., America/Los_Angeles, Asia/Tokyo, Europe/Berlin)"}},"required":["timezone"]}`),
			Execute:     makeSetTimezoneExecute(cfg, db, tzChangeCh),
		},
	}
}

func makeGetTimezoneExecute(cfg *config.Config, db *sql.DB) func(ctx context.Context, input json.RawMessage) (string, error) {
	return func(_ context.Context, _ json.RawMessage) (string, error) {
		loc, source := memory.EffectiveLocation(cfg, db)
		now := time.Now().In(loc)
		return fmt.Sprintf(
			"Timezone: %s (source: %s)\nConfig default: %s\nCurrent local time: %s",
			loc.String(),
			source,
			cfg.Timezone,
			now.Format("2006-01-02 15:04 MST"),
		), nil
	}
}

type setTimezoneInput struct {
	Timezone string `json:"timezone"`
}

func makeSetTimezoneExecute(cfg *config.Config, db *sql.DB, tzChangeCh chan<- struct{}) func(ctx context.Context, input json.RawMessage) (string, error) {
	return func(_ context.Context, input json.RawMessage) (string, error) {
		var params setTimezoneInput
		if err := json.Unmarshal(input, &params); err != nil {
			return "", fmt.Errorf("invalid input: %w", err)
		}

		// Trim first; LLMs paste leading/trailing whitespace surprisingly often
		// and time.LoadLocation rejects it.
		tz := strings.TrimSpace(params.Timezone)
		if tz == "" {
			return "", fmt.Errorf("timezone is required (use a valid IANA name like America/Los_Angeles)")
		}

		newLoc, err := time.LoadLocation(tz)
		if err != nil {
			return "", fmt.Errorf("invalid timezone %q: %w (use a valid IANA name like America/Los_Angeles)", tz, err)
		}

		// No-op when the override would resolve to the same effective location
		// the daemon is already using. Avoids waking the actor for a rebuild
		// that would change nothing.
		currentLoc, _ := memory.EffectiveLocation(cfg, db)
		if currentLoc.String() == newLoc.String() {
			return fmt.Sprintf("Timezone already %s. No change.", newLoc.String()), nil
		}

		if err := memory.SetTimezoneOverride(db, tz); err != nil {
			return "", err
		}

		// Non-blocking send: the actor's poll path catches missed signals
		// within 10 seconds, so dropping is safe.
		if tzChangeCh != nil {
			select {
			case tzChangeCh <- struct{}{}:
			default:
			}
		}

		return fmt.Sprintf("Timezone set to %s. Pending cron reminders will reschedule shortly.", newLoc.String()), nil
	}
}
