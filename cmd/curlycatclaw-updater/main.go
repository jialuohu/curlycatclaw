package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"syscall"
	"time"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	secret := os.Getenv("UPDATER_SECRET")
	if secret == "" {
		slog.Error("UPDATER_SECRET env var is required")
		os.Exit(1)
	}

	serviceName := envOrDefault("SERVICE_NAME", "curlycatclaw")
	if !isValidServiceName(serviceName) {
		slog.Error("SERVICE_NAME contains invalid characters (must be [a-zA-Z0-9_-]+)", "value", serviceName)
		os.Exit(1)
	}

	healthURL := envOrDefault("HEALTH_URL", "http://curlycatclaw:8080/health")
	composeProject := os.Getenv("COMPOSE_PROJECT_NAME")

	statePath := envOrDefault("STATE_PATH", "/data/update-state.json")
	if !isPathUnder(statePath, "/data/") {
		slog.Error("STATE_PATH must resolve under /data/", "value", statePath)
		os.Exit(1)
	}
	buildMode := os.Getenv("BUILD_MODE") == "true" // dev: compose build, not compose pull

	state, err := loadState(statePath)
	if err != nil {
		slog.Warn("failed to load state, starting fresh", "error", err)
		state = &UpdateState{
			PreviousDigests:    []string{},
			UpdateHistory:      []UpdateRecord{},
			BlacklistedDigests: map[string]time.Time{},
		}
	}

	h := &Handler{
		secret:         secret,
		serviceName:    serviceName,
		healthURL:      healthURL,
		composeProject: composeProject,
		statePath:      statePath,
		state:          state,
		startTime:      time.Now(),
		buildMode:      buildMode,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /v1/status", h.authMiddleware(h.handleStatus))
	mux.HandleFunc("POST /v1/check", h.authMiddleware(h.handleCheck))
	mux.HandleFunc("POST /v1/update", h.authMiddleware(h.handleUpdate))
	mux.HandleFunc("POST /v1/rollback", h.authMiddleware(h.handleRollback))

	srv := &http.Server{
		Addr:         ":8081",
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	go func() {
		slog.Info("updater listening", "addr", srv.Addr, "service", serviceName)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown error", "error", err)
	}
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// validServiceName matches Docker Compose service names: alphanumeric, dash, underscore.
var validServiceName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

// isValidServiceName returns true if name is a safe Docker Compose service name.
func isValidServiceName(name string) bool {
	return len(name) > 0 && len(name) <= 64 && validServiceName.MatchString(name)
}

// isPathUnder returns true if the cleaned absolute path is inside the given prefix.
func isPathUnder(path, prefix string) bool {
	cleaned := filepath.Clean(path)
	// filepath.Clean removes trailing slashes, so ensure prefix matching
	// checks directory boundaries.
	prefixClean := filepath.Clean(prefix) + string(filepath.Separator)
	return len(cleaned) > len(prefixClean)-1 &&
		cleaned[:len(prefixClean)] == prefixClean
}
