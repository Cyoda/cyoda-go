package memory_test

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

func TestSMAuditRecord(t *testing.T) {
	factory := memory.NewStoreFactory()
	ctx := ctxWithTenant("tenant-A")
	store, err := factory.StateMachineAuditStore(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	events := []spi.StateMachineEvent{
		{EventType: spi.SMEventStarted, EntityID: "e-1", Details: "started", TransactionID: "tx-1"},
		{EventType: spi.SMEventTransitionMade, EntityID: "e-1", State: "APPROVED", Details: "transition", TransactionID: "tx-1"},
		{EventType: spi.SMEventFinished, EntityID: "e-1", Details: "done", TransactionID: "tx-1"},
	}
	for _, ev := range events {
		if err := store.Record(ctx, "e-1", ev); err != nil {
			t.Fatalf("record failed: %v", err)
		}
	}

	got, err := store.GetEvents(ctx, "e-1")
	if err != nil {
		t.Fatalf("get events failed: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 events, got %d", len(got))
	}
	if got[0].EventType != spi.SMEventStarted {
		t.Errorf("expected first event START, got %s", got[0].EventType)
	}
	if got[1].State != "APPROVED" {
		t.Errorf("expected state APPROVED, got %s", got[1].State)
	}

	// Verify deep copy — mutating returned value must not affect stored data.
	got[0].Details = "mutated"
	got2, _ := store.GetEvents(ctx, "e-1")
	if got2[0].Details == "mutated" {
		t.Error("store returned a shallow copy — mutation leaked")
	}
}

func TestSMAuditFilterByTransaction(t *testing.T) {
	factory := memory.NewStoreFactory()
	ctx := ctxWithTenant("tenant-A")
	store, _ := factory.StateMachineAuditStore(ctx)

	_ = store.Record(ctx, "e-1", spi.StateMachineEvent{EventType: spi.SMEventStarted, TransactionID: "tx-1", Details: "a"})
	_ = store.Record(ctx, "e-1", spi.StateMachineEvent{EventType: spi.SMEventTransitionMade, TransactionID: "tx-2", Details: "b"})
	_ = store.Record(ctx, "e-1", spi.StateMachineEvent{EventType: spi.SMEventFinished, TransactionID: "tx-1", Details: "c"})

	got, err := store.GetEventsByTransaction(ctx, "e-1", "tx-1")
	if err != nil {
		t.Fatalf("filter failed: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 events for tx-1, got %d", len(got))
	}
	if got[0].Details != "a" || got[1].Details != "c" {
		t.Errorf("unexpected filtered events: %+v", got)
	}

	got2, err := store.GetEventsByTransaction(ctx, "e-1", "tx-2")
	if err != nil {
		t.Fatalf("filter failed: %v", err)
	}
	if len(got2) != 1 {
		t.Fatalf("expected 1 event for tx-2, got %d", len(got2))
	}
}

func TestSMAuditFilterByTransaction_NoMatch_NonNil(t *testing.T) {
	// Regression: GetEventsByTransaction must return a non-nil empty slice when
	// events exist for the (tenant, entity) but none match the given transactionID.
	// A nil return would break callers that rely on len(result) == 0 without a
	// nil guard, and diverges from the non-nil contract of the other return paths.
	factory := memory.NewStoreFactory()
	ctx := ctxWithTenant("tenant-A")
	store, _ := factory.StateMachineAuditStore(ctx)

	// Record two events under tx1 so the entity entry exists.
	_ = store.Record(ctx, "e-1", spi.StateMachineEvent{EventType: spi.SMEventStarted, TransactionID: "tx-1", Details: "a"})
	_ = store.Record(ctx, "e-1", spi.StateMachineEvent{EventType: spi.SMEventFinished, TransactionID: "tx-1", Details: "b"})

	// Filter by tx2, which has no events — must get non-nil empty slice.
	got, err := store.GetEventsByTransaction(ctx, "e-1", "tx-2")
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if got == nil {
		t.Fatal("GetEventsByTransaction returned nil slice on filter-no-match; want non-nil empty slice")
	}
	if len(got) != 0 {
		t.Fatalf("expected empty slice for tx-2, got %d events", len(got))
	}
}

func TestSMAuditTenantIsolation(t *testing.T) {
	factory := memory.NewStoreFactory()
	ctxA := ctxWithTenant("tenant-A")
	ctxB := ctxWithTenant("tenant-B")
	storeA, _ := factory.StateMachineAuditStore(ctxA)
	storeB, _ := factory.StateMachineAuditStore(ctxB)

	_ = storeA.Record(ctxA, "e-1", spi.StateMachineEvent{EventType: spi.SMEventStarted, Details: "tenant-A event"})

	// SPI contract: tenant B must see an empty slice even when events exist for
	// this entityID in tenant A. Cross-tenant visibility would be a security
	// defect; returning an error instead of an empty slice would break callers
	// that treat "no events" as a normal state.
	got, err := storeB.GetEvents(ctxB, "e-1")
	if err != nil {
		t.Fatalf("expected nil error for tenant isolation, got: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("tenant B must see 0 events for entity written by tenant A, got %d", len(got))
	}
}
