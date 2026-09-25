package scheduler

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
)

var ticketText = regexp.MustCompile(`^internal error \[ticket: [0-9a-f-]{36}\]$`)

const runPanicLine = "scheduled run panicked; node latched"

func TestService_PanickingRunFailsItsTaskAndLatches(t *testing.T) {
	logs := captureLogs(t)
	claimed := make(chan spi.ScheduledTask, 1)
	h := newHarness(t, testConfig(), firerFunc(func(_ context.Context, task spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		claimed <- task
		panic("injected panic in a scheduled run")
	}))
	runs := withRunMetrics(t, h)
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")} })
	h.start(t)
	task := receive(t, claimed)
	token := task.Claim.Token

	eventually(t, "the task failed", func() bool { return len(h.fs.failsRecorded()) == 1 })
	f := h.fs.failsRecorded()[0]
	if f.Reason != spi.FailureRunPanicked || !ticketText.MatchString(f.Error) {
		t.Errorf("failure = %+v, want RUN_PANICKED with a ticket only", f)
	}
	if h.flag.Load() {
		t.Error("a panicking run did not latch the node")
	}
	eventually(t, "the run ended as panicked", func() bool { return runs()[outcomePanicked] == 1 })

	// The FAILED write and its audit event commit together (§5.7).
	events := failEvents(t, h, task.TenantID, task.EntityID)
	if len(events) != 1 || events[0].Data["reason"] != string(spi.FailureRunPanicked) {
		t.Errorf("SCHEDULED_TRANSITION_FAIL events = %+v, want one with reason RUN_PANICKED", events)
	}

	// A latched node claims nothing, and goes on heartbeating.
	claims := h.fs.claims()
	beats := h.fs.heartbeatCount()
	eventually(t, "heartbeats go on after the latch", func() bool { return h.fs.heartbeatCount() > beats+2 })
	if n := h.fs.claims(); n != claims {
		t.Errorf("%d claims after the latch", n-claims)
	}

	// The ERROR line carries the ticket lastError shows, and the stack; no
	// token is logged, and lastError carries no stack.
	lines := logRecords(t, logs.String(), runPanicLine)
	if len(lines) != 1 {
		t.Fatalf("%d panic lines, want 1", len(lines))
	}
	ticket := strings.TrimSuffix(strings.TrimPrefix(f.Error, "internal error [ticket: "), "]")
	if lines[0]["level"] != "ERROR" || lines[0]["ticket"] != ticket {
		t.Errorf("panic line = %v, want ERROR with the ticket lastError shows (%s)", lines[0], ticket)
	}
	if stack, _ := lines[0]["stack"].(string); !strings.Contains(stack, "goroutine") {
		t.Errorf("panic line carries no stack: %v", lines[0])
	}
	if strings.Contains(f.Error, "goroutine") || strings.Contains(f.Error, "injected") {
		t.Errorf("lastError leaks the panic: %q", f.Error)
	}
	out := logs.String()
	for _, tok := range []uuid.UUID{task.ArmToken, token} {
		if strings.Contains(out, tok.String()) {
			t.Error("a log line carries a token")
		}
	}
}

// A panicked run is never given back (§6.5): not while its FAILED write is
// pending, and not once it is accepted.
func TestService_PanickedRunKeepsItsClaimAfterItsFailIsRecorded(t *testing.T) {
	tokens := make(chan uuid.UUID, 1)
	h := newHarness(t, testConfig(), firerFunc(func(_ context.Context, task spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		tokens <- task.Claim.Token
		panic("injected panic in a scheduled run")
	}))
	runs := withRunMetrics(t, h)
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")} })
	h.start(t)
	token := receive(t, tokens)
	// The run counts as ended only after finish: from here on the live set
	// is what the run left.
	eventually(t, "the run ended as panicked", func() bool { return runs()[outcomePanicked] == 1 })
	if n := len(h.fs.failsRecorded()); n != 1 {
		t.Fatalf("%d FAILED writes accepted, want 1", n)
	}
	if n := liveRuns(h.svc); n != 1 {
		t.Errorf("%d live runs, want the panicked run kept", n)
	}
	from := len(h.fs.giveBackCalls())
	eventually(t, "three more give-backs", func() bool { return len(h.fs.giveBackCalls()) >= from+3 })
	for i, keep := range h.fs.giveBackCalls()[from:] {
		if !slices.Contains(keep, token) {
			t.Fatalf("give-back %d hands back the panicked run's claim", i)
		}
	}
}

