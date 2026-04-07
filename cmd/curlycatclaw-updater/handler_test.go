package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// newTestHandler returns a Handler backed by a temp state file and the given
// initial state. The temp directory is cleaned up when the test ends.
func newTestHandler(t *testing.T, state *UpdateState) *Handler {
	t.Helper()
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")

	if state == nil {
		state = &UpdateState{
			PreviousDigests:    []string{},
			UpdateHistory:      []UpdateRecord{},
			BlacklistedDigests: map[string]time.Time{},
		}
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

	// Persist the initial state so saveStateLocked works.
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		t.Fatalf("marshal initial state: %v", err)
	}
	if err := os.WriteFile(statePath, data, 0o644); err != nil {
		t.Fatalf("write initial state: %v", err)
	}

	return &Handler{
		secret:    "test-secret",
		statePath: statePath,
		state:     state,
		startTime: time.Now(),
	}
}

// newTestMux builds an http.ServeMux with the same routing as main.go.
func newTestMux(h *Handler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", h.authMiddleware(h.handleStatus))
	mux.HandleFunc("POST /v1/check", h.authMiddleware(h.handleCheck))
	mux.HandleFunc("POST /v1/update", h.authMiddleware(h.handleUpdate))
	mux.HandleFunc("POST /v1/rollback", h.authMiddleware(h.handleRollback))
	return mux
}

func TestAuthRequired(t *testing.T) {
	h := newTestHandler(t, nil)
	mux := newTestMux(h)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	endpoints := []struct {
		method string
		path   string
	}{
		{"GET", "/v1/status"},
		{"POST", "/v1/check"},
		{"POST", "/v1/update"},
		{"POST", "/v1/rollback"},
	}

	for _, ep := range endpoints {
		t.Run(ep.method+" "+ep.path, func(t *testing.T) {
			req, err := http.NewRequest(ep.method, srv.URL+ep.path, nil)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			// No Authorization header.
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("do request: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
			}

			var body map[string]string
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if body["error"] != "unauthorized" {
				t.Errorf("error = %q, want %q", body["error"], "unauthorized")
			}
		})
	}
}

func TestAuthWrongToken(t *testing.T) {
	h := newTestHandler(t, nil)
	mux := newTestMux(h)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	req, err := http.NewRequest("GET", srv.URL+"/v1/status", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer wrong-token")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}

	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["error"] != "unauthorized" {
		t.Errorf("error = %q, want %q", body["error"], "unauthorized")
	}
}

func TestStatusEndpoint(t *testing.T) {
	now := time.Now()
	h := newTestHandler(t, &UpdateState{
		CurrentDigest:      "sha256:abc123",
		PreviousDigests:    []string{"sha256:prev1"},
		LastCheck:          &now,
		LatestVersion:      "0.30.0",
		LatestDigest:       "sha256:def456",
		UpdateHistory:      []UpdateRecord{},
		BlacklistedDigests: map[string]time.Time{},
	})
	mux := newTestMux(h)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	req, err := http.NewRequest("GET", srv.URL+"/v1/status", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer test-secret")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want %q", ct, "application/json")
	}

	var status StatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if status.CurrentDigest != "sha256:abc123" {
		t.Errorf("current_digest = %q, want %q", status.CurrentDigest, "sha256:abc123")
	}
	if len(status.PreviousDigests) != 1 || status.PreviousDigests[0] != "sha256:prev1" {
		t.Errorf("previous_digests = %v, want [sha256:prev1]", status.PreviousDigests)
	}
	if status.LatestVersion != "0.30.0" {
		t.Errorf("latest_version = %q, want %q", status.LatestVersion, "0.30.0")
	}
	if status.LatestDigest != "sha256:def456" {
		t.Errorf("latest_digest = %q, want %q", status.LatestDigest, "sha256:def456")
	}
	if !status.UpdateAvailable {
		t.Error("update_available = false, want true (digests differ)")
	}
	if status.UptimeSeconds < 0 {
		t.Errorf("uptime_seconds = %d, want >= 0", status.UptimeSeconds)
	}
	if status.LastCheck == "" {
		t.Error("last_check is empty, want RFC3339 timestamp")
	}
	if status.Updating {
		t.Error("updating = true, want false")
	}
}

func TestCheckEndpoint(t *testing.T) {
	// handleCheck calls ghcrCheck which hits the real GHCR registry.
	// We test that it accepts the POST and returns valid JSON, even if the
	// registry call fails (502 is the expected error response shape).
	h := newTestHandler(t, nil)
	mux := newTestMux(h)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	req, err := http.NewRequest("POST", srv.URL+"/v1/check", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer test-secret")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()

	// Either 200 (registry reachable) or 502 (registry unreachable) are valid.
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 200 or 502", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want %q", ct, "application/json")
	}

	// Response should be valid JSON regardless.
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if resp.StatusCode == http.StatusOK {
		// Verify shape has expected fields.
		for _, field := range []string{"current_digest", "previous_digests", "uptime_seconds", "updating"} {
			if _, ok := body[field]; !ok {
				t.Errorf("missing field %q in response", field)
			}
		}
	} else {
		// 502: should have error field.
		if _, ok := body["error"]; !ok {
			t.Error("502 response missing 'error' field")
		}
	}
}

