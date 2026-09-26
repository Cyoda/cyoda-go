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
// conflictOverDispatchFailure's second documented outcome: the transaction has
// lost no write race, so the processor's own failure stands unchanged. Not a
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
	// doesn't.
	if appErr := common.TxAbortedConflict(err); appErr != nil {
		t.Errorf("TxAbortedConflict(err) = %+v, want nil: a transaction that lost no race is not a door-level conflict", appErr)
	}
}

// lostRaceTx begins a memory transaction that writes entity "target" after a
// rival committed it: the transaction has lost the race, but on memory the
// write was only buffered, so every read in the transaction still succeeds.
func lostRaceTx(t *testing.T) (*Engine, spi.TransactionManager, context.Context) {
	t.Helper()
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)
	engine := NewEngine(factory, uuids, txMgr)

	ctx := ctxWithTenant(testTenant)
	txID, txCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	t.Cleanup(func() { _ = txMgr.Rollback(txCtx, txID) })

	target := func(v string) *spi.Entity {
		return &spi.Entity{
			Meta: spi.EntityMeta{ID: "target", TenantID: testTenant, ModelRef: spi.ModelRef{EntityName: "m", ModelVersion: "1"}, State: "S"},
			Data: []byte(`{"v":"` + v + `"}`),
		}
	}
	rivalID, rivalCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin rival: %v", err)
	}
	rivalES, err := factory.EntityStore(rivalCtx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	if _, err := rivalES.Save(rivalCtx, target("rival")); err != nil {
		t.Fatalf("rival Save: %v", err)
	}
	if err := txMgr.Commit(rivalCtx, rivalID); err != nil {
		t.Fatalf("rival Commit: %v", err)
	}

	es, err := factory.EntityStore(txCtx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	if _, err := es.Save(txCtx, target("mine")); err != nil {
		t.Fatalf("Save: %v (memory buffers the losing write)", err)
	}
	if _, err := es.Get(txCtx, "target"); err != nil {
		t.Fatalf("Get: %v (memory reads the transaction's buffer; the loss shows only at commit)", err)
	}
	return engine, txMgr, txCtx
}

// TestConflictOverDispatchFailure_LostRaceIsConflictOnEveryBackend: a
// processor fails inside a transaction that has lost a write race on a backend
// that detects conflicts at commit. Every read still succeeds there, yet the
// transaction cannot commit: the answer is the conflict (spi.ErrTxAborted, so
// every door answers a retryable 409), with the processor's failure attached.
func TestConflictOverDispatchFailure_LostRaceIsConflictOnEveryBackend(t *testing.T) {
	engine, _, txCtx := lostRaceTx(t)
	dispatchErr := memberFailed("card declined", false)

	got := engine.conflictOverDispatchFailure(txCtx, spi.ProcessorDefinition{Name: "charge"}, dispatchErr)

	if !errors.Is(got, spi.ErrTxAborted) || !errors.Is(got, spi.ErrConflict) {
		t.Fatalf("error = %v, want the transaction's conflict (spi.ErrTxAborted)", got)
	}
	var failure *contract.CalloutFailure
	if !errors.As(got, &failure) || failure != dispatchErr {
		t.Fatalf("the processor's failure must stay attached; got %v", got)
	}
	if appErr := common.TxAbortedConflict(got); appErr == nil || !appErr.Retryable {
		t.Errorf("TxAbortedConflict(err) = %+v, want a retryable 409", appErr)
	}
}

// TestConflictOverDispatchFailure_NoHandOffIsUntouched: a dispatch that
// provably reached no compute node made no callback, so it is returned as it
// is even inside a transaction that has lost a race.
func TestConflictOverDispatchFailure_NoHandOffIsUntouched(t *testing.T) {
	engine, _, txCtx := lostRaceTx(t)
	dispatchErr := &contract.NoHandOffProof{Err: errors.New("no compute member")}

	got := engine.conflictOverDispatchFailure(txCtx, spi.ProcessorDefinition{Name: "charge"}, dispatchErr)

	if got != error(dispatchErr) {
		t.Fatalf("conflictOverDispatchFailure = %v, want dispatchErr unchanged", got)
	}
}

// lostRaceErrTxManager answers LostRace with an error.
type lostRaceErrTxManager struct {
	spi.TransactionManager
	err error
}

func (m lostRaceErrTxManager) LostRace(context.Context, string) (bool, error) { return false, m.err }

// TestConflictOverDispatchFailure_UnansweredLostRaceKeepsProcessorFailure: when
// the transaction manager cannot say whether the transaction lost a race, the
// processor's failure stands as it is. The operation fails either way.
func TestConflictOverDispatchFailure_UnansweredLostRaceKeepsProcessorFailure(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	inner := factory.NewTransactionManager(uuids)
	engine := NewEngine(factory, uuids, lostRaceErrTxManager{TransactionManager: inner, err: spi.ErrTxNotFound})

	txID, txCtx, err := inner.Begin(ctxWithTenant(testTenant))
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	t.Cleanup(func() { _ = inner.Rollback(txCtx, txID) })
	dispatchErr := memberFailed("card declined", true)

	got := engine.conflictOverDispatchFailure(txCtx, spi.ProcessorDefinition{Name: "charge"}, dispatchErr)

	if got != error(dispatchErr) {
		t.Fatalf("conflictOverDispatchFailure = %v, want dispatchErr unchanged", got)
	}
}
