package memory_test

import (
	"errors"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

func newTxManager(t *testing.T) (*memory.StoreFactory, *memory.TransactionManager) {
	t.Helper()
	factory := memory.NewStoreFactory()
	uuids := newTestUUIDGenerator()
	tm := factory.NewTransactionManager(uuids)
	return factory, tm
}

func TestBeginAndCommitEmpty(t *testing.T) {
	_, tm := newTxManager(t)
	ctx := ctxWithTenant("tenant-A")

	txID, _, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin failed: %v", err)
	}
	if txID == "" {
		t.Fatal("expected non-empty txID")
	}

	if err := tm.Commit(ctx, txID); err != nil {
		t.Fatalf("Commit empty tx failed: %v", err)
	}
}

func TestBeginAndRollback(t *testing.T) {
	_, tm := newTxManager(t)
	ctx := ctxWithTenant("tenant-A")

	txID, _, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin failed: %v", err)
	}

	if err := tm.Rollback(ctx, txID); err != nil {
		t.Fatalf("Rollback failed: %v", err)
	}

	// Commit after rollback should fail — transaction no longer active.
	if err := tm.Commit(ctx, txID); err == nil {
		t.Fatal("expected error committing rolled-back transaction")
	}
}

func TestBeginRequiresTenant(t *testing.T) {
	_, tm := newTxManager(t)
	// No user context
	_, _, err := tm.Begin(ctxWithTenant(""))
	if err == nil {
		t.Fatal("expected error when tenant is empty")
	}
}

func TestCommitFlushesBufferToStore(t *testing.T) {
	factory, tm := newTxManager(t)
	ctx := ctxWithTenant("tenant-A")

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin failed: %v", err)
	}

	// Manually populate the transaction buffer.
	tx := spi.GetTransaction(txCtx)
	entity := &spi.Entity{
		Meta: spi.EntityMeta{
			ID:         "e-1",
			TenantID:   "tenant-A",
			ChangeType: "CREATED",
			ChangeUser: "test-user",
		},
		Data: []byte(`{"name":"alice"}`),
	}
	tx.Buffer["e-1"] = entity
	tx.WriteSet["e-1"] = true

	if err := tm.Commit(ctx, txID); err != nil {
		t.Fatalf("Commit failed: %v", err)
	}

	// Verify entity is in the store by reading it directly.
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore failed: %v", err)
	}
	got, err := store.Get(ctx, "e-1")
	if err != nil {
		t.Fatalf("Get after commit failed: %v", err)
	}
	if got.Meta.Version != 1 {
		t.Errorf("expected version 1, got %d", got.Meta.Version)
	}
	if got.Meta.TransactionID != txID {
		t.Errorf("expected transactionID %s, got %s", txID, got.Meta.TransactionID)
	}
	if string(got.Data) != `{"name":"alice"}` {
		t.Errorf("unexpected data: %s", got.Data)
	}
}

func TestCommitFlushesDeletes(t *testing.T) {
	factory, tm := newTxManager(t)
	ctx := ctxWithTenant("tenant-A")

	// Pre-populate an entity directly in the store.
	store, _ := factory.EntityStore(ctx)
	store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{
			ID:         "e-del",
			TenantID:   "tenant-A",
			ChangeType: "CREATED",
		},
		Data: []byte(`{}`),
	})

	// Begin transaction and stage a delete.
	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin failed: %v", err)
	}
	tx := spi.GetTransaction(txCtx)
	tx.Deletes["e-del"] = true
	tx.WriteSet["e-del"] = true

	if err := tm.Commit(ctx, txID); err != nil {
		t.Fatalf("Commit with delete failed: %v", err)
	}

	// Entity should be deleted.
	_, err = store.Get(ctx, "e-del")
	if err == nil {
		t.Fatal("expected entity to be deleted after commit")
	}
}