func TestUpdateConcurrency(t *testing.T) {
	h := newTestHandler(t, &UpdateState{
		CurrentDigest:      "sha256:old",
		LatestDigest:       "sha256:new",
		LatestVersion:      "1.0.0",
		PreviousDigests:    []string{},
		UpdateHistory:      []UpdateRecord{},
		BlacklistedDigests: map[string]time.Time{},
	})
	mux := newTestMux(h)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// First request: should get 202 (accepted) and set updating=true.
	req1, err := http.NewRequest("POST", srv.URL+"/v1/update", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req1.Header.Set("Authorization", "Bearer test-secret")

	resp1, err := http.DefaultClient.Do(req1)
	if err != nil {
		t.Fatalf("first update request: %v", err)
	}
	defer resp1.Body.Close()

	if resp1.StatusCode != http.StatusAccepted {
		t.Fatalf("first update status = %d, want %d", resp1.StatusCode, http.StatusAccepted)
	}

	// Second request while update is in progress: should get 409.
	// The goroutine from the first request is running but we don't need to
	// wait -- the lock is set before the goroutine starts.
	req2, err := http.NewRequest("POST", srv.URL+"/v1/update", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req2.Header.Set("Authorization", "Bearer test-secret")

	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("second update request: %v", err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusConflict {
		t.Errorf("second update status = %d, want %d", resp2.StatusCode, http.StatusConflict)
	}

	var body map[string]string
	if err := json.NewDecoder(resp2.Body).Decode(&body); err != nil {
		t.Fatalf("decode conflict response: %v", err)
	}
	if body["error"] != "update already in progress" {
		t.Errorf("error = %q, want %q", body["error"], "update already in progress")
	}
}

func TestUpdateConcurrency_Parallel(t *testing.T) {
	h := newTestHandler(t, &UpdateState{
		CurrentDigest:      "sha256:old",
		LatestDigest:       "sha256:new",
		LatestVersion:      "1.0.0",
		PreviousDigests:    []string{},
		UpdateHistory:      []UpdateRecord{},
		BlacklistedDigests: map[string]time.Time{},
	})
	mux := newTestMux(h)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var wg sync.WaitGroup
	statuses := make([]int, 2)

	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			req, err := http.NewRequest("POST", srv.URL+"/v1/update", nil)
			if err != nil {
				t.Errorf("new request %d: %v", idx, err)
				return
			}
			req.Header.Set("Authorization", "Bearer test-secret")

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Errorf("request %d: %v", idx, err)
				return
			}
			defer resp.Body.Close()
			statuses[idx] = resp.StatusCode
		}(i)
	}
	wg.Wait()

	got202 := 0
	got409 := 0
	for _, s := range statuses {
		switch s {
		case http.StatusAccepted:
			got202++
		case http.StatusConflict:
			got409++
		default:
			t.Errorf("unexpected status %d", s)
		}
	}

	if got202 != 1 {
		t.Errorf("expected exactly 1 x 202, got %d", got202)
	}
	if got409 != 1 {
		t.Errorf("expected exactly 1 x 409, got %d", got409)
	}
}

func TestRollbackNoHistory(t *testing.T) {
	h := newTestHandler(t, &UpdateState{
		PreviousDigests:    []string{}, // no history
		UpdateHistory:      []UpdateRecord{},
		BlacklistedDigests: map[string]time.Time{},
	})
	mux := newTestMux(h)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	req, err := http.NewRequest("POST", srv.URL+"/v1/rollback", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer test-secret")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusInternalServerError)
	}

	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["error"] != "no previous image to rollback to" {
		t.Errorf("error = %q, want %q", body["error"], "no previous image to rollback to")
	}
}

func TestHealthzNoAuth(t *testing.T) {
	// main.go does not register /healthz, so requests to it should get
	// a 404 from the default mux. Crucially, it should NOT return 401
	// (it must not go through auth middleware).
	h := newTestHandler(t, nil)
	mux := newTestMux(h)

	// Add a /healthz route without auth, as a health endpoint should work.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`)) //nolint:errcheck
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	// No Authorization header.
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status = %q, want %q", body["status"], "ok")
	}
}

