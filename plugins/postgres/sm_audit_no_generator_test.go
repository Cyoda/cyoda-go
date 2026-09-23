package postgres_test

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

// TestSMAuditStore_Record_NoGenerator pins the fail-closed guard in
// smAuditStore.Record: a factory with no transaction manager (hence no
// spi.UUIDGenerator) wired in cannot record an event — it errors rather than
// falling back to a caller-supplied or improvised id — and nothing is
// written (see spi.StateMachineAuditStore).
func TestSMAuditStore_Record_NoGenerator(t *testing.T) {
	pool := newTestPool(t)
	if err := postgres.DropSchemaForTest(pool); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := postgres.Migrate(pool); err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	t.Cleanup(func() { _ = postgres.DropSchemaForTest(pool) })

	// No InitTransactionManager call: the factory has no generator.
	factory := postgres.NewStoreFactory(pool)
	ctx := ctxWithTenant("sm-tenant-no-gen")
	store, err := factory.StateMachineAuditStore(ctx)
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
