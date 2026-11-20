package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	spi "github.com/cyoda/cyoda-go-spi"
	"github.com/cyoda/cyoda-go-spi/spitest"
)

// A create and a delete of one entity in one transaction commit a single
// tombstone, whose entities row is stamped with the commit instant: its
// created_at column and the creation date in its stored meta must agree,
// not leave the meta at the provisional date the buffered create carried.
func TestDeletedBufferedTombstone_MetaCreationDateIsTheCommitInstant(t *testing.T) {
	clock := NewTestClock()
	f, err := NewStoreFactoryForTest(context.Background(), filepath.Join(t.TempDir(), "tombstone.db"), WithClock(clock))
	if err != nil {
		t.Fatalf("NewStoreFactoryForTest: %v", err)
	}
	defer f.Close()
	ctx := attrInternalCtx("tenant-tomb", "alice", spi.PrincipalUser)
	ref := spi.ModelRef{EntityName: "m-tomb", ModelVersion: "1"}
	spitest.EnsureLockedModel(t, f, ctx, ref)

	txID, txCtx, err := f.tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	store, err := f.EntityStore(txCtx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	if _, err := store.Save(txCtx, &spi.Entity{Meta: spi.EntityMeta{ID: "e-tomb", ModelRef: ref, State: "open"}, Data: []byte(`{}`)}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	clock.Advance(time.Second)
	if err := store.Delete(txCtx, "e-tomb"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := f.tm.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	var createdAt, metaCreation int64
	if err := f.db.QueryRowContext(ctx,
		`SELECT created_at, json_extract(json(meta), '$.creation_date') FROM entities WHERE tenant_id = ? AND entity_id = ?`,
		"tenant-tomb", "e-tomb").Scan(&createdAt, &metaCreation); err != nil {
		t.Fatalf("read entities row: %v", err)
	}
	if metaCreation != createdAt {
		t.Fatalf("stored meta creation date = %d, want the created_at column's commit instant %d", metaCreation, createdAt)
	}
}
