package memory

import (
	"context"
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// A commit that fails on an entity conflict applies none of its staged task
// ops and keeps none of them.
func TestTasks_AnEntityConflictDiscardsTheStagedTaskOps(t *testing.T) {
	f := NewStoreFactory()
	t.Cleanup(func() { _ = f.Close() })
	tm := f.NewTransactionManager(newTestUUIDGenerator())
	sts := &scheduledTaskStore{f: f}
	ctx := testCtxWithTenant("tenant-A")

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	spi.GetTransaction(txCtx).WriteSet["entity-E"] = true
	armReq := spi.ReconcileRequest{TenantID: "tenant-A", EntityID: "e1", CurrentState: "S", Arm: []spi.ScheduledTask{{
		ID: "e1:S:T", Type: spi.ScheduledTaskFireTransition, ScheduledTime: 1_000,
		ModelName: "M", ModelVersion: 1, Transition: "T", SourceState: "S",
	}}}
	if _, err := sts.ReconcileForEntity(txCtx, armReq); err != nil {
		t.Fatalf("staged arm: %v", err)
	}

	otherID, otherCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin other: %v", err)
	}
	other := spi.GetTransaction(otherCtx)
	other.WriteSet["entity-E"] = true
	other.Buffer["entity-E"] = &spi.Entity{Meta: spi.EntityMeta{ID: "entity-E", TenantID: "tenant-A", ChangeType: "CREATED"}, Data: []byte(`{}`)}
	if err := tm.Commit(ctx, otherID); err != nil {
		t.Fatalf("other Commit: %v", err)
	}

	if err := tm.Commit(ctx, txID); !errors.Is(err, spi.ErrConflict) {
		t.Fatalf("Commit = %v, want ErrConflict", err)
	}
	if _, ok, _ := sts.Get(context.Background(), "tenant-A", "e1:S:T"); ok {
		t.Fatal("the staged arm of a transaction that failed on an entity conflict was applied")
	}
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if ops, kept := tm.scheduledTaskOps[txID]; kept {
		t.Fatalf("the failed transaction's %d staged task ops were kept", len(ops))
	}
}
