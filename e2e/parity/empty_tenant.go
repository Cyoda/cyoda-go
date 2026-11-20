package parity

import (
	"testing"

	"github.com/google/uuid"

	"github.com/cyoda/cyoda-go/e2e/parity/client"
)

// RunEmptyTenantOperations exercises the read-side API surface against a
// freshly minted tenant with zero data. Catches null-pointer and empty-
// collection divergences across storage backends.
func RunEmptyTenantOperations(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	// 1. GET /api/model/ -- empty array, not an error.
	models, err := c.ListModels(t)
	if err != nil {
		t.Fatalf("ListModels on empty tenant: %v", err)
	}
	if len(models) != 0 {
		t.Errorf("ListModels on empty tenant: expected 0 models, got %d", len(models))
	}

	// 2. GET /api/entity/stats -- should return zero/empty, not 500.
	statsStatus, err := c.GetEntityStatsRaw(t)
	if err != nil {
		t.Fatalf("GetEntityStats on empty tenant: %v", err)
	}
	if statsStatus != 200 {
		t.Errorf("GetEntityStats: expected 200, got %d", statsStatus)
	}

	// 3. GET /api/audit/entity/{nonexistent-uuid} -- 404 or empty items.
	// Should NOT return 500.
	auditStatus, auditErr := c.GetAuditEventsRaw(t, uuid.New())
	if auditStatus == 500 {
		t.Errorf("GetAuditEvents for nonexistent entity returned 500 (should be 404 or empty 200): %v", auditErr)
	}
}
