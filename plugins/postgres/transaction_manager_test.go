package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

func newTestTxManager(t *testing.T) (*postgres.TransactionManager, *pgxpool.Pool) {
	t.Helper()
	pool := newTestPool(t)
	if err := postgres.DropSchemaForTest(pool); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := postgres.Migrate(pool); err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	t.Cleanup(func() { _ = postgres.DropSchemaForTest(pool) })

	uuids := newTestUUIDGenerator()
	tm := postgres.NewTransactionManager(pool, uuids)
	return tm, pool
}

func TestTxManager_BeginAndCommit(t *testing.T) {
	tm, _ := newTestTxManager(t)
	ctx := ctxWithTenant("tx-tenant")

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if txID == "" {
		t.Fatal("expected non-empty txID")
	}
	if txCtx == nil {
		t.Fatal("expected non-nil txCtx")
	}

	// Verify transaction state is in context
	txState := spi.GetTransaction(txCtx)
	if txState == nil {
		t.Fatal("expected TransactionState in context")
	}
	if txState.ID != txID {
		t.Errorf("expected txState.ID=%s, got %s", txID, txState.ID)
	}
	if txState.TenantID != "tx-tenant" {
		t.Errorf("expected TenantID=tx-tenant, got %s", txState.TenantID)
	}

	if err := tm.Commit(ctx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

func TestTxManager_BeginAndRollback(t *testing.T) {
	tm, _ := newTestTxManager(t)
	ctx := ctxWithTenant("tx-tenant")

	txID, _, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	if err := tm.Rollback(ctx, txID); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
}

func TestTxManager_JoinExisting(t *testing.T) {
	tm, _ := newTestTxManager(t)
	ctx := ctxWithTenant("tx-tenant")

	txID, _, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	// Join from a fresh context (simulating another goroutine / handler)
	joinCtx := ctxWithTenant("tx-tenant")
	joinedCtx, err := tm.Join(joinCtx, txID)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}

	txState := spi.GetTransaction(joinedCtx)
	if txState == nil {
		t.Fatal("expected TransactionState in joined context")
	}
	if txState.ID != txID {
		t.Errorf("expected joined txState.ID=%s, got %s", txID, txState.ID)
	}

	// Clean up
	if err := tm.Commit(ctx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

func TestTxManager_JoinNonexistent(t *testing.T) {
	tm, _ := newTestTxManager(t)
	ctx := ctxWithTenant("tx-tenant")

	_, err := tm.Join(ctx, "nonexistent-tx-id")
	if err == nil {
		t.Fatal("expected error when joining nonexistent transaction")
	}
}

func TestTxManager_GetSubmitTime(t *testing.T) {
	tm, _ := newTestTxManager(t)
	ctx := ctxWithTenant("tx-tenant")

	txID, _, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	if err := tm.Commit(ctx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	submitTime, err := tm.GetSubmitTime(ctx, txID)
	if err != nil {
		t.Fatalf("GetSubmitTime: %v", err)
	}
	if submitTime.IsZero() {
		t.Fatal("expected non-zero submit time")
	}

	// Submit time should be recent (within last 10 seconds)
	if time.Since(submitTime) > 10*time.Second {
		t.Errorf("submit time too old: %v", submitTime)
	}
}

func TestTxManager_GetSubmitTimeBeforeCommit(t *testing.T) {
	tm, _ := newTestTxManager(t)
	ctx := ctxWithTenant("tx-tenant")

	txID, _, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tm.Rollback(ctx, txID) //nolint:errcheck

	_, err = tm.GetSubmitTime(ctx, txID)
	if err == nil {
		t.Fatal("expected error when getting submit time before commit")
	}
}

func TestTxManager_DoubleCommit(t *testing.T) {
	tm, _ := newTestTxManager(t)
	ctx := ctxWithTenant("tx-tenant")

	txID, _, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	if err := tm.Commit(ctx, txID); err != nil {
		t.Fatalf("first Commit: %v", err)
	}

	err = tm.Commit(ctx, txID)
	if err == nil {
		t.Fatal("expected error on second commit")
	}
}

func TestTxManager_WriteVisibleInTx(t *testing.T) {
	tm, pool := newTestTxManager(t)
	ctx := ctxWithTenant("tx-tenant")

	txID, _, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	// Get the underlying pgx.Tx via the registry (use pool to insert via the tx)
	// We'll use the kv_store table since it exists from migrations.
	// Write a row within the transaction using the pool's tx.
	// We need to access the tx — use the TxForTest helper.
	pgxTx, ok := tm.LookupTx(txID)
	if !ok {
		t.Fatal("expected to find tx in registry")
	}

	// Insert a row within the transaction
	_, err = pgxTx.Exec(ctx, "INSERT INTO kv_store (tenant_id, namespace, key, value) VALUES ($1, $2, $3, $4)",
		"tx-tenant", "test-ns", "test-key", []byte("test-value"))
	if err != nil {
		t.Fatalf("insert within tx: %v", err)
	}

	// Read it back within the same transaction — should be visible
	var value []byte
	err = pgxTx.QueryRow(ctx, "SELECT value FROM kv_store WHERE tenant_id=$1 AND namespace=$2 AND key=$3",
		"tx-tenant", "test-ns", "test-key").Scan(&value)
	if err != nil {
		t.Fatalf("read within tx: %v", err)
	}
	if string(value) != "test-value" {
		t.Errorf("expected 'test-value', got %q", string(value))
	}

	// Before commit, the row should NOT be visible outside the transaction
	var count int
	err = pool.QueryRow(ctx, "SELECT COUNT(*) FROM kv_store WHERE tenant_id=$1 AND namespace=$2 AND key=$3",
		"tx-tenant", "test-ns", "test-key").Scan(&count)
	if err != nil {
		t.Fatalf("count outside tx: %v", err)
	}
	if count != 0 {
		t.Errorf("expected row to be invisible outside tx, got count=%d", count)
	}

	// Commit
	if err := tm.Commit(ctx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// After commit, row should be visible
	err = pool.QueryRow(ctx, "SELECT COUNT(*) FROM kv_store WHERE tenant_id=$1 AND namespace=$2 AND key=$3",
		"tx-tenant", "test-ns", "test-key").Scan(&count)
	if err != nil {
		t.Fatalf("count after commit: %v", err)
	}
	if count != 1 {
		t.Errorf("expected row to be visible after commit, got count=%d", count)
	}

	// Clean up
	_, _ = pool.Exec(ctx, "DELETE FROM kv_store WHERE tenant_id=$1 AND namespace=$2 AND key=$3",
		"tx-tenant", "test-ns", "test-key")
}

func TestTxManager_SerializationConflict(t *testing.T) {
	tm, pool := newTestTxManager(t)
	ctx := ctxWithTenant("tx-tenant")

	// Setup: insert a row that both transactions will contend on
	_, err := pool.Exec(ctx, "INSERT INTO kv_store (tenant_id, namespace, key, value) VALUES ($1, $2, $3, $4)",
		"tx-tenant", "conflict-ns", "conflict-key", []byte("initial"))
	if err != nil {
		t.Fatalf("setup insert: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM kv_store WHERE tenant_id=$1 AND namespace=$2",
			"tx-tenant", "conflict-ns")
	})

	// Begin tx1 and tx2
	txID1, _, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin tx1: %v", err)
	}

	txID2, _, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin tx2: %v", err)
	}

	pgxTx1, _ := tm.LookupTx(txID1)
	pgxTx2, _ := tm.LookupTx(txID2)

	// tx1 reads the row (establishes read dependency in SERIALIZABLE)
	var val1 []byte
	err = pgxTx1.QueryRow(ctx, "SELECT value FROM kv_store WHERE tenant_id=$1 AND namespace=$2 AND key=$3",
		"tx-tenant", "conflict-ns", "conflict-key").Scan(&val1)
	if err != nil {
		t.Fatalf("tx1 read: %v", err)
	}

	// tx2 updates the row and commits
	_, err = pgxTx2.Exec(ctx, "UPDATE kv_store SET value=$1 WHERE tenant_id=$2 AND namespace=$3 AND key=$4",
		[]byte("from-tx2"), "tx-tenant", "conflict-ns", "conflict-key")
	if err != nil {
		t.Fatalf("tx2 update: %v", err)
	}

	if err := tm.Commit(ctx, txID2); err != nil {
		t.Fatalf("tx2 commit: %v", err)
	}

	// tx1 updates the same row
	_, err = pgxTx1.Exec(ctx, "UPDATE kv_store SET value=$1 WHERE tenant_id=$2 AND namespace=$3 AND key=$4",
		[]byte("from-tx1"), "tx-tenant", "conflict-ns", "conflict-key")
	if err != nil {
		// Some postgres versions detect conflict at exec time
		// That's fine — we just need the conflict to surface somewhere
		t.Logf("tx1 update got error (expected for serialization conflict): %v", err)
	}

	// tx1 commit should fail with ErrConflict
	err = tm.Commit(ctx, txID1)
	if err == nil {
		t.Fatal("expected serialization conflict error on tx1 commit")
	}
	if !errors.Is(err, spi.ErrConflict) {
		t.Errorf("expected ErrConflict, got: %v", err)
	}
}

func TestTxManager_BeginNoTenant(t *testing.T) {
	tm, _ := newTestTxManager(t)
	ctx := context.Background() // no tenant

	_, _, err := tm.Begin(ctx)
	if err == nil {
		t.Fatal("expected error when no tenant in context")
	}
}

// TestCommitProbe_NonPGError_NotConflict verifies that the submit-time probe
// error classification does NOT wrap arbitrary (non-25P02) errors as
// spi.ErrConflict.
//
// Prior to the I3 fix, every probe error was unconditionally wrapped as
// ErrConflict, so a network failure or other infrastructure error would be
// mis-surfaced as a retryable conflict. The fix narrows the classification
// to SQLSTATE 25P02 only.
//
// We test via ClassifyErrorForTest (the underlying classification helper)
// because exercising the probe path in Commit directly requires either a
// real postgres connection in a broken state or a mock — both fragile.
// The classifyError function embodies the same logic that the probe branches on.
func TestCommitProbe_NonPGError_NotConflict(t *testing.T) {
	// Simulate a plain non-postgres error (e.g. io.EOF, context.Canceled).
	plainErr := fmt.Errorf("simulated network failure")
	result := postgres.ClassifyErrorForTest(plainErr)

	// Must NOT be classified as ErrConflict.
	if errors.Is(result, spi.ErrConflict) {
		t.Errorf("non-pgconn error must NOT be classified as ErrConflict; got: %v", result)
	}
	// Must preserve the original error in the chain.
	if !errors.Is(result, plainErr) {
		t.Errorf("original error must remain in the error chain; got: %v", result)
	}
}

func TestTxManager_BeginAllocatesTxState(t *testing.T) {
	tm, _ := newTestTxManager(t)
	ctx := ctxWithTenant("tx-tenant")
	txID, _, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if !postgres.HasTxState(tm, txID) {
		t.Errorf("expected txState registered for %s", txID)
	}
	if err := tm.Commit(ctx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if postgres.HasTxState(tm, txID) {
		t.Errorf("expected txState removed after Commit for %s", txID)
	}
}

func TestTxManager_RollbackCleansTxState(t *testing.T) {
	tm, _ := newTestTxManager(t)
	ctx := ctxWithTenant("tx-tenant")
	txID, _, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := tm.Rollback(ctx, txID); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if postgres.HasTxState(tm, txID) {
		t.Errorf("expected txState removed after Rollback for %s", txID)
	}
}

func TestTxManager_RepeatableRead_SnapshotAndReadYourOwnWrites(t *testing.T) {
	tm, _ := newTestTxManager(t)
	ctx := ctxWithTenant("t1")

	// Tx1: insert a row.
	txID1, txCtx1, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin 1: %v", err)
	}
	tx1, _ := tm.LookupTx(txID1)
	if _, err := tx1.Exec(txCtx1,
		`INSERT INTO entities (tenant_id, entity_id, model_name, model_version, version, deleted, doc)
         VALUES ('t1', 'e1', 'M', '1', 1, false, '{}'::jsonb)`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// Read-your-own-writes.
	var v int64
	if err := tx1.QueryRow(txCtx1,
		`SELECT version FROM entities WHERE tenant_id='t1' AND entity_id='e1'`).Scan(&v); err != nil {
		t.Fatalf("read own write: %v", err)
	}
	if v != 1 {
		t.Errorf("want version=1, got %d", v)
	}
	if err := tm.Commit(ctx, txID1); err != nil {
		t.Fatalf("Commit 1: %v", err)
	}

	// Tx2: takes snapshot.
	txID2, txCtx2, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin 2: %v", err)
	}
	tx2, _ := tm.LookupTx(txID2)
	var v2 int64
	if err := tx2.QueryRow(txCtx2,
		`SELECT version FROM entities WHERE tenant_id='t1' AND entity_id='e1'`).Scan(&v2); err != nil {
		t.Fatalf("tx2 read: %v", err)
	}
	if v2 != 1 {
		t.Errorf("tx2 snapshot: want 1, got %d", v2)
	}

	// Tx3 (outside tx2) commits a version bump.
	txID3, txCtx3, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin 3: %v", err)
	}
	tx3, _ := tm.LookupTx(txID3)
	if _, err := tx3.Exec(txCtx3,
		`UPDATE entities SET version=2 WHERE tenant_id='t1' AND entity_id='e1'`); err != nil {
		t.Fatalf("tx3 update: %v", err)
	}
	if err := tm.Commit(ctx, txID3); err != nil {
		t.Fatalf("Commit 3: %v", err)
	}

	// Tx2 should STILL see version 1 (snapshot preserved).
	if err := tx2.QueryRow(txCtx2,
		`SELECT version FROM entities WHERE tenant_id='t1' AND entity_id='e1'`).Scan(&v2); err != nil {
		t.Fatalf("tx2 re-read: %v", err)
	}
	if v2 != 1 {
		t.Errorf("snapshot preserved after concurrent commit: want 1, got %d", v2)
	}
	_ = tm.Rollback(ctx, txID2)
}

func TestTxManager_Commit_ReadSetConflict(t *testing.T) {
	tm, pool := newTestTxManager(t)
	ctx := ctxWithTenant("t1")

	// Seed.
	_, _ = pool.Exec(ctx, `
		INSERT INTO entities (tenant_id, entity_id, model_name, model_version, version, deleted, doc)
		VALUES ('t1', 'e1', 'M', '1', 5, false, '{}'::jsonb)
	`)

	// Tx A: begin, record a read at version 5.
	txA, _, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin A: %v", err)
	}
	stateA, _ := postgres.LookupTxStateForTest(tm, txA)
	stateA.RecordRead("e1", 5)

	// Tx B: bumps e1 to version 6 and commits.
	txB, txCtxB, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin B: %v", err)
	}
	txBpgx, _ := tm.LookupTx(txB)
	if _, err := txBpgx.Exec(txCtxB,
		`UPDATE entities SET version=6 WHERE tenant_id='t1' AND entity_id='e1'`); err != nil {
		t.Fatalf("B update: %v", err)
	}
	if err := tm.Commit(ctx, txB); err != nil {
		t.Fatalf("B commit: %v", err)
	}

	// Tx A: commit must fail with ErrConflict.
	err = tm.Commit(ctx, txA)
	if err == nil {
		t.Fatal("want ErrConflict on A.Commit, got nil")
	}
	if !errors.Is(err, spi.ErrConflict) {
		t.Errorf("want ErrConflict, got %v", err)
	}
}

