package memory

import (
	"errors"
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

// auditOpsLen reports how many transactions currently have staged
// (uncommitted) audit events, read under the mutex that guards it. Unlike
// GetEvents, which cannot distinguish "cleaned up" from "never appended
// because the transaction never committed", this reaches directly into the
// staging map an abort must clear.
func auditOpsLen(tm *TransactionManager) int {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return len(tm.auditOps)
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
//
// Recorded outside the transaction: an event recorded inside it is staged
// and indexed only at commit.
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
	if err := audit.Record(ctx, "e-1", spi.StateMachineEvent{
		EventType: spi.SMEventStarted, EntityID: "e-1",
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
// transaction's events are never appended, so they are never indexed;
// nothing of it reaches the trail.
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
		EventType: spi.SMEventStarted, EntityID: "e-1",
		TransactionID: txID, Details: "in tx", Timestamp: sentinel,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if got := auditTxIndexSize(factory, tenant); got != 0 {
		t.Fatalf("before rollback: smAuditTxIndex holds %d transactions, want 0 — "+
			"a staged event is indexed only when its transaction commits", got)
	}

	if err := tm.Rollback(ctx, txID); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	if got := auditTxIndexSize(factory, tenant); got != 0 {
		t.Errorf("after rollback: smAuditTxIndex still holds %d transactions, want 0", got)
	}

	// A rolled-back transaction's staged event is never appended to the
	// trail at all.
	events, err := audit.GetEvents(ctx, "e-1")
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("after rollback: GetEvents returned %d events, want 0", len(events))
	}
}

// TestSMAuditTxIndex_StaleLabelDoesNotLeak pins that appending a staged event
// whose TransactionID label names a transaction that has already ended never
// recreates that transaction's smAuditTxIndex entry. Nothing will ever call
// discardAuditTxIndex for that label again — its own transaction's one and
// only commit/rollback already ran — so unconditionally indexing under it at
// append time would leak one entry, and grow it forever, for the life of the
// process.
//
// This is exactly the shape a COMMIT_BEFORE_DISPATCH cascade produces: a
// later segment (TX_post) labels its own events with the entry transaction's
// id (TX_pre) for GetEventsByTransaction correlation, and TX_pre may have
// already committed — and been pruned — before TX_post ever commits.
func TestSMAuditTxIndex_StaleLabelDoesNotLeak(t *testing.T) {
	factory := NewStoreFactory()
	defer func() { _ = factory.Close() }()
	tm := factory.NewTransactionManager(newTestUUIDGenerator())
	const tenant spi.TenantID = "tenant-audit-stale-label"
	ctx := testCtxWithTenant(tenant)

	audit, err := factory.StateMachineAuditStore(ctx)
	if err != nil {
		t.Fatalf("StateMachineAuditStore: %v", err)
	}

	// TX_pre records and commits first; its index entry is pruned by its own
	// commit (TestSMAuditTxIndex_PrunedOnCommit pins that half).
	preTxID, preCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin (pre): %v", err)
	}
	if err := audit.Record(preCtx, "e-1", spi.StateMachineEvent{
		EventType: spi.SMEventStarted, TransactionID: preTxID, Details: "pre",
	}); err != nil {
		t.Fatalf("Record (pre): %v", err)
	}
	if err := tm.Commit(preCtx, preTxID); err != nil {
		t.Fatalf("Commit (pre): %v", err)
	}
	if got := auditTxIndexSize(factory, tenant); got != 0 {
		t.Fatalf("after TX_pre commits: smAuditTxIndex holds %d transactions, want 0", got)
	}

	// TX_post is a separate, later transaction. It records an event labelled
	// with TX_pre's (already-dead) id — simulating a cascade segment's
	// correlation label — then commits.
	postTxID, postCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin (post): %v", err)
	}
	if err := audit.Record(postCtx, "e-1", spi.StateMachineEvent{
		EventType: spi.SMEventTransitionMade, TransactionID: preTxID, Details: "post, labelled pre",
	}); err != nil {
		t.Fatalf("Record (post): %v", err)
	}
	if err := tm.Commit(postCtx, postTxID); err != nil {
		t.Fatalf("Commit (post): %v", err)
	}

	if got := auditTxIndexSize(factory, tenant); got != 0 {
		t.Errorf("after TX_post commits: smAuditTxIndex holds %d transactions, want 0 — "+
			"a label naming an already-ended transaction must never be (re)indexed, since "+
			"nothing will ever prune it again", got)
	}

	// The event itself is still in the trail — only the index entry is
	// withheld, not the event.
	got, err := audit.GetEventsByTransaction(ctx, "e-1", preTxID)
	if err != nil {
		t.Fatalf("GetEventsByTransaction: %v", err)
	}
	var sawPost bool
	for _, ev := range got {
		if ev.Details == "post, labelled pre" {
			sawPost = true
		}
	}
	if !sawPost {
		t.Errorf("GetEventsByTransaction(preTxID) = %+v, want TX_post's labelled event present "+
			"(a full scan still finds it; only the index lookup does not)", got)
	}
}

