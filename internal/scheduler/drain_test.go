package scheduler

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
)

func drainConfig() Config {
	cfg := testConfig()
	cfg.ShutdownDrain = 20 * time.Millisecond
	return cfg
}

func TestDrain_RunsThatFinishWithinTheDrainAreNotCut(t *testing.T) {
	cfg := testConfig()
	cfg.ShutdownDrain = 2 * time.Second
	started := make(chan struct{})
	var cut atomic.Bool
	h := newHarness(t, cfg, firerFunc(func(ctx context.Context, _ spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		close(started)
		time.Sleep(100 * time.Millisecond)
		cut.Store(ctx.Err() != nil)
		return fired
	}))
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")} })
	h.start(t)
	receive(t, started)

	h.svc.Drain(context.Background())
	if cut.Load() {
		t.Error("a run that finished within CYODA_SCHEDULER_SHUTDOWN_DRAIN was cancelled")
	}
	h.fs.with(func() {
		if h.fs.retired != 1 {
			t.Errorf("RetireOwner calls = %d, want 1", h.fs.retired)
		}
	})
	beats := h.fs.heartbeatCount()
	time.Sleep(50 * time.Millisecond)
	if n := h.fs.heartbeatCount(); n != beats {
		t.Errorf("%d heartbeats after Drain returned", n-beats)
	}
}

func TestDrain_ACutRunWithNothingHandedOffGoesBackUncounted(t *testing.T) {
	h := newHarness(t, drainConfig(), firerFunc(func(ctx context.Context, _ spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		return failedOnCancel(ctx)
	}))
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")} })
	h.start(t)
	eventually(t, "the run started", func() bool { return liveRuns(h.svc) == 1 })

	before := time.Now().UnixMilli()
	h.svc.Drain(context.Background())
	attempts := h.fs.attemptsRecorded()
	if len(attempts) != 1 {
		t.Fatalf("%d attempts, want 1", len(attempts))
	}
	a := attempts[0]
	if !a.NotCounted || !a.ClearOwnMark || a.NextAttemptTime < before || a.NextAttemptTime > time.Now().UnixMilli() {
		t.Errorf("attempt = %+v, want uncounted, mark cleared, due now", a)
	}
}

func TestDrain_AnUnsafeCalloutInFlightIsNotCut(t *testing.T) {
	var cut atomic.Bool
	h := newHarness(t, drainConfig(), firerFunc(func(ctx context.Context, _ spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		g := workflow.RunGuardFrom(ctx)
		g.Unsafe.Begin()
		<-g.NoNewUnsafe
		time.Sleep(100 * time.Millisecond) // past step 2's 20ms: step 3 has run
		cut.Store(ctx.Err() != nil)
		g.Unsafe.End()
		return fired // the run went on with safe steps and committed
	}))
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")} })
	h.start(t)
	eventually(t, "the run started", func() bool { return liveRuns(h.svc) == 1 })

	h.svc.Drain(context.Background())
	if cut.Load() {
		t.Error("step 3 cancelled a run whose unsafe callout was in flight")
	}
	if a, f := len(h.fs.attemptsRecorded()), len(h.fs.failsRecorded()); a+f != 0 {
		t.Errorf("a committed run wrote %d attempts and %d failures", a, f)
	}
}

func TestDrain_ReachingANewUnsafeDispatchAfterTheSignalCountsAsCut(t *testing.T) {
	h := newHarness(t, testConfig(), firerFunc(func(ctx context.Context, _ spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		<-workflow.RunGuardFrom(ctx).NoNewUnsafe
		return workflow.RunReport{Outcome: workflow.OutcomeFailed,
			Err: fmt.Errorf("unsafe dispatch refused after shutdown began: %w", context.Canceled)}
	}))
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")} })
	h.start(t)
	eventually(t, "the run started", func() bool { return liveRuns(h.svc) == 1 })

	h.svc.Drain(context.Background())
	attempts := h.fs.attemptsRecorded()
	if len(attempts) != 1 || !attempts[0].NotCounted {
		t.Errorf("attempts = %+v, want one uncounted attempt", attempts)
	}
}

