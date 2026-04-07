package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// UpdateState is persisted to disk as JSON.
type UpdateState struct {
	CurrentDigest      string               `json:"current_digest"`
	PreviousDigests    []string             `json:"previous_digests"`
	LastCheck          *time.Time           `json:"last_check,omitempty"`
	UpdateHistory      []UpdateRecord       `json:"update_history"`
	Updating           bool                 `json:"updating"`
	UpdatingSince      *time.Time           `json:"updating_since,omitempty"`
	BlacklistedDigests map[string]time.Time `json:"blacklisted_digests"`
	LatestVersion      string               `json:"latest_version,omitempty"`
	LatestDigest       string               `json:"latest_digest,omitempty"`
}

// UpdateRecord captures one update attempt.
type UpdateRecord struct {
	Time       time.Time `json:"time"`
	FromDigest string    `json:"from_digest"`
	ToDigest   string    `json:"to_digest"`
	Success    bool      `json:"success"`
	Error      string    `json:"error,omitempty"`
}

// Handler holds the HTTP handler state.
type Handler struct {
	secret         string
	serviceName    string
	healthURL      string
	composeProject string
	statePath      string
	startTime      time.Time

	mu    sync.Mutex
	state *UpdateState
}

// StatusResponse is returned by GET /v1/status and POST /v1/check.
type StatusResponse struct {
	CurrentVersion   string   `json:"current_version"`
	CurrentDigest    string   `json:"current_digest"`
	PreviousDigests  []string `json:"previous_digests"`
	UptimeSeconds    int64    `json:"uptime_seconds"`
	LastCheck        string   `json:"last_check,omitempty"`
	UpdateAvailable  bool     `json:"update_available"`
	LatestVersion    string   `json:"latest_version,omitempty"`
	LatestDigest     string   `json:"latest_digest,omitempty"`
	Updating         bool     `json:"updating"`
}

const (
	ghcrImage         = "ghcr.io/jialuohu/curlycatclaw"
	maxPreviousDigest = 3
	staleTimeout      = 10 * time.Minute
	healthTimeout     = 120 * time.Second
	blacklistTTL      = 24 * time.Hour
)

// authMiddleware validates the bearer token.
func (h *Handler) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		token := strings.TrimPrefix(auth, "Bearer ")
		if !strings.HasPrefix(auth, "Bearer ") || subtle.ConstantTimeCompare([]byte(token), []byte(h.secret)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next(w, r)
	}
}

// handleStatus returns the current updater state.
func (h *Handler) handleStatus(w http.ResponseWriter, _ *http.Request) {
	h.mu.Lock()
	resp := h.buildStatusResponse()
	h.mu.Unlock()
	writeJSON(w, http.StatusOK, resp)
}

// handleCheck forces a fresh registry query and returns updated status.
func (h *Handler) handleCheck(w http.ResponseWriter, _ *http.Request) {
	slog.Info("check requested, querying registry")

	version, digest, err := ghcrCheck(ghcrImage)
	if err != nil {
		slog.Error("registry check failed", "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": fmt.Sprintf("registry unreachable: %v", err)})
		return
	}

	h.mu.Lock()
	now := time.Now()
	h.state.LastCheck = &now
	h.state.LatestVersion = version
	h.state.LatestDigest = digest

	// Refresh current digest from Docker if we don't have one yet.
	if h.state.CurrentDigest == "" {
		if d, err := getCurrentDigest(h.serviceName); err == nil {
			h.state.CurrentDigest = d
		}
	}

	resp := h.buildStatusResponse()
	if err := h.saveStateLocked(); err != nil {
		slog.Error("failed to save state", "error", err)
	}
	h.mu.Unlock()

	slog.Info("check complete", "latest_version", version, "latest_digest", digest, "update_available", resp.UpdateAvailable)
	writeJSON(w, http.StatusOK, resp)
}

// handleUpdate starts an async update. Returns 202 immediately.
func (h *Handler) handleUpdate(w http.ResponseWriter, _ *http.Request) {
	h.mu.Lock()

	// Check stale updating lock.
	if h.state.Updating {
		if h.state.UpdatingSince != nil && time.Since(*h.state.UpdatingSince) < staleTimeout {
			h.mu.Unlock()
			writeJSON(w, http.StatusConflict, map[string]string{"error": "update already in progress"})
			return
		}
		slog.Warn("clearing stale update lock", "since", h.state.UpdatingSince)
		h.state.Updating = false
		h.state.UpdatingSince = nil
	}

	// Must have latest digest info (run /check first).
	if h.state.LatestDigest == "" {
		h.mu.Unlock()
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no latest digest available, run /v1/check first"})
		return
	}

	// Check blacklist.
	h.pruneBlacklistLocked()
	if expiry, ok := h.state.BlacklistedDigests[h.state.LatestDigest]; ok {
		h.mu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]string{
			"error":   "digest blacklisted",
			"digest":  h.state.LatestDigest,
			"expires": expiry.Format(time.RFC3339),
		})
		return
	}

	// No update needed if already on latest.
	if h.state.CurrentDigest != "" && h.state.CurrentDigest == h.state.LatestDigest {
		h.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]string{"message": "already up to date"})
		return
	}

	now := time.Now()
	h.state.Updating = true
	h.state.UpdatingSince = &now
	targetDigest := h.state.LatestDigest
	if err := h.saveStateLocked(); err != nil {
		slog.Error("failed to save state", "error", err)
	}
	h.mu.Unlock()

	go h.runUpdate(targetDigest)

	writeJSON(w, http.StatusAccepted, map[string]any{
		"accepted": true,
		"message":  "Update started",
	})
}

