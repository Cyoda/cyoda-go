package scheduler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// fakeStore scripts the scheduler's side of spi.ScheduledTaskStore. The
// embedded interface is left nil: a method no test scripts panics.
type fakeStore struct {
	spi.ScheduledTaskStore

	mu             sync.Mutex
	due            []spi.ScheduledTask
	claimErr       error // the claim happens, and its reply is lost
	claimReqs      []spi.ClaimRequest
	hbErr          error
	hbFailNext     int
	hbBlock        chan struct{}      // non-nil: Heartbeat parks until it is closed, whatever its ctx says
	heartbeatPanic bool               // Heartbeat panics, after it releases the lock
	claimPanic     bool               // ClaimDue panics
	claimBlock     chan struct{}      // non-nil: ClaimDue claims, then parks until it is closed
	claimedTokens  []uuid.UUID        // every claim token ClaimDue handed out
	settled        map[uuid.UUID]bool // claims whose outcome write was accepted
	givenBack      []uuid.UUID        // claims GiveBackIdle returned to WAITING
	outcomeBlock   chan struct{}      // non-nil: RecordAttempt holds its row open until it is closed
	open           map[uuid.UUID]bool // claims whose outcome write is open; GiveBackIdle skips them
	onGiveBack     func()             // called on every GiveBackIdle, outside the lock
	heldAtRetire   []uuid.UUID        // claims still RUNNING under this owner when RetireOwner ran
	heartbeats     int
	hbFailures     int
	giveBacks      [][]uuid.UUID
	retired        int
	retireErr      error
	sweptOwners    []time.Duration
	sweptMarks     int
	outcomeErrs    []error // returned in order by RecordAttempt and Fail
	outcomeAlways  error   // returned by every RecordAttempt and Fail once outcomeErrs is empty
	outcomePanic   bool
	outcomeCtxErrs []error
	attempts       []spi.Attempt
	fails          []spi.Failure
	failInTx       []bool
}

func (f *fakeStore) with(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

func (f *fakeStore) claims() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.claimReqs)
}

func (f *fakeStore) heartbeatCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.heartbeats
}

func (f *fakeStore) heartbeatFailures() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hbFailures
}

func (f *fakeStore) giveBackCalls() [][]uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.giveBacks)
}

func (f *fakeStore) lastGiveBack() ([]uuid.UUID, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.giveBacks) == 0 {
		return nil, false
	}
	return f.giveBacks[len(f.giveBacks)-1], true
}

func (f *fakeStore) attemptsRecorded() []spi.Attempt {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.attempts)
}

func (f *fakeStore) failsRecorded() []spi.Failure {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.fails)
}

func (f *fakeStore) ClaimDue(_ context.Context, req spi.ClaimRequest) ([]spi.ScheduledTask, error) {
	out, block, err := func() ([]spi.ScheduledTask, chan struct{}, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.claimReqs = append(f.claimReqs, req)
		if f.claimPanic {
			panic("injected panic in a claim")
		}
		n := min(req.Limit, len(f.due))
		out := make([]spi.ScheduledTask, 0, n)
		for _, t := range f.due[:n] {
			t.Status = spi.ScheduledTaskRunning
			t.Claim = &spi.TaskClaim{Token: uuid.New(), Owner: req.Owner}
			f.claimedTokens = append(f.claimedTokens, t.Claim.Token)
			out = append(out, t)
		}
		f.due = f.due[n:]
		return out, f.claimBlock, f.claimErr
	}()
	if block != nil {
		<-block
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (f *fakeStore) claimed() []uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.claimedTokens)
}

func (f *fakeStore) Heartbeat(context.Context, uuid.UUID) error {
	f.mu.Lock()
	f.heartbeats++
	block, err, boom := f.hbBlock, f.hbErr, f.heartbeatPanic
	if err == nil && f.hbFailNext > 0 {
		f.hbFailNext--
		err = errors.New("heartbeat: connection refused")
	}
	if err != nil {
		f.hbFailures++
	}
	f.mu.Unlock()
	if boom {
		panic("injected panic in a heartbeat")
	}
	if block != nil {
		<-block
	}
	return err
}

// held is every claim ClaimDue handed out that is still RUNNING: no outcome
// accepted, not given back. Called with f.mu held.
func (f *fakeStore) held() []uuid.UUID {
	var out []uuid.UUID
	for _, t := range f.claimedTokens {
		if !f.settled[t] && !slices.Contains(f.givenBack, t) {
			out = append(out, t)
		}
	}
	return out
}

// GiveBackIdle returns every held claim not in keep, and skips a row whose
// outcome write is open, as the stores do (SKIP LOCKED, busy).
func (f *fakeStore) GiveBackIdle(_ context.Context, _ uuid.UUID, keep []uuid.UUID) (int, error) {
	hook, n := func() (func(), int) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.giveBacks = append(f.giveBacks, slices.Clone(keep))
		n := 0
		for _, t := range f.held() {
			if !slices.Contains(keep, t) && !f.open[t] {
				f.givenBack = append(f.givenBack, t)
				n++
			}
		}
		return f.onGiveBack, n
	}()
	if hook != nil {
		hook()
	}
	return n, nil
}

func (f *fakeStore) givenBackTokens() []uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.givenBack)
}

func (f *fakeStore) RetireOwner(context.Context, uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.retired++
	f.heldAtRetire = f.held()
	return f.retireErr
}

func (f *fakeStore) SweepOwners(_ context.Context, deadFor time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sweptOwners = append(f.sweptOwners, deadFor)
	return nil
}

func (f *fakeStore) SweepMarks(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sweptMarks++
	return nil
}

