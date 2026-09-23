package postgres_test

import (
	"testing"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

// TestSMAudit_StoreAssignsEventID pins that the postgres backend's
// StateMachineAuditStore.Record assigns the event id itself — a
// version-1 UUID — ignores any TimeUUID the caller set, and that the id
// read back is the event_id column the row was stored under.
func TestSMAudit_StoreAssignsEventID(t *testing.T) {
	factory := setupSMAuditTest(t)
	tenant := spi.TenantID("tenant-evid")
	ctx := ctxWithTenant(tenant)
	as := getSMAuditStore(t, factory, tenant)

	if err := as.Record(ctx, "e-1", spi.StateMachineEvent{EventType: spi.SMEventStarted, EntityID: "e-1", TimeUUID: "caller"}); err != nil {
		t.Fatal(err)
	}
	evs, err := as.GetEvents(ctx, "e-1")
	if err != nil || len(evs) != 1 {
		t.Fatalf("GetEvents = %v, %v", evs, err)
	}
	id, perr := uuid.Parse(evs[0].TimeUUID)
	if perr != nil || id.Version() != 1 {
		t.Fatalf("event id %q: want a version-1 UUID assigned by the store", evs[0].TimeUUID)
	}

	pool := postgres.PoolForTest(factory)
	var col string
	if err := pool.QueryRow(ctx,
		`SELECT event_id FROM sm_audit_events WHERE tenant_id = $1 AND entity_id = $2`,
		string(tenant), "e-1").Scan(&col); err != nil {
		t.Fatal(err)
	}
	if col != evs[0].TimeUUID {
		t.Fatalf("read id %q != stored event_id %q", evs[0].TimeUUID, col)
	}
}
