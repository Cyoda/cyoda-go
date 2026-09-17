package memory

import (
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// auditTxIndexSize reports how many transaction entries smAuditTxIndex holds
// for tenant, read under the mutex that guards it.
func auditTxIndexSize(f *StoreFactory, tenant spi.TenantID) int {
	f.smAuditMu.RLock()
	defer f.smAuditMu.RUnlock()
	return len(f.smAuditTxIndex[tenant])
}

// TestSMAuditTxIndex_PrunedOnCommit pins the lifetime of smAuditTxIndex: its
// only reader is the commit-phase stamp, which runs once, so a transaction's
// entry is dead the moment that stamp has run. Left in place it accumulates
// one reference per audited event for the life of the process, with nothing
// that can ever read it again — every sibling map staged per transaction
// (supersededSaves, deletedBufferModels, scheduledTaskOps) is deleted on this
// same path.
//
// The events themselves must survive the prune: it drops the index, not the
// audit trail.
func TestSMAuditTxIndex_PrunedOnCommit(t *testing.T) {
	factory := NewStoreFactory()
	defer func() { _ = factory.Close() }()
	tm := factory.NewTransactionManager(newTestUUIDGenerator())
	const tenant spi.TenantID = "tenant-audit-prune-commit"
	ctx := testCtxWithTenant(tenant)

	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	audit, err := factory.StateMachineAuditStore(ctx)
	if err != nil {
		t.Fatalf("StateMachineAuditStore: %v", err)
	}
	ref := spi.ModelRef{EntityName: "audit-prune", ModelVersion: "1"}

	// A clock no running process would produce, so a surviving unstamped value
	// is unmistakable.
	sentinel := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := store.Save(txCtx, &spi.Entity{
		Meta: spi.EntityMeta{ID: "e-1", ModelRef: ref}, Data: []byte(`{"n":1}`),
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := audit.Record(txCtx, "e-1", spi.StateMachineEvent{
		EventType: spi.SMEventStarted, EntityID: "e-1", TimeUUID: "u-1",
		TransactionID: txID, Details: "in tx", Timestamp: sentinel,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	if got := auditTxIndexSize(factory, tenant); got != 1 {
		t.Fatalf("before commit: smAuditTxIndex holds %d transactions, want 1 — "+
			"the assertion below cannot discriminate if the event was never indexed", got)
	}

	if err := tm.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	if got := auditTxIndexSize(factory, tenant); got != 0 {
		t.Errorf("after commit: smAuditTxIndex still holds %d transactions, want 0 — "+
			"the commit-phase stamp is its only reader and has already run, so these "+
			"references can never be read again and accumulate for the life of the process", got)
	}

	// The prune drops the index, never the audit trail.
	submit, err := tm.GetSubmitTime(ctx, txID)
	if err != nil {
		t.Fatalf("GetSubmitTime: %v", err)
	}
	events, err := audit.GetEvents(ctx, "e-1")
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected the recorded event to survive the prune, got %d events", len(events))
	}
	if !events[0].Timestamp.Equal(submit) {
		t.Errorf("recorded event carries %s, want the commit instant %s — the prune must run "+
			"after the stamp, not instead of it", events[0].Timestamp, submit)
	}
}

// TestSMAuditTxIndex_PrunedOnRollback pins the other exit. A rolled-back
// transaction is never stamped, so its index entry has no reader at all —
// and Rollback is where every sibling staged map is discarded.
func TestSMAuditTxIndex_PrunedOnRollback(t *testing.T) {
	factory := NewStoreFactory()
	defer func() { _ = factory.Close() }()
	tm := factory.NewTransactionManager(newTestUUIDGenerator())
	const tenant spi.TenantID = "tenant-audit-prune-rollback"
	ctx := testCtxWithTenant(tenant)

	audit, err := factory.StateMachineAuditStore(ctx)
	if err != nil {
		t.Fatalf("StateMachineAuditStore: %v", err)
	}
	sentinel := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := audit.Record(txCtx, "e-1", spi.StateMachineEvent{
		EventType: spi.SMEventStarted, EntityID: "e-1", TimeUUID: "u-1",
		TransactionID: txID, Details: "in tx", Timestamp: sentinel,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if got := auditTxIndexSize(factory, tenant); got != 1 {
		t.Fatalf("before rollback: smAuditTxIndex holds %d transactions, want 1", got)
	}

	if err := tm.Rollback(ctx, txID); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	if got := auditTxIndexSize(factory, tenant); got != 0 {
		t.Errorf("after rollback: smAuditTxIndex still holds %d transactions, want 0 — "+
			"a rolled-back transaction is never stamped, so nothing can ever read them", got)
	}

	// The event itself is not transactional and keeps the time it was recorded
	// with: rollback discards the index, not the audit trail.
	events, err := audit.GetEvents(ctx, "e-1")
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected the recorded event to survive the rollback, got %d events", len(events))
	}
	if !events[0].Timestamp.Equal(sentinel) {
		t.Errorf("rolled-back transaction's event was restamped to %s, want its recorded time %s",
			events[0].Timestamp, sentinel)
	}
}