func TestDrain_ACutRunWhoseUnsafeWorkWasHandedOffEarlierFails(t *testing.T) {
	h := newHarness(t, drainConfig(), firerFunc(func(ctx context.Context, _ spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		<-ctx.Done()
		return workflow.RunReport{Outcome: workflow.OutcomeFailed, MarkHeld: true, UnsafeReached: true,
			Err: fmt.Errorf("run stopped: %w", ctx.Err())}
	}))
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")} })
	h.start(t)
	eventually(t, "the run started", func() bool { return liveRuns(h.svc) == 1 })

	h.svc.Drain(context.Background())
	fails := h.fs.failsRecorded()
	if len(fails) != 1 || fails[0].Reason != spi.FailureUnsafeWorkNotCompleted {
		t.Errorf("failures = %+v, want UNSAFE_WORK_NOT_COMPLETED", fails)
	}
}

func TestDrain_ARunStillLiveAfterStepFourIsNotGivenBack(t *testing.T) {
	release := make(chan struct{})
	tokens := make(chan uuid.UUID, 1)
	h := newHarness(t, drainConfig(), firerFunc(func(ctx context.Context, task spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		g := workflow.RunGuardFrom(ctx)
		g.Unsafe.Begin()
		tokens <- task.Claim.Token
		<-release
		g.Unsafe.End()
		return fired
	}))
	h.svc.deps.CalloutDeadlineMax = 50 * time.Millisecond
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")} })
	h.start(t)
	t.Cleanup(func() { close(release) })
	token := receive(t, tokens)

	select {
	case <-drainAsync(context.Background(), h):
	case <-time.After(5 * time.Second):
		t.Fatal("Drain did not return while a run was still live")
	}
	keep, ok := h.fs.lastGiveBack()
	if !ok || !slices.Contains(keep, token) {
		t.Errorf("the final give-back kept %v; a live run must not be given back", keep)
	}
	h.fs.with(func() {
		if h.fs.retired != 0 {
			t.Error("RetireOwner ran while a run was still live")
		}
	})
}

func TestDrain_BookkeepingStopsAtItsShutdownDeadline(t *testing.T) {
	tokens := make(chan uuid.UUID, 1)
	h := newHarness(t, drainConfig(), firerFunc(func(ctx context.Context, task spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		tokens <- task.Claim.Token
		return failedOnCancel(ctx)
	}))
	h.fs.with(func() {
		h.fs.outcomeAlways = fmt.Errorf("record attempt: %w", context.DeadlineExceeded)
		h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")}
	})
	h.start(t)
	token := receive(t, tokens)

	done := make(chan struct{})
	go func() { h.svc.Drain(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Drain did not return while an outcome write kept failing")
	}
	keep, ok := h.fs.lastGiveBack()
	if !ok || slices.Contains(keep, token) {
		t.Errorf("the final give-back kept %v; a run that ended without its outcome is given back", keep)
	}
	h.fs.with(func() {
		if h.fs.retired != 1 {
			t.Errorf("RetireOwner calls = %d, want 1", h.fs.retired)
		}
	})
	if !h.flag.Load() {
		t.Error("an outage latched the node")
	}
}

func TestDrain_IsIdempotentAndSafeBeforeStart(t *testing.T) {
	h := newHarness(t, testConfig(), reportFirer(fired))
	h.svc.Drain(context.Background())
	h.svc.Stop()

	h2 := newHarness(t, testConfig(), reportFirer(fired))
	h2.start(t)
	h2.svc.Drain(context.Background())
	h2.svc.Drain(context.Background())
	h2.svc.Stop()
	h2.fs.with(func() {
		if h2.fs.retired != 1 {
			t.Errorf("RetireOwner calls = %d, want 1", h2.fs.retired)
		}
	})
}

// drainAsync runs Drain on its own goroutine and returns a channel closed
// when it returns.
func drainAsync(ctx context.Context, h *harness) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.svc.Drain(ctx)
	}()
	return done
}

