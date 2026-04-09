package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// validDigest matches Docker content-addressable digests (sha256:hex) and
// local image IDs (hex only). Prevents injection via crafted digest strings.
var validDigest = regexp.MustCompile(`^(sha256:)?[a-fA-F0-9]{12,128}$`)

// validImageRef matches image references like ghcr.io/owner/repo:tag or
// ghcr.io/owner/repo@sha256:hex. Prevents injection via crafted image refs.
var validImageRef = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._/:-]*$`)

// ghcrTokenResponse is the response from the GHCR token endpoint.
type ghcrTokenResponse struct {
	Token string `json:"token"`
}

// ghcrManifest represents relevant fields from an OCI manifest.
type ghcrManifest struct {
	Digest string `json:"digest"`
	Config struct {
		Digest string `json:"digest"`
	} `json:"config"`
}

// ghcrConfig represents the OCI image config with labels.
type ghcrConfig struct {
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"config"`
}

// ghcrCheck negotiates an anonymous bearer token, fetches the manifest for
// :latest, and extracts the digest and version label.
func ghcrCheck(image string) (version string, digest string, err error) {
	// Parse owner/repo from image like "ghcr.io/jialuohu/curlycatclaw".
	parts := strings.SplitN(image, "/", 3)
	if len(parts) != 3 {
		return "", "", fmt.Errorf("invalid image format: %s", image)
	}
	repo := parts[1] + "/" + parts[2]

	client := &http.Client{Timeout: 30 * time.Second}

	// Step 1: Get anonymous bearer token.
	tokenURL := fmt.Sprintf("https://ghcr.io/token?scope=repository:%s:pull", repo)
	tokenResp, err := client.Get(tokenURL)
	if err != nil {
		return "", "", fmt.Errorf("token request: %w", err)
	}
	defer tokenResp.Body.Close()

	if tokenResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(tokenResp.Body)
		return "", "", fmt.Errorf("token request returned %d: %s", tokenResp.StatusCode, string(body))
	}

	var tokenData ghcrTokenResponse
	if err := json.NewDecoder(tokenResp.Body).Decode(&tokenData); err != nil {
		return "", "", fmt.Errorf("decode token: %w", err)
	}

	// Step 2: Fetch manifest for :latest.
	manifestURL := fmt.Sprintf("https://ghcr.io/v2/%s/manifests/latest", repo)
	manifestReq, err := http.NewRequest("GET", manifestURL, nil)
	if err != nil {
		return "", "", fmt.Errorf("create manifest request: %w", err)
	}
	manifestReq.Header.Set("Authorization", "Bearer "+tokenData.Token)
	// Accept OCI and Docker manifest types.
	manifestReq.Header.Set("Accept", strings.Join([]string{
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.v2+json",
	}, ", "))

	manifestResp, err := client.Do(manifestReq)
	if err != nil {
		return "", "", fmt.Errorf("manifest request: %w", err)
	}
	defer manifestResp.Body.Close()

	if manifestResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(manifestResp.Body)
		return "", "", fmt.Errorf("manifest request returned %d: %s", manifestResp.StatusCode, string(body))
	}

	// The Docker-Content-Digest header gives the manifest digest.
	digest = manifestResp.Header.Get("Docker-Content-Digest")

	var manifest ghcrManifest
	if err := json.NewDecoder(manifestResp.Body).Decode(&manifest); err != nil {
		return "", "", fmt.Errorf("decode manifest: %w", err)
	}

	// If no digest from header, use the one from manifest body.
	if digest == "" {
		digest = manifest.Digest
	}

	// Step 3: Fetch config blob to get labels.
	if manifest.Config.Digest != "" && validDigest.MatchString(manifest.Config.Digest) {
		configURL := fmt.Sprintf("https://ghcr.io/v2/%s/blobs/%s", repo, manifest.Config.Digest)
		configReq, err := http.NewRequest("GET", configURL, nil)
		if err != nil {
			return "", digest, nil // Have digest, no version.
		}
		configReq.Header.Set("Authorization", "Bearer "+tokenData.Token)

		configResp, err := client.Do(configReq)
		if err != nil {
			return "", digest, nil
		}
		defer configResp.Body.Close()

		if configResp.StatusCode == http.StatusOK {
			var cfg ghcrConfig
			if err := json.NewDecoder(configResp.Body).Decode(&cfg); err == nil {
				version = cfg.Config.Labels["org.opencontainers.image.version"]
			}
		}
	}

	return version, digest, nil
}

