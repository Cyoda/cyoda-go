//go:build !short

package driver_test

import (
	"testing"

	"github.com/cyoda/cyoda-go/e2e/externalapi/driver"
	"github.com/cyoda/cyoda-go/e2e/parity/memory"
)

// TestDriverRemoteModeSmoke proves the NewRemote path has no dependency
// on parity.BackendFixture: we boot the memory fixture to get a BaseURL
// + JWT, then hand only those two values to NewRemote and replay a
// concrete scenario spine (create model → lock → create entity →
// delete entity). A regression where the driver reaches back into
// fixture state would surface as a compile failure or runtime panic.
func TestDriverRemoteModeSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("smoke test requires subprocess launch; skipping in short mode")
	}

	fx, cleanup := memory.MustSetup(t)
	defer cleanup()

	baseURL := fx.BaseURL()
	tenant := fx.NewTenant(t)

	d := driver.NewRemote(t, baseURL, tenant.Token)

	if err := d.CreateModelFromSample("smoke_rm", 1, `{"n":1}`); err != nil {
		t.Fatalf("CreateModelFromSample: %v", err)
	}
	if err := d.LockModel("smoke_rm", 1); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	id, err := d.CreateEntity("smoke_rm", 1, `{"n":1}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	if err := d.DeleteEntity(id); err != nil {
		t.Fatalf("DeleteEntity: %v", err)
	}
}