// Step 4 waits for the longest unsafe callout in flight, not only for the
// fixed margin: an exempt run that finishes inside its callout deadline
// records its outcome, and the owner retires.
func TestDrain_StepFourWaitsForTheLongestCalloutInFlight(t *testing.T) {
	tokens := make(chan uuid.UUID, 1)
	h := newHarness(t, drainConfig(), firerFunc(func(ctx context.Context, task spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		g := workflow.RunGuardFrom(ctx)
		g.Unsafe.Begin()
		tokens <- task.Claim.Token
		<-g.NoNewUnsafe
		time.Sleep(400 * time.Millisecond) // past step 2 and past the 100ms margin
		g.Unsafe.End()
		return fired
	}))
	h.svc.deps.CalloutDeadlineMax = 5 * time.Second
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")} })
	h.start(t)
	token := receive(t, tokens)

	h.svc.Drain(context.Background())
	if keep, ok := h.fs.lastGiveBack(); !ok || slices.Contains(keep, token) {
		t.Errorf("the final give-back kept %v; the run had finished within its callout deadline", keep)
	}
	h.fs.with(func() {
		if h.fs.retired != 1 {
			t.Errorf("RetireOwner calls = %d, want 1: step 4 stopped waiting before the callout's deadline", h.fs.retired)
		}
	})
}

// Drain's context ends the waits of steps 2 and 4 early. The run still in
// progress is not given back.
func TestDrain_ItsContextEndsTheWaitsEarly(t *testing.T) {
	cfg := testConfig()
	cfg.ShutdownDrain = time.Minute
	release := make(chan struct{})
	tokens := make(chan uuid.UUID, 1)
	h := newHarness(t, cfg, firerFunc(func(ctx context.Context, task spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		g := workflow.RunGuardFrom(ctx)
		g.Unsafe.Begin()
		tokens <- task.Claim.Token
		<-release
		g.Unsafe.End()
		return fired
	}))
	h.svc.deps.CalloutDeadlineMax = time.Minute
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")} })
	h.start(t)
	t.Cleanup(func() { close(release) })
	token := receive(t, tokens)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	h.svc.Drain(ctx)
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("Drain took %v with its context already ended", d)
	}
	if keep, ok := h.fs.lastGiveBack(); !ok || !slices.Contains(keep, token) {
		t.Errorf("the final give-back kept %v; a live run must not be given back", keep)
	}
}

// After a panic in the claim loop, the loop is gone; step 5's give-back and
// the retire still run.
func TestDrain_AfterALoopPanicTheFinalGiveBackStillRuns(t *testing.T) {
	h := newHarness(t, drainConfig(), firerFunc(func(ctx context.Context, _ spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		return failedOnCancel(ctx)
	}))
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")} })
	h.start(t)
	eventually(t, "the run started", func() bool { return liveRuns(h.svc) == 1 })
	h.fs.with(func() { h.fs.claimPanic = true })
	receive(t, h.svc.loopDone)
	eventually(t, "the run cancelled by the latch released", func() bool { return liveRuns(h.svc) == 0 })
	from := len(h.fs.giveBackCalls())

	done := drainAsync(context.Background(), h)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Drain did not return after the claim loop panicked")
	}
	if n := len(h.fs.giveBackCalls()); n != from+1 {
		t.Errorf("%d give-backs at shutdown, want the final one", n-from)
	}
	h.fs.with(func() {
		if h.fs.retired != 1 {
			t.Errorf("RetireOwner calls = %d, want 1", h.fs.retired)
		}
	})
}

// A panicked run is never given back (§6.5), also at shutdown.
func TestDrain_APanickedRunIsNotGivenBack(t *testing.T) {
	tokens := make(chan uuid.UUID, 1)
	h := newHarness(t, drainConfig(), firerFunc(func(_ context.Context, task spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		tokens <- task.Claim.Token
		panic("injected panic in a scheduled run")
	}))
	runs := withRunMetrics(t, h)
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")} })
	h.start(t)
	token := receive(t, tokens)
	eventually(t, "the run ended as panicked", func() bool { return runs()[outcomePanicked] == 1 })

	h.svc.Drain(context.Background())
	assertKeptAtShutdown(t, h, token)
}

// A run whose outcome the store rejected is never given back, also at
// shutdown: the task stays RUNNING under the latched owner (§5.6).
func TestDrain_ARunWhoseOutcomeTheStoreRejectedIsNotGivenBack(t *testing.T) {
	tokens := make(chan uuid.UUID, 1)
	h := newHarness(t, drainConfig(), firerFunc(func(_ context.Context, task spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		tokens <- task.Claim.Token
		return safeCallout
	}))
	h.fs.with(func() {
		h.fs.outcomeErrs = []error{fmt.Errorf("record attempt: value too long: %w", spi.ErrStoreRejected)}
		h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")}
	})
	h.start(t)
	token := receive(t, tokens)
	eventually(t, "the node latched", func() bool { return !h.flag.Load() })

	h.svc.Drain(context.Background())
	assertKeptAtShutdown(t, h, token)
}

