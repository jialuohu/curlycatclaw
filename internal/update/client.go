package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// StatusResponse is returned by the updater sidecar's GET /v1/status and POST /v1/check.
type StatusResponse struct {
	CurrentVersion  string   `json:"current_version"`
	CurrentDigest   string   `json:"current_digest"`
	PreviousDigests []string `json:"previous_digests"`
	UptimeSeconds   int64    `json:"uptime_seconds"`
	LastCheck       string   `json:"last_check,omitempty"`
	UpdateAvailable bool     `json:"update_available"`
	LatestVersion   string   `json:"latest_version,omitempty"`
	LatestDigest    string   `json:"latest_digest,omitempty"`
	Updating        bool     `json:"updating"`
}

// Client communicates with the curlycatclaw-updater sidecar.
type Client struct {
	baseURL string
	secret  string
	http    *http.Client
}

// NewClient creates a new update client. The baseURL should be the updater
// sidecar's address (e.g. "http://curlycatclaw-updater:8081"). The secret
// is sent as a Bearer token on every request.
func NewClient(baseURL, secret string) *Client {
	return &Client{
		baseURL: baseURL,
		secret:  secret,
		http: &http.Client{
			// Per-request timeouts are applied via context; this is a safety net.
			Timeout: 3 * time.Minute,
		},
	}
}

// Status returns the current updater state (GET /v1/status, 5s timeout).
func (c *Client) Status(ctx context.Context) (*StatusResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var resp StatusResponse
	if err := c.doJSON(ctx, http.MethodGet, "/v1/status", &resp); err != nil {
		return nil, fmt.Errorf("updater status: %w", err)
	}
	return &resp, nil
}

// Check forces a registry query and returns the updated status (POST /v1/check, 30s timeout).
func (c *Client) Check(ctx context.Context) (*StatusResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var resp StatusResponse
	if err := c.doJSON(ctx, http.MethodPost, "/v1/check", &resp); err != nil {
		return nil, fmt.Errorf("updater check: %w", err)
	}
	return &resp, nil
}

// Update starts an async update on the sidecar (POST /v1/update, 10s timeout).
// The sidecar returns 202 immediately; the actual update runs in the background.
func (c *Client) Update(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/update", nil)
	if err != nil {
		return fmt.Errorf("updater update: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.secret)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("updater update: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusOK {
		return nil
	}
	return c.readError(resp)
}

// Rollback rolls back to the previous image (POST /v1/rollback, 180s timeout).
// The sidecar returns 202 immediately; the actual rollback runs in the background.
func (c *Client) Rollback(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/rollback", nil)
	if err != nil {
		return fmt.Errorf("updater rollback: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.secret)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("updater rollback: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusOK {
		return nil
	}
	return c.readError(resp)
}

// maxResponseBytes is the maximum response body size accepted from the
// updater sidecar. StatusResponse payloads are small (< 4 KB); this cap
// prevents a compromised sidecar from causing OOM via an oversized body.
const maxResponseBytes = 1 << 20 // 1 MiB

// doJSON sends a request and decodes the JSON response into dst.
func (c *Client) doJSON(ctx context.Context, method, path string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.secret)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return c.readError(resp)
	}

	// Limit the response body to prevent OOM from a rogue sidecar.
	limited := io.LimitReader(resp.Body, maxResponseBytes)
	if err := json.NewDecoder(limited).Decode(dst); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// readError extracts an error message from a non-2xx response.
func (c *Client) readError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	var errResp struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &errResp) == nil && errResp.Error != "" {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, errResp.Error)
	}
	if len(body) > 0 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	return fmt.Errorf("HTTP %d", resp.StatusCode)
}
