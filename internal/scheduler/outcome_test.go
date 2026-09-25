package scheduler

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
)

var safeFailure = workflow.RunReport{Outcome: workflow.OutcomeFailed, Err: errors.New("boom")}

const retryWarn = "scheduled run outcome not recorded; retrying"

func TestService_OutcomeWriteRetriedThroughAnOutage(t *testing.T) {
	logs := captureLogs(t)
	outage := errors.New("record attempt: dial tcp 10.0.0.5:5432: connect: connection refused")
	h := newHarness(t, testConfig(), reportFirer(safeFailure))
	h.fs.with(func() {
		h.fs.outcomeErrs = []error{outage, outage}
		h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")}
	})
	h.start(t)
	eventually(t, "the attempt accepted after the outage", func() bool { return len(h.fs.attemptsRecorded()) == 1 })
	h.fs.with(func() {
		if n := len(h.fs.outcomeCtxErrs); n != 3 {
			t.Errorf("%d outcome writes, want 3", n)
		}
	})
	if !h.flag.Load() {
		t.Error("an outage latched the node")
	}
	eventually(t, "the run released", func() bool { return liveRuns(h.svc) == 0 })
	if n := strings.Count(logs.String(), retryWarn); n != 1 {
		t.Errorf("%d retry WARN lines for one task within a minute, want 1", n)
	}
}

func TestService_ClearOwnMarkRetriedThroughAnOutage(t *testing.T) {
	outage := errors.New("record attempt: connection refused")
	h := newHarness(t, testConfig(), reportFirer(workflow.RunReport{
		Outcome: workflow.OutcomeFailed, MarkErrored: true, Err: errors.New("mark unsafe: connection refused"),
	}))
	h.fs.with(func() {
		h.fs.outcomeErrs = []error{outage, outage, outage}
		h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")}
	})
	h.start(t)
	eventually(t, "the attempt accepted after the outage", func() bool { return len(h.fs.attemptsRecorded()) == 1 })
	if a := h.fs.attemptsRecorded()[0]; !a.ClearOwnMark || a.NotCounted {
		t.Errorf("attempt = %+v, want a counted attempt that clears the run's own mark", a)
	}
	if !h.flag.Load() {
		t.Error("an outage latched the node")
	}
}

func TestService_LockAndPoolErrorsAreRetriedWithoutLatching(t *testing.T) {
	h := newHarness(t, testConfig(), reportFirer(safeFailure))
	h.fs.with(func() {
		h.fs.outcomeErrs = []error{
			fmt.Errorf("record attempt: %w", context.DeadlineExceeded),
			errors.New("failed to acquire connection: pool exhausted"),
			fmt.Errorf("record attempt: %w", spi.ErrConflict),
			fmt.Errorf("record attempt: %w", spi.ErrTaskBusy),
		}
		h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")}
	})
	h.start(t)
	eventually(t, "the attempt accepted", func() bool { return len(h.fs.attemptsRecorded()) == 1 })
	if !h.flag.Load() {
		t.Error("a lock wait, a full pool, a conflict or a busy row latched the node")
	}
	eventually(t, "the run released", func() bool { return liveRuns(h.svc) == 0 })
}

