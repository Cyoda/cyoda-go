package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

// TestCrashRecovery_CommittedDataSurvivesReopen verifies that committed
// entities persist across close+reopen of the StoreFactory (WAL recovery).
// Uncommitted data from a second (never-committed) transaction must NOT
// be visible after reopen.
func TestCrashRecovery_CommittedDataSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "crash_recovery.db")
	ctx := testCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "widget", ModelVersion: "1"}

	// Phase 1: open factory, commit some data, write uncommitted data, close.
	func() {
		clock := sqlite.NewTestClock()
		factory, err := sqlite.NewStoreFactoryForTest(context.Background(), dbPath, sqlite.WithClock(clock))
		if err != nil {
			t.Fatalf("create factory (phase 1): %v", err)
		}
		defer factory.Close()

		store, err := factory.EntityStore(ctx)
		if err != nil {
			t.Fatalf("EntityStore: %v", err)
		}

		// Save 3 entities outside a transaction (auto-committed).
		for i, data := range []string{
			`{"name":"alpha","val":1}`,
			`{"name":"beta","val":2}`,
			`{"name":"gamma","val":3}`,
		} {
			id := []string{"e1", "e2", "e3"}[i]
			_, err := store.Save(ctx, &spi.Entity{
				Meta: spi.EntityMeta{ID: id, ModelRef: ref},
				Data: []byte(data),
			})
			if err != nil {
				t.Fatalf("Save %s: %v", id, err)
			}
		}

		// Advance clock so the transaction sees a later snapshot.
		clock.Advance(100 * time.Millisecond)

		// Start a transaction, save data, but DO NOT commit.
		tm, err := factory.TransactionManager(ctx)
		if err != nil {
			t.Fatalf("TransactionManager: %v", err)
		}
		txID, txCtx, err := tm.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}

		txStore, err := factory.EntityStore(txCtx)
		if err != nil {
			t.Fatalf("EntityStore (tx): %v", err)
		}

		_, err = txStore.Save(txCtx, &spi.Entity{
			Meta: spi.EntityMeta{ID: "e-uncommitted", ModelRef: ref},
			Data: []byte(`{"name":"uncommitted","val":999}`),
		})
		if err != nil {
			t.Fatalf("Save uncommitted: %v", err)
		}

		// Explicitly rollback to be clean, but the key point is: no Commit.
		_ = tm.Rollback(ctx, txID)

		// Close the factory (simulates clean shutdown).
	}()

	// Phase 2: reopen the same database file and verify data.
	func() {
		factory, err := sqlite.NewStoreFactoryForTest(context.Background(), dbPath)
		if err != nil {
			t.Fatalf("create factory (phase 2): %v", err)
		}
		defer factory.Close()

		store, err := factory.EntityStore(ctx)
		if err != nil {
			t.Fatalf("EntityStore: %v", err)
		}

		// Committed entities must be present.
		for _, id := range []string{"e1", "e2", "e3"} {
			e, err := store.Get(ctx, id)
			if err != nil {
				t.Errorf("committed entity %s not found after reopen: %v", id, err)
				continue
			}
			if e.Meta.ID != id {
				t.Errorf("expected ID %s, got %s", id, e.Meta.ID)
			}
		}

		// Uncommitted entity must NOT be present.
		_, err = store.Get(ctx, "e-uncommitted")
		if err == nil {
			t.Error("uncommitted entity 'e-uncommitted' should not be visible after reopen")
		}
	}()
}

