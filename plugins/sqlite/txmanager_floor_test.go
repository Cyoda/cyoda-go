package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

// Begin does not merely read the monotonic floor, it RESERVES its snapshot
// time as the new floor. Flooring alone leaves the first transaction on a
// quiet database unprotected: with the floor still at zero the snapshot is
// the raw clock value, and the next write stamps max(now, floor+1µs) — the
// same instant — so a write made after the transaction began lands AT its
// snapshot, which the snapshot convention (submit_time <= SnapshotTime)
// counts as visible. Reserving pushes the next stamp one microsecond above
// the snapshot instead.
func TestBegin_ReservesItsSnapshotTimeAsTheFloor(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "floor-reserve.db")
	clock := sqlite.NewTestClockAt(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	factory, err := sqlite.NewStoreFactoryForTest(context.Background(), dbPath, sqlite.WithClock(clock))
	if err != nil {
		t.Fatalf("NewStoreFactoryForTest: %v", err)
	}
	defer factory.Close()

	ctx := testCtx("tenant-reserve")
	ref := spi.ModelRef{EntityName: "m-reserve", ModelVersion: "1"}
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	tm, err := factory.TransactionManager(ctx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
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
