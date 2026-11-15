package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// lifecycleGeneration reads ref's model_lifecycle generation; ok is false
// when the model has no lifecycle row.
func lifecycleGeneration(t *testing.T, fx *pgFixture, ref spi.ModelRef) (gen int64, ok bool) {
	t.Helper()
	err := fx.db.QueryRow(context.Background(), `
		SELECT generation FROM model_lifecycle
		WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3`,
		string(fx.tenantID), ref.EntityName, ref.ModelVersion).Scan(&gen)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false
	}
	if err != nil {
		t.Fatalf("read the lifecycle row of %s: %v", ref, err)
	}
	return gen, true
}

// mustGeneration is lifecycleGeneration for a row that must exist.
func mustGeneration(t *testing.T, fx *pgFixture, ref spi.ModelRef) int64 {
	t.Helper()
	gen, ok := lifecycleGeneration(t, fx, ref)
	if !ok {
		t.Fatalf("model %s has no lifecycle row", ref)
	}
	return gen
}

// beginOther opens a transaction on the fixture's main pool: a connection of
// its own, apart from the model pool the store runs on. It is rolled back at
// cleanup unless the test commits it.
func beginOther(t *testing.T, fx *pgFixture) pgx.Tx {
	t.Helper()
	tx, err := fx.pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}

// holdLifecycleShare takes FOR SHARE on ref's lifecycle row in tx, as an
// entity commit into the model does.
func holdLifecycleShare(t *testing.T, fx *pgFixture, tx pgx.Tx, ref spi.ModelRef) {
	t.Helper()
	var gen int64
	if err := tx.QueryRow(context.Background(), `
		SELECT generation FROM model_lifecycle
		WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3
		FOR SHARE`,
		string(fx.tenantID), ref.EntityName, ref.ModelVersion).Scan(&gen); err != nil {
		t.Fatalf("FOR SHARE of the lifecycle row of %s: %v", ref, err)
	}
}

