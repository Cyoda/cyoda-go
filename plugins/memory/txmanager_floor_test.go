package memory_test

import (
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// A direct (non-transactional) write must stamp its submit time under the
// same monotonic floor a commit uses, exactly as the sqlite plugin does.
// Under a frozen clock — which the conformance and parity suites use — a
// direct write that stamped the raw clock value would land AT the snapshot
// of a transaction already open, and that transaction's snapshot read would
// then see a row written after it began. The floor pushes the stamp one
// microsecond above the snapshot instead, so the write stays invisible to
// the open transaction while remaining immediately visible outside it.
func TestDirectSave_StampsAboveAnOpenSnapshot(t *testing.T) {
	clock := memory.NewTestClockAt(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	factory := memory.NewStoreFactory(memory.WithClock(clock))
	ctx := ctxWithTenant("tenant-floor")
	ref := spi.ModelRef{EntityName: "m-floor", ModelVersion: "1"}

	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	tm := factory.GetTransactionManager()
	if tm == nil {
		t.Fatal("no transaction manager on the factory")
	}

	// Stand in for a write that already stamped this instant, so the
	// monotonic floor stands at the frozen clock value — the state any
	// system that has done work is in. (From a floor of zero the first
	// stamp is simply the clock value, and there is nothing to order it
	// against.)
	if _, err := store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: "e-earlier", TenantID: "tenant-floor", ModelRef: ref, State: "open"},
		Data: []byte(`{"n":0}`),
	}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tm.Rollback(txCtx, txID) }()
	snapshot := spi.GetTransaction(txCtx).SnapshotTime

	// No clock advance: the direct write happens at the snapshot instant.
	if _, err := store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: "e-floor", TenantID: "tenant-floor", ModelRef: ref, State: "open"},
		Data: []byte(`{"n":1}`),
	}); err != nil {
		t.Fatalf("direct Save: %v", err)
	}

	iterable, ok := store.(spi.Iterable)
	if !ok {
		t.Fatal("EntityStore does not implement spi.Iterable")
	}
	it, err := iterable.Iterate(txCtx, ref, spi.Filter{}, spi.IterateOptions{})
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	var seen int
	for it.Next() {
		if it.Entity().Meta.ID == "e-floor" {
			seen++
		}
	}
	if err := it.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := it.Err(); err != nil {
		t.Fatalf("Iterate Err: %v", err)
	}
	if seen != 0 {
		t.Fatalf("the open transaction (snapshot %s) saw a row written after it began (%d times)", snapshot, seen)
	}

	got, err := store.Get(ctx, "e-floor")
	if err != nil {
		t.Fatalf("non-transactional Get: %v", err)
	}
	if got.Meta.ID != "e-floor" {
		t.Fatalf("non-transactional Get returned %q, want e-floor", got.Meta.ID)
	}
}
