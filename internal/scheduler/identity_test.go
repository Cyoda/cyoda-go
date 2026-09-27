package scheduler

import (
	"context"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// TestSystemUserContextValue_HasTenant proves the context the scheduler builds
// from common.SystemUserContextValue (its run and failure-audit contexts)
// carries a real identity — not just a struct that happens to have a Tenant
// field — by driving it through the actual TransactionManager.Begin gate that
// rejects a missing tenant (plugins/memory/txmanager.go Begin). Without
// this identity, the scheduler's background fire (which has no
// caller-derived UserContext at all) could never open a transaction.
func TestSystemUserContextValue_HasTenant(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)

	ctx := spi.WithUserContext(context.Background(), common.SystemUserContextValue(spi.TenantID("tenant-x")))

	uc := spi.GetUserContext(ctx)
	if uc == nil {
		t.Fatal("the system context must carry a UserContext")
	}
	if want := common.SystemPrincipal(); uc.UserID != want.ID || uc.Kind != want.Kind {
		t.Errorf("identity = (%q, %q), want the system principal (%q, %q)", uc.UserID, uc.Kind, want.ID, want.Kind)
	}
	if uc.Tenant.ID != "tenant-x" {
		t.Errorf("Tenant.ID = %q, want %q", uc.Tenant.ID, "tenant-x")
	}

	txID, _, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin(system context) failed: %v", err)
	}
	if txID == "" {
		t.Error("expected a non-empty txID")
	}

	// Prove the identity is load-bearing, not incidental: Begin rejects a
	// context with no tenant at all — exactly what the claim loop's own
	// context.Background() would produce without this helper.
	if _, _, err := txMgr.Begin(context.Background()); err == nil {
		t.Error("Begin(context.Background()) should fail with no user context/tenant")
	}
}
