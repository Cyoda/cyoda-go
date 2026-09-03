package entity

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// nestedGetGuardStore wraps a real spi.EntityStore and records whether
// EntityStore.Get was ever called while one of its own iterators was still
// open.
//
// That window is the defect this fixture exists to pin: on postgres, Iterate
// holds a pooled connection for the iterator's whole lifetime and Get acquires
// a second one, so a Get issued from inside the selection drain is
// hold-and-wait — MaxConns concurrent point-in-time batched deletes wedge the
// pool until every request context expires. The engine must therefore finish
// (and close) the selection scan before it starts reading the version-guard
// baselines.
type nestedGetGuardStore struct {
	spi.EntityStore
	open           atomic.Int32
	gets           atomic.Int32
	getsWhileOpen  atomic.Int32
	iterateOpened  atomic.Int32
	maxOpenObserve atomic.Int32
}

func (s *nestedGetGuardStore) Iterate(ctx context.Context, model spi.ModelRef, filter spi.Filter, opts spi.IterateOptions) (spi.Iterator, error) {
	it, err := s.EntityStore.Iterate(ctx, model, filter, opts)
	if err != nil {
		return nil, err
	}
	s.iterateOpened.Add(1)
	if n := s.open.Add(1); n > s.maxOpenObserve.Load() {
		s.maxOpenObserve.Store(n)
	}
	return &nestedGetGuardIterator{Iterator: it, store: s}, nil
}

func (s *nestedGetGuardStore) Get(ctx context.Context, id string) (*spi.Entity, error) {
	s.gets.Add(1)
	if s.open.Load() > 0 {
		s.getsWhileOpen.Add(1)
	}
	return s.EntityStore.Get(ctx, id)
}

type nestedGetGuardIterator struct {
	spi.Iterator
	store  *nestedGetGuardStore
	closed bool
}

func (it *nestedGetGuardIterator) Close() error {
	err := it.Iterator.Close()
	if !it.closed {
		it.closed = true
		it.store.open.Add(-1)
	}
	return err
}

type nestedGetGuardFactory struct {
	spi.StoreFactory
	store *nestedGetGuardStore
}

func (f *nestedGetGuardFactory) EntityStore(_ context.Context) (spi.EntityStore, error) {
	return f.store, nil
}

// TestDeleteEntitiesConditional_Batched_PointInTime_NoGetInsideOpenIterator
// pins that resolveBatchTargetsOnePass reads each match's version-guard
// baseline AFTER the selection iterator has closed, never from inside the
// drain's visit callback. See nestedGetGuardStore's doc comment for why a
// nested acquire is a pool-wedging hold-and-wait on postgres.
//
// The functional assertions (MatchedCount, RemovedCount, empty IDToError) are
// repeated here deliberately: moving the Get out of the drain must not change
// what the pass resolves, only when it reads it.
func TestDeleteEntitiesConditional_Batched_PointInTime_NoGetInsideOpenIterator(t *testing.T) {
	realFactory := memory.NewStoreFactory()
	t.Cleanup(func() { realFactory.Close() })

	ctx := newDeleteBatchedCtx(t, realFactory)
	hSeed := buildDeleteBatchedHandler(t, realFactory, mustTxMgr(t, realFactory))
	ids := seedPersons(t, hSeed, ctx, 8)

	pit := time.Now()

	realStore, err := realFactory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	guard := &nestedGetGuardStore{EntityStore: realStore}
	guardFactory := &nestedGetGuardFactory{StoreFactory: realFactory, store: guard}

	hDelete := buildDeleteBatchedHandler(t, guardFactory, mustTxMgr(t, realFactory))

	result, err := hDelete.DeleteEntitiesConditional(ctx, "Person", "1", batchDeleteAgeGEZero, &pit, false, 3)
	if err != nil {
		t.Fatalf("DeleteEntitiesConditional: %v", err)
	}

	if guard.iterateOpened.Load() != 1 {
		t.Errorf("Iterate() opens = %d, want exactly 1 (pointInTime selection resolves in a single pass)", guard.iterateOpened.Load())
	}
	if got := guard.getsWhileOpen.Load(); got != 0 {
		t.Errorf("Get() calls issued while a selection iterator was still open = %d, want 0 "+
			"(a nested acquire holds the iterator's connection and waits for a second one — see nestedGetGuardStore)", got)
	}
	if guard.gets.Load() == 0 {
		t.Error("Get() was never called; the version-guard baselines must still be read, just not inside the drain")
	}

	if result.MatchedCount != 8 {
		t.Errorf("MatchedCount = %d, want 8", result.MatchedCount)
	}
	if result.RemovedCount != 8 {
		t.Errorf("RemovedCount = %d, want 8", result.RemovedCount)
	}
	if len(result.IDToError) != 0 {
		t.Errorf("IDToError = %v, want empty", result.IDToError)
	}
	for _, id := range ids {
		if _, gErr := realStore.Get(ctx, id); !errors.Is(gErr, spi.ErrNotFound) {
			t.Errorf("entity %s still exists after the pointInTime batched delete", id)
		}
	}
}

// TestResolveBatchTargetsOnePass_PerIDGetFailureIsolated pins the per-id
// failure semantics the deferred-Get rewrite must preserve: a Get that fails
// for one matched id (here: the entity was hard-deleted between the as-at
// match and the baseline read) lands in IDToError for that id alone, is
// excluded from the returned targets, and does NOT fail the whole resolution
// pass — while MatchedCount still counts it, because the as-at scan did match
// it.
func TestResolveBatchTargetsOnePass_PerIDGetFailureIsolated(t *testing.T) {
	realFactory := memory.NewStoreFactory()
	t.Cleanup(func() { realFactory.Close() })

	ctx := newDeleteBatchedCtx(t, realFactory)
	hSeed := buildDeleteBatchedHandler(t, realFactory, mustTxMgr(t, realFactory))
	ids := seedPersons(t, hSeed, ctx, 4)

	pit := time.Now()

	realStore, err := realFactory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	// Hard-remove one id AFTER the point in time: the as-at scan still
	// matches it, the current-row baseline read cannot find it.
	if _, err := hSeed.DeleteEntity(ctx, ids[1]); err != nil {
		t.Fatalf("DeleteEntity: %v", err)
	}

	guard := &nestedGetGuardStore{EntityStore: realStore}
	guardFactory := &nestedGetGuardFactory{StoreFactory: realFactory, store: guard}
	hDelete := buildDeleteBatchedHandler(t, guardFactory, mustTxMgr(t, realFactory))

	result, err := hDelete.DeleteEntitiesConditional(ctx, "Person", "1", batchDeleteAgeGEZero, &pit, true, 2)
	if err != nil {
		t.Fatalf("DeleteEntitiesConditional: %v", err)
	}

	if result.MatchedCount != 4 {
		t.Errorf("MatchedCount = %d, want 4 (the as-at scan matched every seeded id)", result.MatchedCount)
	}
	if len(result.IDs) != 4 {
		t.Errorf("verbose IDs = %v, want all 4 matched ids", result.IDs)
	}
	if _, ok := result.IDToError[ids[1]]; !ok {
		t.Errorf("IDToError missing the already-removed id %s: %v", ids[1], result.IDToError)
	}
	if result.RemovedCount != 3 {
		t.Errorf("RemovedCount = %d, want 3", result.RemovedCount)
	}
	if got := guard.getsWhileOpen.Load(); got != 0 {
		t.Errorf("Get() calls issued while a selection iterator was still open = %d, want 0", got)
	}
}
