package scheduler

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
)

var fired = workflow.RunReport{Outcome: workflow.OutcomeFired}

// Start logs "scheduler started" at INFO with the incarnation, once (README
// C-R2). The multi-node scenarios map a claim's owner to its pnode by this
// line; nothing else exposes the mapping. A disabled service logs nothing.
func TestService_StartLogsItsIncarnation(t *testing.T) {
	logs := captureLogs(t)
	h := newHarness(t, testConfig(), reportFirer(fired))
	h.start(t)
	if err := h.svc.Start(context.Background()); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	var lines []string
	for _, l := range strings.Split(logs.String(), "\n") {
		if strings.Contains(l, `"msg":"scheduler started"`) {
			lines = append(lines, l)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("%d 'scheduler started' lines, want exactly 1: %v", len(lines), lines)
	}
	if !strings.Contains(lines[0], `"level":"INFO"`) ||
		!strings.Contains(lines[0], `"incarnation":"`+h.svc.incarnation.String()+`"`) {
		t.Errorf("line = %s, want INFO with incarnation=%s", lines[0], h.svc.incarnation)
	}

	off := testConfig()
	off.Enabled = false
	before := strings.Count(logs.String(), `"msg":"scheduler started"`)
	newHarness(t, off, reportFirer(fired)).start(t)
	if after := strings.Count(logs.String(), `"msg":"scheduler started"`); after != before {
		t.Error("a disabled scheduler logged 'scheduler started'")
	}
}

func TestService_DisabledStartsNothing(t *testing.T) {
	cfg := testConfig()
	cfg.Enabled = false
	h := newHarness(t, cfg, reportFirer(fired))
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")} })
	h.start(t)
	time.Sleep(50 * time.Millisecond)
	if n, c := h.fs.heartbeatCount(), h.fs.claims(); n != 0 || c != 0 {
		t.Errorf("a disabled scheduler touched the store: %d heartbeats, %d claims", n, c)
	}
}

func TestService_StartIsIdempotent(t *testing.T) {
	cfg := testConfig()
	cfg.HeartbeatInterval = 20 * time.Millisecond
	h := newHarness(t, cfg, reportFirer(fired))
	h.start(t)
	if err := h.svc.Start(context.Background()); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	// One heartbeat goroutine makes at most 11 calls in 200ms; two would make about 20.
	if n := h.fs.heartbeatCount(); n > 14 {
		t.Errorf("%d heartbeats in 200ms at a 20ms interval: a second Start started a second heartbeat goroutine", n)
	}
}

func TestService_NoClaimBeforeTheFirstHeartbeat(t *testing.T) {
	h := newHarness(t, testConfig(), reportFirer(fired))
	h.fs.with(func() {
		h.fs.hbErr = errors.New("heartbeat: connection refused")
		h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")}
	})
	h.start(t)
	eventually(t, "three failed heartbeats", func() bool { return h.fs.heartbeatFailures() >= 3 })
	if n := h.fs.claims(); n != 0 {
		t.Fatalf("%d claims before any heartbeat succeeded", n)
	}
	if n := len(h.fs.giveBackCalls()); n != 0 {
		t.Fatalf("%d give-backs before any heartbeat succeeded", n)
	}
	h.fs.with(func() { h.fs.hbErr = nil })
	eventually(t, "a claim once a heartbeat succeeds", func() bool { return h.fs.claims() > 0 })
}

func TestService_ClaimRequestCarriesOwnerLimitsAndTenantCounts(t *testing.T) {
	cfg := testConfig()
	cfg.MaxRuns, cfg.MaxRunsPerTenant = 3, 2
	release := make(chan struct{})
	h := newHarness(t, cfg, firerFunc(func(context.Context, spi.ScheduledTask, int, time.Duration) workflow.RunReport {
		<-release
		return fired
	}))
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("tenant-a", "task-1")} })
	testStart := time.Now().UnixMilli()
	h.start(t)
	t.Cleanup(func() { close(release) })
	eventually(t, "a claim made while the run is in progress", func() bool { return h.fs.claims() >= 2 })

	var req spi.ClaimRequest
	h.fs.with(func() { req = h.fs.claimReqs[len(h.fs.claimReqs)-1] })
	if req.Owner != h.svc.incarnation {
		t.Error("the claim does not name this incarnation as its owner")
	}
	if req.Limit != 2 || req.PerTenantLimit != 2 || req.TenantInProgress["tenant-a"] != 1 {
		t.Errorf("Limit=%d PerTenantLimit=%d TenantInProgress=%v, want 2, 2 and tenant-a:1",
			req.Limit, req.PerTenantLimit, req.TenantInProgress)
	}
	if req.StaleAfter != cfg.StaleAfter {
		t.Errorf("StaleAfter = %s, want %s", req.StaleAfter, cfg.StaleAfter)
	}
	if req.NowMs < testStart || req.NowMs > time.Now().UnixMilli() {
		t.Errorf("NowMs = %d, want the pnode clock", req.NowMs)
	}
}

