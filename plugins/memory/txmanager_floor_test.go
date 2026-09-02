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

// Begin does not merely read the monotonic floor, it RESERVES its snapshot
// time as the new floor. Flooring alone leaves the first transaction on a
// quiet factory unprotected: with the floor still at zero the snapshot is the
// raw clock value, and the next write stamps max(now, floor+1µs) — the same
// instant — so a write made after the transaction began lands AT its
// snapshot, which the snapshot convention (submit_time <= SnapshotTime)
// counts as visible. Reserving pushes the next stamp one microsecond above
// the snapshot instead.
func TestBegin_ReservesItsSnapshotTimeAsTheFloor(t *testing.T) {
	clock := memory.NewTestClockAt(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	factory := memory.NewStoreFactory(memory.WithClock(clock))
	ctx := ctxWithTenant("tenant-reserve")
	ref := spi.ModelRef{EntityName: "m-reserve", ModelVersion: "1"}

	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	tm := factory.GetTransactionManager()
	if tm == nil {
		t.Fatal("no transaction manager on the factory")
	}

	// No prior write: the floor stands at zero, so Begin is the only thing
	// that can raise it.
	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tm.Rollback(txCtx, txID) }()
	snapshot := spi.GetTransaction(txCtx).SnapshotTime

	// No clock advance: the direct write happens at the snapshot instant.
	if _, err := store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: "e-reserve", TenantID: "tenant-reserve", ModelRef: ref, State: "open"},
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
		if it.Entity().Meta.ID == "e-reserve" {
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

	got, err := store.Get(ctx, "e-reserve")
	if err != nil {
		t.Fatalf("non-transactional Get: %v", err)
	}
	if got.Meta.ID != "e-reserve" {
		t.Fatalf("non-transactional Get returned %q, want e-reserve", got.Meta.ID)
	}
}

// A transaction manager installed on a factory that already holds rows takes
// over that factory's floor; it does not start from zero. Starting from zero
// would put the first snapshot after the swap below submit times already
// stamped — the stamps stand above the clock, which is the whole point of the
// floor — and the rows written under the old manager would be invisible to
// the first transaction under the new one. The sqlite plugin seeds the same
// value from MAX(submit_time) on open.
func TestNewTransactionManager_CarriesTheSubmitTimeFloor(t *testing.T) {
	clock := memory.NewTestClockAt(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	factory := memory.NewStoreFactory(memory.WithClock(clock))
	ctx := ctxWithTenant("tenant-carry")
	ref := spi.ModelRef{EntityName: "m-carry", ModelVersion: "1"}

	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}

	// Two writes inside one frozen clock tick: the second is stamped a
	// microsecond ABOVE the clock, so the floor now stands ahead of it and a
	// manager that started from zero would floor its snapshot below the row.
	for range 2 {
		if _, err := store.Save(ctx, &spi.Entity{
			Meta: spi.EntityMeta{ID: "e-carry", TenantID: "tenant-carry", ModelRef: ref, State: "open"},
			Data: []byte(`{"n":1}`),
		}); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}
	metas, err := store.GetVersionMetadata(ctx, "e-carry", spi.VersionMetadataOptions{})
	if err != nil {
		t.Fatalf("GetVersionMetadata: %v", err)
	}
	if len(metas) == 0 {
		t.Fatal("no version metadata for e-carry")
	}
	submitTime := metas[0].Timestamp // newest first

	tm := factory.NewTransactionManager(newTestUUIDGenerator())
	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tm.Rollback(txCtx, txID) }()

	if snapshot := spi.GetTransaction(txCtx).SnapshotTime; snapshot.Before(submitTime) {
		t.Fatalf("snapshot %s of a re-created manager sits below a committed submit time %s",
			snapshot, submitTime)
	}
}