// A run whose bookkeeping panicked is never given back, also at shutdown
// (§6.5): its outcome is not known to be recorded.
func TestDrain_ARunWhoseBookkeepingPanickedIsNotGivenBack(t *testing.T) {
	tokens := make(chan uuid.UUID, 1)
	h := newHarness(t, drainConfig(), firerFunc(func(_ context.Context, task spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		tokens <- task.Claim.Token
		return safeFailure
	}))
	runs := withRunMetrics(t, h)
	h.fs.with(func() {
		h.fs.outcomePanic = true
		h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")}
	})
	h.start(t)
	token := receive(t, tokens)
	eventually(t, "the run ended as panicked", func() bool { return runs()[outcomePanicked] == 1 })

	h.svc.Drain(context.Background())
	assertKeptAtShutdown(t, h, token)
}

func assertKeptAtShutdown(t *testing.T, h *harness, token uuid.UUID) {
	t.Helper()
	if keep, ok := h.fs.lastGiveBack(); !ok || !slices.Contains(keep, token) {
		t.Errorf("the final give-back kept %v; the claim must not be given back", keep)
	}
	h.fs.with(func() {
		if h.fs.retired != 0 {
			t.Error("RetireOwner ran while a claim was kept")
		}
	})
}

// The heartbeat stops at step 5 even with a run still live, so the watchdog
// must go on: it cancels the run before another pnode could consider this
// owner stale. It exits once no run is left.
func TestDrain_TheWatchdogOutlivesTheDrainWhileARunIsLive(t *testing.T) {
	cancelled := make(chan struct{})
	h := newHarness(t, drainConfig(), firerFunc(func(ctx context.Context, _ spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		g := workflow.RunGuardFrom(ctx)
		g.Unsafe.Begin()
		defer g.Unsafe.End()
		<-ctx.Done()
		close(cancelled)
		return workflow.RunReport{Outcome: workflow.OutcomeFailed, Err: fmt.Errorf("run stopped: %w", ctx.Err())}
	}))
	h.svc.deps.CalloutDeadlineMax = 50 * time.Millisecond
	h.svc.window = 300 * time.Millisecond
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")} })
	h.start(t)
	eventually(t, "the run started", func() bool { return liveRuns(h.svc) == 1 })

	h.svc.Drain(context.Background())
	select {
	case <-cancelled:
		t.Fatal("the exempt run was cancelled before Drain returned")
	default:
	}
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the watchdog did not cancel the run still live after the drain")
	}
	select {
	case <-h.svc.wdDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the watchdog did not exit once no run was left")
	}
}

// Drain leaves no goroutine behind when no run is left: the claim loop, the
// heartbeat and the watchdog have exited when it returns, whether there was
// never a run or the runs finished within the drain.
func TestDrain_NoGoroutineOutlivesADrainWithNoRunLeft(t *testing.T) {
	for name, due := range map[string][]spi.ScheduledTask{
		"no run":                        nil,
		"a run that finished in step 2": {dueTask("t1", "task-1")},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig()
			cfg.ShutdownDrain = 5 * time.Second
			started := make(chan struct{}, 1)
			h := newHarness(t, cfg, firerFunc(func(context.Context, spi.ScheduledTask, int, time.Duration) workflow.RunReport {
				started <- struct{}{}
				time.Sleep(20 * time.Millisecond)
				return fired
			}))
			h.fs.with(func() { h.fs.due = due })
			h.start(t)
			if due != nil {
				receive(t, started)
			}
			begin := time.Now()
			h.svc.Drain(context.Background())
			if d := time.Since(begin); d >= cfg.ShutdownDrain {
				t.Errorf("Drain took %v; it waits for the runs, not for the whole drain", d)
			}
			for what, ch := range map[string]chan struct{}{
				"claim loop": h.svc.loopDone, "heartbeat": h.svc.hbDone, "watchdog": h.svc.wdDone,
			} {
				select {
				case <-ch:
				default:
					t.Errorf("the %s is still running after Drain returned", what)
				}
			}
		})
	}
}