// TestSMAudit_ConflictAbortDropsStagedEvents pins that a transaction which
// loses Commit's write-write conflict check — applying nothing — also drops
// the audit events it staged. GetEvents alone cannot tell "cleaned up" from
// "the staged event was simply never migrated into the trail" (both look
// like zero events from outside), so this reaches into auditOps directly.
func TestSMAudit_ConflictAbortDropsStagedEvents(t *testing.T) {
	factory := NewStoreFactory()
	defer func() { _ = factory.Close() }()
	tm := factory.NewTransactionManager(newTestUUIDGenerator())
	const tenant spi.TenantID = "tenant-audit-conflict-abort"
	ctx := testCtxWithTenant(tenant)
	audit, err := factory.StateMachineAuditStore(ctx)
	if err != nil {
		t.Fatalf("StateMachineAuditStore: %v", err)
	}

	tx1ID, tx1Ctx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin tx1: %v", err)
	}
	if err := audit.Record(tx1Ctx, "e-1", spi.StateMachineEvent{
		EventType: spi.SMEventStarted, TransactionID: tx1ID, Details: "tx1",
	}); err != nil {
		t.Fatalf("Record tx1: %v", err)
	}
	// Declare tx1's write directly on its exported TransactionState fields,
	// mirroring TestWriteWriteConflictDetection (txmanager_test.go) — no
	// EntityStore call is needed to exercise the conflict check.
	tx1 := spi.GetTransaction(tx1Ctx)
	tx1.WriteSet["entity-E"] = true

	tx2ID, tx2Ctx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin tx2: %v", err)
	}
	tx2 := spi.GetTransaction(tx2Ctx)
	tx2.WriteSet["entity-E"] = true
	tx2.Buffer["entity-E"] = &spi.Entity{
		Meta: spi.EntityMeta{ID: "entity-E", TenantID: tenant, ChangeType: "CREATED"},
		Data: []byte(`{}`),
	}
	if err := tm.Commit(ctx, tx2ID); err != nil {
		t.Fatalf("Commit tx2: %v", err)
	}

	if err := tm.Commit(ctx, tx1ID); !errors.Is(err, spi.ErrConflict) {
		t.Fatalf("Commit tx1: got %v, want ErrConflict", err)
	}

	if got := auditOpsLen(tm); got != 0 {
		t.Errorf("after a conflict-aborted commit: auditOps holds %d transaction(s), want 0 — "+
			"the staged event must be dropped, not merely leaked", got)
	}
}

// TestSMAudit_UniqueViolationAbortDropsStagedEvents pins the same cleanup for
// the other abort path: a transaction that loses Commit's composite
// unique-key claim validation also drops the audit events it staged.
func TestSMAudit_UniqueViolationAbortDropsStagedEvents(t *testing.T) {
	factory := NewStoreFactory()
	defer func() { _ = factory.Close() }()
	tm := factory.NewTransactionManager(newTestUUIDGenerator())
	const tenant spi.TenantID = "tenant-audit-unique-abort"
	ctx := testCtxWithTenant(tenant)
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	audit, err := factory.StateMachineAuditStore(ctx)
	if err != nil {
		t.Fatalf("StateMachineAuditStore: %v", err)
	}
	ref := spi.ModelRef{EntityName: "audit-unique-abort", ModelVersion: "1"}
	keys := []spi.UniqueKey{{ID: "email-key", Fields: []string{"$.email"}}}

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := audit.Record(txCtx, "e-1", spi.StateMachineEvent{
		EventType: spi.SMEventStarted, TransactionID: txID, Details: "before abort",
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	// Two buffered entities claiming the same key value — mirrors
	// TestUniqueClaims_IntraBatchDuplicate (unique_claims_test.go).
	keyCtx := spi.WithUniqueKeys(txCtx, keys)
	if _, err := store.Save(keyCtx, &spi.Entity{
		Meta: spi.EntityMeta{ID: "e-dup-1", ModelRef: ref}, Data: []byte(`{"email":"a@x.com"}`),
	}); err != nil {
		t.Fatalf("Save e-dup-1: %v", err)
	}
	if _, err := store.Save(keyCtx, &spi.Entity{
		Meta: spi.EntityMeta{ID: "e-dup-2", ModelRef: ref}, Data: []byte(`{"email":"a@x.com"}`),
	}); err != nil {
		t.Fatalf("Save e-dup-2 (buffer): %v", err)
	}

	if err := tm.Commit(txCtx, txID); !errors.Is(err, spi.ErrUniqueViolation) {
		t.Fatalf("Commit: got %v, want ErrUniqueViolation", err)
	}

	if got := auditOpsLen(tm); got != 0 {
		t.Errorf("after a unique-violation-aborted commit: auditOps holds %d transaction(s), want 0",
			got)
	}
}

// TestSMAuditTxIndex_StaleLabelDoesNotLeak_RolledBack is
// TestSMAuditTxIndex_StaleLabelDoesNotLeak's sibling for the other way a
// labelled transaction can end: TX_pre rolls back (instead of committing)
// before TX_post — a separate, later transaction whose own staged event is
// labelled with TX_pre's id — commits. Either way TX_pre's own terminal
// call is its one and only chance to prune its smAuditTxIndex entry, so
// TX_post's commit must not recreate one for it.
func TestSMAuditTxIndex_StaleLabelDoesNotLeak_RolledBack(t *testing.T) {
	factory := NewStoreFactory()
	defer func() { _ = factory.Close() }()
	tm := factory.NewTransactionManager(newTestUUIDGenerator())
	const tenant spi.TenantID = "tenant-audit-stale-label-rollback"
	ctx := testCtxWithTenant(tenant)

	audit, err := factory.StateMachineAuditStore(ctx)
	if err != nil {
		t.Fatalf("StateMachineAuditStore: %v", err)
	}

	preTxID, _, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin (pre): %v", err)
	}
	if err := tm.Rollback(ctx, preTxID); err != nil {
		t.Fatalf("Rollback (pre): %v", err)
	}
	if got := auditTxIndexSize(factory, tenant); got != 0 {
		t.Fatalf("after TX_pre rolls back: smAuditTxIndex holds %d transactions, want 0", got)
	}

	postTxID, postCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin (post): %v", err)
	}
	if err := audit.Record(postCtx, "e-1", spi.StateMachineEvent{
		EventType: spi.SMEventTransitionMade, TransactionID: preTxID, Details: "post, labelled pre",
	}); err != nil {
		t.Fatalf("Record (post): %v", err)
	}
	if err := tm.Commit(postCtx, postTxID); err != nil {
		t.Fatalf("Commit (post): %v", err)
	}

	if got := auditTxIndexSize(factory, tenant); got != 0 {
		t.Errorf("after TX_post commits, labelled with the already-rolled-back TX_pre: "+
			"smAuditTxIndex holds %d transactions, want 0", got)
	}
}

