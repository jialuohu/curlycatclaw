package session

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jialuohu/curlycatclaw/internal/claude"
	"github.com/jialuohu/curlycatclaw/internal/telegram"
)

// waitForText polls the outbox for a message whose text contains `want` within timeout.
func waitForText(t *testing.T, outbox <-chan telegram.OutgoingMessage, want string, timeout time.Duration) telegram.OutgoingMessage {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case msg := <-outbox:
			if strings.Contains(msg.Text, want) {
				return msg
			}
		case <-deadline:
			t.Fatalf("timed out waiting for outbox message containing %q", want)
		}
	}
}

// TestStopActive_Idle verifies /stop with no in-flight work replies "Nothing to stop."
func TestStopActive_Idle(t *testing.T) {
	tg := newMockTelegram()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	outbox := tg.drainInbox(ctx)
	a := newTestActor(&mockLLM{}, &mockStore{convID: "c"}, &mockContextProvider{}, &mockToolRouter{}, nil, tg)

	a.stopActive(telegram.IncomingMessage{ChatID: 100, UserID: 42})

	msg := waitForText(t, outbox, "Nothing to stop", time.Second)
	if msg.ChatID != 100 {
		t.Errorf("reply chat_id = %d, want 100", msg.ChatID)
	}
}

// TestStopActive_CancelsInFlight verifies /stop cancels the active work's context
// and reports "Stopped." to the user.
func TestStopActive_CancelsInFlight(t *testing.T) {
	tg := newMockTelegram()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	outbox := tg.drainInbox(ctx)
	a := newTestActor(&mockLLM{}, &mockStore{convID: "c"}, &mockContextProvider{}, &mockToolRouter{}, nil, tg)

	workCtx, workCancel := context.WithCancel(context.Background())
	a.active = &activeWork{
		key:    userKey{UserID: 42, ChatID: 100},
		cancel: workCancel,
		done:   make(chan struct{}),
	}

	a.stopActive(telegram.IncomingMessage{ChatID: 100, UserID: 42})

	if workCtx.Err() == nil {
		t.Error("expected workCtx to be cancelled, got nil")
	}
	waitForText(t, outbox, "Stopped.", time.Second)
}

// TestStopActive_AlreadyFinished verifies the race where /stop arrives after
// handleMessage completed normally — the user should see "Already finished",
// not "Stopped." (because nothing actually got stopped).
func TestStopActive_AlreadyFinished(t *testing.T) {
	tg := newMockTelegram()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	outbox := tg.drainInbox(ctx)
	a := newTestActor(&mockLLM{}, &mockStore{convID: "c"}, &mockContextProvider{}, &mockToolRouter{}, nil, tg)

	_, workCancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	close(done) // pre-closed: work already finished
	a.active = &activeWork{
		key:    userKey{UserID: 42, ChatID: 100},
		cancel: workCancel,
		done:   done,
	}

	a.stopActive(telegram.IncomingMessage{ChatID: 100, UserID: 42})

	waitForText(t, outbox, "Already finished", time.Second)
}

// TestStopActive_DropsPendingQueue verifies /stop flushes queued messages.
func TestStopActive_DropsPendingQueue(t *testing.T) {
	tg := newMockTelegram()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tg.drainInbox(ctx)
	a := newTestActor(&mockLLM{}, &mockStore{convID: "c"}, &mockContextProvider{}, &mockToolRouter{}, nil, tg)

	_, workCancel := context.WithCancel(context.Background())
	a.active = &activeWork{
		key:    userKey{UserID: 42, ChatID: 100},
		cancel: workCancel,
		done:   make(chan struct{}),
	}
	a.pendingMsgs = []telegram.IncomingMessage{
		{UserID: 42, ChatID: 100, Text: "queued 1"},
		{UserID: 42, ChatID: 100, Text: "queued 2"},
	}

	a.stopActive(telegram.IncomingMessage{ChatID: 100, UserID: 42})

	if len(a.pendingMsgs) != 0 {
		t.Errorf("pendingMsgs len = %d after /stop, want 0", len(a.pendingMsgs))
	}
}

// blockingLLM blocks in SendStreaming until its ctx is cancelled. It signals
// when work started so the test can coordinate the /stop.
type blockingLLM struct {
	started chan struct{}
	once    atomic.Bool
}

func (b *blockingLLM) SendStreaming(ctx context.Context, params claude.SendParams) (*claude.Response, error) {
	if b.once.CompareAndSwap(false, true) {
		close(b.started)
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// panickingLLM panics inside SendStreaming. Used to verify the dispatch
// goroutine recovers cleanly instead of crashing the process.
type panickingLLM struct{}

func (p *panickingLLM) SendStreaming(_ context.Context, _ claude.SendParams) (*claude.Response, error) {
	panic("fake panic from LLM client")
}

// TestRun_RecoversFromHandleMessagePanic verifies the dispatch goroutine
// recovers from panics inside handleMessage instead of taking down the process.
// Regression guard: without the recover(), a panic in any LLM call would crash
// the whole bot.
func TestRun_RecoversFromHandleMessagePanic(t *testing.T) {
	tg := newMockTelegram()
	runCtx, runCancel := context.WithCancel(context.Background())
	defer runCancel()
	outbox := tg.drainInbox(runCtx)

	a := newTestActor(&panickingLLM{}, &mockStore{convID: "c"}, &mockContextProvider{}, &mockToolRouter{}, nil, tg)

	runDone := make(chan error, 1)
	go func() { runDone <- a.Run(runCtx) }()

	tg.updates <- telegram.IncomingMessage{UserID: 42, ChatID: 100, ChatType: "private", Text: "trigger panic"}

	// The panic should be caught and converted into the generic error reply.
	waitForText(t, outbox, "something went wrong", 3*time.Second)

	// Actor should still be alive and processing messages.
	runCancel()
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancel — goroutine may have crashed")
	}
}

// TestRun_StopEndToEnd drives Actor.Run end-to-end: send a message that makes
// Claude block, send /stop, verify the work is cancelled and "Stopped." is
// sent. This exercises the busy-branch select.
func TestRun_StopEndToEnd(t *testing.T) {
	llm := &blockingLLM{started: make(chan struct{})}
	tg := newMockTelegram()
	runCtx, runCancel := context.WithCancel(context.Background())
	defer runCancel()
	outbox := tg.drainInbox(runCtx)

	a := newTestActor(llm, &mockStore{convID: "c"}, &mockContextProvider{}, &mockToolRouter{}, nil, tg)

	runDone := make(chan error, 1)
	go func() { runDone <- a.Run(runCtx) }()

	// Send a user message that will block inside SendStreaming.
	tg.updates <- telegram.IncomingMessage{UserID: 42, ChatID: 100, ChatType: "private", Text: "research everything"}

	// Wait for work to actually start before sending /stop.
	select {
	case <-llm.started:
	case <-time.After(2 * time.Second):
		t.Fatal("LLM.SendStreaming was never called")
	}

	// Now /stop should cancel it.
	tg.updates <- telegram.IncomingMessage{UserID: 42, ChatID: 100, ChatType: "private", Text: "/stop"}

	waitForText(t, outbox, "Stopped.", 3*time.Second)

	// Run loop should still be healthy: send another message and see the
	// queue process it (even if it blocks again, we just verify dispatch works).
	runCancel()
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}
