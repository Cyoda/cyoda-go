package memory_test

import (
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// msBase is a millisecond-aligned instant (nanoseconds == 0) so that a
// sub-millisecond Advance keeps both versions inside the same millisecond.
var msBase = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// twoVersionsSameMillisecond saves v1 at msBase and v2 300µs later (same
// millisecond) and returns the store. The buggy round-up rounds msBase up to
// the next millisecond, so a query "as at msBase" wrongly includes v2.
func twoVersionsSameMillisecond(t *testing.T) (spi.EntityStore, interface{ Now() time.Time }) {
	t.Helper()
	clock := memory.NewTestClockAt(msBase)
	factory := memory.NewStoreFactory(memory.WithClock(clock))
	ctx := ctxWithTenant("tenant-pit")
	store, _ := factory.EntityStore(ctx)
	ref := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}

	e := &spi.Entity{
		Meta: spi.EntityMeta{ID: "e-pit", TenantID: "tenant-pit", ModelRef: ref, State: "NEW"},
		Data: []byte(`{"v":1}`),
	}
	if _, err := store.Save(ctx, e); err != nil {
		t.Fatalf("Save v1: %v", err)
	}
	clock.Advance(300 * time.Microsecond)
	e.Data = []byte(`{"v":2}`)
	if _, err := store.Save(ctx, e); err != nil {
		t.Fatalf("Save v2: %v", err)
	}
	return store, clock
}

func TestMemoryPIT_GetAsAt_InclusiveNoRoundUp(t *testing.T) {
	store, _ := twoVersionsSameMillisecond(t)
	ctx := ctxWithTenant("tenant-pit")

	got, err := store.GetAsAt(ctx, "e-pit", msBase)
	if err != nil {
		t.Fatalf("GetAsAt(msBase): %v", err)
	}
	if string(got.Data) != `{"v":1}` {
		t.Errorf("GetAsAt(msBase) = %s, want {\"v\":1} (round-up over-included the same-ms v2)", got.Data)
	}
}

func TestMemoryPIT_GetAllAsAt_InclusiveNoRoundUp(t *testing.T) {
	store, _ := twoVersionsSameMillisecond(t)
	ctx := ctxWithTenant("tenant-pit")
	ref := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}

	got, err := store.GetAllAsAt(ctx, ref, msBase)
	if err != nil {
		t.Fatalf("GetAllAsAt(msBase): %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("GetAllAsAt(msBase) returned %d entities, want 1", len(got))
	}
	if string(got[0].Data) != `{"v":1}` {
		t.Errorf("GetAllAsAt(msBase) data = %s, want {\"v\":1}", got[0].Data)
	}
}

func TestMemoryPIT_Iterate_InclusiveNoRoundUp(t *testing.T) {
	store, _ := twoVersionsSameMillisecond(t)
	ctx := ctxWithTenant("tenant-pit")
	ref := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}

	it := store.(spi.Iterable)
	pit := msBase
	iter, err := it.Iterate(ctx, ref, spi.Filter{}, spi.IterateOptions{PointInTime: &pit})
	if err != nil {
		t.Fatalf("Iterate(msBase): %v", err)
	}
	defer iter.Close()

	var data string
	var seen int
	for iter.Next() {
		seen++
		data = string(iter.Entity().Data)
	}
	if err := iter.Err(); err != nil {
		t.Fatalf("iter err: %v", err)
	}
	if seen != 1 || data != `{"v":1}` {
		t.Errorf("Iterate(msBase) saw %d entities (data=%s), want 1 with {\"v\":1}", seen, data)
	}
}