func TestService_LostOwnerClaimsWaitForAFullStalePeriodOfCleanHeartbeats(t *testing.T) {
	cfg := testConfig()
	cfg.StaleAfter = 300 * time.Millisecond
	h := newHarness(t, cfg, reportFirer(fired))
	h.start(t)
	eventually(t, "a claim", func() bool { return h.fs.claims() > 0 })
	h.fs.with(func() {
		if h.fs.claimReqs[0].AllowLostOwner {
			t.Error("the first claim after start allowed lost-owner claims")
		}
	})
	eventually(t, "lost-owner claims after STALE_AFTER of clean heartbeats", func() bool {
		var allow bool
		h.fs.with(func() { allow = h.fs.claimReqs[len(h.fs.claimReqs)-1].AllowLostOwner })
		return allow
	})

	h.fs.with(func() { h.fs.hbErr = errors.New("heartbeat: connection refused") })
	eventually(t, "two failed heartbeats", func() bool { return h.fs.heartbeatFailures() >= 2 })
	idx := h.fs.claims()
	h.fs.with(func() { h.fs.hbErr = nil })
	eventually(t, "a claim after the outage", func() bool { return h.fs.claims() > idx })
	h.fs.with(func() {
		if h.fs.claimReqs[idx].AllowLostOwner {
			t.Error("the first claim after an outage allowed lost-owner claims")
		}
	})
}

func TestService_AtMostMaxRunsAndAFreedSlotClaimsAtOnce(t *testing.T) {
	cfg := testConfig()
	cfg.MaxRuns = 2
	cfg.ScanInterval = time.Second
	release := map[string]chan struct{}{"task-1": make(chan struct{}), "task-2": make(chan struct{}), "task-3": make(chan struct{})}
	var running, most atomic.Int32
	started := make(chan string, 3)
	h := newHarness(t, cfg, firerFunc(func(_ context.Context, task spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		n := running.Add(1)
		for m := most.Load(); n > m && !most.CompareAndSwap(m, n); m = most.Load() {
		}
		started <- task.ID
		<-release[task.ID]
		running.Add(-1)
		return fired
	}))
	h.fs.with(func() {
		h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1"), dueTask("t2", "task-2"), dueTask("t3", "task-3")}
	})
	h.start(t)
	t.Cleanup(func() {
		for _, ch := range release {
			select {
			case <-ch:
			default:
				close(ch)
			}
		}
	})

	first := receive(t, started)
	receive(t, started)
	h.fs.with(func() {
		if got := h.fs.claimReqs[0].Limit; got != 2 {
			t.Errorf("first claim Limit = %d, want MAX_RUNS 2", got)
		}
	})
	freed := time.Now()
	close(release[first])
	if third := receive(t, started); third != "task-3" {
		t.Errorf("third run = %s, want task-3", third)
	}
	if d := time.Since(freed); d > 500*time.Millisecond {
		t.Errorf("the freed slot was claimed %s after it freed; want at once, not at the next 1s tick", d)
	}
	if m := most.Load(); m > 2 {
		t.Errorf("%d runs at once, want at most MAX_RUNS 2", m)
	}
}

