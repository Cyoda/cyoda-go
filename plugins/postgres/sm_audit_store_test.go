package postgres_test

import (
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

func setupSMAuditTest(t *testing.T) *postgres.StoreFactory {
	t.Helper()
	pool := newTestPool(t)
	if err := postgres.DropSchemaForTest(pool); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := postgres.Migrate(pool); err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	t.Cleanup(func() { _ = postgres.DropSchemaForTest(pool) })
	return postgres.NewStoreFactory(pool)
}

func getSMAuditStore(t *testing.T, factory *postgres.StoreFactory, tid spi.TenantID) spi.StateMachineAuditStore {
	t.Helper()
	ctx := ctxWithTenant(tid)
	store, err := factory.StateMachineAuditStore(ctx)
	if err != nil {
		t.Fatalf("StateMachineAuditStore: %v", err)
	}
	return store
}

func makeEvent(eventType spi.StateMachineEventType, entityID, timeUUID, state, txID, details string, ts time.Time) spi.StateMachineEvent {
	return spi.StateMachineEvent{
		EventType:     eventType,
		EntityID:      entityID,
		TimeUUID:      timeUUID,
		State:         state,
		TransactionID: txID,
		Details:       details,
		Timestamp:     ts,
	}
}

func TestSMAuditStore_RecordAndGetEvents(t *testing.T) {
	factory := setupSMAuditTest(t)
	ctx := ctxWithTenant("sm-tenant")
	store := getSMAuditStore(t, factory, "sm-tenant")

	now := time.Now().UTC().Truncate(time.Microsecond)
	e1 := makeEvent(spi.SMEventStarted, "entity-1", "uuid-1", "NEW", "tx-1", "started", now)
	e2 := makeEvent(spi.SMEventTransitionMade, "entity-1", "uuid-2", "ACTIVE", "tx-1", "transitioned", now.Add(time.Second))

	if err := store.Record(ctx, "entity-1", e1); err != nil {
		t.Fatalf("Record e1: %v", err)
	}
	if err := store.Record(ctx, "entity-1", e2); err != nil {
		t.Fatalf("Record e2: %v", err)
	}

	events, err := store.GetEvents(ctx, "entity-1")
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}

	// Verify order (ascending by timestamp)
	if events[0].TimeUUID != "uuid-1" {
		t.Errorf("expected first event uuid-1, got %s", events[0].TimeUUID)
	}
	if events[1].TimeUUID != "uuid-2" {
		t.Errorf("expected second event uuid-2, got %s", events[1].TimeUUID)
	}

	// Verify field preservation
	if events[0].EventType != spi.SMEventStarted {
		t.Errorf("expected SMEventStarted, got %s", events[0].EventType)
	}
	if events[0].State != "NEW" {
		t.Errorf("expected state NEW, got %s", events[0].State)
	}
	if events[0].TransactionID != "tx-1" {
		t.Errorf("expected transactionID tx-1, got %s", events[0].TransactionID)
	}
	if events[0].Details != "started" {
		t.Errorf("expected details 'started', got %s", events[0].Details)
	}
}

