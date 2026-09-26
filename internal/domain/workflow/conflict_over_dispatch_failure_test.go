package workflow

import (
	"context"
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// TestConflictOverDispatchFailure_HealthyTxKeepsProcessorFailure is
// conflictOverDispatchFailure's second documented outcome: the anchor re-read
// succeeds — the transaction is healthy, not aborted — so the probe finds no
// spi.ErrTxAborted and the processor's own failure stands unchanged. Not a
// conflict: errors.Is(err, spi.ErrConflict) must be false, and the door-level
// classification (common.TxAbortedConflict, the same check
// classifyWorkflowError applies first) must agree that this is not one.
func TestConflictOverDispatchFailure_HealthyTxKeepsProcessorFailure(t *testing.T) {
	_, err, _, _, _ := calloutModeEngine(t, ExecutionModeSync, nil, memberFailed("card declined", true))
	if err == nil {
		t.Fatal("expected the operation to fail")
	}
	if errors.Is(err, spi.ErrConflict) {
		t.Fatalf("error = %v, must not be a conflict: the re-read found a healthy transaction", err)
	}
	var failure *contract.CalloutFailure
	if !errors.As(err, &failure) || failure.Kind != contract.MemberFailed || failure.Retryable == nil || !*failure.Retryable {
		t.Fatalf("failure = %+v, want the processor's own MemberFailed failure, unchanged", failure)
	}
	// The door's own classification agrees: TxAbortedConflict only promotes an
	// error to 409 CONFLICT when it wraps spi.ErrTxAborted, and this one
	// doesn't — a healthy re-read leaves the door's classification exactly as
	// it would have been had conflictOverDispatchFailure never probed at all.
	if appErr := common.TxAbortedConflict(err); appErr != nil {
		t.Errorf("TxAbortedConflict(err) = %+v, want nil: a healthy re-read is not a door-level conflict", appErr)
	}
}

// erroringEntityStore wraps a real spi.EntityStore and makes every Get fail
// with a plain store error — neither spi.ErrNotFound nor spi.ErrTxAborted —
// so a caller's re-read meets neither outcome readAnchor treats specially.
type erroringEntityStore struct {
	spi.EntityStore
	getErr error
}

func (s *erroringEntityStore) Get(context.Context, string) (*spi.Entity, error) {
	return nil, s.getErr
}

// erroringStoreFactory wraps a real spi.StoreFactory and swaps in an
// erroringEntityStore for EntityStore(ctx); every other store is the real one.
type erroringStoreFactory struct {
	spi.StoreFactory
	getErr error
}

func (f *erroringStoreFactory) EntityStore(ctx context.Context) (spi.EntityStore, error) {
	es, err := f.StoreFactory.EntityStore(ctx)
	if err != nil {
		return nil, err
	}
	return &erroringEntityStore{EntityStore: es, getErr: f.getErr}, nil
}

// TestConflictOverDispatchFailure_StoreErrorOnReReadKeepsProcessorFailure is
// conflictOverDispatchFailure's other documented "any other probe outcome"
// case: the anchor re-read itself fails, with a non-conflict error (a plain
// store error, not spi.ErrTxAborted). The doc comment says any such outcome
// "keeps the processor's failure as it is" and the code returns dispatchErr
// untouched — so the returned error must be dispatchErr itself, not wrapped
// around the store error and not replaced by it.
func TestConflictOverDispatchFailure_StoreErrorOnReReadKeepsProcessorFailure(t *testing.T) {
	inner := memory.NewStoreFactory()
	t.Cleanup(func() { inner.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := inner.NewTransactionManager(uuids)

	storeErr := errors.New("store: connection refused")
	factory := &erroringStoreFactory{StoreFactory: inner, getErr: storeErr}
	engine := NewEngine(factory, uuids, txMgr)

	ctx := ctxWithTenant(testTenant)
	txID, txCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	t.Cleanup(func() { _ = txMgr.Rollback(txCtx, txID) })

	dispatchErr := memberFailed("card declined", true)
	proc := spi.ProcessorDefinition{Name: "charge"}

	got := engine.conflictOverDispatchFailure(txCtx, "some-entity", proc, dispatchErr)

	if got != error(dispatchErr) {
		t.Fatalf("conflictOverDispatchFailure = %v, want dispatchErr (%v) unchanged: a store error on the re-read is not spi.ErrTxAborted", got, dispatchErr)
	}
	if errors.Is(got, spi.ErrConflict) {
		t.Fatalf("error = %v, must not be a conflict: the re-read failed with a plain store error, not spi.ErrTxAborted", got)
	}
	if appErr := common.TxAbortedConflict(got); appErr != nil {
		t.Errorf("TxAbortedConflict(err) = %+v, want nil: a store error on the re-read is not a door-level conflict", appErr)
	}
}
