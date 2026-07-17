package scheduler

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// --- fakes ---

// fakeClock is a Clock whose Now() is fixed until the test explicitly
// advances it, so scan-loop tests are deterministic instead of depending on
// wall-clock ticker firings.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(now time.Time) *fakeClock {
	return &fakeClock{now: now}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// set updates the fake clock's current time, letting a test advance "now"
// deterministically between ticks (e.g. to cross a RedispatchBackoff window)
// without any wall-clock sleeping.
func (c *fakeClock) set(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

// fakeRegistry returns a fixed member list.
type fakeRegistry struct {
	members []contract.NodeInfo
}

func (r *fakeRegistry) Register(_ context.Context, _ string, _ string) error { return nil }
func (r *fakeRegistry) Lookup(_ context.Context, _ string) (string, bool, error) {
	return "", false, nil
}
func (r *fakeRegistry) List(_ context.Context) ([]contract.NodeInfo, error) {
	return r.members, nil
}
func (r *fakeRegistry) Deregister(_ context.Context, _ string) error { return nil }

// capturingExecutor records every (task, target) pair passed to Execute.
// Since tick() now dispatches Execute from its own goroutine, Execute may be
// called concurrently, so the recorded slices are mutex-guarded, and calls
// are additionally signalled on a buffered channel so tests can
// deterministically wait for N dispatches instead of guessing with
// time.Sleep.
type capturingExecutor struct {
	mu      sync.Mutex
	tasks   []spi.ScheduledTask
	targets []string
	notify  chan struct{}
}

func newCapturingExecutor() *capturingExecutor {
	return &capturingExecutor{notify: make(chan struct{}, 256)}
}

func (e *capturingExecutor) Execute(_ context.Context, task spi.ScheduledTask, target string) {
	e.mu.Lock()
	e.tasks = append(e.tasks, task)
	e.targets = append(e.targets, target)
	e.mu.Unlock()
	e.notify <- struct{}{}
}

func (e *capturingExecutor) seen() []spi.ScheduledTask {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]spi.ScheduledTask, len(e.tasks))
	copy(out, e.tasks)
	return out
}

func (e *capturingExecutor) seenTargets() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.targets))
	copy(out, e.targets)
	return out
}

// waitForDispatches blocks until n Execute calls have been observed (via the
// notify channel), failing the test if that doesn't happen within timeout.
// This replaces sleep-and-hope polling: since dispatch now happens on
// goroutines spawned by tick(), tests must synchronize on the dispatch
// actually having occurred rather than assuming tick() returning means
// Execute has run.
func (e *capturingExecutor) waitForDispatches(t *testing.T, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for i := 0; i < n; i++ {
		select {
		case <-e.notify:
		case <-deadline:
			t.Fatalf("timed out waiting for dispatch %d/%d", i+1, n)
		}
	}
}

// blockingExecutor is an Executor whose Execute blocks until the test signals
// unblock, and records each call's arrival on started before blocking. Used
// to prove tick() does not wait for Execute to finish.
type blockingExecutor struct {
	started chan struct{}
	unblock chan struct{}
	mu      sync.Mutex
	tasks   []spi.ScheduledTask
	targets []string
}

func newBlockingExecutor() *blockingExecutor {
	return &blockingExecutor{
		started: make(chan struct{}, 256),
		unblock: make(chan struct{}),
	}
}

func (e *blockingExecutor) Execute(_ context.Context, task spi.ScheduledTask, target string) {
	e.mu.Lock()
	e.tasks = append(e.tasks, task)
	e.targets = append(e.targets, target)
	e.mu.Unlock()
	e.started <- struct{}{}
	<-e.unblock
}

func (e *blockingExecutor) seen() []spi.ScheduledTask {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]spi.ScheduledTask, len(e.tasks))
	copy(out, e.tasks)
	return out
}

// countingRegistry wraps fakeRegistry and counts List calls, so tests can
// infer how many ticks a running scan loop has performed without depending
// on wall-clock sleeps to line up with dispatch outcomes.
type countingRegistry struct {
	fakeRegistry
	calls *int32
}

func (r *countingRegistry) List(ctx context.Context) ([]contract.NodeInfo, error) {
	atomic.AddInt32(r.calls, 1)
	return r.fakeRegistry.List(ctx)
}

// --- test helpers ---

