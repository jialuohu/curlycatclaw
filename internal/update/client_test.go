package update

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStatusSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/v1/status" {
			t.Errorf("path = %s, want /v1/status", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(StatusResponse{ //nolint:errcheck
			CurrentVersion:  "0.29.1",
			CurrentDigest:   "sha256:abc123",
			PreviousDigests: []string{"sha256:prev1"},
			UptimeSeconds:   3600,
			LastCheck:       "2026-04-07T12:00:00Z",
			UpdateAvailable: false,
			LatestVersion:   "0.29.1",
			LatestDigest:    "sha256:abc123",
			Updating:        false,
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-secret")
	resp, err := c.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}

	if resp.CurrentVersion != "0.29.1" {
		t.Errorf("CurrentVersion = %q, want %q", resp.CurrentVersion, "0.29.1")
	}
	if resp.CurrentDigest != "sha256:abc123" {
		t.Errorf("CurrentDigest = %q, want %q", resp.CurrentDigest, "sha256:abc123")
	}
	if len(resp.PreviousDigests) != 1 || resp.PreviousDigests[0] != "sha256:prev1" {
		t.Errorf("PreviousDigests = %v, want [sha256:prev1]", resp.PreviousDigests)
	}
	if resp.UptimeSeconds != 3600 {
		t.Errorf("UptimeSeconds = %d, want 3600", resp.UptimeSeconds)
	}
	if resp.UpdateAvailable {
		t.Error("UpdateAvailable = true, want false")
	}
	if resp.Updating {
		t.Error("Updating = true, want false")
	}
}

func TestCheckSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/v1/check" {
			t.Errorf("path = %s, want /v1/check", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(StatusResponse{ //nolint:errcheck
			CurrentDigest:   "sha256:old",
			PreviousDigests: []string{},
			UptimeSeconds:   100,
			UpdateAvailable: true,
			LatestVersion:   "0.30.0",
			LatestDigest:    "sha256:new",
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-secret")
	resp, err := c.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}

	if !resp.UpdateAvailable {
		t.Error("UpdateAvailable = false, want true")
	}
	if resp.LatestVersion != "0.30.0" {
		t.Errorf("LatestVersion = %q, want %q", resp.LatestVersion, "0.30.0")
	}
	if resp.LatestDigest != "sha256:new" {
		t.Errorf("LatestDigest = %q, want %q", resp.LatestDigest, "sha256:new")
	}
}

func TestUpdateAccepted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/v1/update" {
			t.Errorf("path = %s, want /v1/update", r.URL.Path)
		}
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"accepted": true,
			"message":  "Update started",
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-secret")
	err := c.Update(context.Background())
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
}

func TestUpdateOK(t *testing.T) {
	// Update also accepts 200 (already up to date).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"message": "already up to date"}) //nolint:errcheck
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-secret")
	err := c.Update(context.Background())
	if err != nil {
		t.Fatalf("Update (200): %v", err)
	}
}

func TestRollbackSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/v1/rollback" {
			t.Errorf("path = %s, want /v1/rollback", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(RollbackResponse{ //nolint:errcheck
			Success: true,
			Version: "0.29.0",
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-secret")
	resp, err := c.Rollback(context.Background())
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	if !resp.Success {
		t.Error("Success = false, want true")
	}
	if resp.Version != "0.29.0" {
		t.Errorf("Version = %q, want %q", resp.Version, "0.29.0")
	}
	if resp.Error != "" {
		t.Errorf("Error = %q, want empty", resp.Error)
	}
}

func TestAuthHeaderSent(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(StatusResponse{PreviousDigests: []string{}}) //nolint:errcheck
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "my-secret-token")
	_, err := c.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}

	if gotAuth != "Bearer my-secret-token" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer my-secret-token")
	}
}

func TestServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "something broke"}) //nolint:errcheck
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-secret")

	t.Run("status", func(t *testing.T) {
		_, err := c.Status(context.Background())
		if err == nil {
			t.Fatal("expected error for 500 response")
		}
		if !strings.Contains(err.Error(), "500") {
			t.Errorf("error = %q, want to contain '500'", err.Error())
		}
		if !strings.Contains(err.Error(), "something broke") {
			t.Errorf("error = %q, want to contain 'something broke'", err.Error())
		}
	})

	t.Run("check", func(t *testing.T) {
		_, err := c.Check(context.Background())
		if err == nil {
			t.Fatal("expected error for 500 response")
		}
		if !strings.Contains(err.Error(), "500") {
			t.Errorf("error = %q, want to contain '500'", err.Error())
		}
	})

	t.Run("update", func(t *testing.T) {
		err := c.Update(context.Background())
		if err == nil {
			t.Fatal("expected error for 500 response")
		}
		if !strings.Contains(err.Error(), "500") {
			t.Errorf("error = %q, want to contain '500'", err.Error())
		}
	})

	t.Run("rollback", func(t *testing.T) {
		_, err := c.Rollback(context.Background())
		if err == nil {
			t.Fatal("expected error for 500 response")
		}
		if !strings.Contains(err.Error(), "500") {
			t.Errorf("error = %q, want to contain '500'", err.Error())
		}
	})
}

func TestTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Delay longer than the context timeout.
		time.Sleep(3 * time.Second)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(StatusResponse{PreviousDigests: []string{}}) //nolint:errcheck
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-secret")

	// Use a very short context timeout to trigger the deadline.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	// Status uses a 5s internal timeout, but our context is shorter.
	_, err := c.Status(ctx)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	// The error should indicate a context deadline or cancellation.
	errMsg := err.Error()
	if !strings.Contains(errMsg, "deadline") && !strings.Contains(errMsg, "context") && !strings.Contains(errMsg, "canceled") {
		t.Errorf("error = %q, want to contain 'deadline', 'context', or 'canceled'", errMsg)
	}
}

func TestUpdateConflict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]string{"error": "update already in progress"}) //nolint:errcheck
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-secret")
	err := c.Update(context.Background())
	if err == nil {
		t.Fatal("expected error for 409 response")
	}
	if !strings.Contains(err.Error(), "409") {
		t.Errorf("error = %q, want to contain '409'", err.Error())
	}
}
