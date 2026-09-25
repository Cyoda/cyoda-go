package app

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
	"github.com/cyoda-platform/cyoda-go/internal/scheduler"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
	"github.com/google/uuid"
)

// teardownLog records, in order, the scheduler retiring its liveness record
// and the store factory closing.
type teardownLog struct {
	mu     sync.Mutex
	events []string
}

func (l *teardownLog) add(e string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func (l *teardownLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.events)
}

type retireRecordingTasks struct {
	spi.ScheduledTaskStore
	log *teardownLog
}

func (s retireRecordingTasks) RetireOwner(ctx context.Context, owner uuid.UUID) error {
	s.log.add("retire")
	return s.ScheduledTaskStore.RetireOwner(ctx, owner)
}

type orderRecordingFactory struct {
	*memory.StoreFactory
	log *teardownLog
}

func (f orderRecordingFactory) ScheduledTaskStore(ctx context.Context) (spi.ScheduledTaskStore, error) {
	st, err := f.StoreFactory.ScheduledTaskStore(ctx)
	if err != nil {
		return nil, err
	}
	return retireRecordingTasks{ScheduledTaskStore: st, log: f.log}, nil
}

func (f orderRecordingFactory) Close() error {
	f.log.add("close")
	return f.StoreFactory.Close()
}

type idleFirer struct{}

func (idleFirer) FireScheduledTransition(context.Context, spi.ScheduledTask, int, time.Duration) workflow.RunReport {
	return workflow.RunReport{}
}

// startedSchedulerApp builds an App holding only a running scheduler over a
// store that records the teardown order.
func startedSchedulerApp(t *testing.T) (*App, *teardownLog) {
	t.Helper()
	log := &teardownLog{}
	mem := memory.NewStoreFactory()
	f := orderRecordingFactory{StoreFactory: mem, log: log}
	svc := scheduler.New(scheduler.Config(defaultSchedulerConfig()), scheduler.Deps{
		Store:     f,
		TxManager: mem.NewTransactionManager(common.NewTestUUIDGenerator()),
		Firer:     idleFirer{},
		Clock:     scheduler.NewRealClock(),
	})
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("scheduler Start: %v", err)
	}
	return &App{storeFactory: f, scheduler: svc}, log
}

// Close without Shutdown must not leave the scheduler running against a
// store that is closing: the scheduler drains, and retires its liveness
// record, before the store factory closes.
func TestApp_CloseDrainsTheSchedulerBeforeTheStoreCloses(t *testing.T) {
	a, log := startedSchedulerApp(t)
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got, want := log.snapshot(), []string{"retire", "close"}; !slices.Equal(got, want) {
		t.Fatalf("teardown order = %v, want %v", got, want)
	}
}

// Shutdown then Close, the binary's sequence: the scheduler drains once, in
// Shutdown, and Close's drain does nothing.
func TestApp_ShutdownThenCloseDrainsTheSchedulerOnce(t *testing.T) {
	a, log := startedSchedulerApp(t)
	a.Shutdown()
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got, want := log.snapshot(), []string{"retire", "close"}; !slices.Equal(got, want) {
		t.Fatalf("teardown order = %v, want %v", got, want)
	}
}
