package postgres

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// The tests in this file pin the postgres half of the commit-time unique-key
// check. A step that must not wait runs under a short context (boundedCtx),
// so a wait fails the test instead of hanging it.

// ccCodeKey is the single-field unique key over $.code.
var ccCodeKey = spi.UniqueKey{ID: "code", Fields: []string{"$.code"}}

// ccModel creates ref UNLOCKED with keys and locks it, through the store.
func ccModel(t *testing.T, fx *pgFixture, ref spi.ModelRef, keys ...spi.UniqueKey) {
	t.Helper()
	desc := &spi.ModelDescriptor{
		Ref:         ref,
		State:       spi.ModelUnlocked,
		ChangeLevel: spi.ChangeLevelStructural,
		Schema:      []byte(`{}`),
		UniqueKeys:  keys,
	}
	if err := fx.store.Save(fx.ctx, desc); err != nil {
		t.Fatalf("Save model %s: %v", ref, err)
	}
	if err := fx.store.Lock(fx.ctx, ref); err != nil {
		t.Fatalf("Lock model %s: %v", ref, err)
	}
}

// ccEntity is an entity of ref with the given JSON payload.
func ccEntity(ref spi.ModelRef, id, data string) *spi.Entity {
	return &spi.Entity{
		Meta: spi.EntityMeta{ID: id, ModelRef: ref},
		Data: []byte(data),
	}
}