func TestService_StoreRejectionLatchesAndKeepsTheClaim(t *testing.T) {
	tokens := make(chan uuid.UUID, 1)
	h := newHarness(t, testConfig(), firerFunc(func(_ context.Context, task spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		tokens <- task.Claim.Token
		return safeFailure
	}))
	h.fs.with(func() {
		h.fs.outcomeErrs = []error{fmt.Errorf("record attempt: value too long: %w", spi.ErrStoreRejected)}
		h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")}
	})
	h.start(t)
	token := receive(t, tokens)
	eventually(t, "the node latched", func() bool { return !h.flag.Load() })
	claims := h.fs.claims()
	time.Sleep(50 * time.Millisecond)
	h.fs.with(func() {
		if n := len(h.fs.outcomeCtxErrs); n != 1 {
			t.Errorf("%d outcome writes, want one: a rejection is not retried", n)
		}
	})
	if n := h.fs.claims(); n != claims {
		t.Errorf("%d claims after the latch", n-claims)
	}
	// By now the run has returned. Every give-back from here on must keep its
	// claim: a rejected outcome is never handed back.
	from := len(h.fs.giveBackCalls())
	eventually(t, "three more give-backs", func() bool { return len(h.fs.giveBackCalls()) >= from+3 })
	for i, keep := range h.fs.giveBackCalls()[from:] {
		if !slices.Contains(keep, token) {
			t.Fatalf("give-back %d after the rejection hands back the rejected run's claim", i)
		}
	}
	if n := liveRuns(h.svc); n != 1 {
		t.Errorf("%d live runs, want the rejected run kept", n)
	}
}

func TestService_StopEndsTheRetryAndKeepsTheUnrecordedClaim(t *testing.T) {
	h := newHarness(t, testConfig(), reportFirer(safeFailure))
	h.fs.with(func() {
		h.fs.outcomeAlways = errors.New("record attempt: connection refused")
		h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")}
	})
	h.start(t)
	eventually(t, "a retried outcome write", func() bool {
		var n int
		h.fs.with(func() { n = len(h.fs.outcomeCtxErrs) })
		return n >= 2
	})
	h.svc.Stop()
	done := make(chan struct{})
	go func() {
		h.svc.runsWG.Wait()
		close(done)
	}()
	receive(t, done)
	if n := liveRuns(h.svc); n != 1 {
		t.Errorf("%d live runs, want the claim whose outcome is not recorded kept", n)
	}
	if !h.flag.Load() {
		t.Error("an outage latched the node")
	}
}

func TestService_FailAndItsAuditEventCommitTogether(t *testing.T) {
	task := dueTask("t1", "task-1")
	h := newHarness(t, testConfig(), reportFirer(workflow.RunReport{
		Outcome: workflow.OutcomeFailed, MarkHeld: true, UnsafeReached: true,
		Err: &contract.CalloutFailure{Kind: contract.NoAnswer, Message: "DISPATCH_TIMEOUT: processor dispatch timed out after 100ms: no response"},
	}))
	h.fs.with(func() {
		h.fs.outcomeErrs = []error{errors.New("fail: connection refused")}
		h.fs.due = []spi.ScheduledTask{task}
	})
	h.start(t)
	eventually(t, "the task failed", func() bool { return len(h.fs.failsRecorded()) == 1 })
	h.fs.with(func() {
		if !h.fs.failInTx[0] {
			t.Error("Fail ran outside a transaction")
		}
	})
	if got := h.fs.failsRecorded()[0]; got.Reason != spi.FailureUnsafeWorkNotCompleted ||
		got.Error != "DISPATCH_TIMEOUT: processor dispatch timed out after 100ms: no response" {
		t.Errorf("failure = %+v", got)
	}
	eventually(t, "the run released", func() bool { return liveRuns(h.svc) == 0 })

	ctx := common.SystemUserContext("t1")
	audit, err := h.mem.StateMachineAuditStore(ctx)
	if err != nil {
		t.Fatalf("audit store: %v", err)
	}
	events, err := audit.GetEvents(ctx, task.EntityID)
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	var failed []spi.StateMachineEvent
	for _, e := range events {
		if e.EventType == spi.SMEventScheduledTransitionFailed {
			failed = append(failed, e)
		}
	}
	if len(failed) != 1 {
		t.Fatalf("%d SCHEDULED_TRANSITION_FAIL events, want exactly one", len(failed))
	}
	e := failed[0]
	if e.State != task.SourceState {
		t.Errorf("event state = %q, want the source state %q for an entity that does not exist", e.State, task.SourceState)
	}
	for key, want := range map[string]string{
		"transition": task.Transition, "sourceState": task.SourceState,
		"reason": "UNSAFE_WORK_NOT_COMPLETED", "attempts": "0", "lostOwners": "0",
	} {
		if got := fmt.Sprint(e.Data[key]); got != want {
			t.Errorf("event data %s = %q, want %q", key, got, want)
		}
	}
}

