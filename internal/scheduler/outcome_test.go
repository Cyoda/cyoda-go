package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
)

var safeFailure = workflow.RunReport{Outcome: workflow.OutcomeFailed, Err: errors.New("boom")}

const retryWarn = "scheduled run outcome not recorded; retrying"

// safeCallout is a failure whose lastError is client-safe: no ERROR line, no ticket.
var safeCallout = workflow.RunReport{Outcome: workflow.OutcomeFailed,
	Err: &contract.CalloutFailure{Kind: contract.MemberFailed, Message: "card declined"}}

// logRecords parses the JSON log lines whose msg is msg.
func logRecords(t *testing.T, out, msg string) []map[string]any {
	t.Helper()
	var recs []map[string]any
	for line := range strings.SplitSeq(out, "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if msg == "" || rec["msg"] == msg {
			recs = append(recs, rec)
		}
	}
	return recs
}

// withRetryMetric gives the service a meter and returns a reader of
// cyoda.scheduler.bookkeeping.retries. Call it before start.
func withRetryMetric(t *testing.T, h *harness) func() int64 {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	h.svc.deps.Meter = mp.Meter("test")
	return func() int64 {
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatalf("collect: %v", err)
		}
		var n int64
		for _, sm := range rm.ScopeMetrics {
			for _, md := range sm.Metrics {
				if sum, ok := md.Data.(metricdata.Sum[int64]); ok && md.Name == "cyoda.scheduler.bookkeeping.retries" {
					for _, dp := range sum.DataPoints {
						n += dp.Value
					}
				}
			}
		}
		return n
	}
}

// fixedClock is a pnode clock that never moves.
type fixedClock struct{ at time.Time }

func (c fixedClock) Now() time.Time { return c.at }

// flakyTx fails the first Commit without committing and records every
// transaction, commit and rollback.
type flakyTx struct {
	spi.TransactionManager

	mu         sync.Mutex
	begun      []string
	committed  []string
	rolledBack []string
	failed     bool
}

func (f *flakyTx) Begin(ctx context.Context) (string, context.Context, error) {
	id, txCtx, err := f.TransactionManager.Begin(ctx)
	if err == nil {
		f.mu.Lock()
		f.begun = append(f.begun, id)
		f.mu.Unlock()
	}
	return id, txCtx, err
}

func (f *flakyTx) Commit(ctx context.Context, txID string) error {
	f.mu.Lock()
	first := !f.failed
	f.failed = true
	f.mu.Unlock()
	if first {
		return errors.New("commit: connection reset by peer")
	}
	if err := f.TransactionManager.Commit(ctx, txID); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.committed = append(f.committed, txID)
	return nil
}

func (f *flakyTx) Rollback(ctx context.Context, txID string) error {
	f.mu.Lock()
	f.rolledBack = append(f.rolledBack, txID)
	f.mu.Unlock()
	return f.TransactionManager.Rollback(ctx, txID)
}

func (f *flakyTx) snapshot() (begun, committed, rolledBack []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.begun), slices.Clone(f.committed), slices.Clone(f.rolledBack)
}

// failEvents reads the SCHEDULED_TRANSITION_FAIL events of an entity.
func failEvents(t *testing.T, h *harness, tenant spi.TenantID, entityID string) []spi.StateMachineEvent {
	t.Helper()
	ctx := common.SystemUserContext(tenant)
	audit, err := h.mem.StateMachineAuditStore(ctx)
	if err != nil {
		t.Fatalf("audit store: %v", err)
	}
	events, err := audit.GetEvents(ctx, entityID)
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	var failed []spi.StateMachineEvent
	for _, e := range events {
		if e.EventType == spi.SMEventScheduledTransitionFailed {
			failed = append(failed, e)
		}
	}
	return failed
}

var unsafeFailure = workflow.RunReport{Outcome: workflow.OutcomeFailed, MarkHeld: true, UnsafeReached: true,
	Err: &contract.CalloutFailure{Kind: contract.NoAnswer, Message: "DISPATCH_TIMEOUT: no response"}}

func TestService_OutcomeWriteRetriedThroughAnOutage(t *testing.T) {
	logs := captureLogs(t)
	outage := errors.New("record attempt: dial tcp 10.0.0.5:5432: connect: connection refused")
	h := newHarness(t, testConfig(), reportFirer(safeFailure))
	retries := withRetryMetric(t, h)
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
		t.Errorf("%d retry lines for one task within a minute, want 1", n)
	}
	for _, rec := range logRecords(t, logs.String(), retryWarn) {
		if rec["level"] != "WARN" {
			t.Errorf("retry line level = %v, want WARN", rec["level"])
		}
	}
	if n := retries(); n != 2 {
		t.Errorf("cyoda.scheduler.bookkeeping.retries = %d, want 2, one per failed write", n)
	}
}