func TestReadWriteConflictDetection(t *testing.T) {
	_, tm := newTxManager(t)
	ctx := ctxWithTenant("tenant-A")

	// tx1 begins first, reads entity E.
	_, txCtx1, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin tx1 failed: %v", err)
	}
	tx1 := spi.GetTransaction(txCtx1)
	tx1.ReadSet["entity-E"] = true

	// tx2 begins, writes entity E.
	txID2, txCtx2, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin tx2 failed: %v", err)
	}
	tx2 := spi.GetTransaction(txCtx2)
	tx2.WriteSet["entity-E"] = true
	tx2.Buffer["entity-E"] = &spi.Entity{
		Meta: spi.EntityMeta{
			ID:         "entity-E",
			TenantID:   "tenant-A",
			ChangeType: "CREATED",
		},
		Data: []byte(`{}`),
	}

	// Small delay so tx2's commit time is after tx1's snapshot.
	time.Sleep(time.Millisecond)

	// tx2 commits successfully.
	if err := tm.Commit(ctx, txID2); err != nil {
		t.Fatalf("Commit tx2 should succeed: %v", err)
	}

	// tx1 tries to commit — should fail because entity-E was in tx1's read set
	// and tx2 wrote it after tx1's snapshot.
	txID1 := tx1.ID
	err = tm.Commit(ctx, txID1)
	if !errors.Is(err, spi.ErrConflict) {
		t.Fatalf("expected ErrConflict, got: %v", err)
	}
}

func TestWriteWriteConflictDetection(t *testing.T) {
	_, tm := newTxManager(t)
	ctx := ctxWithTenant("tenant-A")

	// tx1 writes entity E.
	_, txCtx1, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin tx1 failed: %v", err)
	}
	tx1 := spi.GetTransaction(txCtx1)
	tx1.WriteSet["entity-E"] = true
	tx1.Buffer["entity-E"] = &spi.Entity{
		Meta: spi.EntityMeta{ID: "entity-E", TenantID: "tenant-A", ChangeType: "CREATED"},
		Data: []byte(`{"v":1}`),
	}

	// tx2 also writes entity E.
	txID2, txCtx2, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin tx2 failed: %v", err)
	}
	tx2 := spi.GetTransaction(txCtx2)
	tx2.WriteSet["entity-E"] = true
	tx2.Buffer["entity-E"] = &spi.Entity{
		Meta: spi.EntityMeta{ID: "entity-E", TenantID: "tenant-A", ChangeType: "CREATED"},
		Data: []byte(`{"v":2}`),
	}

	time.Sleep(time.Millisecond)

	// tx2 commits first — success.
	if err := tm.Commit(ctx, txID2); err != nil {
		t.Fatalf("Commit tx2 should succeed: %v", err)
	}

	// tx1 commits — should fail (write-write conflict).
	txID1 := tx1.ID
	err = tm.Commit(ctx, txID1)
	if !errors.Is(err, spi.ErrConflict) {
		t.Fatalf("expected ErrConflict for write-write conflict, got: %v", err)
	}
}

func TestNoConflictDisjointEntities(t *testing.T) {
	_, tm := newTxManager(t)
	ctx := ctxWithTenant("tenant-A")

	// tx1 writes entity A.
	txID1, txCtx1, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin tx1 failed: %v", err)
	}
	tx1 := spi.GetTransaction(txCtx1)
	tx1.WriteSet["entity-A"] = true
	tx1.Buffer["entity-A"] = &spi.Entity{
		Meta: spi.EntityMeta{ID: "entity-A", TenantID: "tenant-A", ChangeType: "CREATED"},
		Data: []byte(`{}`),
	}

	// tx2 writes entity B.
	txID2, txCtx2, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin tx2 failed: %v", err)
	}
	tx2 := spi.GetTransaction(txCtx2)
	tx2.WriteSet["entity-B"] = true
	tx2.Buffer["entity-B"] = &spi.Entity{
		Meta: spi.EntityMeta{ID: "entity-B", TenantID: "tenant-A", ChangeType: "CREATED"},
		Data: []byte(`{}`),
	}

	time.Sleep(time.Millisecond)

	// Both should commit without conflict.
	if err := tm.Commit(ctx, txID1); err != nil {
		t.Fatalf("Commit tx1 should succeed: %v", err)
	}
	if err := tm.Commit(ctx, txID2); err != nil {
		t.Fatalf("Commit tx2 should succeed: %v", err)
	}
}

