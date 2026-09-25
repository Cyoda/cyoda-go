package sqlite_test

import (
	"context"
	"errors"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// auditConflictEntity builds a minimal entity for the abort-path regression
// tests below: only its id and model matter, its data is irrelevant except
// where a test declares unique keys over it.
func auditConflictEntity(id, model string, data []byte) *spi.Entity {
	if data == nil {
		data = []byte(`{}`)
	}
	return &spi.Entity{
		Meta: spi.EntityMeta{ID: id, ModelRef: spi.ModelRef{EntityName: model, ModelVersion: "1"}},
		Data: data,
	}
}

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
	ctx := tenantCtx(taskTenantA)
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
	ctx := tenantCtx(taskTenantA)
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
	ctx := tenantCtx(taskTenantA)
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
	ctx := tenantCtx(taskTenantA)
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
	asB := auditStore(t, fx, tenantCtx(taskTenantB))
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
	ctx := tenantCtx(taskTenantA)
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

// A staged event's Timestamp is its recording time — read by whichever
// process called Record, before the transaction committed. A committed
// event's timestamp is a commit instant. The two are different clocks and
// must never be compared: GetEvents appends staged events after committed
// ones, unsorted, exactly as the memory plugin does (see withStaged). A
// staged event whose recording time is earlier than an already-committed
// event's commit instant must still come after it.
func TestAudit_StagedEventsAppearAfterCommittedRegardlessOfTimestamp(t *testing.T) {
	fx := newTaskFixture(t)
	ctx := tenantCtx(taskTenantA)
	as := auditStore(t, fx, ctx)

	committedTxID, committedCtx := fx.begin(t, taskTenantA)
	if err := as.Record(committedCtx, "e1", spi.StateMachineEvent{
		EventType: spi.SMEventTransitionMade, TransactionID: committedTxID, Details: "committed", Timestamp: fx.clock.Now(),
	}); err != nil {
		t.Fatalf("Record (committed): %v", err)
	}
	if err := fx.commit(taskTenantA, committedTxID); err != nil {
		t.Fatalf("Commit (committed): %v", err)
	}

	stagedTxID, stagedCtx := fx.begin(t, taskTenantA)
	early := fx.clock.Now().Add(-time.Hour) // before the committed event's commit instant
	if err := as.Record(stagedCtx, "e1", spi.StateMachineEvent{
		EventType: spi.SMEventTransitionMade, TransactionID: stagedTxID, Details: "staged", Timestamp: early,
	}); err != nil {
		t.Fatalf("Record (staged): %v", err)
	}

	got := eventsOf(t, as, stagedCtx)
	if len(got) != 2 || got[0].Details != "committed" || got[1].Details != "staged" {
		t.Fatalf("GetEvents mixed the two clocks: got %+v, want [committed, staged] regardless of timestamp", got)
	}
}

// A conflict abort: two transactions overlap on the same entity: the first
// commits, the second's conflict check (Commit's step 3, before flush ever
// runs) finds the first's write in its read/write set and returns
// spi.ErrConflict via the m.forgetLocked(txID) path in the conflict-detection
// closure. The losing transaction's staged audit event must not survive.
func TestAudit_AConflictAbortDropsStagedEvents(t *testing.T) {
	fx := newTaskFixture(t)
	ctx := tenantCtx(taskTenantA)
	as := auditStore(t, fx, ctx)

	aID, aCtx := fx.begin(t, taskTenantA)
	bID, bCtx := fx.begin(t, taskTenantA) // same snapshot as a: begun before a commits

	esA, err := fx.f.EntityStore(aCtx)
	if err != nil {
		t.Fatalf("EntityStore (a): %v", err)
	}
	if _, err := esA.Save(aCtx, auditConflictEntity("e1", "AuditConflictModel", nil)); err != nil {
		t.Fatalf("Save (a): %v", err)
	}
	if err := fx.commit(taskTenantA, aID); err != nil {
		t.Fatalf("Commit (a): %v", err)
	}

	esB, err := fx.f.EntityStore(bCtx)
	if err != nil {
		t.Fatalf("EntityStore (b): %v", err)
	}
	if _, err := esB.Save(bCtx, auditConflictEntity("e1", "AuditConflictModel", nil)); err != nil {
		t.Fatalf("Save (b): %v", err)
	}
	if err := as.Record(bCtx, "e1", spi.StateMachineEvent{
		EventType: spi.SMEventTransitionMade, TransactionID: bID, Details: "must be dropped", Timestamp: fx.clock.Now(),
	}); err != nil {
		t.Fatalf("Record (b): %v", err)
	}

	if err := fx.commit(taskTenantA, bID); !errors.Is(err, spi.ErrConflict) {
		t.Fatalf("Commit (b) = %v, want ErrConflict", err)
	}

	if got := eventsOf(t, as, ctx); len(got) != 0 {
		t.Fatalf("after a conflict abort: %d events, want 0", len(got))
	}
	// Query through bCtx too — the transaction the event was staged on.
	// A query through ctx (no transaction attached) can never see auditOps
	// at all regardless of cleanup, so it alone would not catch a leaked
	// (never-forgotten) entry; bCtx exercises stagedAuditEvents(bID) directly.
	if got := eventsOf(t, as, bCtx); len(got) != 0 {
		t.Fatalf("after a conflict abort, queried through the aborted tx's own ctx: %d events, want 0 (auditOps for bID must be forgotten)", len(got))
	}
}

// A unique-violation abort: two transactions save distinct entities that
// share a declared unique-key value. The conflict check passes (different
// entity ids, no read/write-set overlap); the collision surfaces only at
// flush, when the second transaction's claim insert hits the DB-level
// UNIQUE constraint. flushToSQLite returns an error, and Commit's
// flush-failure branch (not the conflict-detection closure) runs
// m.forgetLocked(txID). The losing transaction's staged audit event must not
// survive this path either.
func TestAudit_AUniqueViolationAbortDropsStagedEvents(t *testing.T) {
	fx := newTaskFixture(t)
	ctx := tenantCtx(taskTenantA)
	as := auditStore(t, fx, ctx)
	keys := []spi.UniqueKey{{ID: "audit-uv-key", Fields: []string{"$.v"}}}

	aID, aCtx := fx.begin(t, taskTenantA)
	esA, err := fx.f.EntityStore(aCtx)
	if err != nil {
		t.Fatalf("EntityStore (a): %v", err)
	}
	if _, err := esA.Save(spi.WithUniqueKeys(aCtx, keys), auditConflictEntity("e1", "AuditUVModel", []byte(`{"v":"dup"}`))); err != nil {
		t.Fatalf("Save (a): %v", err)
	}
	if err := fx.commit(taskTenantA, aID); err != nil {
		t.Fatalf("Commit (a): %v", err)
	}

	bID, bCtx := fx.begin(t, taskTenantA)
	esB, err := fx.f.EntityStore(bCtx)
	if err != nil {
		t.Fatalf("EntityStore (b): %v", err)
	}
	if _, err := esB.Save(spi.WithUniqueKeys(bCtx, keys), auditConflictEntity("e2", "AuditUVModel", []byte(`{"v":"dup"}`))); err != nil {
		t.Fatalf("Save (b): %v", err)
	}
	if err := as.Record(bCtx, "e2", spi.StateMachineEvent{
		EventType: spi.SMEventTransitionMade, TransactionID: bID, Details: "must be dropped", Timestamp: fx.clock.Now(),
	}); err != nil {
		t.Fatalf("Record (b): %v", err)
	}

	if err := fx.commit(taskTenantA, bID); !errors.Is(err, spi.ErrUniqueViolation) {
		t.Fatalf("Commit (b) = %v, want ErrUniqueViolation", err)
	}

	got, err := as.GetEvents(ctx, "e2")
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("after a unique-violation abort: %d events, want 0", len(got))
	}
	// Query through bCtx too — see the matching comment in
	// TestAudit_AConflictAbortDropsStagedEvents for why ctx alone cannot
	// catch a leaked (never-forgotten) auditOps entry.
	got, err = as.GetEvents(bCtx, "e2")
	if err != nil {
		t.Fatalf("GetEvents (bCtx): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("after a unique-violation abort, queried through the aborted tx's own ctx: %d events, want 0 (auditOps for bID must be forgotten)", len(got))
	}
}

// A transaction aborted by the conflict-detection closure (not the
// flush-failure branch) must be left with RolledBack=true, exactly as the
// flush-failure branch already leaves it. Record checks RolledBack before
// Closed (see spi.TransactionState's field docs), so a later join on the
// same ctx must answer ErrTxRolledBack, never ErrTxAlreadyCommitted — and an
// explicit Rollback on the same, already-forgotten transaction must not
// surface as an unexpected failure.
func TestAudit_RecordAfterAConflictAbortIsRefusedAsRolledBack(t *testing.T) {
	fx := newTaskFixture(t)
	ctx := tenantCtx(taskTenantA)

	aID, aCtx := fx.begin(t, taskTenantA)
	bID, bCtx := fx.begin(t, taskTenantA)

	esA, err := fx.f.EntityStore(aCtx)
	if err != nil {
		t.Fatalf("EntityStore (a): %v", err)
	}
	if _, err := esA.Save(aCtx, auditConflictEntity("e2", "AuditConflictModel2", nil)); err != nil {
		t.Fatalf("Save (a): %v", err)
	}
	if err := fx.commit(taskTenantA, aID); err != nil {
		t.Fatalf("Commit (a): %v", err)
	}

	esB, err := fx.f.EntityStore(bCtx)
	if err != nil {
		t.Fatalf("EntityStore (b): %v", err)
	}
	if _, err := esB.Save(bCtx, auditConflictEntity("e2", "AuditConflictModel2", nil)); err != nil {
		t.Fatalf("Save (b): %v", err)
	}

	if err := fx.commit(taskTenantA, bID); !errors.Is(err, spi.ErrConflict) {
		t.Fatalf("Commit (b) = %v, want ErrConflict", err)
	}

	as := auditStore(t, fx, bCtx)
	err = as.Record(bCtx, "e2", spi.StateMachineEvent{EventType: spi.SMEventTransitionMade, Timestamp: fx.clock.Now()})
	if !errors.Is(err, spi.ErrTxRolledBack) {
		t.Fatalf("Record after a conflict abort = %v, want ErrTxRolledBack", err)
	}
	if errors.Is(err, spi.ErrTxAlreadyCommitted) {
		t.Fatalf("Record after a conflict abort must not also read as ErrTxAlreadyCommitted: %v", err)
	}

	if err := fx.tm.Rollback(ctx, bID); err != nil && !errors.Is(err, spi.ErrTxNotFound) {
		t.Fatalf("Rollback after a conflict abort = %v, want nil or ErrTxNotFound", err)
	}
}