// handleRollback rolls back to the previous image.
func (h *Handler) handleRollback(w http.ResponseWriter, _ *http.Request) {
	h.mu.Lock()

	if h.state.Updating {
		if h.state.UpdatingSince != nil && time.Since(*h.state.UpdatingSince) < staleTimeout {
			h.mu.Unlock()
			writeJSON(w, http.StatusConflict, map[string]string{"error": "update in progress, cannot rollback"})
			return
		}
		slog.Warn("clearing stale update lock for rollback")
		h.state.Updating = false
		h.state.UpdatingSince = nil
	}

	if len(h.state.PreviousDigests) == 0 {
		h.mu.Unlock()
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "no previous image to rollback to"})
		return
	}

	rollbackDigest := h.state.PreviousDigests[0]
	now := time.Now()
	h.state.Updating = true
	h.state.UpdatingSince = &now
	h.mu.Unlock()

	slog.Info("manual rollback requested", "target_digest", rollbackDigest)

	go func() {
		if err := h.performRollback(rollbackDigest); err != nil {
			slog.Error("rollback failed", "error", err)
			h.mu.Lock()
			h.state.Updating = false
			h.state.UpdatingSince = nil
			_ = h.saveStateLocked()
			h.mu.Unlock()
			return
		}

		h.mu.Lock()
		h.state.CurrentDigest = rollbackDigest
		if len(h.state.PreviousDigests) > 0 {
			h.state.PreviousDigests = h.state.PreviousDigests[1:]
		}
		h.state.Updating = false
		h.state.UpdatingSince = nil
		if err := h.saveStateLocked(); err != nil {
			slog.Error("failed to save state after rollback", "error", err)
		}
		h.mu.Unlock()
		slog.Info("rollback complete", "digest", rollbackDigest)
	}()

	writeJSON(w, http.StatusAccepted, map[string]any{
		"accepted": true,
		"message":  "Rollback started",
	})
}

// runUpdate performs the full update sequence in a goroutine.
func (h *Handler) runUpdate(targetDigest string) {
	slog.Info("update starting", "target_digest", targetDigest)

	var fromDigest string

	defer func() {
		h.mu.Lock()
		h.state.Updating = false
		h.state.UpdatingSince = nil
		if err := h.saveStateLocked(); err != nil {
			slog.Error("failed to save state after update", "error", err)
		}
		h.mu.Unlock()
	}()

	// Step 1: Get current digest.
	currentDigest, err := getCurrentDigest(h.serviceName)
	if err != nil {
		slog.Warn("could not get current digest, proceeding anyway", "error", err)
	} else {
		fromDigest = currentDigest
	}

	// Step 2: Tag current image for rollback.
	if fromDigest != "" {
		rollbackTag := ghcrImage + ":rollback-1"
		if err := tagImage(fromDigest, rollbackTag); err != nil {
			slog.Warn("failed to tag rollback image", "error", err)
			// Non-fatal: continue with update.
		} else {
			slog.Info("tagged current image for rollback", "digest", fromDigest, "tag", rollbackTag)
		}
	}

	// Step 3: Pull new image.
	if err := composePull(h.serviceName); err != nil {
		h.recordFailure(fromDigest, targetDigest, fmt.Sprintf("pull failed: %v", err))
		return
	}
	slog.Info("pulled new image")

	// Step 4: Bring up the new container.
	if err := composeUp(h.serviceName, nil); err != nil {
		h.recordFailure(fromDigest, targetDigest, fmt.Sprintf("compose up failed: %v", err))
		h.autoRollback(fromDigest, targetDigest)
		return
	}
	slog.Info("new container started, polling health")

	// Step 5: Poll health.
	if err := pollHealth(h.healthURL, healthTimeout); err != nil {
		slog.Error("health check failed after update", "error", err)
		h.recordFailure(fromDigest, targetDigest, fmt.Sprintf("health check failed: %v", err))
		h.autoRollback(fromDigest, targetDigest)
		return
	}

	// Step 6: Success.
	slog.Info("update successful, service healthy")
	h.mu.Lock()
	// Push old digest to previous.
	if fromDigest != "" {
		h.state.PreviousDigests = prependCapped(h.state.PreviousDigests, fromDigest, maxPreviousDigest)
	}
	h.state.CurrentDigest = targetDigest
	h.state.UpdateHistory = append(h.state.UpdateHistory, UpdateRecord{
		Time:       time.Now(),
		FromDigest: fromDigest,
		ToDigest:   targetDigest,
		Success:    true,
	})
	h.mu.Unlock()
}