func TestCommitUnknownTxID(t *testing.T) {
	_, tm := newTxManager(t)
	ctx := ctxWithTenant("tenant-A")

	err := tm.Commit(ctx, "nonexistent-tx-id")
	if err == nil {
		t.Fatal("expected error for unknown txID")
	}
}

func TestRollbackUnknownTxID(t *testing.T) {
	_, tm := newTxManager(t)
	ctx := ctxWithTenant("tenant-A")

	// Rollback of unknown txID should return an error (not found).
	err := tm.Rollback(ctx, "nonexistent-tx-id")
	if err == nil {
		t.Fatal("Rollback unknown txID should return an error")
	}
}

func TestCommittedLogPruning(t *testing.T) {
	_, tm := newTxManager(t)
	ctx := ctxWithTenant("tenant-A")

	// Commit several transactions with no active transactions holding old snapshots.
	for i := 0; i < 5; i++ {
		txID, _, err := tm.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin failed: %v", err)
		}
		if err := tm.Commit(ctx, txID); err != nil {
			t.Fatalf("Commit failed: %v", err)
		}
	}

	// With no active transactions, the committed log should be empty after pruning.
	// We verify by starting a new tx and committing — if pruning works, old entries are gone.
	// The test mainly ensures no panic or error during pruning.

	// Start a long-lived tx to hold a snapshot.
	longTxID, _, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin long tx failed: %v", err)
	}

	time.Sleep(time.Millisecond)

	// Commit a few more.
	for i := 0; i < 3; i++ {
		txID, _, err := tm.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin failed: %v", err)
		}
		if err := tm.Commit(ctx, txID); err != nil {
			t.Fatalf("Commit failed: %v", err)
		}
	}

	// Now commit the long tx — entries before its snapshot should be pruned.
	if err := tm.Commit(ctx, longTxID); err != nil {
		t.Fatalf("Commit long tx failed: %v", err)
	}

	// Verify the committed log length via the exported method.
	logLen := tm.CommittedLogLen()
	// After pruning with no active transactions, all entries should be prunable.
	// But since we just committed longTxID, and there are no active txs left,
	// the pruning in that commit should have removed entries older than longTxID's snapshot.
	// The exact count depends on timing, but it should be less than 8 (5+3).
	if logLen > 4 {
		t.Errorf("expected committed log to be pruned, got length %d", logLen)
	}
}

func TestCommitPreservesCreationDate(t *testing.T) {
	factory, tm := newTxManager(t)
	ctx := ctxWithTenant("tenant-A")

	// First commit creates the entity.
	txID1, txCtx1, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin failed: %v", err)
	}
	tx1 := spi.GetTransaction(txCtx1)
	tx1.Buffer["e-1"] = &spi.Entity{
		Meta: spi.EntityMeta{ID: "e-1", TenantID: "tenant-A", ChangeType: "CREATED"},
		Data: []byte(`{"v":1}`),
	}
	tx1.WriteSet["e-1"] = true
	if err := tm.Commit(ctx, txID1); err != nil {
		t.Fatalf("Commit 1 failed: %v", err)
	}

	store, _ := factory.EntityStore(ctx)
	first, _ := store.Get(ctx, "e-1")
	creationDate := first.Meta.CreationDate

	time.Sleep(time.Millisecond)

	// Second commit updates the entity.
	txID2, txCtx2, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin failed: %v", err)
	}
	tx2 := spi.GetTransaction(txCtx2)
	tx2.Buffer["e-1"] = &spi.Entity{
		Meta: spi.EntityMeta{ID: "e-1", TenantID: "tenant-A", ChangeType: "UPDATED"},
		Data: []byte(`{"v":2}`),
	}
	tx2.WriteSet["e-1"] = true
	if err := tm.Commit(ctx, txID2); err != nil {
		t.Fatalf("Commit 2 failed: %v", err)
	}

	updated, _ := store.Get(ctx, "e-1")
	if !updated.Meta.CreationDate.Equal(creationDate) {
		t.Errorf("CreationDate changed: was %v, now %v", creationDate, updated.Meta.CreationDate)
	}
	if updated.Meta.Version != 2 {
		t.Errorf("expected version 2, got %d", updated.Meta.Version)
	}
}

