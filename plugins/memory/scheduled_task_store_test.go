package memory_test

import (
	"context"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

func TestMemory_ScheduledTaskStoreConformance(t *testing.T) {
	spi.RunScheduledTaskStoreConformance(t, func() spi.StoreFactory {
		return memory.NewStoreFactory()
	})
}

// TestMemory_ScheduledTaskArm_RollbackIsAtomic proves that a ScheduledTask
// Upsert staged inside a transaction never becomes visible if the
// transaction is rolled back — the staged op is discarded, not applied,
// matching the same-critical-section-as-entity-flush guarantee Commit
// provides for the committed path.
func TestMemory_ScheduledTaskArm_RollbackIsAtomic(t *testing.T) {
	factory, tm := newTxManager(t)
	ctx := ctxWithTenant("tenant-A")

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
