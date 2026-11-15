package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// The tests in this file pin the postgres half of the commit-time unique-key
// and model check: the order of locks and snapshots that makes the check
// atomic with a change of the model, on two connections, in a fixed order.
// A step that must not wait runs under a short context (boundedCtx), so a
// wait fails the test instead of hanging it; a step that must wait is
// observed waiting (waitForLockWaiter) before the test lets it go on.

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

// ccRequireState asserts ref's stored state.
func ccRequireState(t *testing.T, fx *pgFixture, ref spi.ModelRef, locked bool, why string) {
	t.Helper()
	got, err := fx.store.IsLocked(fx.ctx, ref)
	if err != nil {
		t.Fatalf("IsLocked %s: %v", ref, err)
	}
	if got != locked {
		t.Errorf("%s: model %s LOCKED = %v, want %v", why, ref, got, locked)
	}
}

// ccLiveEntities counts ref's live entities.
func ccLiveEntities(t *testing.T, fx *pgFixture, ref spi.ModelRef) int {
	t.Helper()
	var n int
	if err := fx.db.QueryRow(context.Background(), `
		SELECT count(*) FROM entities
		WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3 AND NOT deleted`,
		string(fx.tenantID), ref.EntityName, ref.ModelVersion).Scan(&n); err != nil {
		t.Fatalf("count entities of %s: %v", ref, err)
	}
	return n
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

// TestCommitClaims_ModelCreatedAfterSnapshot_Conflict — a model created and
// locked after the transaction's snapshot has a lifecycle row the snapshot
// cannot see. The commit's probe insert meets it and fails 40001: the commit
// answers spi.ErrConflict, and the retry, on a fresh snapshot, commits.
func TestCommitClaims_ModelCreatedAfterSnapshot_Conflict(t *testing.T) {
	fx := newPGFixture(t)
	tm := fx.txManager(t)
	ref := spi.ModelRef{EntityName: "M", ModelVersion: "1"}

	txID, txCtx, es := ccBegin(t, fx, tm)
	ccModel(t, fx, ref, ccCodeKey)
	id := uuid.NewString()
	ccSave(t, txCtx, es, ccEntity(ref, id, `{"code":"x"}`))
	err := tm.Commit(boundedCtx(t, fx), txID)
	if !errors.Is(err, spi.ErrConflict) {
		t.Fatalf("Commit into a model created after the snapshot = %v, want spi.ErrConflict", err)
	}
	// The conflict is PostgreSQL's: the probe insert meets a row its
	// snapshot cannot see and raises 40001, which the design depends on.
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != pgerrcode.SerializationFailure || !strings.Contains(err.Error(), "probe the lifecycle") {
		t.Fatalf("Commit into a model created after the snapshot = %v, want the probe's 40001", err)
	}
	ccRequireAbsent(t, fx, id, "the refused commit")

	txID, txCtx, es = ccBegin(t, fx, tm)
	ccSave(t, txCtx, es, ccEntity(ref, id, `{"code":"x"}`))
	if err := tm.Commit(txCtx, txID); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	ccRequireClaimed(t, fx, ref, `"x"`, "the retry's value")
}

// TestCommitClaims_ModelNeverExisted_NotLocked — a model that does not exist
// fails the commit with spi.ErrModelNotLocked, and the probe's lifecycle row
// is rolled back with the transaction.
func TestCommitClaims_ModelNeverExisted_NotLocked(t *testing.T) {
	fx := newPGFixture(t)
	tm := fx.txManager(t)
	ref := spi.ModelRef{EntityName: "Ghost", ModelVersion: "1"}

	txID, txCtx, es := ccBegin(t, fx, tm)
	id := uuid.NewString()
	ccSave(t, txCtx, es, ccEntity(ref, id, `{"code":"x"}`))
	err := tm.Commit(boundedCtx(t, fx), txID)
	if !errors.Is(err, spi.ErrModelNotLocked) {
		t.Fatalf("Commit into a model that never existed = %v, want spi.ErrModelNotLocked", err)
	}
	ccRequireAbsent(t, fx, id, "the refused commit")
	if _, ok := lifecycleGeneration(t, fx, ref); ok {
		t.Error("the refused commit left a lifecycle row behind")
	}
}

// TestCommitClaims_LifecycleBumpedAfterSnapshot_Conflict — the model is
// unlocked, given new keys and locked again after the transaction's
// snapshot. The snapshot holds the old keys and a LOCKED state; the commit's
// FOR SHARE meets the lifecycle row bumped since and fails 40001, answered
// as spi.ErrConflict, so the old keys are never enforced. The retry enforces
// the new ones.
func TestCommitClaims_LifecycleBumpedAfterSnapshot_Conflict(t *testing.T) {
	fx := newPGFixture(t)
	tm := fx.txManager(t)
	ref := spi.ModelRef{EntityName: "M", ModelVersion: "1"}
	ccModel(t, fx, ref, ccCodeKey)

	txID, txCtx, es := ccBegin(t, fx, tm)
	id := uuid.NewString()
	ccSave(t, txCtx, es, ccEntity(ref, id, `{"code":"x","other":"y"}`))

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

	err := tm.Commit(boundedCtx(t, fx), txID)
	if !errors.Is(err, spi.ErrConflict) {
		t.Fatalf("Commit after the lifecycle was bumped = %v, want spi.ErrConflict", err)
	}
	ccRequireAbsent(t, fx, id, "the refused commit")

	txID, txCtx, es = ccBegin(t, fx, tm)
	ccSave(t, txCtx, es, ccEntity(ref, id, `{"code":"x","other":"y"}`))
	if err := tm.Commit(txCtx, txID); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	_, err = ccStore(t, fx).Save(boundedCtx(t, fx), ccEntity(ref, uuid.NewString(), `{"code":"z","other":"y"}`))
	if !errors.Is(err, spi.ErrUniqueViolation) {
		t.Errorf("a save of the new key's value = %v, want spi.ErrUniqueViolation", err)
	}
	if _, err := ccStore(t, fx).Save(boundedCtx(t, fx), ccEntity(ref, uuid.NewString(), `{"code":"x","other":"w"}`)); err != nil {
		t.Errorf("a save of the old key's value: %v (the old key is gone)", err)
	}
}

// TestCommitClaims_ExtendAndChangeLevel_DoNotBlock — an extension and a
// change-level change touch the models row and not the lifecycle row. Neither
// committed during the transaction nor holding the models row at its commit
// does either fail or delay the commit.
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

// TestNonTxSave_WaitsOnUncommittedUnlock_Refused — a save outside a
// transaction takes FOR SHARE on the lifecycle row as a statement of its
// own: it waits for an uncommitted Unlock's bump, and its models read, a
// later statement, sees the Unlock once it committed. The save is refused
// with spi.ErrModelNotLocked and stores nothing.
func TestNonTxSave_WaitsOnUncommittedUnlock_Refused(t *testing.T) {
	fx := newPGFixture(t)
	fx.txManager(t)
	ref := spi.ModelRef{EntityName: "M", ModelVersion: "1"}
	ccModel(t, fx, ref, ccCodeKey)

	unlock := beginOther(t, fx)
	if err := updateStateField(context.Background(), unlock, fx.tenantID, ref, spi.ModelUnlocked, "unlock"); err != nil {
		t.Fatalf("unlock's state update: %v", err)
	}
	if err := bumpLifecycle(context.Background(), unlock, fx.tenantID, ref); err != nil {
		t.Fatalf("unlock's bump: %v", err)
	}

	id := uuid.NewString()
	var saveErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, saveErr = ccStore(t, fx).Save(fx.ctx, ccEntity(ref, id, `{"code":"x"}`))
	}()
	waitForLockWaiter(t, fx, "model_lifecycle", done)
	if err := unlock.Commit(context.Background()); err != nil {
		t.Fatalf("commit the unlock: %v", err)
	}
	<-done
	if !errors.Is(saveErr, spi.ErrModelNotLocked) {
		t.Fatalf("a save after the Unlock committed = %v, want spi.ErrModelNotLocked", saveErr)
	}
	ccRequireAbsent(t, fx, id, "the refused save")
}