// ccBegin opens an entity transaction and pins its snapshot.
func ccBegin(t *testing.T, fx *pgFixture, tm spi.TransactionManager) (string, context.Context, spi.EntityStore) {
	t.Helper()
	txID, txCtx, err := tm.Begin(fx.ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	t.Cleanup(func() { _ = tm.Rollback(txCtx, txID) })
	fx.pinSnapshot(t, txCtx)
	es, err := fx.factory.EntityStore(txCtx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	return txID, txCtx, es
}

// ccSave saves e and requires that the save succeed.
func ccSave(t *testing.T, ctx context.Context, es spi.EntityStore, e *spi.Entity) {
	t.Helper()
	if _, err := es.Save(ctx, e); err != nil {
		t.Fatalf("Save %s: %v", e.Meta.ID, err)
	}
}

// ccStore is an entity store outside any transaction.
func ccStore(t *testing.T, fx *pgFixture) spi.EntityStore {
	t.Helper()
	es, err := fx.factory.EntityStore(fx.ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	return es
}

// ccRequireAbsent asserts that no committed entity id exists.
func ccRequireAbsent(t *testing.T, fx *pgFixture, id, why string) {
	t.Helper()
	if _, err := ccStore(t, fx).Get(fx.ctx, id); !errors.Is(err, spi.ErrNotFound) {
		t.Errorf("%s: Get %s = %v, want spi.ErrNotFound", why, id, err)
	}
}

// ccRequireClaimed asserts that value is claimed in ref: a new entity with it,
// saved outside a transaction, is refused.
func ccRequireClaimed(t *testing.T, fx *pgFixture, ref spi.ModelRef, value, why string) {
	t.Helper()
	_, err := ccStore(t, fx).Save(boundedCtx(t, fx), ccEntity(ref, uuid.NewString(), fmt.Sprintf(`{"code":%s}`, value)))
	if !errors.Is(err, spi.ErrUniqueViolation) {
		t.Errorf("%s: a save of %s = %v, want spi.ErrUniqueViolation", why, value, err)
	}
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

// boundedCtx gives an operation that must not wait on a lock a short bound.
func boundedCtx(t *testing.T, fx *pgFixture) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(fx.ctx, 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestCommitClaims_UsesWrittenBytes — the commit computes claims from the
// bytes the transaction's last save was given. A key value of 1e100 commits
// and is claimed; a re-read of the stored JSONB would render it with 101
// digits, which the claim's number canonicalisation refuses.
func TestCommitClaims_UsesWrittenBytes(t *testing.T) {
	fx := newPGFixture(t)
	tm := fx.txManager(t)
	ref := spi.ModelRef{EntityName: "M", ModelVersion: "1"}
	ccModel(t, fx, ref, ccCodeKey)

	txID, txCtx, es := ccBegin(t, fx, tm)
	ccSave(t, txCtx, es, ccEntity(ref, uuid.NewString(), `{"code":1e100}`))
	if err := tm.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit of a key value 1e100: %v", err)
	}
	ccRequireClaimed(t, fx, ref, "1e100", "the committed value")
}

// TestCommitClaims_KeysReadAtCommit_ModelLockedAfterSnapshot — the model is
// created and locked after the transaction's snapshot, and an entity holding
// a key value is committed into it elsewhere. The transaction then saves an
// entity of the model with the same value. The commit reads the model's keys
// as they are at commit, not in its snapshot (which has no row for the
// model): the duplicate is refused.
func TestCommitClaims_KeysReadAtCommit_ModelLockedAfterSnapshot(t *testing.T) {
	fx := newPGFixture(t)
	tm := fx.txManager(t)
	ref := spi.ModelRef{EntityName: "M", ModelVersion: "1"}

	txID, txCtx, es := ccBegin(t, fx, tm)
	ccModel(t, fx, ref, ccCodeKey)
	if _, err := ccStore(t, fx).Save(fx.ctx, ccEntity(ref, uuid.NewString(), `{"code":"x"}`)); err != nil {
		t.Fatalf("seed the holder: %v", err)
	}

	id := uuid.NewString()
	ccSave(t, txCtx, es, ccEntity(ref, id, `{"code":"x"}`))
	if err := tm.Commit(boundedCtx(t, fx), txID); !errors.Is(err, spi.ErrUniqueViolation) {
		t.Fatalf("Commit of a duplicate into a model locked after the snapshot = %v, want spi.ErrUniqueViolation", err)
	}
	ccRequireAbsent(t, fx, id, "the refused commit")
}

// TestCommitClaims_KeysReadAtCommit_ModelRekeyedAfterSnapshot — the model,
// with no entities, is unlocked, given a new key and locked again after the
// transaction's snapshot, and an entity holding a value of the new key is
// committed elsewhere. The commit enforces the new key, not the one in its
// snapshot.
func TestCommitClaims_KeysReadAtCommit_ModelRekeyedAfterSnapshot(t *testing.T) {
	fx := newPGFixture(t)
	tm := fx.txManager(t)
	ref := spi.ModelRef{EntityName: "M", ModelVersion: "1"}
	ccModel(t, fx, ref, ccCodeKey)

	txID, txCtx, es := ccBegin(t, fx, tm)
	if err := fx.store.Unlock(fx.ctx, ref); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	otherKey := spi.UniqueKey{ID: "other", Fields: []string{"$.other"}}
	if err := fx.store.Save(fx.ctx, &spi.ModelDescriptor{
		Ref: ref, State: spi.ModelUnlocked, ChangeLevel: spi.ChangeLevelStructural,
		Schema: []byte(`{}`), UniqueKeys: []spi.UniqueKey{otherKey},
	}); err != nil {
		t.Fatalf("Save new keys: %v", err)
	}
	if err := fx.store.Lock(fx.ctx, ref); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if _, err := ccStore(t, fx).Save(fx.ctx, ccEntity(ref, uuid.NewString(), `{"code":"a","other":"y"}`)); err != nil {
		t.Fatalf("seed the holder: %v", err)
	}

	id := uuid.NewString()
	ccSave(t, txCtx, es, ccEntity(ref, id, `{"code":"b","other":"y"}`))
	if err := tm.Commit(boundedCtx(t, fx), txID); !errors.Is(err, spi.ErrUniqueViolation) {
		t.Fatalf("Commit of a duplicate of the new key = %v, want spi.ErrUniqueViolation", err)
	}
	ccRequireAbsent(t, fx, id, "the refused commit")
}

// TestCommitClaims_KeysReadOnModelPool — the commit reads the models' keys
// on the model pool, not the main pool. With a main pool of one connection,
// which the committing transaction holds, a key read on the main pool could
// not get a connection; on the model pool the commit goes through and
// claims its value.
func TestCommitClaims_KeysReadOnModelPool(t *testing.T) {
	fx := newPGFixtureSized(t, defaultStoreConfig().SchemaSavepointInterval, 1)
	tm := fx.txManager(t)
	ref := spi.ModelRef{EntityName: "M", ModelVersion: "1"}
	ccModel(t, fx, ref, ccCodeKey)

	txID, txCtx, es := ccBegin(t, fx, tm)
	ccSave(t, txCtx, es, ccEntity(ref, uuid.NewString(), `{"code":"x"}`))
	if err := tm.Commit(boundedCtx(t, fx), txID); err != nil {
		t.Fatalf("Commit with the one main-pool connection held by the transaction: %v", err)
	}
	ccRequireClaimed(t, fx, ref, `"x"`, "the committed value")
}

// TestCommitClaims_ExtendAndChangeLevel_DoNotBlock — the commit reads the
// models' keys without a lock. An extension and a change-level change, which
// write the models row, neither fail nor delay the commit, whether committed
// during the transaction or holding the models row at its commit.
func TestCommitClaims_ExtendAndChangeLevel_DoNotBlock(t *testing.T) {
	fx := newPGFixture(t)
	tm := fx.txManager(t)
	ref := spi.ModelRef{EntityName: "M", ModelVersion: "1"}
	ccModel(t, fx, ref, ccCodeKey)

	t.Log("committed during the transaction")
	txID, txCtx, es := ccBegin(t, fx, tm)
	ccSave(t, txCtx, es, ccEntity(ref, uuid.NewString(), `{"code":"a"}`))
	if err := fx.store.ExtendSchema(boundedCtx(t, fx), ref, spi.SchemaDelta(`{"op":"x"}`)); err != nil {
		t.Fatalf("ExtendSchema: %v", err)
	}
	if err := fx.store.SetChangeLevel(boundedCtx(t, fx), ref, spi.ChangeLevelArrayElements); err != nil {
		t.Fatalf("SetChangeLevel: %v", err)
	}
	commitCtx, cancel := context.WithTimeout(txCtx, 5*time.Second)
	defer cancel()
	if err := tm.Commit(commitCtx, txID); err != nil {
		t.Fatalf("Commit after an extension and a change-level change: %v", err)
	}

	t.Log("holding the models row at the commit")
	txID, txCtx, es = ccBegin(t, fx, tm)
	ccSave(t, txCtx, es, ccEntity(ref, uuid.NewString(), `{"code":"b"}`))
	holder := beginOther(t, fx)
	if _, err := holder.Exec(context.Background(), `
		UPDATE models SET doc = jsonb_set(doc, '{changeLevel}', to_jsonb($4::text))
		WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3`,
		string(fx.tenantID), ref.EntityName, ref.ModelVersion, string(spi.ChangeLevelType)); err != nil {
		t.Fatalf("hold the models row: %v", err)
	}
	commitCtx2, cancel2 := context.WithTimeout(txCtx, 5*time.Second)
	defer cancel2()
	if err := tm.Commit(commitCtx2, txID); err != nil {
		t.Fatalf("Commit while another transaction holds the models row: %v", err)
	}
	if err := holder.Commit(context.Background()); err != nil {
		t.Fatalf("commit the holder: %v", err)
	}
	ccRequireClaimed(t, fx, ref, `"a"`, "the first commit")
	ccRequireClaimed(t, fx, ref, `"b"`, "the second commit")
}

// TestCommitClaims_UncommittedDeleteDoesNotFreeValue — a delete inside a
// transaction releases the holder's claim at that transaction's commit, not
// at the delete. Until then the value is held: another transaction's commit
// claiming it is refused at once, without waiting for the deleting one.
func TestCommitClaims_UncommittedDeleteDoesNotFreeValue(t *testing.T) {
	fx := newPGFixture(t)
	tm := fx.txManager(t)
	ref := spi.ModelRef{EntityName: "M", ModelVersion: "1"}
	ccModel(t, fx, ref, ccCodeKey)
	holder := uuid.NewString()
	if _, err := ccStore(t, fx).Save(fx.ctx, ccEntity(ref, holder, `{"code":"x"}`)); err != nil {
		t.Fatalf("seed the holder: %v", err)
	}

	_, delCtx, delES := ccBegin(t, fx, tm)
	if err := delES.Delete(delCtx, holder); err != nil {
		t.Fatalf("Delete in the transaction: %v", err)
	}

	txID, txCtx, es := ccBegin(t, fx, tm)
	id := uuid.NewString()
	ccSave(t, txCtx, es, ccEntity(ref, id, `{"code":"x"}`))
	commitCtx, cancel := context.WithTimeout(txCtx, 5*time.Second)
	defer cancel()
	if err := tm.Commit(commitCtx, txID); !errors.Is(err, spi.ErrUniqueViolation) {
		t.Fatalf("Commit claiming a value an uncommitted delete frees = %v, want spi.ErrUniqueViolation", err)
	}
	ccRequireAbsent(t, fx, id, "the refused commit")
}
