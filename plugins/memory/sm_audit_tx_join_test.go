package memory_test

import (
	"context"
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func auditStore(t *testing.T, fx taskFixture, ctx context.Context) spi.StateMachineAuditStore {
	t.Helper()
	as, err := fx.f.StateMachineAuditStore(ctx)
	if err != nil {
		t.Fatalf("StateMachineAuditStore: %v", err)
	}
	return as
}

func eventsOf(t *testing.T, as spi.StateMachineAuditStore, ctx context.Context) []spi.StateMachineEvent {
	t.Helper()
	got, err := as.GetEvents(ctx, "e1")
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	return got
}

func TestAudit_ARecordInARolledBackTransactionIsDiscarded(t *testing.T) {
	fx := newTaskFixture(t)
	ctx := ctxWithTenant(taskTenantA)
	txID, txCtx := fx.begin(t, taskTenantA)
	as := auditStore(t, fx, txCtx)
	if err := as.Record(txCtx, "e1", spi.StateMachineEvent{
		EventType: spi.SMEventTransitionMade, TransactionID: txID, State: "S", Details: "in tx", Timestamp: fx.clock.Now(),
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if got := eventsOf(t, as, txCtx); len(got) != 1 || got[0].TimeUUID == "" {
		t.Fatalf("inside the transaction: %+v, want its own event with an id", got)
	}
	if got := eventsOf(t, as, ctx); len(got) != 0 {
		t.Fatalf("outside the transaction before commit: %d events, want 0", len(got))
	}
	fx.rollback(t, taskTenantA, txID)
	if got := eventsOf(t, as, ctx); len(got) != 0 {
		t.Fatalf("after rollback: %d events, want 0", len(got))
	}
}

func TestAudit_ARecordInACommittedTransactionIsKeptAtTheCommitInstant(t *testing.T) {
	fx := newTaskFixture(t)
	ctx := ctxWithTenant(taskTenantA)
	txID, txCtx := fx.begin(t, taskTenantA)
	as := auditStore(t, fx, txCtx)
	if err := as.Record(txCtx, "e1", spi.StateMachineEvent{
		EventType: spi.SMEventTransitionMade, TransactionID: txID, State: "S", Details: "in tx", Timestamp: fx.clock.Now(),
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := fx.commit(taskTenantA, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	submit, err := fx.tm.GetSubmitTime(ctx, txID)
	if err != nil {
		t.Fatalf("GetSubmitTime: %v", err)
	}
	got := eventsOf(t, as, ctx)
	if len(got) != 1 || !got[0].Timestamp.Equal(submit) {
		t.Fatalf("after commit: %+v, want one event at the commit instant %s", got, submit)
	}
}

func TestAudit_ARecordAfterARolledBackSavepointIsDropped(t *testing.T) {
	fx := newTaskFixture(t)
	ctx := ctxWithTenant(taskTenantA)
	txID, txCtx := fx.begin(t, taskTenantA)
	as := auditStore(t, fx, txCtx)
	record := func(details string) {
		t.Helper()
		if err := as.Record(txCtx, "e1", spi.StateMachineEvent{
			EventType: spi.SMEventTransitionMade, TransactionID: txID, Details: details, Timestamp: fx.clock.Now(),
		}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	record("kept")
	spID, err := fx.tm.Savepoint(ctx, txID)
	if err != nil {
		t.Fatalf("Savepoint: %v", err)
	}
	record("dropped")
	if err := fx.tm.RollbackToSavepoint(ctx, txID, spID); err != nil {
		t.Fatalf("RollbackToSavepoint: %v", err)
	}
	if err := fx.commit(taskTenantA, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if got := eventsOf(t, as, ctx); len(got) != 1 || got[0].Details != "kept" {
		t.Fatalf("after commit: %+v, want only the event recorded before the savepoint", got)
	}
}

// An event whose JSON form cannot be written is refused on every backend:
// the SQL stores marshal it, and the marshal fails the same way every time.
func TestAudit_AnEventThatCannotBeWrittenIsAStoreRejection(t *testing.T) {
	fx := newTaskFixture(t)
	ctx := ctxWithTenant(taskTenantA)
	as := auditStore(t, fx, ctx)
	err := as.Record(ctx, "e1", spi.StateMachineEvent{
		EventType: spi.SMEventTransitionMade, Details: "bad", Data: map[string]any{"c": make(chan int)},
	})
	if !errors.Is(err, spi.ErrStoreRejected) {
		t.Fatalf("Record = %v, want ErrStoreRejected", err)
	}
}

func TestAudit_RecordOnARolledBackTransactionCtxIsRefused(t *testing.T) {
	fx := newTaskFixture(t)
	txID, txCtx := fx.begin(t, taskTenantA)
	fx.rollback(t, taskTenantA, txID)
	as := auditStore(t, fx, txCtx)
	err := as.Record(txCtx, "e1", spi.StateMachineEvent{
		EventType: spi.SMEventTransitionMade, TransactionID: txID, Timestamp: fx.clock.Now(),
	})
	if !errors.Is(err, spi.ErrTxRolledBack) {
		t.Fatalf("Record on a rolled-back transaction's ctx = %v, want ErrTxRolledBack", err)
	}
}

func TestAudit_RecordOnAnAlreadyCommittedTransactionCtxIsRefused(t *testing.T) {
	fx := newTaskFixture(t)
	txID, txCtx := fx.begin(t, taskTenantA)
	if err := fx.commit(taskTenantA, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	as := auditStore(t, fx, txCtx)
	err := as.Record(txCtx, "e1", spi.StateMachineEvent{
		EventType: spi.SMEventTransitionMade, TransactionID: txID, Timestamp: fx.clock.Now(),
	})
	if !errors.Is(err, spi.ErrTxAlreadyCommitted) {
		t.Fatalf("Record on an already-committed transaction's ctx = %v, want ErrTxAlreadyCommitted", err)
	}
}

func TestAudit_RecordWithATransactionOfAnotherTenantIsRefused(t *testing.T) {
	fx := newTaskFixture(t)
	_, txCtx := fx.begin(t, taskTenantA)
	// asB is scoped to a different tenant than the transaction on txCtx.
	asB := auditStore(t, fx, ctxWithTenant(taskTenantB))
	err := asB.Record(txCtx, "e1", spi.StateMachineEvent{
		EventType: spi.SMEventTransitionMade, Timestamp: fx.clock.Now(),
	})
	if !errors.Is(err, spi.ErrTxTenantMismatch) {
		t.Fatalf("Record with a cross-tenant transaction = %v, want ErrTxTenantMismatch", err)
	}
}

// A read through the same transaction that recorded the event, via
// GetEventsByTransaction rather than GetEvents, must also see the staged
// event before commit and not see it from outside the transaction.
func TestAudit_GetEventsByTransactionSeesStagedEventsInTheTransaction(t *testing.T) {
	fx := newTaskFixture(t)
	ctx := ctxWithTenant(taskTenantA)
	txID, txCtx := fx.begin(t, taskTenantA)
	as := auditStore(t, fx, txCtx)
	if err := as.Record(txCtx, "e1", spi.StateMachineEvent{
		EventType: spi.SMEventTransitionMade, TransactionID: txID, Details: "in tx", Timestamp: fx.clock.Now(),
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	inTx, err := as.GetEventsByTransaction(txCtx, "e1", txID)
	if err != nil {
		t.Fatalf("GetEventsByTransaction (in tx): %v", err)
	}
	if len(inTx) != 1 || inTx[0].Details != "in tx" {
		t.Fatalf("GetEventsByTransaction(txCtx, ..., txID) = %+v, want the one staged event", inTx)
	}

	outside, err := as.GetEventsByTransaction(ctx, "e1", txID)
	if err != nil {
		t.Fatalf("GetEventsByTransaction (outside tx, before commit): %v", err)
	}
	if len(outside) != 0 {
		t.Fatalf("GetEventsByTransaction(ctx, ..., txID) before commit = %+v, want 0", outside)
	}
}