// composeBuild runs docker compose build for the given service.
func composeBuild(service string) error {
	slog.Info("building image", "service", service)
	cmd := exec.Command("docker", "compose", "build", service)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker compose build: %w: %s", err, string(out))
	}
	return nil
}

func composePull(service string) error {
	slog.Info("pulling image", "service", service)
	cmd := exec.Command("docker", "compose", "pull", service)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker compose pull: %w: %s", err, string(out))
	}
	return nil
}

// composeUp runs docker compose up -d for the given service with optional
// environment variable overrides (used for rollback).
func composeUp(service string, envOverrides map[string]string) error {
	slog.Info("starting service", "service", service, "env_overrides", envOverrides)
	cmd := exec.Command("docker", "compose", "up", "-d", service)

	// Inherit current environment and add overrides.
	cmd.Env = append(cmd.Environ(), mapToEnv(envOverrides)...)

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker compose up: %w: %s", err, string(out))
	}
	return nil
}

// tagImage tags an image by digest with a new tag name.
func tagImage(currentDigest, tagName string) error {
	if !validDigest.MatchString(currentDigest) {
		return fmt.Errorf("invalid digest format: %q", currentDigest)
	}
	if !validImageRef.MatchString(tagName) {
		return fmt.Errorf("invalid tag name format: %q", tagName)
	}
	slog.Info("tagging image", "digest", currentDigest, "tag", tagName)
	cmd := exec.Command("docker", "tag", currentDigest, tagName)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker tag: %w: %s", err, string(out))
	}
	return nil
}

// getCurrentDigest inspects the running container to get its image digest.
func getCurrentDigest(service string) (string, error) {
	// Use docker compose to find the container, then inspect.
	cmd := exec.Command("docker", "compose", "ps", "-q", service)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("docker compose ps: %w", err)
	}

	containerID := strings.TrimSpace(string(out))
	if containerID == "" {
		return "", fmt.Errorf("no running container for service %s", service)
	}
	// Validate container ID is a hex string (Docker container IDs are hex).
	if !validDigest.MatchString(containerID) {
		return "", fmt.Errorf("unexpected container ID format: %q", containerID)
	}

	// Get the image digest via docker inspect.
	inspectCmd := exec.Command("docker", "inspect",
		"--format", "{{index .Image}}", containerID)
	inspectOut, err := inspectCmd.Output()
	if err != nil {
		return "", fmt.Errorf("docker inspect: %w", err)
	}

	digest := strings.TrimSpace(string(inspectOut))
	if digest == "" {
		return "", fmt.Errorf("empty digest for container %s", containerID)
	}
	if !validDigest.MatchString(digest) {
		return "", fmt.Errorf("unexpected image digest format: %q", digest)
	}

	// Also try to get the repo digest (more useful for comparison with
	// registry digests). RepoDigests is an image field, not a container
	// field, so we inspect the image (digest), not the container.
	repoDigestCmd := exec.Command("docker", "inspect",
		"--format", "{{index .RepoDigests 0}}", digest)
	if repoOut, err := repoDigestCmd.Output(); err == nil {
		repoDigest := strings.TrimSpace(string(repoOut))
		// Extract just the digest part after @.
		if idx := strings.Index(repoDigest, "@"); idx >= 0 {
			return repoDigest[idx+1:], nil
		}
	}

	return digest, nil
}

// pollHealth polls the health URL until it returns 200 or the timeout expires.
// Connection-refused and DNS errors are treated as transient (the container is
// still starting up).
func pollHealth(url string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	client := &http.Client{Timeout: 5 * time.Second}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	var lastErr error
	for {
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return fmt.Errorf("health timeout after %s: last error: %w", timeout, lastErr)
			}
			return fmt.Errorf("health timeout after %s", timeout)
		case <-ticker.C:
			resp, err := client.Get(url)
			if err != nil {
				if isTransientNetError(err) {
					slog.Debug("health check transient error", "error", err)
					lastErr = err
					continue
				}
				lastErr = err
				continue
			}
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			lastErr = fmt.Errorf("health returned status %d", resp.StatusCode)
			slog.Debug("health check non-200", "status", resp.StatusCode)
		}
	}
}