// TestSMAudit_UntransactedRecordWithLabelThatNeverExisted_DoesNotLeakIndex
// pins the other source of unbounded smAuditTxIndex growth: Record's
// untransacted branch (ctx carries no transaction) indexes an event under
// its TransactionID label whenever one is set. resolveAuditTxID
// (internal/domain/workflow/engine.go) mints a fresh, random label purely
// for correlation when the entity it is auditing has no transaction id of
// its own — a label that will never back a real Begin/Commit/Rollback, so
// nothing will EVER call stampAuditEventsForTx or discardAuditTxIndex for
// it. Indexing it anyway leaks one entry per such Record call, for the life
// of the process — and this is the untransacted engine path
// (TestEngine_ManualCriterionNoMatch_EnrichesError exercises exactly this
// shape), not a rare cascade corner case.
func TestSMAudit_UntransactedRecordWithLabelThatNeverExisted_DoesNotLeakIndex(t *testing.T) {
	factory := NewStoreFactory()
	defer func() { _ = factory.Close() }()
	const tenant spi.TenantID = "tenant-audit-never-a-tx"
	ctx := testCtxWithTenant(tenant)
	audit, err := factory.StateMachineAuditStore(ctx)
	if err != nil {
		t.Fatalf("StateMachineAuditStore: %v", err)
	}

	const neverATransaction = "label-that-never-was-a-transaction"
	if err := audit.Record(ctx, "e-1", spi.StateMachineEvent{
		EventType: spi.SMEventStarted, TransactionID: neverATransaction, Details: "no tx at all",
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	if got := auditTxIndexSize(factory, tenant); got != 0 {
		t.Errorf("after recording with a label that never named a transaction: "+
			"smAuditTxIndex holds %d transactions, want 0 — nothing will ever call "+
			"stampAuditEventsForTx or discardAuditTxIndex for a label that was never "+
			"a real transaction", got)
	}

	// The event itself is still in the trail — only the index entry is
	// withheld, exactly as with a stale-but-once-real label (see
	// TestSMAuditTxIndex_StaleLabelDoesNotLeak): GetEvents and
	// GetEventsByTransaction never consult smAuditTxIndex, so this changes
	// no reachable query result.
	events, err := audit.GetEvents(ctx, "e-1")
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	if len(events) != 1 || events[0].Details != "no tx at all" {
		t.Fatalf("GetEvents = %+v, want the one recorded event still present", events)
	}
	byTx, err := audit.GetEventsByTransaction(ctx, "e-1", neverATransaction)
	if err != nil {
		t.Fatalf("GetEventsByTransaction: %v", err)
	}
	if len(byTx) != 1 {
		t.Fatalf("GetEventsByTransaction(neverATransaction) = %+v, want the one event "+
			"(the index is only an optimisation for stampAuditEventsForTx; the query itself "+
			"is a full scan and must still find it)", byTx)
	}
}
