package postgres_test

import (
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// Tenant-isolation regression tests for the postgres plugin's TM lifecycle
// methods. Issue #199 PR-C2: the postgres TM relied solely on PostgreSQL's
// row-level security (RLS) for tenant isolation. RLS is row-level and does
// NOT extend to transaction-lifecycle commands (BEGIN/COMMIT/ROLLBACK/
// SAVEPOINT/etc.) — those operate on connections and don't trigger any
// policy. So a caller authenticated as tenant A who learned a tenant B
// txID could:
//
//   - Commit(ctxA, txBID) — commit tenant B's tx prematurely.
//   - Rollback(ctxA, txBID) — abort tenant B's in-flight work.
//   - Join(ctxA, txBID) — receive a context driving tenant B's tx.
//   - Savepoint/RollbackToSavepoint/ReleaseSavepoint(ctxA, txBID, ...) —
//     manipulate tenant B's tx state.
//
// All operations remained RLS-bound at the data layer (any DML inside the
// pgxTx still ran with app.current_tenant=B, set at Begin), but the
// lifecycle disruption is real. PR-C2 closes the gap by adding
// application-layer tenant verification on every TM lifecycle method,
// matching the memory and sqlite plugins.
//
// These tests require Docker (testcontainers-go for PostgreSQL).

func TestPostgresCommit_RejectsCrossTenant(t *testing.T) {
	tm, _ := newTestTxManager(t)
	ctxA := ctxWithTenant("tenant-A")
	ctxB := ctxWithTenant("tenant-B")

	txAID, _, err := tm.Begin(ctxA)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	if err := tm.Commit(ctxB, txAID); err == nil {
		t.Fatal("expected error when tenant B commits tenant A's tx")
	} else if !errors.Is(err, spi.ErrTxTenantMismatch) {
		t.Fatalf("expected ErrTxTenantMismatch, got: %v", err)
	}

	_ = tm.Rollback(ctxA, txAID)
}

func TestPostgresRollback_RejectsCrossTenant(t *testing.T) {
	tm, _ := newTestTxManager(t)
	ctxA := ctxWithTenant("tenant-A")
	ctxB := ctxWithTenant("tenant-B")

	txAID, _, err := tm.Begin(ctxA)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	if err := tm.Rollback(ctxB, txAID); err == nil {
		t.Fatal("expected error when tenant B rolls back tenant A's tx")
	} else if !errors.Is(err, spi.ErrTxTenantMismatch) {
		t.Fatalf("expected ErrTxTenantMismatch, got: %v", err)
	}

	_ = tm.Rollback(ctxA, txAID)
}

func TestPostgresJoin_RejectsCrossTenant(t *testing.T) {
	tm, _ := newTestTxManager(t)
	ctxA := ctxWithTenant("tenant-A")
	ctxB := ctxWithTenant("tenant-B")

	txAID, _, err := tm.Begin(ctxA)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	if _, err := tm.Join(ctxB, txAID); err == nil {
		t.Fatal("expected error when tenant B joins tenant A's tx")
	} else if !errors.Is(err, spi.ErrTxTenantMismatch) {
		t.Fatalf("expected ErrTxTenantMismatch, got: %v", err)
	}

	_ = tm.Rollback(ctxA, txAID)
}

func TestPostgresSavepoint_RejectsCrossTenant(t *testing.T) {
	tm, _ := newTestTxManager(t)
	ctxA := ctxWithTenant("tenant-A")
	ctxB := ctxWithTenant("tenant-B")

	txAID, _, err := tm.Begin(ctxA)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	if _, err := tm.Savepoint(ctxB, txAID); err == nil {
		t.Fatal("expected error when tenant B takes savepoint on tenant A's tx")
	} else if !errors.Is(err, spi.ErrTxTenantMismatch) {
		t.Fatalf("expected ErrTxTenantMismatch, got: %v", err)
	}

	_ = tm.Rollback(ctxA, txAID)
}

func TestPostgresRollbackToSavepoint_RejectsCrossTenant(t *testing.T) {
	tm, _ := newTestTxManager(t)
	ctxA := ctxWithTenant("tenant-A")
	ctxB := ctxWithTenant("tenant-B")

	txAID, _, err := tm.Begin(ctxA)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	spID, err := tm.Savepoint(ctxA, txAID)
	if err != nil {
		t.Fatalf("Savepoint: %v", err)
	}

	if err := tm.RollbackToSavepoint(ctxB, txAID, spID); err == nil {
		t.Fatal("expected error when tenant B rolls back tenant A's savepoint")
	} else if !errors.Is(err, spi.ErrTxTenantMismatch) {
		t.Fatalf("expected ErrTxTenantMismatch, got: %v", err)
	}

	_ = tm.Rollback(ctxA, txAID)
}

func TestPostgresGetSubmitTime_RejectsCrossTenant(t *testing.T) {
	tm, _ := newTestTxManager(t)
	ctxA := ctxWithTenant("tenant-A")
	ctxB := ctxWithTenant("tenant-B")

	txAID, txCtxA, err := tm.Begin(ctxA)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	// Safety net: an assertion failure below must not leave the tx open —
	// pool.Close in cleanup would wait on its connection forever. The
	// error from rolling back an already-committed tx is ignored.
	defer func() { _ = tm.Rollback(txCtxA, txAID) }()

	// In flight: tenant B must not learn "exists but not yet committed".
	if _, err := tm.GetSubmitTime(ctxB, txAID); err == nil {
		t.Fatal("expected error when tenant B resolves tenant A's in-flight tx")
	} else if !errors.Is(err, spi.ErrTxTenantMismatch) {
		t.Fatalf("expected ErrTxTenantMismatch, got: %v", err)
	}

	if err := tm.Commit(txCtxA, txAID); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Committed: tenant B must not learn the submit timestamp.
	if _, err := tm.GetSubmitTime(ctxB, txAID); err == nil {
		t.Fatal("expected error when tenant B resolves tenant A's committed tx")
	} else if !errors.Is(err, spi.ErrTxTenantMismatch) {
		t.Fatalf("expected ErrTxTenantMismatch, got: %v", err)
	}

	// The owning tenant still resolves it.
	if _, err := tm.GetSubmitTime(ctxA, txAID); err != nil {
		t.Fatalf("owning tenant's GetSubmitTime: %v", err)
	}
}

func TestPostgresGetSubmitTime_UnknownTx_NotFound(t *testing.T) {
	tm, _ := newTestTxManager(t)
	ctx := ctxWithTenant("tenant-A")

	if _, err := tm.GetSubmitTime(ctx, "no-such-tx"); err == nil {
		t.Fatal("expected error for unknown txID")
	} else if !errors.Is(err, spi.ErrTxNotFound) {
		t.Fatalf("expected ErrTxNotFound, got: %v", err)
	}
}

func TestPostgresReleaseSavepoint_RejectsCrossTenant(t *testing.T) {
	tm, _ := newTestTxManager(t)
	ctxA := ctxWithTenant("tenant-A")
	ctxB := ctxWithTenant("tenant-B")

	txAID, _, err := tm.Begin(ctxA)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	spID, err := tm.Savepoint(ctxA, txAID)
	if err != nil {
		t.Fatalf("Savepoint: %v", err)
	}

	if err := tm.ReleaseSavepoint(ctxB, txAID, spID); err == nil {
		t.Fatal("expected error when tenant B releases tenant A's savepoint")
	} else if !errors.Is(err, spi.ErrTxTenantMismatch) {
		t.Fatalf("expected ErrTxTenantMismatch, got: %v", err)
	}

	_ = tm.Rollback(ctxA, txAID)
}
