package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

// The one-version rule — within one transaction an entity has at most one
// pending version, which a later write replaces in place — is pinned for
// every backend by the spitest OneVersionPerTx cases. The tests here cover
// what is specific to how postgres finds the pending version: by the
// entity_versions row's transaction_id column, which a write outside a
// transaction leaves empty.

func oneVersionEntity(id string, mref spi.ModelRef, data string) *spi.Entity {
	return &spi.Entity{
		Meta: spi.EntityMeta{ID: id, ModelRef: mref, State: "LIVE"},
		Data: []byte(data),
	}
}

func versionsOf(ctx context.Context, t *testing.T, store spi.EntityStore, id string) []spi.EntityVersionMeta {
	t.Helper()
	metas, err := store.GetVersionMetadata(ctx, id, spi.VersionMetadataOptions{})
	if err != nil {
		t.Fatalf("GetVersionMetadata: %v", err)
	}
	return metas
}

// TestSaveOn_NonTransactionalAddsVersionEachTime pins that a write outside a
// transaction is its own transaction and its own version. Such a write stores
// an empty transaction_id, so two of them in a row look alike by that column;
// the pending-version lookup must not take the first for the second's pending
// version.
func TestSaveOn_NonTransactionalAddsVersionEachTime(t *testing.T) {
	factory := setupEntityTest(t)
	ctx := ctxWithTenant("entity-tenant")
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	mref := spi.ModelRef{EntityName: "one-version-nontx", ModelVersion: "1"}
	id := uuid.NewString()

	for i, data := range []string{`{"n":1}`, `{"n":2}`} {
		v, err := store.Save(ctx, oneVersionEntity(id, mref, data))
		if err != nil {
			t.Fatalf("Save %d: %v", i+1, err)
		}
		if v != int64(i+1) {
			t.Fatalf("Save %d returned version %d, want %d", i+1, v, i+1)
		}
	}

	metas := versionsOf(ctx, t, store, id)
	if len(metas) != 2 {
		t.Fatalf("two saves outside a transaction must record two versions, got %d", len(metas))
	}
	if metas[0].ChangeType != "UPDATED" || metas[1].ChangeType != "CREATED" {
		t.Fatalf("change types newest first: got %s, %s; want UPDATED, CREATED", metas[0].ChangeType, metas[1].ChangeType)
	}

	if err := store.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := len(versionsOf(ctx, t, store, id)); got != 3 {
		t.Fatalf("a delete outside a transaction must record its own tombstone version, got %d versions", got)
	}
}

// TestSaveOn_PendingVersionKeepsTheModelGuard pins that replacing a pending
// version in place still refuses a write under a different model: an entity's
// model is fixed at creation, also within the transaction that creates it.
func TestSaveOn_PendingVersionKeepsTheModelGuard(t *testing.T) {
	factory, tm := setupEntityTestWithTM(t)
	baseCtx := ctxWithTenant("entity-tenant")
	mref := spi.ModelRef{EntityName: "one-version-model", ModelVersion: "1"}
	other := spi.ModelRef{EntityName: "one-version-model", ModelVersion: "2"}
	id := uuid.NewString()

	txID, txCtx := postgres.BeginGuardedForTest(t, tm, baseCtx)
	txStore, err := factory.EntityStore(txCtx)
	if err != nil {
		t.Fatalf("EntityStore (tx): %v", err)
	}
	if _, err := txStore.Save(txCtx, oneVersionEntity(id, mref, `{"n":1}`)); err != nil {
		t.Fatalf("tx save: %v", err)
	}
	_, err = txStore.Save(txCtx, oneVersionEntity(id, other, `{"n":2}`))
	if !errors.Is(err, spi.ErrEntityModelMismatch) {
		t.Fatalf("a second save under a different model in the same transaction must wrap ErrEntityModelMismatch, got %v", err)
	}
	if err := tm.Rollback(txCtx, txID); err != nil {
		t.Fatalf("rollback: %v", err)
	}
}

// TestSaveOn_RecreateAfterCommittedDeleteStaysUpdated pins where the change
// type of a replaced pending version comes from: whether the entity has a
// version from before this transaction. An entity deleted by an earlier
// transaction has one, so a transaction that saves it twice records one
// UPDATED version — not CREATED, although the entity was deleted when the
// transaction began.
func TestSaveOn_RecreateAfterCommittedDeleteStaysUpdated(t *testing.T) {
	factory, tm := setupEntityTestWithTM(t)
	baseCtx := ctxWithTenant("entity-tenant")
	store, err := factory.EntityStore(baseCtx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	mref := spi.ModelRef{EntityName: "one-version-recreate", ModelVersion: "1"}
	id := uuid.NewString()

	if _, err := store.Save(baseCtx, oneVersionEntity(id, mref, `{"n":0}`)); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	if err := store.Delete(baseCtx, id); err != nil {
		t.Fatalf("seed Delete: %v", err)
	}

	txID, txCtx := postgres.BeginGuardedForTest(t, tm, baseCtx)
	txStore, err := factory.EntityStore(txCtx)
	if err != nil {
		t.Fatalf("EntityStore (tx): %v", err)
	}
	for _, data := range []string{`{"n":1}`, `{"n":2}`} {
		if _, err := txStore.Save(txCtx, oneVersionEntity(id, mref, data)); err != nil {
			t.Fatalf("tx save %s: %v", data, err)
		}
	}
	if err := tm.Commit(baseCtx, txID); err != nil {
		t.Fatalf("commit: %v", err)
	}

	metas := versionsOf(baseCtx, t, store, id)
	if len(metas) != 3 {
		t.Fatalf("seed, tombstone and the transaction's one version: want 3 versions, got %d", len(metas))
	}
	if metas[0].TransactionID != txID || metas[0].ChangeType != "UPDATED" || metas[0].Version != 3 {
		t.Fatalf("the transaction's version: got {version %d, %s, tx %q}; want {version 3, UPDATED, tx %q}",
			metas[0].Version, metas[0].ChangeType, metas[0].TransactionID, txID)
	}
	got, err := store.Get(baseCtx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got.Data) != `{"n":2}` || got.Meta.Version != 3 {
		t.Fatalf("current entity: got version %d %s; want version 3 {\"n\":2}", got.Meta.Version, got.Data)
	}
}