func TestGetTransactionManagerInterface(t *testing.T) {
	factory := memory.NewStoreFactory()
	uuids := newTestUUIDGenerator()
	factory.NewTransactionManager(uuids)

	// GetTransactionManager should return the spi.TransactionManager interface.
	mgr := factory.GetTransactionManager()
	if mgr == nil {
		t.Fatal("expected non-nil TransactionManager")
	}
}

func TestSavepoint_RollbackRestoresBuffer(t *testing.T) {
	_, tm := newTxManager(t)
	ctx := ctxWithTenant("tenant-A")

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin failed: %v", err)
	}

	// Write a parent entity before the savepoint.
	tx := spi.GetTransaction(txCtx)
	tx.Buffer["parent-e"] = &spi.Entity{
		Meta: spi.EntityMeta{ID: "parent-e", TenantID: "tenant-A", ChangeType: "CREATED"},
		Data: []byte(`{"role":"parent"}`),
	}
	tx.WriteSet["parent-e"] = true

	// Create savepoint.
	spID, err := tm.Savepoint(ctx, txID)
	if err != nil {
		t.Fatalf("Savepoint failed: %v", err)
	}

	// Write a child entity after the savepoint.
	tx.Buffer["child-e"] = &spi.Entity{
		Meta: spi.EntityMeta{ID: "child-e", TenantID: "tenant-A", ChangeType: "CREATED"},
		Data: []byte(`{"role":"child"}`),
	}
	tx.WriteSet["child-e"] = true

	// Rollback to savepoint.
	if err := tm.RollbackToSavepoint(ctx, txID, spID); err != nil {
		t.Fatalf("RollbackToSavepoint failed: %v", err)
	}

	// Re-read tx from context (the pointer is the same, maps were replaced).
	if _, exists := tx.Buffer["child-e"]; exists {
		t.Error("expected child entity to be gone after rollback")
	}
	if _, exists := tx.Buffer["parent-e"]; !exists {
		t.Error("expected parent entity to remain after rollback")
	}
	if tx.WriteSet["child-e"] {
		t.Error("expected child-e to be absent from write set after rollback")
	}
	if !tx.WriteSet["parent-e"] {
		t.Error("expected parent-e to remain in write set after rollback")
	}
}

func TestSavepoint_ReleaseKeepsWork(t *testing.T) {
	_, tm := newTxManager(t)
	ctx := ctxWithTenant("tenant-A")

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin failed: %v", err)
	}

	// Create savepoint.
	spID, err := tm.Savepoint(ctx, txID)
	if err != nil {
		t.Fatalf("Savepoint failed: %v", err)
	}

	// Write entity after the savepoint.
	tx := spi.GetTransaction(txCtx)
	tx.Buffer["sp-entity"] = &spi.Entity{
		Meta: spi.EntityMeta{ID: "sp-entity", TenantID: "tenant-A", ChangeType: "CREATED"},
		Data: []byte(`{"v":1}`),
	}
	tx.WriteSet["sp-entity"] = true

	// Release savepoint — work should remain.
	if err := tm.ReleaseSavepoint(ctx, txID, spID); err != nil {
		t.Fatalf("ReleaseSavepoint failed: %v", err)
	}

	if _, exists := tx.Buffer["sp-entity"]; !exists {
		t.Error("expected entity to remain in buffer after release")
	}
	if !tx.WriteSet["sp-entity"] {
		t.Error("expected entity to remain in write set after release")
	}
}

