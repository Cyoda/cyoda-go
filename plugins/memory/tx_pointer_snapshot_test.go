package memory_test

import (
	"context"
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// An in-transaction Count answers over the same merged view GetAll and
// GetPage present. A buffered write of a DIFFERENT model does not hide the
// committed row of the model being counted: the buffer overlay is
// model-scoped everywhere else, and the count must scope its skip the same
// way or it silently under-counts.
func TestTx_Count_BufferedOtherModelDoesNotHideCommittedRow(t *testing.T) {
	f, tm := newTxManager(t)
	ctx := tenantCtx(spi.TenantID("tenant-ptr"))
	refA := spi.ModelRef{EntityName: "m-ptr", ModelVersion: "1"}
	refB := spi.ModelRef{EntityName: "m-ptr", ModelVersion: "2"}
	store, err := f.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	if _, err := store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: "e-ptr", TenantID: "tenant-ptr", ModelRef: refA, State: "open"},
		Data: []byte(`{"n":1}`),
	}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	t.Cleanup(func() { _ = tm.Rollback(txCtx, txID) })

	// Buffer the same id under a different model.
	if _, err := store.Save(txCtx, &spi.Entity{
		Meta: spi.EntityMeta{ID: "e-ptr", TenantID: "tenant-ptr", ModelRef: refB, State: "open"},
		Data: []byte(`{"n":2}`),
	}); err != nil {
		t.Fatalf("in-tx Save: %v", err)
	}

	all, err := store.GetAll(txCtx, refA)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	n, err := store.Count(txCtx, refA)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != int64(len(all)) {
		t.Fatalf("Count(refA) = %d, GetAll(refA) = %d rows — the count must match the merged view", n, len(all))
	}

	byState, err := store.CountByState(txCtx, refA, nil)
	if err != nil {
		t.Fatalf("CountByState: %v", err)
	}
	var total int64
	for _, c := range byState {
		total += c
	}
	if total != int64(len(all)) {
		t.Fatalf("CountByState(refA) total = %d, GetAll(refA) = %d rows", total, len(all))
	}
}

// The pointer snapshot is the memory plugin's real scan path for in-tx
// reads, so it observes cancellation the way the copying variant does — an
// already-cancelled context aborts the scan rather than walking the whole
// tenant first.
func TestTx_GetPage_CancelledContextAbortsPointerSnapshot(t *testing.T) {
	f, tm := newTxManager(t)
	ctx := tenantCtx(spi.TenantID("tenant-ptr-cancel"))
	ref := spi.ModelRef{EntityName: "m-ptr-cancel", ModelVersion: "1"}
	store, err := f.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	if _, err := store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: "e-ptr-cancel", TenantID: "tenant-ptr-cancel", ModelRef: ref, State: "open"},
		Data: []byte(`{"n":1}`),
	}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	t.Cleanup(func() { _ = tm.Rollback(txCtx, txID) })

	cancelled, cancel := context.WithCancel(txCtx)
	cancel()

	if _, err := store.GetPage(cancelled, ref, 10, 0, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("GetPage on a cancelled context: err = %v, want context.Canceled", err)
	}
}
