package entity

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// insertStormStore wraps a real spi.EntityStore but answers every Iterate
// with a fresh set of never-before-seen ids, simulating the one condition
// the streamed batched delete cannot shrink out of: entities matching the
// filter are created at least as fast as they are removed, so no cycle ever
// comes up empty. Every other call (Get, Delete) passes through to the real
// store, where these synthetic ids do not exist — each lands in IDToError
// and therefore in deleteBatched's `seen` set, exactly as a genuinely
// undeletable id would.
type insertStormStore struct {
	spi.EntityStore
	perCycle int
	opens    atomic.Int32
}

func (s *insertStormStore) Iterate(_ context.Context, _ spi.ModelRef, _ spi.Filter, _ spi.IterateOptions) (spi.Iterator, error) {
	s.opens.Add(1)
	ents := make([]*spi.Entity, 0, s.perCycle)
	for i := 0; i < s.perCycle; i++ {
		ents = append(ents, &spi.Entity{
			Meta: spi.EntityMeta{ID: uuid.NewString(), Version: 1},
			Data: []byte(`{}`),
		})
	}
	return &sliceIterator{ents: ents}, nil
}

// sliceIterator serves a fixed slice of entities as a spi.Iterator.
type sliceIterator struct {
	ents []*spi.Entity
	idx  int
}

func (it *sliceIterator) Next() bool {
	if it.idx >= len(it.ents) {
		return false
	}
	it.idx++
	return true
}
func (it *sliceIterator) Entity() *spi.Entity { return it.ents[it.idx-1] }
func (it *sliceIterator) Err() error          { return nil }
func (it *sliceIterator) Close() error        { return nil }

type insertStormFactory struct {
	spi.StoreFactory
	store *insertStormStore
}

func (f *insertStormFactory) EntityStore(_ context.Context) (spi.EntityStore, error) {
	return f.store, nil
}

// TestDeleteEntitiesConditional_Batched_Streamed_NonConvergenceFailsClosed
// pins the streamed branch's termination guarantee. Under sustained
// concurrent inserts the per-cycle re-scan never runs dry, so without a
// bound the request loops forever: MatchedCount and result.IDs grow without
// limit and the connection hangs until the client gives up. The only
// pre-existing backstop was ctx.Err() between cycles, which does nothing at
// all for a client that sent no deadline.
//
// The bound must fail CLOSED (.claude/rules/correctness-over-availability.md):
// a delete that could not be driven to completion is reported as an error,
// never as a successful DeleteResult whose counts describe only the part
// that happened to fit inside the budget.
//
// The test ctx carries a deadline purely so a regression fails in seconds
// instead of hanging CI — a deadline abort produces a DIFFERENT error, so
// the assertions below still distinguish the two outcomes.
func TestDeleteEntitiesConditional_Batched_Streamed_NonConvergenceFailsClosed(t *testing.T) {
	realFactory := memory.NewStoreFactory()
	t.Cleanup(func() { realFactory.Close() })

	ctx := newDeleteBatchedCtx(t, realFactory)

	realStore, err := realFactory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	storm := &insertStormStore{EntityStore: realStore, perCycle: 2}
	stormFactory := &insertStormFactory{StoreFactory: realFactory, store: storm}

	h := buildDeleteBatchedHandler(t, stormFactory, mustTxMgr(t, realFactory))
	h.maxDeleteCycles = 5

	deadlineCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	result, err := h.DeleteEntitiesConditional(deadlineCtx, "Person", "1", nil, nil, false, 2)
	if err == nil {
		t.Fatalf("DeleteEntitiesConditional returned a result (%+v) for a delete that never converged; "+
			"a partial pass reported as a complete one is the substituted answer the correctness rule forbids", result)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the request only stopped because the test's own deadline fired (%v); "+
			"there is no progress guard, so a client that sends no deadline hangs forever", err)
	}

	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("err = %v (%T), want a classified *common.AppError", err, err)
	}
	// The code is DELETE_NOT_CONVERGED, not the generic CONFLICT: CONFLICT is
	// entity-level optimistic concurrency (a version guard lost its race), which
	// an operator diagnoses and remedies differently from "the condition keeps
	// matching newly created entities".
	if appErr.Status != http.StatusConflict || appErr.Code != common.ErrCodeDeleteNotConverged {
		t.Errorf("err = %d/%s, want %d/%s", appErr.Status, appErr.Code, http.StatusConflict, common.ErrCodeDeleteNotConverged)
	}
	if !appErr.Retryable {
		t.Error("Retryable = false, want true (the condition clears once the concurrent writers stop)")
	}
	if appErr.Level != common.LevelOperational {
		t.Errorf("Level = %v, want LevelOperational (the message is actionable and client-safe)", appErr.Level)
	}
	if appErr.Message == "" || appErr.Err != nil {
		t.Errorf("err = %+v, want a self-contained actionable message and no internal cause attached", appErr)
	}

	if got := int(storm.opens.Load()); got > h.maxDeleteCycles+1 {
		t.Errorf("Iterate() opens = %d, want <= the cycle budget %d", got, h.maxDeleteCycles+1)
	}
}

// TestDeleteEntitiesConditional_Batched_Streamed_ConvergingDeleteIsUnbounded
// is the other half of the guard: a delete that DOES converge must not be
// tripped by the bound. 10 entities at batchSize 1 need 10 delete cycles
// plus a confirming one — comfortably inside a budget set just above that,
// and the request must still succeed with every entity removed.
func TestDeleteEntitiesConditional_Batched_Streamed_ConvergingDeleteIsUnbounded(t *testing.T) {
	realFactory := memory.NewStoreFactory()
	t.Cleanup(func() { realFactory.Close() })

	ctx := newDeleteBatchedCtx(t, realFactory)
	h := buildDeleteBatchedHandler(t, realFactory, mustTxMgr(t, realFactory))
	ids := seedPersons(t, h, ctx, 10)

	h.maxDeleteCycles = 12

	result, err := h.DeleteEntitiesConditional(ctx, "Person", "1", batchDeleteAgeGEZero, nil, false, 1)
	if err != nil {
		t.Fatalf("DeleteEntitiesConditional: %v", err)
	}
	if result.RemovedCount != 10 {
		t.Errorf("RemovedCount = %d, want 10", result.RemovedCount)
	}

	realStore, err := realFactory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	for _, id := range ids {
		if _, gErr := realStore.Get(ctx, id); !errors.Is(gErr, spi.ErrNotFound) {
			t.Errorf("entity %s still exists after the batched delete", id)
		}
	}
}
