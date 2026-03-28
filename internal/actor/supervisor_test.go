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

	fastCfg := SupervisorConfig{InitialBackoff: 10 * time.Millisecond, MaxBackoff: 100 * time.Millisecond, HealthyPeriod: time.Hour}

	done := make(chan struct{})
	go func() {
		SuperviseWithConfig(ctx, a, fastCfg)
		close(done)
	}()

	deadline := time.After(2 * time.Second)
	for a.runCount.Load() < 2 {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for restart; runCount = %d", a.runCount.Load())
		case <-time.After(5 * time.Millisecond):
		}
	}

	cancel()
	<-done

	if a.runCount.Load() < 2 {
		t.Errorf("expected at least 2 runs after panic, got %d", a.runCount.Load())
	}
}

func TestSupervise_RestartOnError(t *testing.T) {
	var firstDone atomic.Bool
	a := &mockActor{name: "errorer"}
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

	fastCfg := SupervisorConfig{InitialBackoff: 10 * time.Millisecond, MaxBackoff: 100 * time.Millisecond, HealthyPeriod: time.Hour}

	done := make(chan struct{})
	go func() {
		SuperviseWithConfig(ctx, a, fastCfg)
		close(done)
	}()

	deadline := time.After(2 * time.Second)
	for a.runCount.Load() < 2 {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for restart; runCount = %d", a.runCount.Load())
		case <-time.After(5 * time.Millisecond):
		}
	}

	cancel()
	<-done

	if a.runCount.Load() < 2 {
		t.Errorf("expected at least 2 runs after error, got %d", a.runCount.Load())
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

func TestSupervise_BackoffEscalation(t *testing.T) {
	// Use fast backoff parameters to avoid wall-clock flakiness.
	// Initial=10ms, so gaps should be ~10ms, ~20ms, ~40ms.
	cfg := SupervisorConfig{
		InitialBackoff: 10 * time.Millisecond,
		MaxBackoff:     200 * time.Millisecond,
		HealthyPeriod:  time.Hour, // effectively never resets
	}

	var timestamps [4]time.Time
	var idx atomic.Int32

	a := &mockActor{name: "backoff-check"}
	a.runFunc = func(ctx context.Context) error {
		i := idx.Add(1)
		if int(i) <= len(timestamps) {
			timestamps[i-1] = time.Now()
		}
		return errors.New("fail fast")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		SuperviseWithConfig(ctx, a, cfg)
		close(done)
	}()

	// Wait for at least 4 runs.
	deadline := time.After(3 * time.Second)
	for idx.Load() < 4 {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for 4 runs; got %d", idx.Load())
		case <-time.After(5 * time.Millisecond):
		}
	}

	cancel()
	<-done

	gap1 := timestamps[1].Sub(timestamps[0])
	gap2 := timestamps[2].Sub(timestamps[1])
	gap3 := timestamps[3].Sub(timestamps[2])

	// Verify backoff is increasing.
	if gap2 <= gap1 {
		t.Errorf("expected gap2 > gap1 (exponential backoff), got gap1=%v gap2=%v", gap1, gap2)
	}
	if gap3 <= gap2 {
		t.Errorf("expected gap3 > gap2 (exponential backoff), got gap2=%v gap3=%v", gap2, gap3)
	}
}

func TestSupervise_BackoffResetsAfterHealthyRun(t *testing.T) {
	// Use fast parameters. HealthyPeriod=50ms, so any run lasting >50ms resets backoff.
	cfg := SupervisorConfig{
		InitialBackoff: 10 * time.Millisecond,
		MaxBackoff:     200 * time.Millisecond,
		HealthyPeriod:  50 * time.Millisecond,
	}

	var runDurations []time.Duration
	var idx atomic.Int32
	var timestamps [3]time.Time

	a := &mockActor{name: "reset-check"}
	a.runFunc = func(ctx context.Context) error {
		i := idx.Add(1)
		if int(i) <= len(timestamps) {
			timestamps[i-1] = time.Now()
		}
		switch i {
		case 1:
			// Fail fast — backoff escalates.
			return errors.New("fail")
		case 2:
			// Run "healthy" for longer than HealthyPeriod to reset backoff.
			time.Sleep(80 * time.Millisecond)
			return errors.New("fail after healthy")
		default:
			// Third run: record and block.
			<-ctx.Done()
			return ctx.Err()
		}
	}
	_ = runDurations

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		SuperviseWithConfig(ctx, a, cfg)
		close(done)
	}()

	deadline := time.After(3 * time.Second)
	for idx.Load() < 3 {
		select {
		case <-deadline:
			t.Fatalf("timed out; got %d runs", idx.Load())
		case <-time.After(5 * time.Millisecond):
		}
	}

	cancel()
	<-done

	// After healthy run #2, backoff should reset to initial (10ms).
	// gap between run 2 and run 3 should be close to initial, not escalated.
	gap := timestamps[2].Sub(timestamps[1])
	// Subtract the ~80ms the healthy run took.
	effectiveBackoff := gap - 80*time.Millisecond
	if effectiveBackoff > 50*time.Millisecond {
		t.Errorf("backoff after healthy run should be ~10ms (reset), got effective %v (raw gap %v)", effectiveBackoff, gap)
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
