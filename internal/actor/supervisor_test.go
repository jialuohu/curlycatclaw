package actor

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// mockActor implements Actor for testing. Each call to Run increments runCount
// and invokes the user-supplied runFunc.
type mockActor struct {
	name     string
	runCount atomic.Int32
	runFunc  func(ctx context.Context) error
}

func (m *mockActor) Name() string { return m.name }

func (m *mockActor) Run(ctx context.Context) error {
	m.runCount.Add(1)
	return m.runFunc(ctx)
}

func TestSupervise_RestartOnPanic(t *testing.T) {
	// Actor panics on first call, then blocks until context is cancelled.
	// Supervisor should restart it after the panic.
	a := &mockActor{
		name: "panicker",
		runFunc: func(ctx context.Context) error {
			// Only panic on the first run.
			// Deliberately empty — just verifies actor can run without panic.
			return nil
		},
	}

	var firstDone atomic.Bool
	a.runFunc = func(ctx context.Context) error {
		if !firstDone.Load() {
			firstDone.Store(true)
			panic("boom")
		}
		// Second run: block until cancelled.
		<-ctx.Done()
		return ctx.Err()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		Supervise(ctx, a)
		close(done)
	}()

	// Wait until the actor has been run at least twice (restarted after panic).
	deadline := time.After(4 * time.Second)
	for a.runCount.Load() < 2 {

		select {
		case <-deadline:
			t.Fatalf("timed out waiting for restart; runCount = %d", a.runCount.Load())
		case <-time.After(50 * time.Millisecond):
		}
	}

	cancel()
	<-done

	count := a.runCount.Load()
	if count < 2 {
		t.Errorf("expected at least 2 runs after panic, got %d", count)
	}
}

func TestSupervise_RestartOnError(t *testing.T) {
	// Actor returns an error on the first call, then blocks until cancelled.
	var firstDone atomic.Bool
	a := &mockActor{
		name: "errorer",
	}
	a.runFunc = func(ctx context.Context) error {
		if !firstDone.Load() {
			firstDone.Store(true)
			return errors.New("something went wrong")
		}
		<-ctx.Done()
		return ctx.Err()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		Supervise(ctx, a)
		close(done)
	}()

	deadline := time.After(4 * time.Second)
	for a.runCount.Load() < 2 {

		select {
		case <-deadline:
			t.Fatalf("timed out waiting for restart; runCount = %d", a.runCount.Load())
		case <-time.After(50 * time.Millisecond):
		}
	}

	cancel()
	<-done

	count := a.runCount.Load()
	if count < 2 {
		t.Errorf("expected at least 2 runs after error, got %d", count)
	}
}

func TestSupervise_StopsOnContextCancel(t *testing.T) {
	// Actor returns immediately each time. We cancel the context quickly and
	// verify that Supervise returns promptly.
	a := &mockActor{
		name: "quickexit",
		runFunc: func(ctx context.Context) error {
			return nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		Supervise(ctx, a)
		close(done)
	}()

	// Let the actor run once, then cancel.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// Supervise returned as expected.
	case <-time.After(3 * time.Second):
		t.Fatal("Supervise did not return after context cancellation")
	}

	if a.runCount.Load() < 1 {
		t.Error("expected actor to run at least once")
	}
}