func TestService_PanicWhileRecordingTheOutcomeLatchesAndKeepsTheClaim(t *testing.T) {
	logs := captureLogs(t)
	tokens := make(chan uuid.UUID, 1)
	h := newHarness(t, testConfig(), firerFunc(func(_ context.Context, task spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		tokens <- task.Claim.Token
		return workflow.RunReport{Outcome: workflow.OutcomeFailed, Err: context.DeadlineExceeded}
	}))
	runs := withRunMetrics(t, h)
	h.fs.with(func() {
		h.fs.outcomePanic = true
		h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")}
	})
	h.start(t)
	token := receive(t, tokens)
	eventually(t, "the node latched", func() bool { return !h.flag.Load() })
	eventually(t, "the run ended as panicked", func() bool { return runs()[outcomePanicked] == 1 })
	if n := liveRuns(h.svc); n != 1 {
		t.Errorf("%d live runs, want the run whose bookkeeping panicked kept", n)
	}
	from := len(h.fs.giveBackCalls())
	eventually(t, "three more give-backs", func() bool { return len(h.fs.giveBackCalls()) >= from+3 })
	for i, keep := range h.fs.giveBackCalls()[from:] {
		if !slices.Contains(keep, token) {
			t.Fatalf("give-back %d hands back the claim whose bookkeeping panicked", i)
		}
	}
	h.fs.with(func() {
		if n := len(h.fs.outcomeCtxErrs); n != 1 {
			t.Errorf("%d outcome writes, want one: a panicked write is not retried", n)
		}
	})
	lines := logRecords(t, logs.String(), "scheduled run bookkeeping panicked; node latched")
	if len(lines) != 1 {
		t.Fatalf("%d bookkeeping panic lines, want 1", len(lines))
	}
	if ticket, _ := lines[0]["ticket"].(string); lines[0]["level"] != "ERROR" || uuid.Validate(ticket) != nil {
		t.Errorf("bookkeeping panic line = %v, want ERROR with a ticket", lines[0])
	}
}

// A panic inside the transaction that fails the task rolls it back: no audit
// event is kept, and the claim is kept.
func TestService_PanicInsideTheFailTransactionRollsItBack(t *testing.T) {
	task := dueTask("t1", "task-1")
	h := newHarness(t, testConfig(), reportFirer(unsafeFailure))
	tx := &flakyTx{TransactionManager: h.svc.deps.TxManager, failed: true}
	h.svc.deps.TxManager = tx
	runs := withRunMetrics(t, h)
	h.fs.with(func() {
		h.fs.outcomePanic = true
		h.fs.due = []spi.ScheduledTask{task}
	})
	h.start(t)
	eventually(t, "the run ended as panicked", func() bool { return runs()[outcomePanicked] == 1 })
	if h.flag.Load() {
		t.Error("a panic in the FAILED write did not latch the node")
	}
	begun, committed, rolledBack := tx.snapshot()
	if len(begun) != 1 || len(committed) != 0 || !slices.Equal(rolledBack, begun) {
		t.Errorf("begun %v, committed %v, rolled back %v: want the one transaction rolled back", begun, committed, rolledBack)
	}
	if events := failEvents(t, h, task.TenantID, task.EntityID); len(events) != 0 {
		t.Errorf("%d SCHEDULED_TRANSITION_FAIL events kept after the panic", len(events))
	}
	if n := liveRuns(h.svc); n != 1 {
		t.Errorf("%d live runs, want the claim kept", n)
	}
}

