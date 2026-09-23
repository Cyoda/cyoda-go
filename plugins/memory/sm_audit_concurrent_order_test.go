package memory_test

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// pausingUUIDGenerator wraps a delegate spi.UUIDGenerator and blocks the
// Nth call to NewTimeUUID (1-indexed) until the test signals resume — so a
// test can pin exactly when one goroutine's mint has happened relative to
// another's, without depending on race-detector timing.
type pausingUUIDGenerator struct {
	delegate spi.UUIDGenerator
	pauseOn  int64
	n        atomic.Int64
	paused   chan struct{}
	resume   chan struct{}
}

func newPausingUUIDGenerator(delegate spi.UUIDGenerator, pauseOn int64) *pausingUUIDGenerator {
	return &pausingUUIDGenerator{
		delegate: delegate, pauseOn: pauseOn,
		paused: make(chan struct{}), resume: make(chan struct{}),
	}
}

func (g *pausingUUIDGenerator) NewTimeUUID() [16]byte {
	id := g.delegate.NewTimeUUID()
	if g.n.Add(1) == g.pauseOn {
		close(g.paused)
		<-g.resume
	}
	return id
}

// TestSMAudit_ConcurrentRecord_IDOrderMatchesAppendOrder pins that minting an
// event's id and appending it to the audit trail are one atomic step: two
// concurrent Records for the same entity must produce ids in the same order
// as the events they label appear in GetEvents, never the reverse.
//
// Minting the id before taking smAuditMu lets a second, unblocked Record
// mint AND append while a first, delayed Record is still holding its
// (already-minted) id and waiting for the lock — so the first-minted id can
// be appended SECOND. Pinned deterministically via a UUID generator that
// pauses one goroutine's mint call, rather than relying on -race timing.
func TestSMAudit_ConcurrentRecord_IDOrderMatchesAppendOrder(t *testing.T) {
	gen := newPausingUUIDGenerator(newTestUUIDGenerator(), 1) // pause the FIRST mint call
	f := memory.NewStoreFactory()
	defer func() { _ = f.Close() }()
	f.NewTransactionManager(gen)
	ctx := ctxWithTenant("tenant-audit-order")
	store, err := f.StateMachineAuditStore(ctx)
	if err != nil {
		t.Fatalf("StateMachineAuditStore: %v", err)
	}

	var wg sync.WaitGroup
	var errA, errB error

	wg.Add(1)
	go func() {
		defer wg.Done()
		errA = store.Record(ctx, "e-1", spi.StateMachineEvent{
			EventType: spi.SMEventStarted, EntityID: "e-1", Details: "A",
		})
	}()

	<-gen.paused // goroutine A has minted (call #1) and is now blocked

	wg.Add(1)
	go func() {
		defer wg.Done()
		errB = store.Record(ctx, "e-1", spi.StateMachineEvent{
			EventType: spi.SMEventStarted, EntityID: "e-1", Details: "B",
		})
	}()

	// Best-effort scheduling headroom so B runs as far as it can before A is
	// released: on unfixed code B mints (call #2) and appends fully; on
	// fixed code B cannot get past smAuditMu.Lock (A holds it), so this
	// sleep cannot manufacture a false pass — it only makes the bug
	// reliably reproducible, it never masks it.
	time.Sleep(20 * time.Millisecond)
	close(gen.resume) // release A

	wg.Wait()
	if errA != nil {
		t.Fatalf("Record A: %v", errA)
	}
	if errB != nil {
		t.Fatalf("Record B: %v", errB)
	}

	events, err := store.GetEvents(ctx, "e-1")
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].TimeUUID >= events[1].TimeUUID {
		t.Errorf("append order %q, %q does not match id (mint) order — the id minted first "+
			"(by goroutine A, call #1) must be the id appended first", events[0].TimeUUID, events[1].TimeUUID)
	}
}
