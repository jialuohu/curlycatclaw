package actor

import "context"

// Actor is the base interface for all actors in the system.
// Each actor owns its goroutine and communicates via typed message channels.
type Actor interface {
	// Run starts the actor's event loop. It blocks until the context is
	// cancelled or a fatal error occurs. The actor owns its own channel
	// and processes messages sequentially.
	Run(ctx context.Context) error

	// Name returns a human-readable identifier for logging and supervision.
	Name() string
}