// A run cancelled by the latch during the drain is recorded as the latch
// cancelled it: a counted attempt, not an uncounted shutdown cut. Step 3
// does not relabel it.
func TestDrain_ARunCancelledByTheLatchDuringTheDrainIsCounted(t *testing.T) {
	cfg := testConfig()
	cfg.ShutdownDrain = 300 * time.Millisecond
	signalled := make(chan struct{})
	h := newHarness(t, cfg, firerFunc(func(ctx context.Context, _ spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		<-workflow.RunGuardFrom(ctx).NoNewUnsafe
		close(signalled)
		<-ctx.Done()
		time.Sleep(500 * time.Millisecond) // past step 2: step 3 has run
		return workflow.RunReport{Outcome: workflow.OutcomeFailed, Err: fmt.Errorf("run stopped: %w", ctx.Err())}
	}))
	h.svc.stepFourMargin = 5 * time.Second
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")} })
	h.start(t)
	eventually(t, "the run started", func() bool { return liveRuns(h.svc) == 1 })

	done := drainAsync(context.Background(), h)
	receive(t, signalled)
	h.svc.latchAndCancel()
	receive(t, done)

	attempts := h.fs.attemptsRecorded()
	if len(attempts) != 1 || attempts[0].NotCounted || attempts[0].Error != cancelledText {
		t.Errorf("attempts = %+v, want one counted attempt cancelled by the latch", attempts)
	}
}

func TestDrain_ARetireFailureIsLogged(t *testing.T) {
	logs := captureLogs(t)
	h := newHarness(t, testConfig(), reportFirer(fired))
	h.fs.with(func() { h.fs.retireErr = errors.New("retire owner: connection refused") })
	h.start(t)
	h.svc.Drain(context.Background())
	lines := logRecords(t, logs.String(), "scheduler could not retire its liveness record")
	if len(lines) != 1 || lines[0]["level"] != "WARN" {
		t.Errorf("retire lines = %v, want one WARN", lines)
	}
}

func draining(s *Service) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.draining
}

// A claim in flight when step 1 closes is not started: it never ran, so it
// goes back uncounted, and no run is registered while the drain waits.
func TestDrain_AClaimInFlightAtStepOneIsGivenBackWithoutRunning(t *testing.T) {
	var fires atomic.Int32
	h := newHarness(t, drainConfig(), firerFunc(func(context.Context, spi.ScheduledTask, int, time.Duration) workflow.RunReport {
		fires.Add(1)
		return fired
	}))
	block := make(chan struct{})
	h.fs.with(func() {
		h.fs.claimBlock = block
		h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")}
	})
	h.start(t)
	eventually(t, "a claim in flight", func() bool { return len(h.fs.claimed()) == 1 })

	done := drainAsync(context.Background(), h)
	eventually(t, "the drain began", func() bool { return draining(h.svc) })
	// Step 1 waits for the claim loop, and so for the claim in flight.
	select {
	case <-done:
		t.Fatal("Drain returned while a claim was still in flight")
	case <-time.After(50 * time.Millisecond):
	}
	from := len(h.fs.giveBackCalls())
	close(block)
	receive(t, done)

	token := h.fs.claimed()[0]
	if n := fires.Load(); n != 0 {
		t.Errorf("%d runs fired for a claim that returned after step 1", n)
	}
	// Step 5 returns the refused claim; the loop gives nothing back for it.
	if n := len(h.fs.giveBackCalls()) - from; n != 1 {
		t.Errorf("%d give-backs after the claim returned, want only step 5's", n)
	}
	if !slices.Contains(h.fs.givenBackTokens(), token) {
		t.Error("the refused claim was not given back")
	}
	if a, f := len(h.fs.attemptsRecorded()), len(h.fs.failsRecorded()); a+f != 0 {
		t.Errorf("a claim that never ran wrote %d attempts and %d failures", a, f)
	}
	if keep, ok := h.fs.lastGiveBack(); !ok || slices.Contains(keep, token) {
		t.Errorf("the final give-back kept %v; a claim that never ran is given back", keep)
	}
	if n := liveRuns(h.svc); n != 0 {
		t.Errorf("%d live runs, want none", n)
	}
	h.fs.with(func() {
		if h.fs.retired != 1 {
			t.Errorf("RetireOwner calls = %d, want 1", h.fs.retired)
		}
	})
}

