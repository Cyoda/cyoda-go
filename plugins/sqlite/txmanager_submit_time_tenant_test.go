package sqlite_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

// TestGetSubmitTime_PersistentFallback_RejectsCrossTenant covers the
// persisted submit_times path: after close+reopen of the StoreFactory the
// in-memory submit-time map is empty, so GetSubmitTime falls back to the
// submit_times table. That fallback must enforce the same tenant check as
// the in-memory path — a tenant-B caller resolving a tenant-A txID gets
// ErrTxTenantMismatch, never the timestamp.
func TestGetSubmitTime_PersistentFallback_RejectsCrossTenant(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "submit_time_tenant.db")
	ctxA := testCtx("tenant-A")
	ctxB := testCtx("tenant-B")
	ref := spi.ModelRef{EntityName: "widget", ModelVersion: "1"}

	// Phase 1: tenant A commits a transaction, then the factory is closed
	// so the in-memory submit-time cache is dropped.
	var txID string
	func() {
		factory, err := sqlite.NewStoreFactoryForTest(context.Background(), dbPath)
		if err != nil {
			t.Fatalf("create factory (phase 1): %v", err)
		}
		defer factory.Close()

		tm, err := factory.TransactionManager(ctxA)
		if err != nil {
			t.Fatalf("TransactionManager: %v", err)
		}
		id, txCtx, err := tm.Begin(ctxA)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		txID = id

		txStore, err := factory.EntityStore(txCtx)
		if err != nil {
			t.Fatalf("EntityStore (tx): %v", err)
		}
		if _, err := txStore.Save(txCtx, &spi.Entity{
			Meta: spi.EntityMeta{ID: "e1", ModelRef: ref},
			Data: []byte(`{"name":"alpha"}`),
		}); err != nil {
			t.Fatalf("Save: %v", err)
		}
		if err := tm.Commit(txCtx, txID); err != nil {
			t.Fatalf("Commit: %v", err)
		}
	}()

	// Phase 2: reopen. The lookup can only be served from the persisted
	// submit_times table now.
	factory, err := sqlite.NewStoreFactoryForTest(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("create factory (phase 2): %v", err)
	}
	defer factory.Close()

	tm, err := factory.TransactionManager(ctxB)
	if err != nil {
		t.Fatalf("TransactionManager (phase 2): %v", err)
	}

	if _, err := tm.GetSubmitTime(ctxB, txID); err == nil {
		t.Fatal("expected error when tenant B resolves tenant A's submit time via the persisted table")
	} else if !errors.Is(err, spi.ErrTxTenantMismatch) {
		t.Fatalf("expected ErrTxTenantMismatch, got: %v", err)
	}

	// The owning tenant still resolves it from the persisted table.
	if _, err := tm.GetSubmitTime(ctxA, txID); err != nil {
		t.Fatalf("owning tenant's GetSubmitTime via persisted table: %v", err)
	}
}

// TestGetSubmitTime_QueryFailureIsNotNotFound pins that an infrastructure
// failure on the persistent-fallback query is reported as an error in its
// own right — not conflated with ErrTxNotFound, which would present a store
// outage as the definitive answer "this transaction does not exist"
// (correctness-over-availability: an unavailable dependency fails the
// operation, it does not substitute an answer).
func TestGetSubmitTime_QueryFailureIsNotNotFound(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "submit_time_err.db")
	ctx := testCtx("tenant-A")

	factory, err := sqlite.NewStoreFactoryForTest(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("create factory: %v", err)
	}
	tm, err := factory.TransactionManager(ctx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}

	// Close the factory so the fallback query fails with a real DB error.
	if err := factory.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	_, err = tm.GetSubmitTime(ctx, "0e0f1a10-1155-11f0-bcd5-ae468cd3ed16")
	if err == nil {
		t.Fatal("expected an error from GetSubmitTime on a closed store")
	}
	if errors.Is(err, spi.ErrTxNotFound) {
		t.Fatalf("query failure must not masquerade as ErrTxNotFound; got: %v", err)
	}
}