// waitForLockWaiter waits until a backend whose query contains fragment
// waits on a lock, or fails the test when done closes first.
func waitForLockWaiter(t *testing.T, fx *pgFixture, fragment string, done <-chan struct{}) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second) // realtime: test wait bound
	for {
		var n int
		if err := fx.db.QueryRow(context.Background(), `
			SELECT COUNT(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'
			  AND position($1 IN query) > 0`, fragment).Scan(&n); err != nil {
			t.Fatalf("read pg_stat_activity: %v", err)
		}
		if n > 0 {
			return
		}
		select {
		case <-done:
			t.Fatalf("the operation returned without waiting on a lock in %q", fragment)
		default:
		}
		if time.Now().After(deadline) { // realtime: test wait bound
			t.Fatalf("no backend waits on a lock in %q", fragment)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// boundedCtx gives an operation that must not wait on a lock a short bound.
func boundedCtx(t *testing.T, fx *pgFixture) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(fx.ctx, 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestSetUniqueKeysSaveAfterLockCommits_Refused — a Lock holds the model row,
// uncommitted, while a Save of new unique keys waits on it. When the Lock
// commits, the Save's upsert re-checks its WHERE against the row the Lock
// wrote and is refused with spi.ErrModelLocked: the stored keys and state are
// the locked ones, and the lifecycle row carries the Lock's bump alone.
func TestSetUniqueKeysSaveAfterLockCommits_Refused(t *testing.T) {
	fx := newPGFixture(t)
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	fx.saveModel(t, ref, []byte(`{}`), spi.ModelUnlocked)
	before := mustGeneration(t, fx, ref)

	lock := beginOther(t, fx)
	if _, err := lock.Exec(context.Background(), `
		UPDATE models SET doc = jsonb_set(doc, '{state}', to_jsonb($4::text))
		WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3`,
		string(fx.tenantID), ref.EntityName, ref.ModelVersion, string(spi.ModelLocked)); err != nil {
		t.Fatalf("lock's state update: %v", err)
	}
	if err := bumpLifecycle(context.Background(), lock, fx.tenantID, ref); err != nil {
		t.Fatalf("lock's bump: %v", err)
	}

	keyed := &spi.ModelDescriptor{
		Ref:         ref,
		State:       spi.ModelUnlocked,
		ChangeLevel: spi.ChangeLevelStructural,
		Schema:      []byte(`{}`),
		UniqueKeys:  []spi.UniqueKey{{ID: "k1", Fields: []string{"$.code"}}},
	}
	var saveErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		saveErr = fx.store.Save(fx.ctx, keyed)
	}()
	waitForLockWaiter(t, fx, "ON CONFLICT", done)
	if err := lock.Commit(context.Background()); err != nil {
		t.Fatalf("commit the lock: %v", err)
	}
	<-done

	if !errors.Is(saveErr, spi.ErrModelLocked) {
		t.Fatalf("Save after the lock committed = %v, want spi.ErrModelLocked", saveErr)
	}
	got, err := fx.store.Get(fx.ctx, ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != spi.ModelLocked {
		t.Errorf("state = %s, want LOCKED", got.State)
	}
	if len(got.UniqueKeys) != 0 {
		t.Errorf("unique keys = %v, want none: the refused Save changes nothing", got.UniqueKeys)
	}
	if gen := mustGeneration(t, fx, ref); gen != before+1 {
		t.Errorf("generation = %d, want %d (the lock's bump alone)", gen, before+1)
	}
}

// TestLockIdempotent_NoBump — Lock of an UNLOCKED model bumps the lifecycle
// row; Lock of a model already LOCKED neither bumps it nor touches it, so it
// does not wait for an entity commit holding the row FOR SHARE. Lock of a
// missing model is spi.ErrNotFound and creates no lifecycle row.
func TestLockIdempotent_NoBump(t *testing.T) {
	fx := newPGFixture(t)
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	fx.saveModel(t, ref, []byte(`{}`), spi.ModelUnlocked)
	g0 := mustGeneration(t, fx, ref)

	if err := fx.store.Lock(fx.ctx, ref); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	g1 := mustGeneration(t, fx, ref)
	if g1 != g0+1 {
		t.Fatalf("generation after Lock = %d, want %d", g1, g0+1)
	}

	commit := beginOther(t, fx)
	holdLifecycleShare(t, fx, commit, ref)
	if err := fx.store.Lock(boundedCtx(t, fx), ref); err != nil {
		t.Fatalf("Lock of a LOCKED model while a commit holds its lifecycle row: %v", err)
	}
	if err := commit.Commit(context.Background()); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if gen := mustGeneration(t, fx, ref); gen != g1 {
		t.Errorf("generation after a second Lock = %d, want %d", gen, g1)
	}

	missing := spi.ModelRef{EntityName: "missing", ModelVersion: "1"}
	if err := fx.store.Lock(fx.ctx, missing); !errors.Is(err, spi.ErrNotFound) {
		t.Errorf("Lock of a missing model = %v, want spi.ErrNotFound", err)
	}
	if _, ok := lifecycleGeneration(t, fx, missing); ok {
		t.Error("Lock of a missing model created a lifecycle row")
	}
}

// TestUnlockBumpsGeneration — Unlock bumps the lifecycle row. An entity
// commit holding the row FOR SHARE makes Unlock's bump wait, and the count
// after the bump sees the entity that commit wrote: Unlock is refused with a
// *spi.ModelHasEntitiesError and rolls back, leaving the model LOCKED and the
// generation where it was. With no entities, Unlock commits and bumps.
func TestUnlockBumpsGeneration(t *testing.T) {
	fx := newPGFixture(t)
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	fx.SaveModel(t, ref, []byte(`{}`))
	g0 := mustGeneration(t, fx, ref)

	commit := beginOther(t, fx)
	holdLifecycleShare(t, fx, commit, ref)
	if _, err := commit.Exec(context.Background(), `
		INSERT INTO entities (tenant_id, entity_id, model_name, model_version, version, deleted, doc, creation_date, last_modified)
		VALUES ($1, 'e1', $2, $3, 1, false, '{}'::jsonb, now(), now())`,
		string(fx.tenantID), ref.EntityName, ref.ModelVersion); err != nil {
		t.Fatalf("write an entity: %v", err)
	}

	var unlockErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		unlockErr = fx.store.Unlock(fx.ctx, ref)
	}()
	waitForLockWaiter(t, fx, "model_lifecycle", done)
	if err := commit.Commit(context.Background()); err != nil {
		t.Fatalf("commit the entity: %v", err)
	}
	<-done

	var mhe *spi.ModelHasEntitiesError
	if !errors.As(unlockErr, &mhe) {
		t.Fatalf("Unlock after the entity committed = %v, want *spi.ModelHasEntitiesError", unlockErr)
	}
	if mhe.Count != 1 || mhe.Ref != ref {
		t.Errorf("refusal = %+v, want ref %s, count 1", mhe, ref)
	}
	locked, err := fx.store.IsLocked(fx.ctx, ref)
	if err != nil {
		t.Fatalf("IsLocked: %v", err)
	}
	if !locked {
		t.Error("a refused Unlock left the model UNLOCKED")
	}
	if gen := mustGeneration(t, fx, ref); gen != g0 {
		t.Errorf("generation after a refused Unlock = %d, want %d", gen, g0)
	}

	if _, err := fx.db.Exec(context.Background(),
		`UPDATE entities SET deleted = true WHERE tenant_id = $1 AND entity_id = 'e1'`,
		string(fx.tenantID)); err != nil {
		t.Fatalf("delete the entity: %v", err)
	}
	if err := fx.store.Unlock(fx.ctx, ref); err != nil {
		t.Fatalf("Unlock with no live entities: %v", err)
	}
	if gen := mustGeneration(t, fx, ref); gen != g0+1 {
		t.Errorf("generation after Unlock = %d, want %d", gen, g0+1)
	}
}

// TestDeleteRemovesLifecycleRow — Delete of an UNLOCKED model removes its
// lifecycle row with it. A Delete that waits behind an uncommitted Lock
// checks the state of the row the Lock wrote: it is refused with
// spi.ErrModelLocked and leaves the model and its lifecycle row in place.
func TestDeleteRemovesLifecycleRow(t *testing.T) {
	fx := newPGFixture(t)
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	fx.saveModel(t, ref, []byte(`{}`), spi.ModelUnlocked)

	lock := beginOther(t, fx)
	if _, err := lock.Exec(context.Background(), `
		UPDATE models SET doc = jsonb_set(doc, '{state}', to_jsonb($4::text))
		WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3`,
		string(fx.tenantID), ref.EntityName, ref.ModelVersion, string(spi.ModelLocked)); err != nil {
		t.Fatalf("lock's state update: %v", err)
	}
	if err := bumpLifecycle(context.Background(), lock, fx.tenantID, ref); err != nil {
		t.Fatalf("lock's bump: %v", err)
	}
	var deleteErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		deleteErr = fx.store.Delete(fx.ctx, ref)
	}()
	waitForLockWaiter(t, fx, "DELETE FROM models", done)
	if err := lock.Commit(context.Background()); err != nil {
		t.Fatalf("commit the lock: %v", err)
	}
	<-done
	if !errors.Is(deleteErr, spi.ErrModelLocked) {
		t.Fatalf("Delete after the lock committed = %v, want spi.ErrModelLocked", deleteErr)
	}
	if _, err := fx.store.Get(fx.ctx, ref); err != nil {
		t.Errorf("a refused Delete removed the model: %v", err)
	}
	gen := mustGeneration(t, fx, ref)

	if err := fx.store.Unlock(fx.ctx, ref); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if got := mustGeneration(t, fx, ref); got != gen+1 {
		t.Fatalf("generation after Unlock = %d, want %d", got, gen+1)
	}
	if err := fx.store.Delete(fx.ctx, ref); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := lifecycleGeneration(t, fx, ref); ok {
		t.Error("Delete left the lifecycle row")
	}
}

// TestSetChangeLevelAndExtend_DoNotBump — SetChangeLevel and ExtendSchema
// change neither the state nor the unique keys, so they do not touch the
// lifecycle row: both complete while an entity commit holds it FOR SHARE,
// and the generation is unchanged.
func TestSetChangeLevelAndExtend_DoNotBump(t *testing.T) {
	fx := newPGFixture(t)
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	fx.SaveModel(t, ref, []byte(`{}`))
	g0 := mustGeneration(t, fx, ref)

	commit := beginOther(t, fx)
	holdLifecycleShare(t, fx, commit, ref)
	if err := fx.store.SetChangeLevel(boundedCtx(t, fx), ref, spi.ChangeLevelArrayElements); err != nil {
		t.Fatalf("SetChangeLevel while a commit holds the lifecycle row: %v", err)
	}
	if err := fx.store.ExtendSchema(boundedCtx(t, fx), ref, spi.SchemaDelta(`{"op":"x"}`)); err != nil {
		t.Fatalf("ExtendSchema while a commit holds the lifecycle row: %v", err)
	}
	if err := commit.Commit(context.Background()); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if gen := mustGeneration(t, fx, ref); gen != g0 {
		t.Errorf("generation after SetChangeLevel and ExtendSchema = %d, want %d", gen, g0)
	}
}