func TestService_GiveBackIdleKeepsEveryLiveRun(t *testing.T) {
	release := make(chan struct{})
	tokens := make(chan uuid.UUID, 1)
	h := newHarness(t, testConfig(), firerFunc(func(_ context.Context, task spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		tokens <- task.Claim.Token
		<-release
		return fired
	}))
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")} })
	h.start(t)
	token := receive(t, tokens)
	eventually(t, "a give-back that keeps the live run", func() bool {
		keep, ok := h.fs.lastGiveBack()
		return ok && slices.Contains(keep, token)
	})
	close(release)
	eventually(t, "a give-back that no longer keeps the finished run", func() bool {
		keep, ok := h.fs.lastGiveBack()
		return ok && !slices.Contains(keep, token)
	})
}

func TestService_LostClaimReplyIsGivenBack(t *testing.T) {
	h := newHarness(t, testConfig(), firerFunc(func(context.Context, spi.ScheduledTask, int, time.Duration) workflow.RunReport {
		t.Error("a task whose claim reply was lost was run")
		return fired
	}))
	h.fs.with(func() {
		h.fs.claimErr = errors.New("claim: connection reset after commit")
		h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")}
	})
	h.start(t)
	eventually(t, "the failed claim", func() bool { return h.fs.claims() >= 1 })
	calls := len(h.fs.giveBackCalls())
	eventually(t, "a later give-back", func() bool { return len(h.fs.giveBackCalls()) > calls })
	for _, keep := range h.fs.giveBackCalls()[calls:] {
		if len(keep) != 0 {
			t.Errorf("a give-back kept %v; nothing is live, so the lost claim must be given back", keep)
		}
	}
}

func TestService_SweepsDeadOwnersAndEndedMarks(t *testing.T) {
	cfg := testConfig()
	h := newHarness(t, cfg, reportFirer(fired))
	h.svc.sweepEvery = 20 * time.Millisecond
	h.start(t)
	eventually(t, "an owner sweep and a mark sweep", func() bool {
		var owners, marks int
		h.fs.with(func() { owners, marks = len(h.fs.sweptOwners), h.fs.sweptMarks })
		return owners > 0 && marks > 0
	})
	h.fs.with(func() {
		if got := h.fs.sweptOwners[0]; got != 10*cfg.StaleAfter {
			t.Errorf("SweepOwners(%s), want 10 x STALE_AFTER = %s", got, 10*cfg.StaleAfter)
		}
	})
}

func TestService_RunsUnderTheSystemIdentityAndItsRunGuard(t *testing.T) {
	cfg := testConfig()
	type seen struct {
		uc      *spi.UserContext
		guard   *workflow.RunGuard
		task    spi.ScheduledTask
		maxLost int
		retry   time.Duration
	}
	got := make(chan seen, 1)
	h := newHarness(t, cfg, firerFunc(func(ctx context.Context, task spi.ScheduledTask, maxLost int, retry time.Duration) workflow.RunReport {
		got <- seen{spi.GetUserContext(ctx), workflow.RunGuardFrom(ctx), task, maxLost, retry}
		return fired
	}))
	task := dueTask("tenant-x", "task-1")
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{task} })
	h.start(t)
	s := receive(t, got)

	if s.uc == nil || s.uc.Kind != spi.PrincipalSystem || s.uc.Tenant.ID != "tenant-x" {
		t.Errorf("run identity = %+v, want the system principal of tenant-x", s.uc)
	}
	if s.guard == nil {
		t.Fatal("no run guard on the run's context")
	}
	want := spi.TaskRef{TenantID: "tenant-x", ID: "task-1", ArmToken: task.ArmToken, ClaimToken: s.task.Claim.Token}
	if s.guard.Ref != want {
		t.Errorf("guard ref = %+v, want %+v", s.guard.Ref, want)
	}
	if s.guard.Store == nil || s.guard.Done == nil || s.guard.NoNewUnsafe == nil || s.guard.Unsafe == nil {
		t.Errorf("run guard incomplete: %+v", s.guard)
	}
	if s.maxLost != cfg.MaxLostOwners || s.retry != cfg.RetryDelay {
		t.Errorf("engine got maxLostOwners=%d retryDelay=%s, want %d and %s", s.maxLost, s.retry, cfg.MaxLostOwners, cfg.RetryDelay)
	}
}

