package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

func TestSqlite_ScheduledTaskStoreConformance(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "conformance.db")
	spi.RunScheduledTaskStoreConformance(t, func() spi.StoreFactory {
		f, err := sqlite.NewStoreFactoryForTest(context.Background(), dbPath)
		if err != nil {
			t.Fatalf("NewStoreFactoryForTest failed: %v", err)
		}
		return f
	})
}

// TestSqlite_ScheduledTaskArm_RollbackIsAtomic proves that a ScheduledTask
// Upsert staged inside a transaction never becomes visible if the
// transaction is rolled back — the staged op is discarded, not flushed,
// matching the same-sqlTx-as-entity-flush guarantee flushToSQLite provides
// for the committed path.
func TestSqlite_ScheduledTaskArm_RollbackIsAtomic(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "rollback.db")
	factory, err := sqlite.NewStoreFactoryForTest(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("NewStoreFactoryForTest failed: %v", err)
	}
	t.Cleanup(func() { _ = factory.Close() })

	ctx := testCtx("tenant-A")
	tm, err := factory.TransactionManager(ctx)
	if err != nil {
		t.Fatalf("TransactionManager failed: %v", err)
	}

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin failed: %v", err)
	}

	sts, err := factory.ScheduledTaskStore(txCtx)
	if err != nil {
		t.Fatalf("ScheduledTaskStore: %v", err)
	}

	task := spi.ScheduledTask{
		ID:            "e1:S:T",
		TenantID:      "tenant-A",
		Type:          spi.ScheduledTaskFireTransition,
		ScheduledTime: 1000,
		EntityID:      "e1",
		SourceState:   "S",
		Transition:    "T",
	}
	if err := sts.Upsert(txCtx, task); err != nil {
		t.Fatalf("Upsert failed: %v", err)
	}

	if err := tm.Rollback(ctx, txID); err != nil {
		t.Fatalf("Rollback failed: %v", err)
	}

	// Read outside any transaction — the staged arm must never have reached
	// committed state.
	if _, found, err := sts.Get(context.Background(), "e1:S:T"); err != nil {
		t.Fatalf("Get failed: %v", err)
	} else if found {
		t.Fatal("expected scheduled task to be absent after rollback (staged op must be discarded, not applied)")
	}
}

// TestSqlite_ScheduledTaskArm_SavepointTruncates proves that a ScheduledTask
// Upsert staged AFTER a savepoint is discarded by RollbackToSavepoint, while
// an Upsert staged BEFORE the savepoint survives the rollback and still
// commits. transactionManager.scheduledTaskOps must be savepoint-scoped the
// same way tx.Buffer/ReadSet/WriteSet/Deletes are — a staged op surviving a
// RollbackToSavepoint would orphan it from the entity work it must be atomic
// with.
func TestSqlite_ScheduledTaskArm_SavepointTruncates(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "savepoint.db")
	factory, err := sqlite.NewStoreFactoryForTest(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("NewStoreFactoryForTest failed: %v", err)
	}
	t.Cleanup(func() { _ = factory.Close() })

	ctx := testCtx("tenant-A")
	tm, err := factory.TransactionManager(ctx)
	if err != nil {
		t.Fatalf("TransactionManager failed: %v", err)
	}

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin failed: %v", err)
	}

	sts, err := factory.ScheduledTaskStore(txCtx)
	if err != nil {
		t.Fatalf("ScheduledTaskStore: %v", err)
	}

	first := spi.ScheduledTask{
		ID:            "e1:S:T1",
		TenantID:      "tenant-A",
		Type:          spi.ScheduledTaskFireTransition,
		ScheduledTime: 1000,
		EntityID:      "e1",
		SourceState:   "S",
		Transition:    "T1",
	}
	if err := sts.Upsert(txCtx, first); err != nil {
		t.Fatalf("Upsert (first) failed: %v", err)
	}

	spID, err := tm.Savepoint(ctx, txID)
	if err != nil {
		t.Fatalf("Savepoint failed: %v", err)
	}

	second := first
	second.ID = "e1:S:T2"
	second.Transition = "T2"
	if err := sts.Upsert(txCtx, second); err != nil {
		t.Fatalf("Upsert (second) failed: %v", err)
	}

	if err := tm.RollbackToSavepoint(ctx, txID, spID); err != nil {
		t.Fatalf("RollbackToSavepoint failed: %v", err)
	}

	if err := tm.Commit(ctx, txID); err != nil {
		t.Fatalf("Commit failed: %v", err)
	}

	if _, found, err := sts.Get(context.Background(), first.ID); err != nil {
		t.Fatalf("Get(first) failed: %v", err)
	} else if !found {
		t.Fatal("expected task staged before the savepoint to survive commit")
	}

	if _, found, err := sts.Get(context.Background(), second.ID); err != nil {
		t.Fatalf("Get(second) failed: %v", err)
	} else if found {
		t.Fatal("expected task staged after the savepoint to be discarded by RollbackToSavepoint, not committed")
	}
}