func TestService_OutcomeRetryBackoffIsCappedAtTheHeartbeat(t *testing.T) {
	outage := errors.New("record attempt: connection refused")
	h := newHarness(t, testConfig(), reportFirer(safeFailure))
	errs := make([]error, 10)
	for i := range errs {
		errs[i] = outage
	}
	h.fs.with(func() {
		h.fs.outcomeErrs = errs
		h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")}
	})
	start := time.Now()
	h.start(t)
	eventually(t, "the attempt accepted after ten failures", func() bool { return len(h.fs.attemptsRecorded()) == 1 })
	// Capped at the 10ms heartbeat the ten waits take about 100ms. Uncapped,
	// doubling from 10ms, they would take about 10s.
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("ten retries took %v: the backoff is not capped at the heartbeat interval", d)
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
	logs := captureLogs(t)
	tokens := make(chan uuid.UUID, 1)
	h := newHarness(t, testConfig(), firerFunc(func(_ context.Context, task spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
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
	var errorLines []map[string]any
	for _, rec := range logRecords(t, logs.String(), "") {
		if rec["level"] == "ERROR" {
			errorLines = append(errorLines, rec)
		}
	}
	if len(errorLines) != 1 {
		t.Fatalf("%d ERROR lines, want exactly one for the rejection: %v", len(errorLines), errorLines)
	}
	if ticket, _ := errorLines[0]["ticket"].(string); uuid.Validate(ticket) != nil {
		t.Errorf("the rejection's ERROR line carries no ticket: %v", errorLines[0])
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
	failed := logRecords(t, out, "scheduled task FAILED")
	if len(failed) != 1 {
		t.Fatalf("%d FAILED lines, want 1", len(failed))
	}
	if failed[0]["level"] != "ERROR" || failed[0]["ticket"] != ticket {
		t.Errorf("FAILED line = %v, want ERROR with the ticket lastError shows (%s)", failed[0], ticket)
	}
}

func TestService_FailedLineCarriesATicketWhenLastErrorHasNone(t *testing.T) {
	logs := captureLogs(t)
	h := newHarness(t, testConfig(), reportFirer(unsafeFailure))
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")} })
	h.start(t)
	eventually(t, "the FAILED line", func() bool { return strings.Contains(logs.String(), "scheduled task FAILED") })
	failed := logRecords(t, logs.String(), "scheduled task FAILED")
	if len(failed) != 1 {
		t.Fatalf("%d FAILED lines, want 1", len(failed))
	}
	rec := failed[0]
	if ticket, _ := rec["ticket"].(string); rec["level"] != "ERROR" || uuid.Validate(ticket) != nil ||
		rec["reason"] != string(spi.FailureUnsafeWorkNotCompleted) {
		t.Errorf("FAILED line = %v, want ERROR with its reason and a ticket", rec)
	}
}

func TestService_FailedCommitRollsBackAndTheRetryCommitsTheEventOnce(t *testing.T) {
	task := dueTask("t1", "task-1")
	h := newHarness(t, testConfig(), reportFirer(unsafeFailure))
	tx := &flakyTx{TransactionManager: h.svc.deps.TxManager}
	h.svc.deps.TxManager = tx
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{task} })
	h.start(t)
	// The fake's Fail is not transactional: it lands on both attempts.
	eventually(t, "two Fail writes", func() bool { return len(h.fs.failsRecorded()) == 2 })
	eventually(t, "the run released", func() bool { return liveRuns(h.svc) == 0 })

	begun, committed, rolledBack := tx.snapshot()
	if len(begun) != 2 || len(committed) != 1 || committed[0] != begun[1] {
		t.Fatalf("begun %v, committed %v: want two transactions, the second committed", begun, committed)
	}
	if !slices.Contains(rolledBack, begun[0]) {
		t.Errorf("rolled back %v: the transaction whose commit failed was not rolled back", rolledBack)
	}
	failed := failEvents(t, h, task.TenantID, task.EntityID)
	if len(failed) != 1 {
		t.Fatalf("%d SCHEDULED_TRANSITION_FAIL events, want exactly one", len(failed))
	}
	if failed[0].TransactionID != committed[0] {
		t.Errorf("event transaction = %q, want the committed one %q", failed[0].TransactionID, committed[0])
	}
}

func TestService_FailEventCarriesTheEntityStateAndTheTaskCounters(t *testing.T) {
	at := time.Date(2031, 4, 5, 6, 7, 8, 9_000_000, time.UTC)
	task := dueTask("t1", "task-1")
	task.Attempts, task.LostOwners = 2, 1
	h := newHarness(t, testConfig(), reportFirer(unsafeFailure))
	h.svc.deps.Clock = fixedClock{at: at}

	ctx := common.SystemUserContext(task.TenantID)
	entities, err := h.mem.EntityStore(ctx)
	if err != nil {
		t.Fatalf("entity store: %v", err)
	}
	if _, err := entities.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: task.EntityID, TenantID: task.TenantID, State: "REVIEW",
			ModelRef: spi.ModelRef{EntityName: task.ModelName, ModelVersion: "1"}},
		Data: []byte(`{}`),
	}); err != nil {
		t.Fatalf("save entity: %v", err)
	}
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{task} })
	h.start(t)
	eventually(t, "the task failed", func() bool { return len(h.fs.failsRecorded()) == 1 })
	eventually(t, "the run released", func() bool { return liveRuns(h.svc) == 0 })

	failed := failEvents(t, h, task.TenantID, task.EntityID)
	if len(failed) != 1 {
		t.Fatalf("%d SCHEDULED_TRANSITION_FAIL events, want exactly one", len(failed))
	}
	e := failed[0]
	if e.State != "REVIEW" {
		t.Errorf("event state = %q, want the entity's state REVIEW, not the source state", e.State)
	}
	if got := fmt.Sprint(e.Data["attempts"], "/", e.Data["lostOwners"]); got != "2/1" {
		t.Errorf("event attempts/lostOwners = %s, want 2/1", got)
	}
	// The store stamps the transaction's events with its commit instant; the
	// time the scheduler submits does not survive.
	submit, err := h.svc.deps.TxManager.GetSubmitTime(ctx, e.TransactionID)
	if err != nil {
		t.Fatalf("GetSubmitTime: %v", err)
	}
	if !e.Timestamp.Equal(submit) || e.Timestamp.Equal(time.UnixMilli(at.UnixMilli())) {
		t.Errorf("event timestamp = %v, want the commit instant %v of its transaction", e.Timestamp, submit)
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