// outcome is called with f.mu held.
func (f *fakeStore) outcome(ctx context.Context) error {
	f.outcomeCtxErrs = append(f.outcomeCtxErrs, ctx.Err())
	if f.outcomePanic {
		panic("injected panic while recording an outcome")
	}
	if len(f.outcomeErrs) > 0 {
		err := f.outcomeErrs[0]
		f.outcomeErrs = f.outcomeErrs[1:]
		return err
	}
	return f.outcomeAlways
}

func (f *fakeStore) RecordAttempt(ctx context.Context, ref spi.TaskRef, a spi.Attempt) error {
	block := func() chan struct{} {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.outcomeBlock != nil {
			f.setOpen(ref.ClaimToken, true)
		}
		return f.outcomeBlock
	}()
	if block != nil {
		<-block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setOpen(ref.ClaimToken, false)
	if err := f.outcome(ctx); err != nil {
		return err
	}
	f.attempts = append(f.attempts, a)
	f.settle(ref.ClaimToken)
	return nil
}

// setOpen and settle are called with f.mu held.
func (f *fakeStore) setOpen(token uuid.UUID, open bool) {
	if f.open == nil {
		f.open = make(map[uuid.UUID]bool)
	}
	f.open[token] = open
}

func (f *fakeStore) settle(token uuid.UUID) {
	if f.settled == nil {
		f.settled = make(map[uuid.UUID]bool)
	}
	f.settled[token] = true
}

func (f *fakeStore) Fail(ctx context.Context, ref spi.TaskRef, fl spi.Failure) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.outcome(ctx); err != nil {
		return err
	}
	f.settle(ref.ClaimToken)
	f.fails = append(f.fails, fl)
	f.failInTx = append(f.failInTx, spi.GetTransaction(ctx) != nil)
	return nil
}

// testFactory is the memory factory with the scripted task store.
type testFactory struct {
	spi.StoreFactory
	sts spi.ScheduledTaskStore
}

func (f testFactory) ScheduledTaskStore(context.Context) (spi.ScheduledTaskStore, error) {
	return f.sts, nil
}

type firerFunc func(ctx context.Context, task spi.ScheduledTask, maxLostOwners int, retryDelay time.Duration) workflow.RunReport

func (f firerFunc) FireScheduledTransition(ctx context.Context, task spi.ScheduledTask, maxLostOwners int, retryDelay time.Duration) workflow.RunReport {
	return f(ctx, task, maxLostOwners, retryDelay)
}

func reportFirer(r workflow.RunReport) Firer {
	return firerFunc(func(context.Context, spi.ScheduledTask, int, time.Duration) workflow.RunReport { return r })
}

// failedOnCancel is what the engine reports for a run stopped by its cancellation.
func failedOnCancel(ctx context.Context) workflow.RunReport {
	<-ctx.Done()
	return workflow.RunReport{Outcome: workflow.OutcomeFailed, Err: fmt.Errorf("run stopped: %w", ctx.Err())}
}

func testConfig() Config {
	return Config{
		Enabled:           true,
		ScanInterval:      5 * time.Millisecond,
		MaxRuns:           8,
		MaxRunsPerTenant:  4,
		HeartbeatInterval: 10 * time.Millisecond,
		StaleAfter:        time.Hour,
		MaxLostOwners:     3,
		RetryDelay:        30 * time.Second,
		RetryDelayMax:     15 * time.Minute,
		ShutdownDrain:     50 * time.Millisecond,
	}
}

type harness struct {
	svc  *Service
	fs   *fakeStore
	flag *atomic.Bool
	mem  *memory.StoreFactory
}

func newHarness(t *testing.T, cfg Config, firer Firer) *harness {
	t.Helper()
	mem := memory.NewStoreFactory()
	t.Cleanup(func() { _ = mem.Close() })
	fs := &fakeStore{}
	flag := &atomic.Bool{}
	flag.Store(true)
	svc := New(cfg, Deps{
		Store:              testFactory{StoreFactory: mem, sts: fs},
		TxManager:          mem.NewTransactionManager(common.NewTestUUIDGenerator()),
		Firer:              firer,
		Clock:              NewRealClock(),
		HealthFlag:         flag,
		CalloutDeadlineMax: time.Second,
	})
	// The test configs use a short STALE_AFTER that no watchdog window could be
	// derived from; tests that exercise the watchdog set their own.
	svc.window = time.Minute
	// Step 4 waits CommitBudget + 15s past the longest callout in production.
	svc.stepFourMargin = 100 * time.Millisecond
	// Step 5 waits one store-call budget for outcome writes under way, out
	// of the step-4 margin.
	svc.bookWait = 50 * time.Millisecond
	return &harness{svc: svc, fs: fs, flag: flag, mem: mem}
}

func (h *harness) start(t *testing.T) {
	t.Helper()
	if err := h.svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(h.svc.Stop)
}

func dueTask(tenant spi.TenantID, id string) spi.ScheduledTask {
	return spi.ScheduledTask{
		ID: id, TenantID: tenant, Type: spi.ScheduledTaskFireTransition, ScheduledTime: 1,
		EntityID: uuid.NewString(), ModelName: "M", ModelVersion: 1, Transition: "auto", SourceState: "OPEN",
		Status: spi.ScheduledTaskWaiting, ArmToken: uuid.New(), NextAttemptTime: 1,
	}
}

func liveRuns(s *Service) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.runs)
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting on a channel")
		var zero T
		return zero
	}
}

// syncBuffer is a log sink the service's goroutines may write while a test
// reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// captureLogs sends the default logger to a JSON buffer until the test ends.
func captureLogs(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}