// TestTxManager_Savepoint_RollsBackTxStateEntries verifies that
// RollbackToSavepoint restores the txState readSet/writeSet to the
// snapshot captured at Savepoint time: entries added after the savepoint
// are dropped, entries added before are preserved.
func TestTxManager_Savepoint_RollsBackTxStateEntries(t *testing.T) {
	tm, pool := newTestTxManager(t)
	ctx := ctxWithTenant("sp-tenant")

	// Seed two entities x and y at version 1.
	for _, id := range []string{"sp-x", "sp-y"} {
		_, err := pool.Exec(ctx, `
			INSERT INTO entities (tenant_id, entity_id, model_name, model_version, version, deleted, doc)
			VALUES ('sp-tenant', $1, 'M', '1', 1, false, '{}'::jsonb)`, id)
		if err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM entities WHERE tenant_id='sp-tenant'`)
	})

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tm.Rollback(txCtx, txID) //nolint:errcheck

	pgxTx, _ := tm.LookupTx(txID)
	state, ok := postgres.LookupTxStateForTest(tm, txID)
	if !ok {
		t.Fatal("txState not found")
	}

	// Read x within the transaction (records x in readSet).
	var vx int64
	if err := pgxTx.QueryRow(txCtx,
		`SELECT version FROM entities WHERE tenant_id='sp-tenant' AND entity_id='sp-x'`).Scan(&vx); err != nil {
		t.Fatalf("read x: %v", err)
	}
	state.RecordRead("sp-x", vx)

	// Push savepoint — x is in readSet at this point.
	spID, err := tm.Savepoint(txCtx, txID)
	if err != nil {
		t.Fatalf("Savepoint: %v", err)
	}

	// Read y within the transaction (records y in readSet after the savepoint).
	var vy int64
	if err := pgxTx.QueryRow(txCtx,
		`SELECT version FROM entities WHERE tenant_id='sp-tenant' AND entity_id='sp-y'`).Scan(&vy); err != nil {
		t.Fatalf("read y: %v", err)
	}
	state.RecordRead("sp-y", vy)

	// Verify both are in readSet before rollback.
	if postgres.ReadSetVersionForTest(state, "sp-x") != 1 {
		t.Errorf("pre-rollback: sp-x not in readSet at v1")
	}
	if postgres.ReadSetVersionForTest(state, "sp-y") != 1 {
		t.Errorf("pre-rollback: sp-y not in readSet at v1")
	}

	// Rollback to savepoint — should drop y, preserve x.
	if err := tm.RollbackToSavepoint(txCtx, txID, spID); err != nil {
		t.Fatalf("RollbackToSavepoint: %v", err)
	}

	// y must be gone from readSet.
	if v := postgres.ReadSetVersionForTest(state, "sp-y"); v != 0 {
		t.Errorf("post-rollback: sp-y should be absent from readSet, got version %d", v)
	}
	// x must still be present.
	if v := postgres.ReadSetVersionForTest(state, "sp-x"); v != 1 {
		t.Errorf("post-rollback: sp-x should remain in readSet at v1, got %d", v)
	}
}
