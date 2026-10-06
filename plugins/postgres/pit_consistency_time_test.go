package postgres_test

// pit_consistency_time_test.go — the postgres plugin's point-in-time reads
// are committed-only and carry no wall-clock guard: an instant is final
// because the engine takes it from ConsistencyTime, not because the query
// filters on the database clock.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

// A version listing is committed-only: inside a transaction that has saved a
// new version of an entity, the listing shows the committed versions alone.
func TestGetVersionMetadata_CommittedOnlyInTx(t *testing.T) {
	f, ctx := newCTFactory(t)
	tm := ctTM(t, f, ctx)
	es, err := f.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	id := uuid.NewString()
	if _, err := es.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: id, ModelRef: ctModel}, Data: []byte(`{"v":1}`),
	}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tm.Rollback(txCtx, txID) }()
	esTx, err := f.EntityStore(txCtx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	got, err := esTx.Get(txCtx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, err := esTx.Save(txCtx, got); err != nil {
		t.Fatalf("Save in tx: %v", err)
	}
	vs, err := esTx.GetVersionMetadata(txCtx, id, spi.VersionMetadataOptions{})
	if err != nil {
		t.Fatalf("GetVersionMetadata: %v", err)
	}
	if len(vs) != 1 {
		t.Fatalf("listed %d versions, want 1: the transaction's own uncommitted version must not be listed", len(vs))
	}
}

// The stamp floor can run ahead of the database clock (after a clock step
// back or a failover). A committed version stamped from the floor is then
// above CURRENT_TIMESTAMP; a read at the consistency time must still find it.
func TestPIT_FloorAheadOfDBClock_RowsVisible(t *testing.T) {
	f, ctx := newCTFactoryOwnDB(t)
	pool := postgres.PoolForTest(f)
	ahead := time.Now().Add(time.Hour).UnixMicro()
	if _, err := pool.Exec(context.Background(), `SELECT setval('cyoda_stamp_floor', $1, true)`, ahead); err != nil {
		t.Fatalf("setval: %v", err)
	}
	es, err := f.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	id := uuid.NewString()
	if _, err := es.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: id, ModelRef: ctModel}, Data: []byte(`{"n":1}`),
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	c, err := ctTM(t, f, ctx).ConsistencyTime(ctx)
	if err != nil {
		t.Fatalf("ConsistencyTime: %v", err)
	}
	if _, err := es.GetAsAt(ctx, id, c); err != nil {
		t.Fatalf("GetAsAt at the consistency time did not find the committed entity: %v", err)
	}
	n, err := es.Count(ctx, ctModel, &c)
	if err != nil || n != 1 {
		t.Fatalf("Count at the consistency time = %d, %v; want 1", n, err)
	}
}