func TestSMAuditStore_GetEventsNotFound(t *testing.T) {
	factory := setupSMAuditTest(t)
	ctx := ctxWithTenant("sm-tenant")
	store := getSMAuditStore(t, factory, "sm-tenant")

	// SPI contract: GetEvents for an entity with no events returns empty slice, not error.
	events, err := store.GetEvents(ctx, "nonexistent-entity")
	if err != nil {
		t.Fatalf("expected nil error for entity with no events, got: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("expected empty slice for entity with no events, got %d events", len(events))
	}
}

func TestSMAuditStore_GetEventsByTransaction(t *testing.T) {
	factory := setupSMAuditTest(t)
	ctx := ctxWithTenant("sm-tenant")
	store := getSMAuditStore(t, factory, "sm-tenant")

	now := time.Now().UTC().Truncate(time.Microsecond)
	e1 := makeEvent(spi.SMEventStarted, "entity-1", "uuid-1", "NEW", "tx-A", "e1", now)
	e2 := makeEvent(spi.SMEventTransitionMade, "entity-1", "uuid-2", "ACTIVE", "tx-A", "e2", now.Add(time.Second))
	e3 := makeEvent(spi.SMEventFinished, "entity-1", "uuid-3", "DONE", "tx-B", "e3", now.Add(2*time.Second))

	for _, e := range []spi.StateMachineEvent{e1, e2, e3} {
		if err := store.Record(ctx, "entity-1", e); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}

	events, err := store.GetEventsByTransaction(ctx, "entity-1", "tx-A")
	if err != nil {
		t.Fatalf("GetEventsByTransaction: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events for tx-A, got %d", len(events))
	}
	if events[0].TimeUUID != "uuid-1" || events[1].TimeUUID != "uuid-2" {
		t.Errorf("unexpected event order: %v, %v", events[0].TimeUUID, events[1].TimeUUID)
	}
}

func TestSMAuditStore_GetEventsByTransaction_NoMatch(t *testing.T) {
	factory := setupSMAuditTest(t)
	ctx := ctxWithTenant("sm-tenant")
	store := getSMAuditStore(t, factory, "sm-tenant")

	now := time.Now().UTC().Truncate(time.Microsecond)
	e1 := makeEvent(spi.SMEventStarted, "entity-1", "uuid-1", "NEW", "tx-A", "started", now)

	if err := store.Record(ctx, "entity-1", e1); err != nil {
		t.Fatalf("Record: %v", err)
	}

	events, err := store.GetEventsByTransaction(ctx, "entity-1", "tx-B")
	if err != nil {
		t.Fatalf("GetEventsByTransaction with no match should return nil error, got: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events for tx-B, got %d", len(events))
	}
}

func TestSMAuditStore_TenantIsolation(t *testing.T) {
	factory := setupSMAuditTest(t)
	ctxA := ctxWithTenant("tenant-A")
	ctxB := ctxWithTenant("tenant-B")

	storeA, err := factory.StateMachineAuditStore(ctxA)
	if err != nil {
		t.Fatalf("StateMachineAuditStore for A: %v", err)
	}
	storeB, err := factory.StateMachineAuditStore(ctxB)
	if err != nil {
		t.Fatalf("StateMachineAuditStore for B: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	e := makeEvent(spi.SMEventStarted, "entity-1", "uuid-1", "NEW", "tx-1", "tenant-A event", now)

	if err := storeA.Record(ctxA, "entity-1", e); err != nil {
		t.Fatalf("Record for tenant-A: %v", err)
	}

	// Tenant-B should not see tenant-A's events — SPI contract: isolation via
	// empty slice return, not an error.
	eventsB, err := storeB.GetEvents(ctxB, "entity-1")
	if err != nil {
		t.Fatalf("expected nil error for tenant-B (isolation), got: %v", err)
	}
	if len(eventsB) != 0 {
		t.Fatalf("tenant-B should see 0 events (tenant isolation), got %d", len(eventsB))
	}
}

func TestSMAuditStore_EventDataPreservation(t *testing.T) {
	factory := setupSMAuditTest(t)
	ctx := ctxWithTenant("sm-tenant")
	store := getSMAuditStore(t, factory, "sm-tenant")

	now := time.Now().UTC().Truncate(time.Microsecond)
	e := spi.StateMachineEvent{
		EventType:     spi.SMEventStateProcessResult,
		EntityID:      "entity-1",
		TimeUUID:      "uuid-data",
		State:         "PROCESSING",
		TransactionID: "tx-data",
		Details:       "data test",
		Timestamp:     now,
		Data: map[string]any{
			"stringField": "hello",
			"numberField": float64(42),
			"boolField":   true,
		},
	}

	if err := store.Record(ctx, "entity-1", e); err != nil {
		t.Fatalf("Record: %v", err)
	}

	events, err := store.GetEvents(ctx, "entity-1")
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	got := events[0]
	if got.Data == nil {
		t.Fatal("expected non-nil Data map")
	}

	if v, ok := got.Data["stringField"].(string); !ok || v != "hello" {
		t.Errorf("stringField: expected 'hello', got %v (%T)", got.Data["stringField"], got.Data["stringField"])
	}
	if v, ok := got.Data["numberField"].(float64); !ok || v != 42 {
		t.Errorf("numberField: expected float64(42), got %v (%T)", got.Data["numberField"], got.Data["numberField"])
	}
	if v, ok := got.Data["boolField"].(bool); !ok || !v {
		t.Errorf("boolField: expected true, got %v (%T)", got.Data["boolField"], got.Data["boolField"])
	}
}