func TestService_TheLatchCancelsEveryRunInProgress(t *testing.T) {
	blocked := make(chan struct{})
	h := newHarness(t, testConfig(), firerFunc(func(ctx context.Context, task spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		if task.ID == "task-block" {
			close(blocked)
			return failedOnCancel(ctx)
		}
		<-blocked
		panic("injected panic in a scheduled run")
	}))
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-block"), dueTask("t2", "task-boom")} })
	h.start(t)
	eventually(t, "the blocked run's attempt", func() bool { return len(h.fs.attemptsRecorded()) == 1 })
	if a := h.fs.attemptsRecorded()[0]; a.NotCounted || a.Error != cancelledText {
		t.Errorf("attempt = %+v, want a counted attempt cancelled by the latch", a)
	}
	eventually(t, "the blocked run released", func() bool { return liveRuns(h.svc) == 1 })
}

func TestService_APanicInTheLoopHeartbeatOrWatchdogLatchesAndCancels(t *testing.T) {
	h := newHarness(t, testConfig(), firerFunc(func(ctx context.Context, _ spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		return failedOnCancel(ctx)
	}))
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")} })
	h.start(t)
	eventually(t, "the run started", func() bool { return liveRuns(h.svc) == 1 })

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer h.svc.recoverLatch("watchdog")
		panic("injected panic in the watchdog")
	}()
	receive(t, done)
	if h.flag.Load() {
		t.Error("the recovered panic did not latch the node")
	}
	eventually(t, "the run cancelled by the latch", func() bool { return len(h.fs.attemptsRecorded()) == 1 })
	if a := h.fs.attemptsRecorded()[0]; a.NotCounted || a.Error != cancelledText {
		t.Errorf("attempt = %+v, want the run cancelled by the latch", a)
	}
	claims := h.fs.claims()
	time.Sleep(50 * time.Millisecond)
	if n := h.fs.claims(); n != claims {
		t.Errorf("%d claims after the latch", n-claims)
	}
}

// A store rejection latches the node but cancels nothing: its runs in
// progress go on (§5.3, §5.6, §6.5).
func TestService_StoreRejectionDoesNotCancelTheRunsInProgress(t *testing.T) {
	sibling := make(chan context.Context, 1)
	release := make(chan struct{})
	h := newHarness(t, testConfig(), firerFunc(func(ctx context.Context, task spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		if task.ID == "task-sibling" {
			sibling <- ctx
			<-release
			return fired
		}
		return safeCallout
	}))
	h.fs.with(func() {
		h.fs.outcomeErrs = []error{fmt.Errorf("record attempt: value too long: %w", spi.ErrStoreRejected)}
		// The sibling is claimed first, so it is running when the other
		// run's outcome is rejected.
		h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-sibling")}
	})
	h.start(t)
	ctx := receive(t, sibling)
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t2", "task-rejected")} })
	eventually(t, "the node latched", func() bool { return !h.flag.Load() })
	time.Sleep(50 * time.Millisecond)
	if err := ctx.Err(); err != nil {
		t.Errorf("the sibling run was cancelled by a store rejection: %v", err)
	}
	close(release)
	eventually(t, "the sibling run released", func() bool { return liveRuns(h.svc) == 1 })
}

// A panic in the heartbeat goroutine latches the node and cancels the runs;
// the heartbeat stops, so the runs can be taken over (§6.5).
func TestService_HeartbeatPanicLatchesCancelsAndStopsTheHeartbeat(t *testing.T) {
	h := newHarness(t, testConfig(), firerFunc(func(ctx context.Context, _ spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		return failedOnCancel(ctx)
	}))
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")} })
	h.start(t)
	eventually(t, "the run started", func() bool { return liveRuns(h.svc) == 1 })
	h.fs.with(func() { h.fs.heartbeatPanic = true })
	eventually(t, "the node latched", func() bool { return !h.flag.Load() })
	eventually(t, "the run cancelled by the latch", func() bool { return len(h.fs.attemptsRecorded()) == 1 })
	if a := h.fs.attemptsRecorded()[0]; a.NotCounted || a.Error != cancelledText {
		t.Errorf("attempt = %+v, want the run cancelled by the latch", a)
	}
	beats, claims := h.fs.heartbeatCount(), h.fs.claims()
	time.Sleep(50 * time.Millisecond)
	if n := h.fs.heartbeatCount(); n != beats {
		t.Errorf("%d heartbeats after the heartbeat panicked", n-beats)
	}
	if n := h.fs.claims(); n != claims {
		t.Errorf("%d claims after the latch", n-claims)
	}
}