func TestService_OutcomeLogsCarryTheTicketAndNoToken(t *testing.T) {
	logs := captureLogs(t)
	claimed := make(chan spi.ScheduledTask, 2)
	h := newHarness(t, testConfig(), firerFunc(func(_ context.Context, task spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		claimed <- task
		if task.ID == "task-safe" {
			return workflow.RunReport{Outcome: workflow.OutcomeFailed, Err: &contract.CalloutFailure{Kind: contract.MemberFailed, Message: "card declined"}}
		}
		return workflow.RunReport{Outcome: workflow.OutcomeFailed, MarkHeld: true, UnsafeReached: true,
			Err: errors.New("failed to read entity: pq: host=db.internal user=cyoda")}
	}))
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-safe"), dueTask("t2", "task-unsafe")} })
	h.start(t)
	tasks := []spi.ScheduledTask{receive(t, claimed), receive(t, claimed)}
	eventually(t, "both outcome log lines", func() bool {
		out := logs.String()
		return strings.Contains(out, "the task waits for its next attempt") && strings.Contains(out, "scheduled task FAILED")
	})

	out := logs.String()
	for _, task := range tasks {
		for _, token := range []uuid.UUID{task.ArmToken, task.Claim.Token} {
			if strings.Contains(out, token.String()) {
				t.Errorf("a log line carries a token of %s", task.ID)
			}
		}
	}
	fail := h.fs.failsRecorded()[0]
	if strings.Contains(fail.Error, "pq:") {
		t.Errorf("lastError leaks the store error: %q", fail.Error)
	}
	ticket := strings.TrimSuffix(strings.TrimPrefix(fail.Error, "internal error [ticket: "), "]")
	if _, err := uuid.Parse(ticket); err != nil {
		t.Fatalf("lastError %q carries no ticket", fail.Error)
	}
	if !strings.Contains(out, `"ticket":"`+ticket+`"`) {
		t.Error("no ERROR line carries the ticket lastError shows")
	}
}

// storageDown is a plugin's transient-unavailability marker.
type storageDown struct{}

func (storageDown) Error() string            { return "dial tcp db.internal:5432: connect: connection refused" }
func (storageDown) StorageUnavailable() bool { return true }

func TestService_StorageUnavailableWarnLogsItsCause(t *testing.T) {
	logs := captureLogs(t)
	h := newHarness(t, testConfig(), reportFirer(workflow.RunReport{
		Outcome: workflow.OutcomeFailed,
		Err:     fmt.Errorf("failed to read entity: %w", common.Internal("read entity", storageDown{})),
	}))
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")} })
	h.start(t)
	eventually(t, "the attempt recorded", func() bool { return len(h.fs.attemptsRecorded()) == 1 })
	a := h.fs.attemptsRecorded()[0]
	if !strings.HasPrefix(a.Error, common.ErrCodeStorageUnavailable+":") || strings.Contains(a.Error, "db.internal") {
		t.Errorf("lastError = %q, want the STORAGE_UNAVAILABLE text without its cause", a.Error)
	}
	eventually(t, "the WARN line", func() bool { return strings.Contains(logs.String(), "the task waits for its next attempt") })
	var line string
	for l := range strings.SplitSeq(logs.String(), "\n") {
		if strings.Contains(l, "the task waits for its next attempt") {
			line = l
		}
	}
	if !strings.Contains(line, `"level":"WARN"`) || strings.Contains(line, `"ticket"`) {
		t.Errorf("outcome line = %s, want WARN without a ticket", line)
	}
	if !strings.Contains(line, `"cause":"dial tcp db.internal:5432: connect: connection refused"`) {
		t.Errorf("outcome line = %s, want the storage failure's cause", line)
	}
}
