package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

// TestSMAudit_StoreAssignsEventID pins that the sqlite backend's
// StateMachineAuditStore.Record assigns the event id itself — a
// version-1 UUID — ignores any TimeUUID the caller set, and that the id
// read back is the event_id column the row was stored under.
func TestSMAudit_StoreAssignsEventID(t *testing.T) {
	dir := t.TempDir()
	factory, err := sqlite.NewStoreFactoryForTest(context.Background(), filepath.Join(dir, "audit-evid.db"))
	if err != nil {
		t.Fatalf("NewStoreFactoryForTest: %v", err)
	}
	defer factory.Close()

	tenant := spi.TenantID("tenant-evid")
	ctx := testCtx(string(tenant))
	as, err := factory.StateMachineAuditStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
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

	db := sqlite.DBForTest(factory)
	var col string
	if err := db.QueryRow(`SELECT event_id FROM sm_audit_events WHERE tenant_id = ? AND entity_id = ?`,
		string(tenant), "e-1").Scan(&col); err != nil {
		t.Fatal(err)
	}
	if col != evs[0].TimeUUID {
		t.Fatalf("read id %q != stored event_id %q", evs[0].TimeUUID, col)
	}
}