// A panic in the claim loop latches the node and cancels the runs; the
// heartbeat goes on, so the runs are not taken over while they stop (§6.5).
func TestService_ClaimLoopPanicLatchesCancelsAndKeepsHeartbeating(t *testing.T) {
	h := newHarness(t, testConfig(), firerFunc(func(ctx context.Context, _ spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		return failedOnCancel(ctx)
	}))
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")} })
	h.start(t)
	eventually(t, "the run started", func() bool { return liveRuns(h.svc) == 1 })
	h.fs.with(func() { h.fs.claimPanic = true })
	eventually(t, "the node latched", func() bool { return !h.flag.Load() })
	eventually(t, "the run cancelled by the latch", func() bool { return len(h.fs.attemptsRecorded()) == 1 })
	if a := h.fs.attemptsRecorded()[0]; a.NotCounted || a.Error != cancelledText {
		t.Errorf("attempt = %+v, want the run cancelled by the latch", a)
	}
	beats, claims := h.fs.heartbeatCount(), h.fs.claims()
	eventually(t, "heartbeats go on after the loop panicked", func() bool { return h.fs.heartbeatCount() > beats+2 })
	if n := h.fs.claims(); n != claims {
		t.Errorf("%d claims after the latch", n-claims)
	}
}

// A panicked run whose FAILED write is refused as stale is still never given
// back (§6.5).
func TestService_PanickedRunKeepsItsClaimWhenItsFailIsRefused(t *testing.T) {
	tokens := make(chan uuid.UUID, 1)
	h := newHarness(t, testConfig(), firerFunc(func(_ context.Context, task spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		tokens <- task.Claim.Token
		panic("injected panic in a scheduled run")
	}))
	runs := withRunMetrics(t, h)
	h.fs.with(func() {
		h.fs.outcomeErrs = []error{fmt.Errorf("fail: %w", spi.ErrStaleClaim)}
		h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")}
	})
	h.start(t)
	token := receive(t, tokens)
	eventually(t, "the run ended as superseded", func() bool { return runs()[outcomeSuperseded] == 1 })
	if n := liveRuns(h.svc); n != 1 {
		t.Errorf("%d live runs, want the panicked run kept", n)
	}
	from := len(h.fs.giveBackCalls())
	eventually(t, "three more give-backs", func() bool { return len(h.fs.giveBackCalls()) >= from+3 })
	for i, keep := range h.fs.giveBackCalls()[from:] {
		if !slices.Contains(keep, token) {
			t.Fatalf("give-back %d hands back the panicked run's claim", i)
		}
	}
}

// A panic in one run's bookkeeping cancels every other run in progress
// (§6.5: the recovery itself cancels them).
func TestService_BookkeepingPanicCancelsTheRunsInProgress(t *testing.T) {
	sibling := make(chan context.Context, 1)
	h := newHarness(t, testConfig(), firerFunc(func(ctx context.Context, task spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		if task.ID == "task-sibling" {
			sibling <- ctx
			return failedOnCancel(ctx)
		}
		return workflow.RunReport{Outcome: workflow.OutcomeFailed, Err: context.DeadlineExceeded}
	}))
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-sibling")} })
	h.start(t)
	ctx := receive(t, sibling)
	h.fs.with(func() {
		h.fs.outcomePanic = true
		h.fs.due = []spi.ScheduledTask{dueTask("t2", "task-boom")}
	})
	eventually(t, "the node latched", func() bool { return !h.flag.Load() })
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the bookkeeping panic did not cancel the sibling run")
	}
}
