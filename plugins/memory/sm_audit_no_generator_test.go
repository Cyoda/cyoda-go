package memory_test

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// TestSMAuditStore_Record_NoGenerator pins the fail-closed guard in
// StateMachineAuditStore.Record: a factory with no spi.UUIDGenerator wired in
// cannot record an event — it errors rather than falling back to a
// caller-supplied or improvised id — and nothing is written (see
// spi.StateMachineAuditStore). NewStoreFactory always wires a generator, so
// the zero-value *memory.StoreFactory{} — never otherwise reachable through
// the package's own constructors — is the cheap way to observe the guard.
func TestSMAuditStore_Record_NoGenerator(t *testing.T) {
	f := &memory.StoreFactory{}
	ctx := ctxWithTenant("sm-tenant-no-gen")
	store, err := f.StateMachineAuditStore(ctx)
	if err != nil {
		t.Fatalf("StateMachineAuditStore: %v", err)
	}

	if err := store.Record(ctx, "entity-1", spi.StateMachineEvent{
		EventType: spi.SMEventStarted, EntityID: "entity-1", State: "NEW",
	}); err == nil {
		t.Fatal("Record with no generator configured: want an error, got nil")
	}

	events, err := store.GetEvents(ctx, "entity-1")
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("Record errored but %d events were recorded; want 0", len(events))
	}
}
