package sqlite_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

// Tenant-isolation regression tests for the sqlite plugin's three savepoint
// methods. Issue #199 PR-C1: pre-fix Savepoint, RollbackToSavepoint, and
// ReleaseSavepoint took _ context.Context and never compared the caller's
// tenant against tx.TenantID. A caller authenticated as tenant A who learned
// a tenant B txID could record / rollback / release savepoints on tenant B's
// tx-state. Mirrors the gap PR-A closed in the memory plugin.

func newTxMgrForTenantTest(t *testing.T) (*sqlite.StoreFactory, context.Context) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "tenant.db")
	factory, err := sqlite.NewStoreFactoryForTest(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("NewStoreFactoryForTest failed: %v", err)
	}
	t.Cleanup(func() { _ = factory.Close() })
	return factory, testCtx("tenant-A")
}

func TestSqliteSavepoint_RejectsCrossTenant(t *testing.T) {
	factory, ctxA := newTxMgrForTenantTest(t)
	ctxB := testCtx("tenant-B")
	tm, err := factory.TransactionManager(ctxA)
	if err != nil {
		t.Fatalf("TransactionManager failed: %v", err)
	}

	txAID, _, err := tm.Begin(ctxA)
	if err != nil {
		t.Fatalf("Begin failed: %v", err)
	}

	if _, err := tm.Savepoint(ctxB, txAID); err == nil {
		t.Fatal("expected error when tenant B takes savepoint on tenant A's tx")
	} else if !strings.Contains(err.Error(), "tenant mismatch") {
		t.Fatalf("expected tenant-mismatch error, got: %v", err)
	}

	_ = tm.Rollback(ctxA, txAID)
}

func TestSqliteRollbackToSavepoint_RejectsCrossTenant(t *testing.T) {
	factory, ctxA := newTxMgrForTenantTest(t)
	ctxB := testCtx("tenant-B")
	tm, err := factory.TransactionManager(ctxA)
	if err != nil {
		t.Fatalf("TransactionManager failed: %v", err)
	}

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
	} else if !strings.Contains(err.Error(), "tenant mismatch") {
		t.Fatalf("expected tenant-mismatch error, got: %v", err)
	}

	_ = tm.Rollback(ctxA, txAID)
}

func TestSqliteReleaseSavepoint_RejectsCrossTenant(t *testing.T) {
	factory, ctxA := newTxMgrForTenantTest(t)
	ctxB := testCtx("tenant-B")
	tm, err := factory.TransactionManager(ctxA)
	if err != nil {
		t.Fatalf("TransactionManager failed: %v", err)
	}

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
	} else if !strings.Contains(err.Error(), "tenant mismatch") {
		t.Fatalf("expected tenant-mismatch error, got: %v", err)
	}

	_ = tm.Rollback(ctxA, txAID)
}
