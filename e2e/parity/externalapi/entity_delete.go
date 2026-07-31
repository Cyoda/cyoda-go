package externalapi

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/externalapi/driver"
	"github.com/cyoda-platform/cyoda-go/e2e/parity"
)

func init() {
	parity.Register(
		parity.NamedTest{Name: "ExternalAPI_06_01_DeleteSingle", Fn: RunExternalAPI_06_01_DeleteSingle},
		parity.NamedTest{Name: "ExternalAPI_06_02_DeleteByModel", Fn: RunExternalAPI_06_02_DeleteByModel},
		parity.NamedTest{Name: "ExternalAPI_06_06_DeleteAtPointInTime", Fn: RunExternalAPI_06_06_DeleteAtPointInTime},
	)
}

// RunExternalAPI_06_01_DeleteSingle — dictionary 06/01.
// Register delone/1, lock, create one entity. Delete by ID via d.DeleteEntity(id).
// After delete, d.GetEntity(id) must error (the entity is gone).
func RunExternalAPI_06_01_DeleteSingle(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)
	if err := d.CreateModelFromSample("delone", 1, `{"k":1}`); err != nil {
		t.Fatalf("CreateModelFromSample: %v", err)
	}
	if err := d.LockModel("delone", 1); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	id, err := d.CreateEntity("delone", 1, `{"k":1}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	if err := d.DeleteEntity(id); err != nil {
		t.Fatalf("DeleteEntity: %v", err)
	}
	// GetEntity should now error — the entity is gone.
	if _, err := d.GetEntity(id); err == nil {
		t.Fatal("expected GetEntity to fail after delete")
	}
}

// RunExternalAPI_06_02_DeleteByModel — dictionary 06/02.
// Register delmany/1, lock, create 5 entities. Call DeleteEntitiesByModel.
// Verify ListEntitiesByModel returns 0 afterwards.
func RunExternalAPI_06_02_DeleteByModel(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)
	if err := d.CreateModelFromSample("delmany", 1, `{"k":1}`); err != nil {
		t.Fatalf("CreateModelFromSample: %v", err)
	}
	if err := d.LockModel("delmany", 1); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	for i := 0; i < 5; i++ {
		if _, err := d.CreateEntity("delmany", 1, `{"k":1}`); err != nil {
			t.Fatalf("CreateEntity[%d]: %v", i, err)
		}
	}
	if err := d.DeleteEntitiesByModel("delmany", 1); err != nil {
		t.Fatalf("DeleteEntitiesByModel: %v", err)
	}
	list, err := d.ListEntitiesByModel("delmany", 1)
	if err != nil {
		t.Fatalf("ListEntitiesByModel: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("after delete-by-model: got %d entities, want 0", len(list))
	}
}

// RunExternalAPI_06_06_DeleteAtPointInTime — dictionary 06/06.
//
// delete-all-by-model with pointInTime=T1 selectively removes only
// entities created at or before T1, leaving newer entities intact.
//
// Skipped pending #124. The OpenAPI declares
// DeleteEntitiesParams.PointInTime, but internal/domain/entity.Handler.DeleteEntities
// ignores it and the storage SPI has no DeleteAllAsAt method. Cross-repo
// fix targeted for v0.7.0 (SPI bump + plugin impls + handler wiring).
// The test body below is the contract for when that gap closes — remove
// the t.Skip and the assertion exercises pointInTime selectivity across
// all backends.
func RunExternalAPI_06_06_DeleteAtPointInTime(t *testing.T, fixture parity.BackendFixture) {
	t.Skip("pending #124 — DeleteEntities handler ignores params.PointInTime; v0.7.0 SPI bump required")
	d := driver.NewInProcess(t, fixture)
	if err := d.CreateModelFromSample("delpit", 1, `{"k":1}`); err != nil {
		t.Fatalf("CreateModelFromSample: %v", err)
	}
	if err := d.LockModel("delpit", 1); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	// Phase A: 3 entities created before T1.
	var lastPhaseA uuid.UUID
	for i := 0; i < 3; i++ {
		id, err := d.CreateEntity("delpit", 1, `{"k":1}`)
		if err != nil {
			t.Fatalf("CreateEntity[before-T1][%d]: %v", i, err)
		}
		lastPhaseA = id
	}
	// T1 is the last phase-A write's own server-stamped time — inclusive <=
	// covers all three. Taking it from time.Now() would compare the test's
	// clock against server-stamped versions (see e2e/parity/pit_time.go).
	t1 := latestChangeTime(t, d, lastPhaseA)
	// Ensure observable temporal delta. Server timestamps are usually
	// millisecond-precision; sleep beyond that.
	time.Sleep(100 * time.Millisecond)
	// Phase B: 2 entities created after T1.
	for i := 0; i < 2; i++ {
		if _, err := d.CreateEntity("delpit", 1, `{"k":1}`); err != nil {
			t.Fatalf("CreateEntity[after-T1][%d]: %v", i, err)
		}
	}

	// Delete with pointInTime=T1 — only the 3 phase-A entities should go.
	if err := d.DeleteEntitiesByModelAt("delpit", 1, t1); err != nil {
		t.Fatalf("DeleteEntitiesByModelAt: %v", err)
	}

	// Phase B's 2 entities must remain.
	list, err := d.ListEntitiesByModel("delpit", 1)
	if err != nil {
		t.Fatalf("ListEntitiesByModel: %v", err)
	}
	if len(list) != 2 {
		t.Errorf("after delete-at-T1: got %d entities, want 2 (only post-T1 entities should remain)", len(list))
	}
}
