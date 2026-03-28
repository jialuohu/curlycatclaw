package actor

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

const (
	initialBackoff = time.Second
	maxBackoff     = 30 * time.Second
	healthyPeriod  = 60 * time.Second
)

// SupervisorConfig holds tunable parameters for the supervision loop.
// Zero values fall back to package defaults.
type SupervisorConfig struct {
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	HealthyPeriod  time.Duration
}

func (c SupervisorConfig) initialOrDefault() time.Duration {
	if c.InitialBackoff > 0 {
		return c.InitialBackoff
	}
	return initialBackoff
}

func (c SupervisorConfig) maxOrDefault() time.Duration {
	if c.MaxBackoff > 0 {
		return c.MaxBackoff
	}
	return maxBackoff
}

func (c SupervisorConfig) healthyOrDefault() time.Duration {
	if c.HealthyPeriod > 0 {
		return c.HealthyPeriod
	}
	return healthyPeriod
}

// Supervise runs an actor in a supervision loop with default backoff parameters.
func Supervise(ctx context.Context, a Actor) {
	SuperviseWithConfig(ctx, a, SupervisorConfig{})
}

// SuperviseWithConfig runs an actor in a supervision loop with configurable
// backoff parameters. If the actor panics or returns an error, it is restarted
// with exponential backoff. The backoff resets after the actor runs for longer
// than the healthy period.
//
// SuperviseWithConfig blocks until ctx is cancelled.
func SuperviseWithConfig(ctx context.Context, a Actor, cfg SupervisorConfig) {
	backoff := cfg.initialOrDefault()
	initial := backoff

	for {
		startedAt := time.Now()

		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("actor panicked",
						"actor", a.Name(),
						"panic", r,
						"restart_in", backoff,
					)
				}
			}()
			if err := a.Run(ctx); err != nil {
				slog.Error("actor exited with error",
					"actor", a.Name(),
					"err", err,
				)
			}
		}()

		// Reset backoff if the actor ran healthily for the configured period.
		if time.Since(startedAt) > cfg.healthyOrDefault() {
			backoff = initial
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		backoff = min(backoff*2, cfg.maxOrDefault())
		slog.Info("restarting actor", "actor", a.Name(), "backoff", backoff)
	}
}

// SuperviseAll starts multiple actors under supervision, each in its own
// goroutine. It blocks until ctx is cancelled, then waits up to timeout
// for all actors to drain before returning.
func SuperviseAll(ctx context.Context, timeout time.Duration, actors ...Actor) {
	var wg sync.WaitGroup
	for _, a := range actors {
		wg.Add(1)
		go func(a Actor) {
			defer wg.Done()
			Supervise(ctx, a)
		}(a)
	}

	<-ctx.Done()
	slog.Info("waiting for actors to drain", "timeout", timeout)

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		slog.Info("all actors drained")
	case <-time.After(timeout):
		slog.Warn("actor drain timed out, forcing shutdown", "timeout", timeout)
	}
}