// TestInterleave_LockUncommittedThenUnlockThenCommit — a Lock of an UNLOCKED
// model is in flight. An entity commit into the model waits on the Lock's
// bump, and an Unlock waits on the Lock's models row. When the Lock commits,
// the entity commit, whose snapshot predates the Lock, fails 40001 and the
// Unlock goes through: there is never an entity in an unlocked model.
func TestInterleave_LockUncommittedThenUnlockThenCommit(t *testing.T) {
	fx := newPGFixture(t)
	tm := fx.txManager(t)
	ref := spi.ModelRef{EntityName: "M", ModelVersion: "1"}
	ccModel(t, fx, ref, ccCodeKey)
	if err := fx.store.Unlock(fx.ctx, ref); err != nil {
		t.Fatalf("Unlock: %v", err)
	}

	lock := beginOther(t, fx)
	if _, err := lockIfUnlocked(context.Background(), lock, fx.tenantID, ref); err != nil {
		t.Fatalf("lock's state update: %v", err)
	}
	if err := bumpLifecycle(context.Background(), lock, fx.tenantID, ref); err != nil {
		t.Fatalf("lock's bump: %v", err)
	}

	txID, txCtx, es := ccBegin(t, fx, tm)
	id := uuid.NewString()
	ccSave(t, txCtx, es, ccEntity(ref, id, `{"code":"x"}`))
	var commitErr error
	commitDone := make(chan struct{})
	go func() {
		defer close(commitDone)
		commitErr = tm.Commit(txCtx, txID)
	}()
	waitForLockWaiter(t, fx, "model_lifecycle", commitDone)

	var unlockErr error
	unlockDone := make(chan struct{})
	go func() {
		defer close(unlockDone)
		unlockErr = fx.store.Unlock(fx.ctx, ref)
	}()
	waitForLockWaiter(t, fx, "UPDATE models", unlockDone)

	if err := lock.Commit(context.Background()); err != nil {
		t.Fatalf("commit the lock: %v", err)
	}
	<-commitDone
	<-unlockDone
	if !errors.Is(commitErr, spi.ErrConflict) {
		t.Errorf("the entity commit = %v, want spi.ErrConflict", commitErr)
	}
	if unlockErr != nil {
		t.Errorf("the Unlock = %v, want success", unlockErr)
	}
	ccRequireState(t, fx, ref, false, "after the Unlock")
	if n := ccLiveEntities(t, fx, ref); n != 0 {
		t.Errorf("an UNLOCKED model has %d live entities, want 0", n)
	}
}