func armTestTask(t *testing.T, factory spi.StoreFactory, id string, scheduledTime int64) {
	t.Helper()
	ctx := context.Background()
	sts, err := factory.ScheduledTaskStore(ctx)
	if err != nil {
		t.Fatalf("ScheduledTaskStore: %v", err)
	}
	task := spi.ScheduledTask{
		ID:            id,
		TenantID:      "tenant-1",
		Type:          spi.ScheduledTaskFireTransition,
		ScheduledTime: scheduledTime,
		EntityID:      "entity-1",
		ModelName:     "TestModel",
		ModelVersion:  1,
		Transition:    "auto-close",
		SourceState:   "OPEN",
	}
	if err := sts.Upsert(ctx, task); err != nil {
		t.Fatalf("Upsert task %s: %v", id, err)
	}
}

// --- tests ---

func TestService_ScansAndDispatchesDueTasks(t *testing.T) {
	factory := memory.NewStoreFactory()
	clock := newFakeClock(time.UnixMilli(10_000))

	armTestTask(t, factory, "due-task", 9_000)     // due: scheduledTime <= now
	armTestTask(t, factory, "future-task", 20_000) // not due yet

	exec := newCapturingExecutor()
	svc := NewService(Config{
		Enabled:           true,
		ScanInterval:      time.Hour, // irrelevant — test calls tick() directly
		RedispatchBackoff: 5 * time.Minute,
		BatchSize:         10,
	}, Deps{
		Store:        factory,
		Registry:     &fakeRegistry{members: []contract.NodeInfo{{NodeID: "n1"}}},
		Coordinator:  LowestLiveNodeID{},
		Distribution: Self{},
		Clock:        clock,
		Executor:     exec,
		SelfID:       "n1",
	})

	svc.tick()
	// tick() only hands due tasks off to goroutines; wait for the dispatch to
	// actually land before asserting on it.
	exec.waitForDispatches(t, 1, 2*time.Second)

	seen := exec.seen()
	if len(seen) != 1 {
		t.Fatalf("expected 1 dispatched task, got %d: %+v", len(seen), seen)
	}
	if seen[0].ID != "due-task" {
		t.Errorf("expected due-task dispatched, got %s", seen[0].ID)
	}

	targets := exec.seenTargets()
	if len(targets) != 1 || targets[0] != "n1" {
		t.Errorf("expected Execute to receive the Distribution.Pick target %q, got %+v", "n1", targets)
	}

	// MarkRedispatch should have pushed due-task's RedispatchAfter into the
	// future, so a second tick at the same clock reading must not re-dispatch it.
	// Nothing is dispatched on this tick, so no async goroutine is spawned —
	// exec.seen() is stable to read immediately.
	svc.tick()

	seen = exec.seen()
	if len(seen) != 1 {
		t.Fatalf("expected still only 1 dispatched task after second tick (redispatch throttle), got %d: %+v", len(seen), seen)
	}
}

func TestService_NonCoordinatorDoesNothing(t *testing.T) {
	factory := memory.NewStoreFactory()
	clock := newFakeClock(time.UnixMilli(10_000))

	armTestTask(t, factory, "due-task", 9_000)

	exec := newCapturingExecutor()
	svc := NewService(Config{
		Enabled:           true,
		ScanInterval:      time.Hour,
		RedispatchBackoff: 5 * time.Minute,
		BatchSize:         10,
	}, Deps{
		Store:    factory,
		Registry: &fakeRegistry{members: []contract.NodeInfo{{NodeID: "n1"}, {NodeID: "n2"}}},
		// selfID "n2" is not the min of {n1, n2} → not coordinator.
		Coordinator:  LowestLiveNodeID{},
		Distribution: Self{},
		Clock:        clock,
		Executor:     exec,
		SelfID:       "n2",
	})

	svc.tick()

	if seen := exec.seen(); len(seen) != 0 {
		t.Fatalf("non-coordinator should not dispatch, got %+v", seen)
	}
}

