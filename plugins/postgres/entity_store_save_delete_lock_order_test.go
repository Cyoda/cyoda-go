package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

// TestNonTxSaveDeleteConcurrent_NoTornWrite guards the Save/Delete lock-order
// inversion this task's atomicity fix made possible: non-transactional Save
// takes the entities row lock (its own upsert) BEFORE inserting
// entity_versions, while non-transactional Delete inserts entity_versions
// BEFORE taking the entities row lock (its own UPDATE) — see the lock-order
// comments at both sites in entity_store.go (saveOn's entities upsert;
// deleteOn's entity_versions insert and its entities UPDATE). A concurrent
// Save and Delete on the SAME entity, each computing the same next version
// from the row's pre-write state, can wait on each other in a genuine cycle
// that autocommit statements — which never hold a lock across a second
// statement — could never produce. PostgreSQL's deadlock detector breaks
// that cycle with 40P01 after deadlock_timeout (~1s); classifySQLState maps
// it to spi.ErrConflict, the same fail-closed, retryable outcome as any
// other lock conflict in this plugin.
//
// This test forces genuine contention deterministically rather than hoping
// goroutine timing produces it: a manually held FOR UPDATE lock on the
// entities row queues a real Save and a real Delete behind the SAME lock at
// the SAME moment (mirroring TestNonTxCompareAndSave_StampsAfterTheLockWait's
// technique), guaranteeing both attempt the entities row lock concurrently
// and both compute the same next version from the row's pre-release state —
// exactly what turns "one waits for the other" into a genuine cycle once the
// manual lock releases and one of the two acquires it.
//
// The outcome from that point on is NOT forced — PostgreSQL's own lock-queue
// order decides whether the two interleave into the deadlock or simply
// serialize cleanly one after the other — so this test asserts the
// CONSISTENCY property across either outcome (both succeed with no torn
// write, or exactly one fails with spi.ErrConflict and no torn write), not a
// specific interleaving.
func TestNonTxSaveDeleteConcurrent_NoTornWrite(t *testing.T) {
	factory := setupEntityTest(t)
	const tenant spi.TenantID = "tenant-save-delete-lock-order"
	ctx := ctxWithTenant(tenant)
	ref := spi.ModelRef{EntityName: "m-save-delete-lock-order", ModelVersion: "1"}
	pool := postgres.PoolForTest(factory)

	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}

	const id = "e-save-delete-lock-order"
	if _, err := store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: id, TenantID: tenant, ModelRef: ref},
		Data: []byte(`{"n":0}`),
	}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}

	// Hold the entities row lock so the real Save and the real Delete below
	// both queue behind the SAME lock at the SAME moment.
	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin lock holder: %v", err)
	}
	defer func() { _ = blocker.Rollback(context.Background()) }()
	if _, err := blocker.Exec(ctx,
		`SELECT 1 FROM entities WHERE tenant_id = $1 AND entity_id = $2 FOR UPDATE`,
		string(tenant), id); err != nil {
		t.Fatalf("take the row lock: %v", err)
	}

	saveDone := make(chan error, 1)
	deleteDone := make(chan error, 1)
	go func() {
		_, err := store.Save(ctx, &spi.Entity{
			Meta: spi.EntityMeta{ID: id, TenantID: tenant, ModelRef: ref},
			Data: []byte(`{"n":1}`),
		})
		saveDone <- err
	}()
	go func() {
		deleteDone <- store.Delete(ctx, id)
	}()

	// Wait until BOTH are demonstrably queued on the lock, not just started —
	// releasing the lock before both actually queued would pass this test
	// without exercising the cycle at all.
	waitStart := time.Now()
	for {
		var blocked int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_stat_activity
			 WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&blocked); err != nil {
			t.Fatalf("poll for blocked backends: %v", err)
		}
		if blocked >= 2 {
			break
		}
		if time.Since(waitStart) > 10*time.Second {
			t.Fatalf("only %d backend(s) ever blocked on the row lock, want 2", blocked)
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := blocker.Rollback(ctx); err != nil {
		t.Fatalf("release the row lock: %v", err)
	}

	saveErr := <-saveDone
	deleteErr := <-deleteDone

	// Never anything but a clean success or a classified conflict — any other
	// error is a real bug this test must not paper over.
	for name, err := range map[string]error{"Save": saveErr, "Delete": deleteErr} {
		if err != nil && !errors.Is(err, spi.ErrConflict) {
			t.Fatalf("%s returned an unexpected error (want nil or spi.ErrConflict): %v", name, err)
		}
	}
	if saveErr != nil && deleteErr != nil {
		t.Fatalf("both Save and Delete failed — want at most one: save=%v delete=%v", saveErr, deleteErr)
	}

	// No torn write: entities.version must equal the highest entity_versions
	// row for this entity, and entities.deleted must agree with whether that
	// top row is a DELETED tombstone — whichever of the two operations
	// actually won (or both, if they serialized instead of deadlocking).
	var entitiesVersion int64
	var entitiesDeleted bool
	if err := pool.QueryRow(ctx,
		`SELECT version, deleted FROM entities WHERE tenant_id = $1 AND entity_id = $2`,
		string(tenant), id).Scan(&entitiesVersion, &entitiesDeleted); err != nil {
		t.Fatalf("read back entities row: %v", err)
	}

	var maxVersion int64
	var topDeleted bool
	if err := pool.QueryRow(ctx,
		`SELECT version, (doc->'_meta'->>'deleted')::boolean IS TRUE
		 FROM entity_versions WHERE tenant_id = $1 AND entity_id = $2
		 ORDER BY version DESC LIMIT 1`,
		string(tenant), id).Scan(&maxVersion, &topDeleted); err != nil {
		t.Fatalf("read back top entity_versions row: %v", err)
	}

	if entitiesVersion != maxVersion {
		t.Errorf("entities.version = %d, latest entity_versions row = %d — torn write", entitiesVersion, maxVersion)
	}
	if entitiesDeleted != topDeleted {
		t.Errorf("entities.deleted = %v, latest entity_versions tombstone flag = %v — torn write", entitiesDeleted, topDeleted)
	}

	var versionCount int64
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM entity_versions WHERE tenant_id = $1 AND entity_id = $2`,
		string(tenant), id).Scan(&versionCount); err != nil {
		t.Fatalf("count entity_versions rows: %v", err)
	}
	if versionCount != maxVersion {
		// Versions start at 1 (the seed Save) and increment by exactly 1 per
		// successful write — a gap or duplicate here is itself a torn write,
		// on top of whatever entities/entity_versions disagreement it produces.
		t.Errorf("entity_versions has %d rows but the latest version number is %d — gap or duplicate, torn write", versionCount, maxVersion)
	}
}