// autoRollback restores the previous image after a failed update.
func (h *Handler) autoRollback(fromDigest, failedDigest string) {
	if fromDigest == "" {
		slog.Error("cannot auto-rollback: no previous digest")
		return
	}

	slog.Warn("auto-rollback triggered", "from_digest", failedDigest, "to_digest", fromDigest)

	if err := h.performRollback(fromDigest); err != nil {
		slog.Error("auto-rollback failed", "error", err)
		return
	}

	// Blacklist the failed digest.
	h.mu.Lock()
	h.state.BlacklistedDigests[failedDigest] = time.Now().Add(blacklistTTL)
	h.mu.Unlock()

	slog.Info("auto-rollback complete, blacklisted failed digest", "digest", failedDigest)
}

// performRollback brings up the service with the rollback image.
func (h *Handler) performRollback(rollbackDigest string) error {
	envOverrides := map[string]string{
		"CURLYCATCLAW_IMAGE": ghcrImage + ":rollback-1",
	}
	if err := composeUp(h.serviceName, envOverrides); err != nil {
		return fmt.Errorf("compose up for rollback: %w", err)
	}
	if err := pollHealth(h.healthURL, healthTimeout); err != nil {
		return fmt.Errorf("health check after rollback: %w", err)
	}
	return nil
}

// recordFailure logs an update failure.
func (h *Handler) recordFailure(fromDigest, toDigest, errMsg string) {
	slog.Error("update failed", "from", fromDigest, "to", toDigest, "error", errMsg)
	h.mu.Lock()
	h.state.UpdateHistory = append(h.state.UpdateHistory, UpdateRecord{
		Time:       time.Now(),
		FromDigest: fromDigest,
		ToDigest:   toDigest,
		Success:    false,
		Error:      errMsg,
	})
	h.mu.Unlock()
}

// buildStatusResponse must be called with h.mu held.
func (h *Handler) buildStatusResponse() StatusResponse {
	resp := StatusResponse{
		CurrentDigest:   h.state.CurrentDigest,
		PreviousDigests: h.state.PreviousDigests,
		UptimeSeconds:   int64(time.Since(h.startTime).Seconds()),
		LatestVersion:   h.state.LatestVersion,
		LatestDigest:    h.state.LatestDigest,
		Updating:        h.state.Updating,
	}
	if resp.PreviousDigests == nil {
		resp.PreviousDigests = []string{}
	}
	if h.state.LastCheck != nil {
		resp.LastCheck = h.state.LastCheck.Format(time.RFC3339)
	}
	if h.state.LatestDigest != "" && h.state.CurrentDigest != "" {
		resp.UpdateAvailable = h.state.LatestDigest != h.state.CurrentDigest
	}
	// Current version: if we know the latest and are on it, use that; otherwise read from state.
	if h.state.CurrentDigest != "" && h.state.CurrentDigest == h.state.LatestDigest {
		resp.CurrentVersion = h.state.LatestVersion
	}
	return resp
}

// pruneBlacklistLocked removes expired entries. Must be called with h.mu held.
func (h *Handler) pruneBlacklistLocked() {
	now := time.Now()
	for digest, expiry := range h.state.BlacklistedDigests {
		if now.After(expiry) {
			delete(h.state.BlacklistedDigests, digest)
		}
	}
}

// saveStateLocked writes state to disk. Must be called with h.mu held.
func (h *Handler) saveStateLocked() error {
	return saveState(h.statePath, h.state)
}

// prependCapped inserts item at front and caps length.
func prependCapped(items []string, item string, maxLen int) []string {
	result := make([]string, 0, maxLen)
	result = append(result, item)
	for i, existing := range items {
		if i >= maxLen-1 {
			break
		}
		result = append(result, existing)
	}
	return result
}

// loadState reads state from disk.
func loadState(path string) (*UpdateState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var state UpdateState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parse state: %w", err)
	}
	if state.PreviousDigests == nil {
		state.PreviousDigests = []string{}
	}
	if state.UpdateHistory == nil {
		state.UpdateHistory = []UpdateRecord{}
	}
	if state.BlacklistedDigests == nil {
		state.BlacklistedDigests = map[string]time.Time{}
	}
	return &state, nil
}

// saveState writes state to disk atomically.
func saveState(path string, state *UpdateState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write temp state: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename state: %w", err)
	}
	return nil
}

// writeJSON sends a JSON response.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("failed to encode response", "error", err)
	}
}