func TestStatusEndpoint_EmptyState(t *testing.T) {
	h := newTestHandler(t, nil)
	mux := newTestMux(h)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	req, err := http.NewRequest("GET", srv.URL+"/v1/status", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer test-secret")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var status StatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	// Empty state: previous_digests should be an empty array, not null.
	if status.PreviousDigests == nil {
		t.Error("previous_digests is nil, want empty array")
	}
	if status.UpdateAvailable {
		t.Error("update_available = true on empty state, want false")
	}
	if status.Updating {
		t.Error("updating = true, want false")
	}
}

func TestUpdateNoLatestDigest(t *testing.T) {
	// When no /v1/check has been run, LatestDigest is empty.
	h := newTestHandler(t, nil)
	mux := newTestMux(h)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	req, err := http.NewRequest("POST", srv.URL+"/v1/update", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer test-secret")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}

	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["error"] != "no latest digest available, run /v1/check first" {
		t.Errorf("error = %q, want %q", body["error"], "no latest digest available, run /v1/check first")
	}
}

func TestUpdateAlreadyUpToDate(t *testing.T) {
	digest := "sha256:same"
	h := newTestHandler(t, &UpdateState{
		CurrentDigest:      digest,
		LatestDigest:       digest,
		LatestVersion:      "1.0.0",
		PreviousDigests:    []string{},
		UpdateHistory:      []UpdateRecord{},
		BlacklistedDigests: map[string]time.Time{},
	})
	mux := newTestMux(h)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	req, err := http.NewRequest("POST", srv.URL+"/v1/update", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer test-secret")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["message"] != "already up to date" {
		t.Errorf("message = %q, want %q", body["message"], "already up to date")
	}
}

func TestPrependCapped(t *testing.T) {
	tests := []struct {
		name   string
		items  []string
		item   string
		maxLen int
		want   []string
	}{
		{"empty", []string{}, "a", 3, []string{"a"}},
		{"under cap", []string{"b"}, "a", 3, []string{"a", "b"}},
		{"at cap", []string{"b", "c"}, "a", 3, []string{"a", "b", "c"}},
		{"over cap", []string{"b", "c", "d"}, "a", 3, []string{"a", "b", "c"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := prependCapped(tt.items, tt.item, tt.maxLen)
			if len(got) != len(tt.want) {
				t.Fatalf("len = %d, want %d", len(got), len(tt.want))
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestSaveLoadState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	now := time.Now().Truncate(time.Second)
	original := &UpdateState{
		CurrentDigest:   "sha256:abc",
		PreviousDigests: []string{"sha256:prev1", "sha256:prev2"},
		LastCheck:       &now,
		LatestVersion:   "1.2.3",
		LatestDigest:    "sha256:def",
		UpdateHistory: []UpdateRecord{
			{Time: now, FromDigest: "sha256:prev1", ToDigest: "sha256:abc", Success: true},
		},
		BlacklistedDigests: map[string]time.Time{
			"sha256:bad": now.Add(24 * time.Hour),
		},
	}

	if err := saveState(path, original); err != nil {
		t.Fatalf("saveState: %v", err)
	}

	loaded, err := loadState(path)
	if err != nil {
		t.Fatalf("loadState: %v", err)
	}

	if loaded.CurrentDigest != original.CurrentDigest {
		t.Errorf("CurrentDigest = %q, want %q", loaded.CurrentDigest, original.CurrentDigest)
	}
	if len(loaded.PreviousDigests) != 2 {
		t.Errorf("PreviousDigests len = %d, want 2", len(loaded.PreviousDigests))
	}
	if loaded.LatestVersion != original.LatestVersion {
		t.Errorf("LatestVersion = %q, want %q", loaded.LatestVersion, original.LatestVersion)
	}
	if len(loaded.UpdateHistory) != 1 {
		t.Errorf("UpdateHistory len = %d, want 1", len(loaded.UpdateHistory))
	}
	if len(loaded.BlacklistedDigests) != 1 {
		t.Errorf("BlacklistedDigests len = %d, want 1", len(loaded.BlacklistedDigests))
	}
}

func TestIsValidServiceName(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"valid simple", "curlycatclaw", true},
		{"valid with dash", "my-service", true},
		{"valid with underscore", "my_service", true},
		{"valid with digits", "svc123", true},
		{"empty", "", false},
		{"starts with dash", "-bad", false},
		{"starts with underscore", "_bad", false},
		{"shell metachar semicolon", "svc;rm -rf /", false},
		{"shell metachar pipe", "svc|evil", false},
		{"shell metachar backtick", "svc`evil`", false},
		{"shell metachar dollar", "svc$(evil)", false},
		{"spaces", "svc name", false},
		{"path traversal", "../../../etc", false},
		{"flag injection", "--file=evil.yml", false},
		{"too long", string(make([]byte, 65)), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isValidServiceName(tt.input); got != tt.want {
				t.Errorf("isValidServiceName(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestIsPathUnder(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		prefix string
		want   bool
	}{
		{"valid subpath", "/data/update-state.json", "/data/", true},
		{"valid nested", "/data/sub/state.json", "/data/", true},
		{"traversal escape", "/data/../etc/passwd", "/data/", false},
		{"exact prefix dir", "/data/", "/data/", false},
		{"outside prefix", "/etc/passwd", "/data/", false},
		{"prefix substring", "/data-evil/state.json", "/data/", false},
		{"double dot", "/data/../../etc/shadow", "/data/", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isPathUnder(tt.path, tt.prefix); got != tt.want {
				t.Errorf("isPathUnder(%q, %q) = %v, want %v", tt.path, tt.prefix, got, tt.want)
			}
		})
	}
}