func TestSavepoint_WrongTxIDRejected(t *testing.T) {
	_, tm := newTxManager(t)
	ctx := ctxWithTenant("tenant-A")

	// Begin two independent transactions.
	tx1ID, txCtx1, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin tx1 failed: %v", err)
	}
	tx2ID, _, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin tx2 failed: %v", err)
	}

	// Write something into tx1 so the savepoint captures state.
	tx1 := spi.GetTransaction(txCtx1)
	tx1.Buffer["e-1"] = &spi.Entity{
		Meta: spi.EntityMeta{ID: "e-1", TenantID: "tenant-A", ChangeType: "CREATED"},
		Data: []byte(`{"v":1}`),
	}
	tx1.WriteSet["e-1"] = true

	// Create a savepoint on tx1.
	sp1ID, err := tm.Savepoint(ctx, tx1ID)
	if err != nil {
		t.Fatalf("Savepoint on tx1 failed: %v", err)
	}

	// tx2 should NOT be able to rollback to tx1's savepoint.
	err = tm.RollbackToSavepoint(ctx, tx2ID, sp1ID)
	if err == nil {
		t.Error("expected error when tx2 rolls back to tx1's savepoint, got nil")
	}

	// tx2 should NOT be able to release tx1's savepoint.
	err = tm.ReleaseSavepoint(ctx, tx2ID, sp1ID)
	if err == nil {
		t.Error("expected error when tx2 releases tx1's savepoint, got nil")
	}

	// tx1 should still be able to use its own savepoint.
	err = tm.RollbackToSavepoint(ctx, tx1ID, sp1ID)
	if err != nil {
		t.Errorf("expected tx1 to rollback to its own savepoint, got: %v", err)
	}
}

// TestSavepoint_RejectsCrossTenant verifies that Savepoint refuses to operate
// on a transaction belonging to a different tenant. Surfaced by the issue
// #199 audit / PR-A code review (Item I-1): pre-fix, the three savepoint
// methods discarded the caller's tenant context entirely (took _ context.Context),
// allowing tenant A to manipulate tenant B's tx-state if the txID was known.
// Mirrors Commit/Rollback's existing tenant-mismatch protection.
func TestSavepoint_RejectsCrossTenant(t *testing.T) {
	_, tm := newTxManager(t)
	ctxA := ctxWithTenant("tenant-A")
	ctxB := ctxWithTenant("tenant-B")

	txAID, _, err := tm.Begin(ctxA)
	if err != nil {
		t.Fatalf("Begin failed: %v", err)
	}

	if _, err := tm.Savepoint(ctxB, txAID); err == nil {
		t.Fatal("expected error when tenant B takes savepoint on tenant A's tx")
	} else if !errors.Is(err, spi.ErrTxTenantMismatch) {
		t.Fatalf("expected ErrTxTenantMismatch, got: %v", err)
	}

	_ = tm.Rollback(ctxA, txAID)
}

// TestRollbackToSavepoint_RejectsCrossTenant verifies that RollbackToSavepoint
// refuses to operate on another tenant's transaction. Critical because
// RollbackToSavepoint is destructive on tx-state — without tenant verification,
// an authenticated tenant A caller could roll back tenant B's tx-state.
func TestRollbackToSavepoint_RejectsCrossTenant(t *testing.T) {
	_, tm := newTxManager(t)
	ctxA := ctxWithTenant("tenant-A")
	ctxB := ctxWithTenant("tenant-B")

	txAID, _, err := tm.Begin(ctxA)
	if err != nil {
		t.Fatalf("Begin failed: %v", err)
	}
	spID, err := tm.Savepoint(ctxA, txAID)
	if err != nil {
		t.Fatalf("Savepoint failed: %v", err)
	}

	if err := tm.RollbackToSavepoint(ctxB, txAID, spID); err == nil {
		t.Fatal("expected error when tenant B rolls back tenant A's savepoint")
	} else if !errors.Is(err, spi.ErrTxTenantMismatch) {
		t.Fatalf("expected ErrTxTenantMismatch, got: %v", err)
	}

	_ = tm.Rollback(ctxA, txAID)
}

// TestReleaseSavepoint_RejectsCrossTenant verifies that ReleaseSavepoint
// refuses to operate on another tenant's transaction. Less destructive than
// RollbackToSavepoint (only removes the snapshot record from m.savepoints)
// but still tenant-scoped state — enforces the consistent isolation contract.
func TestReleaseSavepoint_RejectsCrossTenant(t *testing.T) {
	_, tm := newTxManager(t)
	ctxA := ctxWithTenant("tenant-A")
	ctxB := ctxWithTenant("tenant-B")

	txAID, _, err := tm.Begin(ctxA)
	if err != nil {
		t.Fatalf("Begin failed: %v", err)
	}
	spID, err := tm.Savepoint(ctxA, txAID)
	if err != nil {
		t.Fatalf("Savepoint failed: %v", err)
	}

	if err := tm.ReleaseSavepoint(ctxB, txAID, spID); err == nil {
		t.Fatal("expected error when tenant B releases tenant A's savepoint")
	} else if !errors.Is(err, spi.ErrTxTenantMismatch) {
		t.Fatalf("expected ErrTxTenantMismatch, got: %v", err)
	}

	_ = tm.Rollback(ctxA, txAID)
}

