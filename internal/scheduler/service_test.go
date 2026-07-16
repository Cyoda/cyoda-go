package scheduler

import (
	"context"
	"sync"
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

// capturingExecutor records every task passed to Execute.
type capturingExecutor struct {
	mu    sync.Mutex
	tasks []spi.ScheduledTask
}

func (e *capturingExecutor) Execute(_ context.Context, task spi.ScheduledTask) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.tasks = append(e.tasks, task)
}

func (e *capturingExecutor) seen() []spi.ScheduledTask {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]spi.ScheduledTask, len(e.tasks))
	copy(out, e.tasks)
	return out
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

	exec := &capturingExecutor{}
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

	seen := exec.seen()
	if len(seen) != 1 {
		t.Fatalf("expected 1 dispatched task, got %d: %+v", len(seen), seen)
	}
	if seen[0].ID != "due-task" {
		t.Errorf("expected due-task dispatched, got %s", seen[0].ID)
	}

	// MarkRedispatch should have pushed due-task's RedispatchAfter into the
	// future, so a second tick at the same clock reading must not re-dispatch it.
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

	exec := &capturingExecutor{}
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

	exec := &capturingExecutor{}
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
		Executor:     &capturingExecutor{},
		SelfID:       "n1",
	})

	svc.Start()
	time.Sleep(5 * time.Millisecond)
	svc.Stop()
	// Stop must be idempotent/safe to call more than once.
	svc.Stop()
}