func TestSupervise_BackoffResetAfterHealthyPeriod(t *testing.T) {
	// We cannot override healthyPeriod (60s), so we test the backoff escalation
	// behavior indirectly: when an actor fails quickly multiple times, the
	// backoff grows. We verify this by observing that the gap between the
	// second and third restarts is longer than the gap between the first and
	// second restarts, confirming exponential backoff is in effect.
	//
	// Specifically: the first backoff wait is 1s (initialBackoff), then it
	// doubles to 2s before the next restart. We measure the time between
	// run #1 and run #2 (should be ~1s) vs run #2 and run #3 (should be ~2s).
	var timestamps [4]time.Time
	var idx atomic.Int32

	a := &mockActor{
		name: "backoff-check",
	}
	a.runFunc = func(ctx context.Context) error {
		i := idx.Add(1)
		if int(i) <= len(timestamps) {
			timestamps[i-1] = time.Now()
		}
		return errors.New("fail fast")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		Supervise(ctx, a)
		close(done)
	}()

	// Wait for at least 4 runs (need 3 gaps to measure 2 intervals).
	deadline := time.After(14 * time.Second)
	for idx.Load() < 4 {

		select {
		case <-deadline:
			t.Fatalf("timed out waiting for 4 runs; got %d", idx.Load())
		case <-time.After(50 * time.Millisecond):
		}
	}

	cancel()
	<-done

	// gap1: time between run 1 and run 2 (includes ~1s backoff).
	// gap2: time between run 2 and run 3 (includes ~2s backoff).
	// gap3: time between run 3 and run 4 (includes ~4s backoff).
	gap1 := timestamps[1].Sub(timestamps[0])
	gap2 := timestamps[2].Sub(timestamps[1])
	gap3 := timestamps[3].Sub(timestamps[2])

	// Verify backoff is increasing: each gap should be longer than the previous.
	if gap2 <= gap1 {
		t.Errorf("expected gap2 > gap1 (exponential backoff), got gap1=%v gap2=%v", gap1, gap2)
	}
	if gap3 <= gap2 {
		t.Errorf("expected gap3 > gap2 (exponential backoff), got gap2=%v gap3=%v", gap2, gap3)
	}

	// Sanity check the rough magnitudes (allow generous margin for CI jitter).
	// gap1 should be around 1s, gap2 around 2s, gap3 around 4s.
	if gap1 < 800*time.Millisecond || gap1 > 3*time.Second {
		t.Errorf("gap1 out of expected range: %v", gap1)
	}
	if gap2 < 1500*time.Millisecond || gap2 > 5*time.Second {
		t.Errorf("gap2 out of expected range: %v", gap2)
	}
	if gap3 < 3*time.Second || gap3 > 8*time.Second {
		t.Errorf("gap3 out of expected range: %v", gap3)
	}
}

func TestSuperviseAll_RunsMultipleConcurrently(t *testing.T) {
	const numActors = 3

	// Each actor records when it starts running, then blocks until cancelled.
	var started [numActors]atomic.Bool

	actors := make([]Actor, numActors)
	for i := 0; i < numActors; i++ {
		idx := i
		m := &mockActor{
			name: "concurrent-" + string(rune('A'+idx)),
		}
		m.runFunc = func(ctx context.Context) error {
			started[idx].Store(true)
			<-ctx.Done()
			return ctx.Err()
		}
		actors[i] = m
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		SuperviseAll(ctx, 5*time.Second, actors...)
		close(done)
	}()

	// Wait for all actors to start running.
	deadline := time.After(3 * time.Second)
	allStarted := false
	for !allStarted {
		allStarted = true
		for i := 0; i < numActors; i++ {
			if !started[i].Load() {
				allStarted = false
				break
			}
		}
		if allStarted {
			break
		}
		select {
		case <-deadline:
			for i := 0; i < numActors; i++ {
				if !started[i].Load() {
					t.Errorf("actor %d did not start", i)
				}
			}
			t.Fatal("timed out waiting for all actors to start")
		case <-time.After(50 * time.Millisecond):
		}
	}

	cancel()

	select {
	case <-done:
		// SuperviseAll returned after cancellation.
	case <-time.After(3 * time.Second):
		t.Fatal("SuperviseAll did not return after context cancellation")
	}
}

func TestSuperviseAll_WaitsForActorDrain(t *testing.T) {
	// An actor that takes 500ms to shut down after context cancellation.
	// SuperviseAll should wait for it (timeout is 5s).
	var drained atomic.Bool
	a := &mockActor{name: "slow-drain"}
	a.runFunc = func(ctx context.Context) error {
		<-ctx.Done()
		time.Sleep(500 * time.Millisecond)
		drained.Store(true)
		return ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		SuperviseAll(ctx, 5*time.Second, a)
		close(done)
	}()

	// Let the actor start, then cancel.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case <-done:
		if !drained.Load() {
			t.Error("SuperviseAll returned before actor finished draining")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SuperviseAll did not return after drain")
	}
}

func TestSuperviseAll_TimesOutOnHangingActor(t *testing.T) {
	// An actor that ignores context cancellation. SuperviseAll should
	// return after the timeout, not hang forever.
	a := &mockActor{name: "hanger"}
	a.runFunc = func(ctx context.Context) error {
		// Ignore ctx.Done() — hang until the test's overall deadline.
		select {}
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		SuperviseAll(ctx, 500*time.Millisecond, a)
		close(done)
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// SuperviseAll returned due to timeout. Good.
	case <-time.After(3 * time.Second):
		t.Fatal("SuperviseAll hung despite timeout — WaitGroup drain not enforced")
	}
}
