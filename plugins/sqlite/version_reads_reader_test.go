package sqlite_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

// occupyWriterConnection checks out the writer pool's single connection by
// leaving a *sql.Rows open mid-scan, and returns a func that releases it.
// Anything still issuing statements on the writer pool blocks until then.
func occupyWriterConnection(t *testing.T, factory *sqlite.StoreFactory) func() {
	t.Helper()
	db := sqlite.DBForTest(factory)
	rows, err := db.Query("SELECT entity_id FROM entities")
	if err != nil {
		t.Fatalf("occupy writer connection: %v", err)
	}
	if !rows.Next() {
		rows.Close()
		t.Fatalf("occupy writer connection: no seeded rows to hold the cursor open (err=%v)", rows.Err())
	}
	return func() { rows.Close() }
}

// seedVersionReadFixture saves one entity inside a committed transaction, so
// both a version history and a non-empty transaction_id exist to read back.
func seedVersionReadFixture(t *testing.T) (*sqlite.StoreFactory, context.Context, string, string) {
	t.Helper()
	factory, tm := newAttrFactory(t)
	ctx := attrCtx("tenant-vr", "alice", spi.PrincipalUser)
	modelRef := spi.ModelRef{EntityName: "m-vr", ModelVersion: "1"}

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	txStore, err := factory.EntityStore(txCtx)
	if err != nil {
		t.Fatalf("EntityStore(txCtx): %v", err)
	}
	if _, err := txStore.Save(txCtx, &spi.Entity{
		Meta: spi.EntityMeta{ID: "e-vr", ModelRef: modelRef, State: "NEW"},
		Data: []byte(`{"v":1}`),
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := tm.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return factory, ctx, "e-vr", txID
}

// TestGetVersionMetadata_UsesReaderConnection: an audit read must not queue
// behind whatever is occupying the writer pool's single connection. Both
// version-history reads used to run on the writer db, contradicting the
// rationale GetPage states in the same file for moving reads off it — the
// transaction manager buffers writes in memory and flushes them in one
// sqlTx at commit, so an entity_versions read never has an ambient SQL
// transaction to join and gains nothing from the writer connection.
func TestGetVersionMetadata_UsesReaderConnection(t *testing.T) {
	factory, ctx, entityID, _ := seedVersionReadFixture(t)
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}

	release := occupyWriterConnection(t, factory)
	t.Cleanup(release)

	done := make(chan error, 1)
	go func() {
		metas, err := store.GetVersionMetadata(ctx, entityID, spi.VersionMetadataOptions{})
		if err != nil {
			done <- fmt.Errorf("GetVersionMetadata: %w", err)
			return
		}
		if len(metas) == 0 {
			done <- errors.New("GetVersionMetadata: want at least one version, got none")
			return
		}
		done <- nil
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("GetVersionMetadata blocked for 5s behind an occupied writer connection — " +
			"an audit read must use the dedicated reader connection, like GetPage")
	}
}

// TestGetVersionByTransaction_UsesReaderConnection is the sibling of the test
// above for the other version-history read.
func TestGetVersionByTransaction_UsesReaderConnection(t *testing.T) {
	factory, ctx, entityID, txID := seedVersionReadFixture(t)
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}

	release := occupyWriterConnection(t, factory)
	t.Cleanup(release)

	done := make(chan error, 1)
	go func() {
		ev, err := store.GetVersionByTransaction(ctx, entityID, txID)
		if err != nil {
			done <- fmt.Errorf("GetVersionByTransaction: %w", err)
			return
		}
		if ev == nil || ev.Entity == nil || ev.Entity.Meta.ID != entityID {
			done <- fmt.Errorf("GetVersionByTransaction: unexpected result %+v", ev)
			return
		}
		done <- nil
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("GetVersionByTransaction blocked for 5s behind an occupied writer connection — " +
			"an audit read must use the dedicated reader connection, like GetPage")
	}
}