func TestService_CommittedOutcomesRecordNothing(t *testing.T) {
	for _, outcome := range []workflow.ScheduledOutcome{
		workflow.OutcomeFired, workflow.OutcomeDeclined, workflow.OutcomeExpired,
		workflow.OutcomeCancelled, workflow.OutcomeSuperseded,
	} {
		t.Run(string(outcome), func(t *testing.T) {
			h := newHarness(t, testConfig(), reportFirer(workflow.RunReport{Outcome: outcome}))
			h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")} })
			h.start(t)
			eventually(t, "the run claimed and released", func() bool { return h.fs.claims() > 0 && liveRuns(h.svc) == 0 })
			time.Sleep(20 * time.Millisecond)
			if a, f := len(h.fs.attemptsRecorded()), len(h.fs.failsRecorded()); a+f != 0 {
				t.Errorf("a %s run wrote %d attempts and %d failures", outcome, a, f)
			}
			if !h.flag.Load() {
				t.Error("a normal run latched the node")
			}
		})
	}
}

func TestService_FailedRunRecordsAnAttempt(t *testing.T) {
	cfg := testConfig()
	h := newHarness(t, cfg, reportFirer(workflow.RunReport{Outcome: workflow.OutcomeFailed,
		Err: fmt.Errorf("criterion failed: %w", &contract.CalloutFailure{Kind: contract.MemberFailed, Message: "card declined"})}))
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")} })
	before := time.Now().UnixMilli()
	h.start(t)
	eventually(t, "one recorded attempt", func() bool { return len(h.fs.attemptsRecorded()) == 1 })
	a := h.fs.attemptsRecorded()[0]
	if a.Error != "card declined" || a.NotCounted || a.ClearOwnMark {
		t.Errorf("attempt = %+v, want a counted attempt with the compute node's text", a)
	}
	if lo, hi := before+cfg.RetryDelay.Milliseconds(), time.Now().UnixMilli()+cfg.RetryDelay.Milliseconds(); a.NextAttemptTime < lo || a.NextAttemptTime > hi {
		t.Errorf("NextAttemptTime = %d, want now + RETRY_DELAY", a.NextAttemptTime)
	}
	eventually(t, "the run released", func() bool { return liveRuns(h.svc) == 0 })
}

func TestService_EngineDecidedFailureFailsTheTask(t *testing.T) {
	h := newHarness(t, testConfig(), reportFirer(workflow.RunReport{Outcome: workflow.OutcomeFailed, FailReason: spi.FailureOwnerLostRepeatedly}))
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")} })
	h.start(t)
	eventually(t, "the task failed", func() bool { return len(h.fs.failsRecorded()) == 1 })
	if got := h.fs.failsRecorded()[0].Reason; got != spi.FailureOwnerLostRepeatedly {
		t.Errorf("reason = %s, want OWNER_LOST_REPEATEDLY", got)
	}
}

func TestService_RefusedOutcomeMeansSuperseded(t *testing.T) {
	h := newHarness(t, testConfig(), reportFirer(workflow.RunReport{Outcome: workflow.OutcomeFailed, Err: errors.New("boom")}))
	h.fs.with(func() {
		h.fs.outcomeErrs = []error{fmt.Errorf("record attempt: %w", spi.ErrStaleClaim)}
		h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")}
	})
	h.start(t)
	eventually(t, "the run released", func() bool { return h.fs.claims() > 0 && liveRuns(h.svc) == 0 })
	time.Sleep(20 * time.Millisecond)
	h.fs.with(func() {
		if n := len(h.fs.outcomeCtxErrs); n != 1 {
			t.Errorf("%d outcome writes, want exactly one: a refusal is final", n)
		}
	})
}