// A claim in flight when the node latches is not started either: it goes back
// uncounted at once, also when failing heartbeats stop the ticks that would
// otherwise give it back.
func TestService_AClaimInFlightWhenTheNodeLatchesIsGivenBackWithoutRunning(t *testing.T) {
	var fires atomic.Int32
	h := newHarness(t, testConfig(), firerFunc(func(context.Context, spi.ScheduledTask, int, time.Duration) workflow.RunReport {
		fires.Add(1)
		return fired
	}))
	block := make(chan struct{})
	h.fs.with(func() {
		h.fs.claimBlock = block
		h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")}
	})
	h.start(t)
	eventually(t, "a claim in flight", func() bool { return len(h.fs.claimed()) == 1 })
	h.svc.latch()
	h.fs.with(func() { h.fs.hbErr = errors.New("heartbeat: connection refused") })
	eventually(t, "unhealthy", func() bool { return !h.svc.isHealthy() })
	from := len(h.fs.giveBackCalls())
	close(block)

	token := h.fs.claimed()[0]
	eventually(t, "a give-back after the claim returned", func() bool { return len(h.fs.giveBackCalls()) > from })
	if keep := h.fs.giveBackCalls()[from]; slices.Contains(keep, token) {
		t.Errorf("the give-back kept %v; a claim that never ran is given back", keep)
	}
	if n := fires.Load(); n != 0 {
		t.Errorf("%d runs fired for a claim that returned after the latch", n)
	}
	if a, f := len(h.fs.attemptsRecorded()), len(h.fs.failsRecorded()); a+f != 0 {
		t.Errorf("a claim that never ran wrote %d attempts and %d failures", a, f)
	}
	if n := liveRuns(h.svc); n != 0 {
		t.Errorf("%d live runs, want none", n)
	}
}

// Step 4 lets an outcome write retry: bookkeeping stops only at its end, so
// a transient error during the drain does not cost the run its outcome.
func TestDrain_OutcomeWritesRetryUntilTheEndOfStepFour(t *testing.T) {
	cfg := testConfig()
	cfg.ShutdownDrain = 2 * time.Second
	tokens := make(chan uuid.UUID, 1)
	h := newHarness(t, cfg, firerFunc(func(ctx context.Context, task spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		tokens <- task.Claim.Token
		<-workflow.RunGuardFrom(ctx).NoNewUnsafe
		return safeFailure
	}))
	h.fs.with(func() {
		h.fs.outcomeErrs = []error{errors.New("record attempt: connection refused")}
		h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")}
	})
	h.start(t)
	token := receive(t, tokens)

	h.svc.Drain(context.Background())
	attempts := h.fs.attemptsRecorded()
	if len(attempts) != 1 || attempts[0].NotCounted {
		t.Errorf("attempts = %+v, want one counted attempt", attempts)
	}
	if slices.Contains(h.fs.givenBackTokens(), token) {
		t.Error("the claim was given back; its outcome write should have been retried and accepted")
	}
}

// Step 5 gives the claims back while the heartbeat still runs; the heartbeat
// stops only then (§6.4).
func TestDrain_TheFinalGiveBackComesBeforeTheHeartbeatStops(t *testing.T) {
	h := newHarness(t, drainConfig(), reportFirer(fired))
	var afterHeartbeat atomic.Bool
	h.fs.with(func() {
		h.fs.onGiveBack = func() {
			select {
			case <-h.svc.hbDone:
				afterHeartbeat.Store(true)
			default:
			}
		}
	})
	h.start(t)
	eventually(t, "a heartbeat", func() bool { return h.fs.heartbeatCount() > 0 })
	h.svc.Drain(context.Background())
	if len(h.fs.giveBackCalls()) == 0 {
		t.Fatal("no give-back at shutdown")
	}
	if afterHeartbeat.Load() {
		t.Error("the final give-back ran after the heartbeat had stopped")
	}
}

// A Drain before Start leaves nothing to stop later: a Start after it does
// nothing.
func TestDrain_AStartAfterADrainDoesNothing(t *testing.T) {
	h := newHarness(t, testConfig(), reportFirer(fired))
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")} })
	h.svc.Drain(context.Background())
	h.start(t)
	time.Sleep(50 * time.Millisecond)
	if n, c := h.fs.heartbeatCount(), h.fs.claims(); n+c != 0 {
		t.Errorf("%d heartbeats and %d claims after a Start that followed a Drain", n, c)
	}
}
