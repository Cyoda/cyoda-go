package sqlite_test

import (
	"context"
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// Inside a transaction, a read that fails for any reason other than the
// entity being absent is that failure, not "not found": a Delete or Exists
// that cannot read the snapshot must not report a live entity as gone.
func TestTxReadFault_IsNotReportedAsNotFound(t *testing.T) {
	factory, tm := newAttrFactory(t)
	ctx := attrCtx("tenant-readfault", "u1", spi.PrincipalUser)
	modelRef := spi.ModelRef{EntityName: "m-readfault", ModelVersion: "1"}
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	ids := seedPageDeleteEntities(t, store, ctx, modelRef, 1)

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tm.Rollback(ctx, txID) }()
	// A cancelled statement context stands for any read fault.
	faulting, cancel := context.WithCancel(txCtx)
	cancel()

	if err := store.Delete(faulting, ids[0]); err == nil || errors.Is(err, spi.ErrNotFound) {
		t.Errorf("Delete = %v, want the read's own failure, not ErrNotFound", err)
	}
	if exists, err := store.Exists(faulting, ids[0]); err == nil {
		t.Errorf("Exists = (%v, nil), want the read's own failure", exists)
	}
}

// A second Delete of the same entity in one transaction finds it already
// gone, as on PostgreSQL (its staged delete hides the entity from the
// transaction, as Get already sees).
func TestTxDelete_SecondDeleteIsNotFound(t *testing.T) {
	factory, tm := newAttrFactory(t)
	ctx := attrCtx("tenant-twodel", "u1", spi.PrincipalUser)
	modelRef := spi.ModelRef{EntityName: "m-twodel", ModelVersion: "1"}
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	ids := seedPageDeleteEntities(t, store, ctx, modelRef, 1)
	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tm.Rollback(ctx, txID) }()
	if err := store.Delete(txCtx, ids[0]); err != nil {
		t.Fatalf("first Delete: %v", err)
	}
	if err := store.Delete(txCtx, ids[0]); !errors.Is(err, spi.ErrNotFound) {
		t.Errorf("second Delete = %v, want spi.ErrNotFound", err)
	}
}