// --- Follow-on-action attribution (#430) ---

// TestBeginCapturesOriginFromUserContext verifies that Begin resolves
// TransactionState.Origin from the caller's UserContext-derived Principal
// via spi.ResolveOrigin.
func TestBeginCapturesOriginFromUserContext(t *testing.T) {
	_, tm := newTxManager(t)
	ctx := ctxWithUserKind("tenant-A", "alice", spi.PrincipalUser)

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin failed: %v", err)
	}
	defer func() { _ = tm.Rollback(txCtx, txID) }()

	tx := spi.GetTransaction(txCtx)
	if tx == nil {
		t.Fatal("expected Begin to populate TransactionState in the returned context")
	}
	want := spi.Principal{ID: "alice", Kind: spi.PrincipalUser}
	if tx.Origin != want {
		t.Errorf("tx.Origin = %+v, want %+v", tx.Origin, want)
	}
	if tx.DeleteAttribution == nil {
		t.Error("expected Begin to initialise tx.DeleteAttribution")
	}
}

// TestBeginCapturesAmbientOrigin verifies that Begin honours an
// ambient-seeded origin (WithAmbientOrigin) over the caller's own
// UserContext-derived Principal when no parent tx exists — the
// scheduled-fire case, per ResolveOrigin's documented precedence.
func TestBeginCapturesAmbientOrigin(t *testing.T) {
	_, tm := newTxManager(t)
	base := ctxWithUserKind("tenant-A", "ambient-caller", spi.PrincipalUser)
	seed := spi.Principal{ID: "scheduler", Kind: spi.PrincipalSystem}
	ctx := spi.WithAmbientOrigin(base, seed)

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin failed: %v", err)
	}
	defer func() { _ = tm.Rollback(txCtx, txID) }()

	tx := spi.GetTransaction(txCtx)
	if tx == nil {
		t.Fatal("expected Begin to populate TransactionState in the returned context")
	}
	if tx.Origin != seed {
		t.Errorf("tx.Origin = %+v, want ambient seed %+v", tx.Origin, seed)
	}
}