func TestService_DisabledDoesNothing(t *testing.T) {
	factory := memory.NewStoreFactory()
	clock := newFakeClock(time.UnixMilli(10_000))

	armTestTask(t, factory, "due-task", 9_000)

	exec := newCapturingExecutor()
	svc := NewService(Config{
		Enabled:           false,
		ScanInterval:      time.Hour,
		RedispatchBackoff: 5 * time.Minute,
		BatchSize:         10,
	}, Deps{
		Store:        factory,
		Registry:     &fakeRegistry{members: []contract.NodeInfo{{NodeID: "n1"}}},
		Coordinator:  LowestLiveNodeID{},
		Distribution: Self{},
		Clock:        clock,
		Executor:     exec,
		SelfID:       "n1",
	})

	svc.tick()

	if seen := exec.seen(); len(seen) != 0 {
		t.Fatalf("disabled service should not dispatch, got %+v", seen)
	}
}

func TestService_StartStop(t *testing.T) {
	factory := memory.NewStoreFactory()
	svc := NewService(Config{
		Enabled:           true,
		ScanInterval:      time.Millisecond,
		RedispatchBackoff: time.Minute,
		BatchSize:         10,
	}, Deps{
		Store:        factory,
		Registry:     &fakeRegistry{members: []contract.NodeInfo{{NodeID: "n1"}}},
		Coordinator:  LowestLiveNodeID{},
		Distribution: Self{},
		Clock:        NewRealClock(),
		Executor:     newCapturingExecutor(),
		SelfID:       "n1",
	})

	svc.Start()
	time.Sleep(5 * time.Millisecond)
	svc.Stop()
	// Stop must be idempotent/safe to call more than once.
	svc.Stop()
}

// TestService_SlowExecuteDoesNotBlockTick proves the Important finding #2
// fix structurally: tick() must hand every due task off to Execute and
// return without waiting for any of them to finish, even when Execute blocks
// indefinitely (e.g. a slow local processor run or a slow peer RPC).
func TestService_SlowExecuteDoesNotBlockTick(t *testing.T) {
	factory := memory.NewStoreFactory()
	clock := newFakeClock(time.UnixMilli(10_000))

	armTestTask(t, factory, "due-task-1", 9_000)
	armTestTask(t, factory, "due-task-2", 9_500)

	exec := newBlockingExecutor()
	svc := NewService(Config{
		Enabled:           true,
		ScanInterval:      time.Hour,
		RedispatchBackoff: 5 * time.Minute,
		BatchSize:         10,
	}, Deps{
		Store:        factory,
		Registry:     &fakeRegistry{members: []contract.NodeInfo{{NodeID: "n1"}}},
		Coordinator:  LowestLiveNodeID{},
		Distribution: Self{},
		Clock:        clock,
		Executor:     exec,
		SelfID:       "n1",
	})

	tickDone := make(chan struct{})
	go func() {
		svc.tick()
		close(tickDone)
	}()

	// tick() must return well before either blocked Execute call is
	// unblocked — that's the whole claim under test.
	select {
	case <-tickDone:
	case <-time.After(2 * time.Second):
		t.Fatal("tick() did not return promptly — a slow Execute is blocking the scan loop")
	}

	// Both dispatches must have been handed off (Execute entered and is
	// blocked on e.unblock) even though tick() already returned.
	for i := 0; i < 2; i++ {
		select {
		case <-exec.started:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for dispatch %d/2 to start after tick() returned", i+1)
		}
	}

	if got := len(exec.seen()); got != 2 {
		t.Fatalf("expected both due tasks dispatched, got %d: %+v", got, exec.seen())
	}

	// Drain: unblock both Execute calls so the test doesn't leak goroutines.
	close(exec.unblock)
}

// TestService_StartIsIdempotent guards against a second Start() call
// spawning a duplicate scan-loop goroutine, which would double the tick (and
// therefore dispatch) rate. It counts Registry.List calls — made exactly
// once per tick — over a short fixed window and asserts the count stays
// within a single loop's expected range.
func TestService_StartIsIdempotent(t *testing.T) {
	factory := memory.NewStoreFactory()

	var listCalls int32
	registry := &countingRegistry{
		fakeRegistry: fakeRegistry{members: []contract.NodeInfo{{NodeID: "n1"}}},
		calls:        &listCalls,
	}

	svc := NewService(Config{
		Enabled:           true,
		ScanInterval:      2 * time.Millisecond,
		RedispatchBackoff: time.Minute,
		BatchSize:         10,
	}, Deps{
		Store:        factory,
		Registry:     registry,
		Coordinator:  LowestLiveNodeID{},
		Distribution: Self{},
		Clock:        NewRealClock(),
		Executor:     newCapturingExecutor(),
		SelfID:       "n1",
	})

	svc.Start()
	svc.Start() // must be a no-op: only the first Start may spawn a loop
	svc.Start()

	time.Sleep(50 * time.Millisecond)
	svc.Stop()

	got := atomic.LoadInt32(&listCalls)
	if got == 0 {
		t.Fatal("expected at least one tick to have run in 50ms at a 2ms interval")
	}
	// A single 2ms-interval loop produces roughly 25 ticks in 50ms. Three
	// unguarded concurrent loops would produce roughly 3x that (~75). 60 sits
	// well clear of the single-loop case (generous margin for a loaded CI
	// runner) while still catching a duplicate-loop regression.
	const maxSingleLoopTicks = 60
	if got > maxSingleLoopTicks {
		t.Fatalf("Registry.List called %d times in ~50ms at a 2ms interval after 3 Start() calls — looks like more than one scan loop is running (want <= %d)", got, maxSingleLoopTicks)
	}
}

