package scheduler

import (
	"context"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// TestSystemUserContext_HasTenant proves common.SystemUserContext synthesises a
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

	ctx := common.SystemUserContext(spi.TenantID("tenant-x"))

	uc := spi.GetUserContext(ctx)
	if uc == nil {
		t.Fatal("common.SystemUserContext must attach a UserContext")
	}
	if uc.UserID != "system" {
		t.Errorf("UserID = %q, want %q", uc.UserID, "system")
	}
	if uc.Kind != spi.PrincipalSystem {
		t.Errorf("Kind = %q, want %q", uc.Kind, spi.PrincipalSystem)
	}
	if uc.Tenant.ID != "tenant-x" {
		t.Errorf("Tenant.ID = %q, want %q", uc.Tenant.ID, "tenant-x")
	}

	txID, _, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin(common.SystemUserContext(...)) failed: %v", err)
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