// TestCommitDeleteAttribution_StagerNotCommitter is the central regression
// test for the delete-attribution bug: a delete staged by a joined,
// service-kind caller inside a user-origin transaction must be attributed to
// the tx's origin user with an Executor recording the *stager* (service),
// regardless of which context is later used to Commit. Before the fix,
// Commit's flush stamped the tombstone's user from its own ctx parameter —
// the committer, not the stager.
func TestCommitDeleteAttribution_StagerNotCommitter(t *testing.T) {
	factory, tm := newTxManager(t)
	rootCtx := ctxWithUserKind("tenant-A", "root-user", spi.PrincipalUser)

	// Pre-populate the entity to be deleted.
	store, err := factory.EntityStore(rootCtx)
	if err != nil {
		t.Fatalf("EntityStore failed: %v", err)
	}
	if _, err := store.Save(rootCtx, &spi.Entity{
		Meta: spi.EntityMeta{
			ID:       "e-del",
			TenantID: "tenant-A",
			ModelRef: spi.ModelRef{EntityName: "Order", ModelVersion: "1"},
		},
		Data: []byte(`{}`),
	}); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	txID, txCtx, err := tm.Begin(rootCtx)
	if err != nil {
		t.Fatalf("Begin failed: %v", err)
	}
	tx := spi.GetTransaction(txCtx)
	wantOrigin := spi.Principal{ID: "root-user", Kind: spi.PrincipalUser}
	if tx.Origin != wantOrigin {
		t.Fatalf("tx.Origin = %+v, want %+v", tx.Origin, wantOrigin)
	}

	// Join as a distinct, service-kind actor and stage the delete through it.
	svcCtx := ctxWithUserKind("tenant-A", "svc-x", spi.PrincipalService)
	joinedCtx, err := tm.Join(svcCtx, txID)
	if err != nil {
		t.Fatalf("Join failed: %v", err)
	}
	joinedStore, err := factory.EntityStore(joinedCtx)
	if err != nil {
		t.Fatalf("EntityStore(joinedCtx) failed: %v", err)
	}
	if err := joinedStore.Delete(joinedCtx, "e-del"); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	// DeleteAttribution must be captured immediately at stage time, under
	// the joined (service) ctx — not deferred to Commit.
	wantExecutor := spi.Principal{ID: "svc-x", Kind: spi.PrincipalService}
	attr, ok := tx.DeleteAttribution["e-del"]
	if !ok {
		t.Fatal("expected tx.DeleteAttribution to record the staged delete immediately")
	}
	if attr.Attributed != wantOrigin {
		t.Errorf("staged Attributed = %+v, want tx.Origin %+v", attr.Attributed, wantOrigin)
	}
	if attr.Executor != wantExecutor {
		t.Errorf("staged Executor = %+v, want stager %+v", attr.Executor, wantExecutor)
	}

	// Commit using the ROOT ctx (the committer), which is a *different*
	// actor from the stager. The fix must not re-derive attribution from
	// this ctx.
	if err := tm.Commit(rootCtx, txID); err != nil {
		t.Fatalf("Commit failed: %v", err)
	}

	history, err := store.GetVersionHistory(rootCtx, "e-del")
	if err != nil {
		t.Fatalf("GetVersionHistory failed: %v", err)
	}
	tomb := history[len(history)-1]
	if !tomb.Deleted {
		t.Fatal("expected last version to be the DELETE tombstone")
	}
	if tomb.User != wantOrigin.ID {
		t.Errorf("tombstone User = %q, want origin user %q", tomb.User, wantOrigin.ID)
	}
	if tomb.AttributedKind != wantOrigin.Kind {
		t.Errorf("tombstone AttributedKind = %v, want %v", tomb.AttributedKind, wantOrigin.Kind)
	}
	if tomb.Executor != wantExecutor {
		t.Errorf("tombstone Executor = %+v, want stager %+v (not the committer)", tomb.Executor, wantExecutor)
	}
}

// TestCommitFlushesDeletes_FallbackAttribution extends
// TestCommitFlushesDeletes: when a delete is staged directly on tx.Deletes
// without a corresponding tx.DeleteAttribution entry (simulating a caller
// that bypassed EntityStore.Delete), Commit's flush must fall back to
// spi.AttributionFor(ctx) evaluated at commit time.
func TestCommitFlushesDeletes_FallbackAttribution(t *testing.T) {
	factory, tm := newTxManager(t)
	ctx := ctxWithUserKind("tenant-A", "test-user", spi.PrincipalUser)

	store, _ := factory.EntityStore(ctx)
	if _, err := store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{
			ID:         "e-del-fallback",
			TenantID:   "tenant-A",
			ChangeType: "CREATED",
		},
		Data: []byte(`{}`),
	}); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin failed: %v", err)
	}
	tx := spi.GetTransaction(txCtx)
	// Stage the delete directly, bypassing EntityStore.Delete — no
	// DeleteAttribution entry is recorded.
	tx.Deletes["e-del-fallback"] = true
	tx.WriteSet["e-del-fallback"] = true
	if _, ok := tx.DeleteAttribution["e-del-fallback"]; ok {
		t.Fatal("test setup error: DeleteAttribution must be absent to exercise the fallback path")
	}

	if err := tm.Commit(ctx, txID); err != nil {
		t.Fatalf("Commit failed: %v", err)
	}

	history, err := store.GetVersionHistory(ctx, "e-del-fallback")
	if err != nil {
		t.Fatalf("GetVersionHistory failed: %v", err)
	}
	tomb := history[len(history)-1]
	if !tomb.Deleted {
		t.Fatal("expected last version to be the DELETE tombstone")
	}
	want := spi.Principal{ID: "test-user", Kind: spi.PrincipalUser}
	if tomb.User != want.ID {
		t.Errorf("tombstone User = %q, want fallback (commit ctx) user %q", tomb.User, want.ID)
	}
	if tomb.Executor != want {
		t.Errorf("tombstone Executor = %+v, want fallback %+v", tomb.Executor, want)
	}
}
