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

// Supervise runs an actor in a supervision loop. If the actor panics or
// returns an error, it is restarted with exponential backoff. The backoff
// resets after the actor runs for longer than healthyPeriod (60s).
//
// Supervise blocks until ctx is cancelled.
func Supervise(ctx context.Context, a Actor) {
	backoff := initialBackoff

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

		// Reset backoff if the actor ran healthily for >60s.
		if time.Since(startedAt) > healthyPeriod {
			backoff = initialBackoff
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		backoff = min(backoff*2, maxBackoff)
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