// TestCrashRecovery_TransactionalCommitSurvives verifies that entities
// committed inside an SSI transaction survive close+reopen.
func TestCrashRecovery_TransactionalCommitSurvives(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "crash_tx.db")
	ctx := testCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "item", ModelVersion: "1"}

	// Phase 1: commit via transaction, then close.
	func() {
		clock := sqlite.NewTestClock()
		factory, err := sqlite.NewStoreFactoryForTest(context.Background(), dbPath, sqlite.WithClock(clock))
		if err != nil {
			t.Fatalf("create factory (phase 1): %v", err)
		}
		defer factory.Close()

		tm, err := factory.TransactionManager(ctx)
		if err != nil {
			t.Fatalf("TransactionManager: %v", err)
		}

		_, txCtx, err := tm.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}

		store, err := factory.EntityStore(txCtx)
		if err != nil {
			t.Fatalf("EntityStore: %v", err)
		}

		_, err = store.Save(txCtx, &spi.Entity{
			Meta: spi.EntityMeta{ID: "tx-e1", ModelRef: ref},
			Data: []byte(`{"name":"transactional","val":42}`),
		})
		if err != nil {
			t.Fatalf("Save in tx: %v", err)
		}

		clock.Advance(50 * time.Millisecond)

		txID := spi.GetTransaction(txCtx).ID
		if err := tm.Commit(ctx, txID); err != nil {
			t.Fatalf("Commit: %v", err)
		}
	}()

	// Phase 2: reopen and verify.
	func() {
		factory, err := sqlite.NewStoreFactoryForTest(context.Background(), dbPath)
		if err != nil {
			t.Fatalf("create factory (phase 2): %v", err)
		}
		defer factory.Close()

		store, err := factory.EntityStore(ctx)
		if err != nil {
			t.Fatalf("EntityStore: %v", err)
		}

		e, err := store.Get(ctx, "tx-e1")
		if err != nil {
			t.Fatalf("committed transaction entity not found: %v", err)
		}
		if string(e.Data) != `{"name":"transactional","val":42}` {
			t.Errorf("unexpected data: %s", string(e.Data))
		}
	}()
}

// TestCrashRecovery_VersionHistoryPreserved verifies that version history
// is intact after close+reopen.
func TestCrashRecovery_VersionHistoryPreserved(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "crash_history.db")
	ctx := testCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "doc", ModelVersion: "1"}

	// Phase 1: create, update, update, then close.
	func() {
		factory, err := sqlite.NewStoreFactoryForTest(context.Background(), dbPath)
		if err != nil {
			t.Fatalf("create factory (phase 1): %v", err)
		}
		defer factory.Close()

		store, err := factory.EntityStore(ctx)
		if err != nil {
			t.Fatalf("EntityStore: %v", err)
		}

		// v1: create
		_, err = store.Save(ctx, &spi.Entity{
			Meta: spi.EntityMeta{ID: "doc-1", ModelRef: ref},
			Data: []byte(`{"title":"draft"}`),
		})
		if err != nil {
			t.Fatalf("Save v1: %v", err)
		}

		// v2: update
		_, err = store.Save(ctx, &spi.Entity{
			Meta: spi.EntityMeta{ID: "doc-1", ModelRef: ref},
			Data: []byte(`{"title":"review"}`),
		})
		if err != nil {
			t.Fatalf("Save v2: %v", err)
		}

		// v3: update
		_, err = store.Save(ctx, &spi.Entity{
			Meta: spi.EntityMeta{ID: "doc-1", ModelRef: ref},
			Data: []byte(`{"title":"published"}`),
		})
		if err != nil {
			t.Fatalf("Save v3: %v", err)
		}
	}()

	// Phase 2: reopen and verify version history.
	func() {
		factory, err := sqlite.NewStoreFactoryForTest(context.Background(), dbPath)
		if err != nil {
			t.Fatalf("create factory (phase 2): %v", err)
		}
		defer factory.Close()

		store, err := factory.EntityStore(ctx)
		if err != nil {
			t.Fatalf("EntityStore: %v", err)
		}

		// GetVersionMetadata is newest-first (tie-break Version DESC), unlike
		// GetVersionHistory's oldest-first order — versions 1,2,3 come back
		// as metas[0]=3, metas[1]=2, metas[2]=1.
		metas, err := store.GetVersionMetadata(ctx, "doc-1", spi.VersionMetadataOptions{})
		if err != nil {
			t.Fatalf("GetVersionMetadata: %v", err)
		}

		if len(metas) != 3 {
			t.Fatalf("expected 3 versions, got %d", len(metas))
		}

		// Verify version ordering (newest-first).
		for i, m := range metas {
			want := int64(len(metas) - i)
			if m.Version != want {
				t.Errorf("version[%d]: expected version %d, got %d", i, want, m.Version)
			}
		}

		// Verify latest data.
		e, err := store.Get(ctx, "doc-1")
		if err != nil {
			t.Fatalf("Get latest: %v", err)
		}
		if string(e.Data) != `{"title":"published"}` {
			t.Errorf("expected published, got %s", string(e.Data))
		}
	}()
}
