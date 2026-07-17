package scheduler

import (
	"context"
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// TestSystemUserContext_HasTenant proves SystemUserContext synthesises a
// real identity — not just a struct that happens to have a Tenant field —
// by driving it through the actual TransactionManager.Begin gate that
// rejects a missing tenant (plugins/memory/txmanager.go Begin). Without
// this identity, the scheduler's background fire (which has no
// caller-derived UserContext at all) could never open a transaction.
func TestSystemUserContext_HasTenant(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)

	ctx := SystemUserContext(spi.TenantID("tenant-x"))

	uc := spi.GetUserContext(ctx)
	if uc == nil {
		t.Fatal("SystemUserContext must attach a UserContext")
	}
	if uc.UserID != "scheduler" {
		t.Errorf("UserID = %q, want %q", uc.UserID, "scheduler")
	}
	if uc.Tenant.ID != "tenant-x" {
		t.Errorf("Tenant.ID = %q, want %q", uc.Tenant.ID, "tenant-x")
	}

	txID, _, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin(SystemUserContext(...)) failed: %v", err)
	}
	if txID == "" {
		t.Error("expected a non-empty txID")
	}

	// Prove the identity is load-bearing, not incidental: Begin rejects a
	// context with no tenant at all — exactly what the scan loop's own
	// context.Background() would produce without this helper.
	if _, _, err := txMgr.Begin(context.Background()); err == nil {
		t.Error("Begin(context.Background()) should fail with no user context/tenant")
	}
}

// fakeEngine is a scheduler.Engine test double that records what it was
// called with, so tests can assert LocalExecutor built the right context
// and passed the task through untouched.
type fakeEngine struct {
	calls   int
	gotCtx  context.Context
	gotTask spi.ScheduledTask
	outcome string
	err     error
}

func (f *fakeEngine) FireScheduledTransition(ctx context.Context, task spi.ScheduledTask) (string, error) {
	f.calls++
	f.gotCtx = ctx
	f.gotTask = task
	return f.outcome, f.err
}

func TestLocalExecutor_FiresViaEngineWithSystemContext(t *testing.T) {
	fake := &fakeEngine{outcome: "fired"}
	exec := NewLocalExecutor(fake)

	task := spi.ScheduledTask{ID: "t1", TenantID: spi.TenantID("tenant-x"), EntityID: "e1"}
	exec.Execute(context.Background(), task, "self")

	if fake.calls != 1 {
		t.Fatalf("expected 1 engine call, got %d", fake.calls)
	}
	if fake.gotTask.ID != "t1" {
		t.Errorf("task not passed through unchanged: got %+v", fake.gotTask)
	}
	uc := spi.GetUserContext(fake.gotCtx)
	if uc == nil {
		t.Fatal("engine did not receive a UserContext at all")
	}
	if uc.Tenant.ID != "tenant-x" {
		t.Errorf("engine's context tenant = %q, want %q", uc.Tenant.ID, "tenant-x")
	}
}

func TestLocalExecutor_ErrorDoesNotPanic(t *testing.T) {
	fake := &fakeEngine{err: errors.New("boom")}
	exec := NewLocalExecutor(fake)

	// Must not panic even though the engine reports an error.
	exec.Execute(context.Background(), spi.ScheduledTask{ID: "t2", TenantID: "tenant-x"}, "self")

	if fake.calls != 1 {
		t.Fatalf("expected the engine to still be invoked once, got %d", fake.calls)
	}
}