// isTransientNetError returns true for connection-refused and DNS errors,
// which are expected while a container is restarting.
func isTransientNetError(err error) bool {
	if err == nil {
		return false
	}

	// Check for connection refused.
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}

	// Check for DNS errors.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}

	// Fallback: check error string for common transient patterns.
	msg := err.Error()
	return strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "dial tcp")
}

// mapToEnv converts a map to a slice of KEY=VALUE strings.
func mapToEnv(m map[string]string) []string {
	if m == nil {
		return nil
	}
	result := make([]string, 0, len(m))
	for k, v := range m {
		result = append(result, k+"="+v)
	}
	return result
}

// composeServiceUp starts a managed service using the base + managed overlay.
func composeServiceUp(name string) error {
	slog.Info("starting managed service", "name", name)
	cmd := exec.Command("docker", "compose",
		"-f", "/compose-project/docker-compose.yml",
		"-f", "/data/docker-compose.managed.yml",
		"up", "-d", name)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker compose up %s: %w: %s", name, err, string(out))
	}
	return nil
}

// composeServiceStop stops a managed service.
func composeServiceStop(name string) error {
	slog.Info("stopping managed service", "name", name)
	cmd := exec.Command("docker", "compose",
		"-f", "/compose-project/docker-compose.yml",
		"-f", "/data/docker-compose.managed.yml",
		"stop", name)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker compose stop %s: %w: %s", name, err, string(out))
	}
	return nil
}

// composeServiceRM removes a stopped managed service container.
func composeServiceRM(name string) error {
	cmd := exec.Command("docker", "compose",
		"-f", "/compose-project/docker-compose.yml",
		"-f", "/data/docker-compose.managed.yml",
		"rm", "-f", name)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker compose rm %s: %w: %s", name, err, string(out))
	}
	return nil
}

// ServiceStatus represents the runtime state of a managed service.
type ServiceStatus struct {
	Name   string `json:"name"`
	Status string `json:"status"` // "running", "exited", "not_found"
	Health string `json:"health"` // "healthy", "unhealthy", "starting", ""
}

// composeServicePS gets the status of a managed service.
func composeServicePS(name string) ServiceStatus {
	cmd := exec.Command("docker", "compose",
		"-f", "/compose-project/docker-compose.yml",
		"-f", "/data/docker-compose.managed.yml",
		"ps", "--format", "json", name)
	out, err := cmd.Output()
	if err != nil {
		return ServiceStatus{Name: name, Status: "not_found"}
	}

	// Handle both JSON array (compose v2.20+) and NDJSON (older) formats.
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" || trimmed == "[]" {
		return ServiceStatus{Name: name, Status: "not_found"}
	}

	type composePSEntry struct {
		Name   string `json:"Name"`
		State  string `json:"State"`
		Health string `json:"Health"`
	}

	var entries []composePSEntry
	if strings.HasPrefix(trimmed, "[") {
		// JSON array format
		_ = json.Unmarshal([]byte(trimmed), &entries)
	} else {
		// NDJSON: one JSON object per line
		for _, line := range strings.Split(trimmed, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var e composePSEntry
			if json.Unmarshal([]byte(line), &e) == nil {
				entries = append(entries, e)
			}
		}
	}

	if len(entries) == 0 {
		return ServiceStatus{Name: name, Status: "not_found"}
	}
	e := entries[0]
	return ServiceStatus{Name: name, Status: e.State, Health: e.Health}
}

// inspectHealth gets the health status of a container by ID or name.
//
//nolint:unused // available for future health enrichment in service handlers
func inspectHealth(containerNameOrID string) string {
	cmd := exec.Command("docker", "inspect",
		"--format", "{{.State.Health.Status}}",
		containerNameOrID)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
