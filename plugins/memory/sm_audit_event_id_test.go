package memory_test

import (
	"testing"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// TestSMAudit_StoreAssignsEventID pins that the memory backend's
// StateMachineAuditStore.Record assigns the event id itself — a
// version-1 UUID — and ignores any TimeUUID the caller set.
func TestSMAudit_StoreAssignsEventID(t *testing.T) {
	f := memory.NewStoreFactory()
	defer func() { _ = f.Close() }()
	ctx := ctxWithTenant("tenant-evid")
	as, err := f.StateMachineAuditStore(ctx)
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
}