func TestService_FailingHeartbeatsSelfCancelAtTheWindow(t *testing.T) {
	h := newHarness(t, testConfig(), firerFunc(func(ctx context.Context, _ spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		return failedOnCancel(ctx)
	}))
	h.svc.window = 150 * time.Millisecond
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")} })
	h.start(t)
	eventually(t, "the run started", func() bool { return liveRuns(h.svc) == 1 })
	h.fs.with(func() { h.fs.hbErr = errors.New("heartbeat: connection refused") })

	eventually(t, "the self-cancelled run's attempt", func() bool { return len(h.fs.attemptsRecorded()) == 1 })
	a := h.fs.attemptsRecorded()[0]
	if a.NotCounted || a.Error != cancelledText {
		t.Errorf("attempt = %+v, want a counted attempt with %q", a, cancelledText)
	}
	h.fs.with(func() {
		if err := h.fs.outcomeCtxErrs[0]; err != nil {
			t.Errorf("the outcome write inherited the run's cancellation: %v", err)
		}
	})

	idle := h.fs.claims()
	time.Sleep(50 * time.Millisecond)
	if n := h.fs.claims(); n != idle {
		t.Errorf("%d claims while heartbeats were failing", n-idle)
	}
	h.fs.with(func() { h.fs.hbErr = nil })
	eventually(t, "claims resume after a heartbeat succeeds", func() bool { return h.fs.claims() > idle })
}

func TestService_OneFailedHeartbeatDoesNotSelfCancel(t *testing.T) {
	cancelled := make(chan struct{})
	h := newHarness(t, testConfig(), firerFunc(func(ctx context.Context, _ spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		r := failedOnCancel(ctx)
		close(cancelled)
		return r
	}))
	h.svc.window = 100 * time.Millisecond // ten heartbeat intervals
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")} })
	h.start(t)
	eventually(t, "the run started", func() bool { return liveRuns(h.svc) == 1 })
	h.fs.with(func() { h.fs.hbFailNext = 1 })
	eventually(t, "the one failed heartbeat", func() bool { return h.fs.heartbeatFailures() == 1 })
	select {
	case <-cancelled:
		t.Fatal("one failed heartbeat cancelled the pnode's runs")
	case <-time.After(300 * time.Millisecond):
	}
}

func TestService_HungHeartbeatDoesNotStopTheWatchdog(t *testing.T) {
	h := newHarness(t, testConfig(), firerFunc(func(ctx context.Context, _ spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		return failedOnCancel(ctx)
	}))
	h.svc.window = 100 * time.Millisecond
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")} })
	h.start(t)
	eventually(t, "the run started", func() bool { return liveRuns(h.svc) == 1 })
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	h.fs.with(func() { h.fs.hbBlock = hang })
	eventually(t, "the run cancelled while the heartbeat hangs", func() bool { return len(h.fs.attemptsRecorded()) == 1 })
}

func TestService_HeartbeatThatSucceedsAfterItsWindowCountsAsFailed(t *testing.T) {
	h := newHarness(t, testConfig(), reportFirer(fired))
	h.start(t)
	eventually(t, "healthy", h.svc.isHealthy)
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	h.fs.with(func() { h.fs.hbBlock = hang })
	beats := h.fs.heartbeatCount()
	eventually(t, "the heartbeat goroutine parked", func() bool { return h.fs.heartbeatCount() > beats })

	h.svc.heartbeatDone(time.Now().Add(-2*h.svc.window), nil)
	if h.svc.isHealthy() {
		t.Error("a heartbeat that succeeded after its window left the pnode claiming")
	}
}

// The watchdog's timer and a heartbeat that re-arms it can be ready at the
// same moment. A timer that fires after a later heartbeat moved the deadline
// on must not cancel the pnode's runs: the store's stamp of that heartbeat is
// fresh, so no other pnode can consider this one stale.
func TestService_WatchdogFiringAfterARearmDoesNotSelfCancel(t *testing.T) {
	cancelled := make(chan struct{})
	h := newHarness(t, testConfig(), firerFunc(func(ctx context.Context, _ spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		r := failedOnCancel(ctx)
		close(cancelled)
		return r
	}))
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")} })
	h.start(t)
	eventually(t, "the run started", func() bool { return liveRuns(h.svc) == 1 })

	h.svc.heartbeatDone(time.Now(), nil)
	h.svc.selfCancel()
	if !h.svc.isHealthy() {
		t.Error("a stale watchdog timer made the pnode stop claiming")
	}
	select {
	case <-cancelled:
		t.Fatal("a stale watchdog timer cancelled a run whose pnode had just heartbeated")
	case <-time.After(50 * time.Millisecond):
	}
}