// TestUnlock_WaitsOnCommitHoldingLifecycleRow — an entity commit holds the
// model's lifecycle row FOR SHARE from its first step to its end; here it
// waits in its claim insert on another transaction's uncommitted claim of
// the same value. An Unlock of the model waits on the commit's lock. When the
// commit goes through, the Unlock answers *spi.ModelHasEntitiesError and the
// model stays LOCKED; when the commit is refused and rolls back, the Unlock
// goes through.
func TestUnlock_WaitsOnCommitHoldingLifecycleRow(t *testing.T) {
	for _, writerCommits := range []bool{true, false} {
		t.Run(fmt.Sprintf("writerCommits=%v", writerCommits), func(t *testing.T) {
			fx := newPGFixture(t)
			tm := fx.txManager(t)
			ref := spi.ModelRef{EntityName: "M", ModelVersion: "1"}
			ccModel(t, fx, ref, ccCodeKey)

			// A rival holding an uncommitted claim of the value: the commit's
			// claim insert waits on it, with the lifecycle row held.
			rival := beginOther(t, fx)
			if _, err := rival.Exec(context.Background(), `
				INSERT INTO unique_claims (tenant_id, model_name, model_version, key_id, signature, entity_id)
				SELECT $1, $2, $3, $4, signature, 'rival'
				  FROM (SELECT $5::text AS signature) s`,
				string(fx.tenantID), ref.EntityName, ref.ModelVersion, ccCodeKey.ID,
				ccSignature(t, `{"code":"x"}`)); err != nil {
				t.Fatalf("rival claim: %v", err)
			}

			txID, txCtx, es := ccBegin(t, fx, tm)
			id := uuid.NewString()
			ccSave(t, txCtx, es, ccEntity(ref, id, `{"code":"x"}`))
			var commitErr error
			commitDone := make(chan struct{})
			go func() {
				defer close(commitDone)
				commitErr = tm.Commit(txCtx, txID)
			}()
			waitForLockWaiter(t, fx, "unique_claims", commitDone)

			var unlockErr error
			unlockDone := make(chan struct{})
			go func() {
				defer close(unlockDone)
				unlockErr = fx.store.Unlock(fx.ctx, ref)
			}()
			waitForLockWaiter(t, fx, "model_lifecycle", unlockDone)

			// The writer commits when the rival rolls back, and is refused
			// when the rival commits.
			if writerCommits {
				if err := rival.Rollback(context.Background()); err != nil {
					t.Fatalf("roll the rival back: %v", err)
				}
			} else if err := rival.Commit(context.Background()); err != nil {
				t.Fatalf("commit the rival: %v", err)
			}
			<-commitDone
			<-unlockDone

			if writerCommits {
				if commitErr != nil {
					t.Fatalf("the entity commit: %v", commitErr)
				}
				var mhe *spi.ModelHasEntitiesError
				if !errors.As(unlockErr, &mhe) || mhe.Count != 1 {
					t.Fatalf("the Unlock after the commit = %v, want *spi.ModelHasEntitiesError with count 1", unlockErr)
				}
				ccRequireState(t, fx, ref, true, "a refused Unlock")
				return
			}
			if !errors.Is(commitErr, spi.ErrUniqueViolation) {
				t.Fatalf("the entity commit = %v, want spi.ErrUniqueViolation", commitErr)
			}
			if unlockErr != nil {
				t.Fatalf("the Unlock after the commit rolled back: %v", unlockErr)
			}
			ccRequireState(t, fx, ref, false, "the Unlock")
			ccRequireAbsent(t, fx, id, "the refused commit")
		})
	}
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

// ccSignature is the claim signature of data's $.code under ccCodeKey.
func ccSignature(t *testing.T, data string) string {
	t.Helper()
	claims, err := spi.ComputeClaims([]spi.UniqueKey{ccCodeKey}, []byte(data))
	if err != nil || len(claims) != 1 {
		t.Fatalf("ComputeClaims(%s) = %v, %v", data, claims, err)
	}
	return claims[0].Signature
}
