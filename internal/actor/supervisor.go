package actor

import (
	"context"
	"log/slog"
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
// goroutine. It blocks until ctx is cancelled.
func SuperviseAll(ctx context.Context, actors ...Actor) {
	done := make(chan struct{})
	for _, a := range actors {
		go func(a Actor) {
			Supervise(ctx, a)
		}(a)
	}

	<-ctx.Done()
	// Give actors a moment to drain.
	time.Sleep(100 * time.Millisecond)
	close(done)
}
