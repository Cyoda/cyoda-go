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