// TestService_DeadWorkerRedispatchAfterBackoffElapses proves the
// at-least-once failover property end-to-end (design §6.1/§6.3): a task
// dispatched to a "dead worker" — one that records the dispatch but never
// resolves the task (never fires it, never deletes the row, exactly what a
// worker that crashes mid-flight leaves behind) — is NOT re-dispatched while
// MarkRedispatch's throttle window is still open, but IS re-dispatched once
// the clock passes redispatchAfter. That re-dispatch is the property under
// test: a task a dead worker never completed is not lost, it is retried once
// its backoff lapses.
//
// capturingExecutor (already used by TestService_ScansAndDispatchesDueTasks)
// stands in for the dead worker unmodified: its Execute only records the
// call, it never touches the store, so the task row survives exactly as a
// crashed worker would leave it.
func TestService_DeadWorkerRedispatchAfterBackoffElapses(t *testing.T) {
	factory := memory.NewStoreFactory()
	const start = int64(10_000)
	const backoff = 5 * time.Minute
	clock := newFakeClock(time.UnixMilli(start))

	armTestTask(t, factory, "dead-worker-task", 9_000) // due

	exec := newCapturingExecutor()
	svc := NewService(Config{
		Enabled:           true,
		ScanInterval:      time.Hour, // irrelevant — test calls tick() directly
		RedispatchBackoff: backoff,
		BatchSize:         10,
	}, Deps{
		Store:        factory,
		Registry:     &fakeRegistry{members: []contract.NodeInfo{{NodeID: "n1"}}},
		Coordinator:  LowestLiveNodeID{},
		Distribution: Self{},
		Clock:        clock,
		Executor:     exec,
		SelfID:       "n1",
	})

	// tick 1: the due task is dispatched to the dead worker.
	svc.tick()
	exec.waitForDispatches(t, 1, 2*time.Second)
	if seen := exec.seen(); len(seen) != 1 || seen[0].ID != "dead-worker-task" {
		t.Fatalf("expected dead-worker-task dispatched once, got %+v", seen)
	}

	// The task row is still present — the dead worker never resolved it.
	sts, err := factory.ScheduledTaskStore(context.Background())
	if err != nil {
		t.Fatalf("ScheduledTaskStore: %v", err)
	}
	if _, found, err := sts.Get(context.Background(), "dead-worker-task"); err != nil || !found {
		t.Fatalf("expected task row still present after dead-worker dispatch, found=%v err=%v", found, err)
	}

	// tick 2: clock is unchanged (still inside the backoff window) —
	// MarkRedispatch's throttle must hold, so no re-dispatch.
	svc.tick()
	if seen := exec.seen(); len(seen) != 1 {
		t.Fatalf("expected still only 1 dispatch inside the backoff window, got %d: %+v", len(seen), seen)
	}

	// Advance the clock past redispatchAfter (start + backoff) — the
	// failover window has lapsed.
	clock.set(time.UnixMilli(start).Add(backoff + time.Millisecond))

	// tick 3: the dead worker's task is picked up again.
	svc.tick()
	exec.waitForDispatches(t, 1, 2*time.Second)

	seen := exec.seen()
	if len(seen) != 2 {
		t.Fatalf("expected the dead worker's task re-dispatched once backoff elapsed, got %d dispatches: %+v", len(seen), seen)
	}
	if seen[1].ID != "dead-worker-task" {
		t.Errorf("re-dispatched task ID = %q, want dead-worker-task", seen[1].ID)
	}
}
