package postgres_test

import (
	"strings"
	"testing"
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
	} else if !strings.Contains(err.Error(), "tenant mismatch") {
		t.Fatalf("expected tenant-mismatch error, got: %v", err)
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
	} else if !strings.Contains(err.Error(), "tenant mismatch") {
		t.Fatalf("expected tenant-mismatch error, got: %v", err)
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
	} else if !strings.Contains(err.Error(), "tenant mismatch") {
		t.Fatalf("expected tenant-mismatch error, got: %v", err)
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
	} else if !strings.Contains(err.Error(), "tenant mismatch") {
		t.Fatalf("expected tenant-mismatch error, got: %v", err)
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
	} else if !strings.Contains(err.Error(), "tenant mismatch") {
		t.Fatalf("expected tenant-mismatch error, got: %v", err)
	}

	_ = tm.Rollback(ctxA, txAID)
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
	} else if !strings.Contains(err.Error(), "tenant mismatch") {
		t.Fatalf("expected tenant-mismatch error, got: %v", err)
	}

	_ = tm.Rollback(ctxA, txAID)
}
