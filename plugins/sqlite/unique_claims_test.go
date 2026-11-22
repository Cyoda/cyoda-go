package sqlite_test

// unique_claims_test.go — integration tests for composite unique-key enforcement
// in the sqlite entity store. The keys are declared on the stored model only;
// the store reads them at commit (or at a save outside a transaction).

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	spi "github.com/cyoda/cyoda-go-spi"
	"github.com/cyoda/cyoda-go-spi/spitest"
	sqlite3 "github.com/ncruces/go-sqlite3"

	"github.com/cyoda/cyoda-go/plugins/sqlite"
)

// ucFixture is a fresh StoreFactory with UCModel created and LOCKED in
// "uc-tenant".
type ucFixture struct {
	factory     *sqlite.StoreFactory
	store       spi.EntityStore
	tm          spi.TransactionManager
	ctx         context.Context
	countClaims func() int // rows in unique_claims for "uc-tenant"
}

// setupUC creates a fresh StoreFactory and locks UCModel with keys as its
// unique keys.
func setupUC(t *testing.T, keys ...spi.UniqueKey) ucFixture {
	t.Helper()
	factory, err := sqlite.NewStoreFactoryForTest(context.Background(), filepath.Join(t.TempDir(), "uc.db"))
	if err != nil {
		t.Fatalf("NewStoreFactoryForTest: %v", err)
	}
	t.Cleanup(func() { _ = factory.Close() })

	ctx := testCtx("uc-tenant")
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	tm, err := factory.TransactionManager(ctx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	spitest.EnsureLockedModel(t, factory, ctx, ucModel, keys...)

	db := sqlite.DBForTest(factory)
	countClaims := func() int {
		var n int
		if err := db.QueryRowContext(context.Background(),
			"SELECT count(*) FROM unique_claims WHERE tenant_id = ?", "uc-tenant",
		).Scan(&n); err != nil {
			t.Fatalf("count unique_claims: %v", err)
		}
		return n
	}
	return ucFixture{factory: factory, store: store, tm: tm, ctx: ctx, countClaims: countClaims}
}

// ucModel is the model of every ucEntity.
var ucModel = spi.ModelRef{EntityName: "UCModel", ModelVersion: "1"}

// emailKeys returns a single composite unique key over the $.email field.
func emailKeys() []spi.UniqueKey {
	return []spi.UniqueKey{{ID: "email-key", Fields: []string{"$.email"}}}
}

// ucEntity builds a minimal entity with the given id and email payload.
func ucEntity(id, email string) *spi.Entity {
	return &spi.Entity{
		Meta: spi.EntityMeta{
			ID:       id,
			ModelRef: ucModel,
		},
		Data: []byte(fmt.Sprintf(`{"email":%q}`, email)),
	}
}

// TestUniqueClaims_DuplicateSignature verifies that saving two distinct entities
// with the same key value returns spi.ErrUniqueViolation on the second save.
func TestUniqueClaims_DuplicateSignature(t *testing.T) {
	fx := setupUC(t, emailKeys()...)
	store, ctx := fx.store, fx.ctx

	if _, err := store.Save(ctx, ucEntity("e1", "a@x.com")); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	_, err := store.Save(ctx, ucEntity("e2", "a@x.com"))
	if !errors.Is(err, spi.ErrUniqueViolation) {
		t.Fatalf("duplicate key: expected ErrUniqueViolation, got %v", err)
	}
}

// TestUniqueClaims_SoftDeleteFreesValue verifies that soft-deleting the holder
// of a key value allows another entity to claim that same value.
func TestUniqueClaims_SoftDeleteFreesValue(t *testing.T) {
	fx := setupUC(t, emailKeys()...)
	store, ctx := fx.store, fx.ctx

	if _, err := store.Save(ctx, ucEntity("e1", "a@x.com")); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	if err := store.Delete(ctx, "e1"); err != nil {
		t.Fatalf("Delete e1: %v", err)
	}
	// e2 should now claim the freed value without conflict.
	if _, err := store.Save(ctx, ucEntity("e2", "a@x.com")); err != nil {
		t.Fatalf("Save e2 after delete: %v", err)
	}
}

// TestUniqueClaims_DeleteAllFreesValues verifies that DeleteAll releases all
// claim rows for the model, allowing previously-used values to be re-saved.
func TestUniqueClaims_DeleteAllFreesValues(t *testing.T) {
	fx := setupUC(t, emailKeys()...)
	store, ctx, countClaims := fx.store, fx.ctx, fx.countClaims
	modelRef := ucModel

	if _, err := store.Save(ctx, ucEntity("e1", "a@x.com")); err != nil {
		t.Fatalf("Save e1: %v", err)
	}
	if _, err := store.Save(ctx, ucEntity("e2", "b@x.com")); err != nil {
		t.Fatalf("Save e2: %v", err)
	}
	if err := store.DeleteAll(ctx, modelRef); err != nil {
		t.Fatalf("DeleteAll: %v", err)
	}
	if n := countClaims(); n != 0 {
		t.Errorf("expected 0 claim rows after DeleteAll, got %d", n)
	}
	// Re-saving with the same value must succeed.
	if _, err := store.Save(ctx, ucEntity("e1", "a@x.com")); err != nil {
		t.Fatalf("re-Save after DeleteAll: %v", err)
	}
}

// TestUniqueClaims_UpdateMovesKey verifies that updating an entity to a new key
// value frees the old claim so another entity can take the old value.
func TestUniqueClaims_UpdateMovesKey(t *testing.T) {
	fx := setupUC(t, emailKeys()...)
	store, ctx := fx.store, fx.ctx

	// e1 claims a@x.com.
	if _, err := store.Save(ctx, ucEntity("e1", "a@x.com")); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	// e1 is updated to b@x.com — old claim must be released.
	if _, err := store.Save(ctx, ucEntity("e1", "b@x.com")); err != nil {
		t.Fatalf("update Save: %v", err)
	}
	// e2 may now claim the previously-held a@x.com.
	if _, err := store.Save(ctx, ucEntity("e2", "a@x.com")); err != nil {
		t.Fatalf("Save e2 with freed old value: %v", err)
	}
	// e3 must NOT steal b@x.com (still held by e1).
	_, err := store.Save(ctx, ucEntity("e3", "b@x.com"))
	if !errors.Is(err, spi.ErrUniqueViolation) {
		t.Fatalf("Save e3 with e1's active value: expected ErrUniqueViolation, got %v", err)
	}
}

// TestUniqueClaims_MixedModelBatch: two entities of different models, each
// with its own unique key, are committed in one transaction. Each is enforced
// with its own model's stored key: B's data has no $.emailA, so enforcing A's
// key on B would leave B with no claim and its duplicate would be accepted.
func TestUniqueClaims_MixedModelBatch(t *testing.T) {
	fx := setupUC(t)
	m1 := spi.ModelRef{EntityName: "M1", ModelVersion: "1"}
	m2 := spi.ModelRef{EntityName: "M2", ModelVersion: "1"}
	spitest.EnsureLockedModel(t, fx.factory, fx.ctx, m1, spi.UniqueKey{ID: "key-a", Fields: []string{"$.emailA"}})
	spitest.EnsureLockedModel(t, fx.factory, fx.ctx, m2, spi.UniqueKey{ID: "key-b", Fields: []string{"$.emailB"}})

	txID, txCtx, err := fx.tm.Begin(fx.ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := fx.store.Save(txCtx, &spi.Entity{
		Meta: spi.EntityMeta{ID: "a1", ModelRef: m1},
		Data: []byte(`{"emailA":"a@m1.com"}`),
	}); err != nil {
		t.Fatalf("Save A: %v", err)
	}
	if _, err := fx.store.Save(txCtx, &spi.Entity{
		Meta: spi.EntityMeta{ID: "b1", ModelRef: m2},
		Data: []byte(`{"emailB":"b@m2.com"}`),
	}); err != nil {
		t.Fatalf("Save B: %v", err)
	}
	if err := fx.tm.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	_, err = fx.store.Save(fx.ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: "a2", ModelRef: m1},
		Data: []byte(`{"emailA":"a@m1.com"}`),
	})
	if !errors.Is(err, spi.ErrUniqueViolation) {
		t.Fatalf("A's key not enforced: expected ErrUniqueViolation, got %v", err)
	}
	_, err = fx.store.Save(fx.ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: "b2", ModelRef: m2},
		Data: []byte(`{"emailB":"b@m2.com"}`),
	})
	if !errors.Is(err, spi.ErrUniqueViolation) {
		t.Fatalf("B's key not enforced: expected ErrUniqueViolation, got %v", err)
	}
}

// TestUniqueClaims_NonScalarKeyPath verifies that a non-scalar value at a
// declared key path causes Save to return ErrPartialUniqueKey (not a 5xx).
func TestUniqueClaims_NonScalarKeyPath(t *testing.T) {
	fx := setupUC(t, emailKeys()...)
	store, ctx := fx.store, fx.ctx

	ent := &spi.Entity{
		Meta: spi.EntityMeta{
			ID:       "e1",
			ModelRef: ucModel,
		},
		Data: []byte(`{"email":{"nested":"object"}}`),
	}
	_, err := store.Save(ctx, ent)
	if !errors.Is(err, spi.ErrPartialUniqueKey) {
		t.Fatalf("non-scalar key path: expected ErrPartialUniqueKey, got %v", err)
	}
}

// TestUniqueClaims_KeylessModelWritesNoClaims verifies that an entity of a
// model with no unique keys is saved and writes zero rows to unique_claims.
func TestUniqueClaims_KeylessModelWritesNoClaims(t *testing.T) {
	fx := setupUC(t)

	if _, err := fx.store.Save(fx.ctx, ucEntity("e1", "a@x.com")); err != nil {
		t.Fatalf("Save into a keyless model: %v", err)
	}
	if n := fx.countClaims(); n != 0 {
		t.Errorf("expected 0 claim rows for a keyless model, got %d", n)
	}
}

// TestUniqueClaims_ClassifyClaimError_Discrimination verifies that
// classifyClaimError maps CONSTRAINT_UNIQUE to ErrUniqueViolation and NOT to
// ErrConflict, while classifyError maps CONSTRAINT_UNIQUE to ErrConflict and
// NOT to ErrUniqueViolation. The two functions are used on distinct code paths
// so that entity-PK conflicts are retryable (ErrConflict) and claim conflicts
// surface as a semantic 409 (ErrUniqueViolation).
func TestUniqueClaims_ClassifyClaimError_Discrimination(t *testing.T) {
	rawErr := sqlite3.CONSTRAINT_UNIQUE

	// classifyClaimError: CONSTRAINT_UNIQUE → ErrUniqueViolation, NOT ErrConflict.
	claimResult := sqlite.ClassifyClaimErrorForTest(rawErr)
	if !errors.Is(claimResult, spi.ErrUniqueViolation) {
		t.Errorf("classifyClaimError: expected ErrUniqueViolation, got %v", claimResult)
	}
	if errors.Is(claimResult, spi.ErrConflict) {
		t.Errorf("classifyClaimError: must NOT also be ErrConflict, got %v", claimResult)
	}

	// classifyError: CONSTRAINT_UNIQUE → ErrConflict, NOT ErrUniqueViolation.
	entityResult := sqlite.ClassifyErrorForTest(rawErr)
	if !errors.Is(entityResult, spi.ErrConflict) {
		t.Errorf("classifyError: expected ErrConflict, got %v", entityResult)
	}
	if errors.Is(entityResult, spi.ErrUniqueViolation) {
		t.Errorf("classifyError: must NOT also be ErrUniqueViolation, got %v", entityResult)
	}
}

// TestUniqueClaims_UpdateToAllNullFreesValue is the regression test for B1:
// when an entity is updated so that ALL declared key fields go null/absent
// (the "all-null exempt" case), the old claim row must be deleted.
// A subsequent entity claiming the same value must succeed — not see a
// spurious 409.
func TestUniqueClaims_UpdateToAllNullFreesValue(t *testing.T) {
	fx := setupUC(t, emailKeys()...)
	store, ctx, countClaims := fx.store, fx.ctx, fx.countClaims

	// e1 claims a@x.com.
	if _, err := store.Save(ctx, ucEntity("e1", "a@x.com")); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	if n := countClaims(); n != 1 {
		t.Fatalf("expected 1 claim after first Save, got %d", n)
	}

	// Update e1 so the email field is absent — all-null exempt.
	// This must NOT be a 422 and must release the old claim.
	nullEntity := &spi.Entity{
		Meta: spi.EntityMeta{
			ID:       "e1",
			ModelRef: ucModel,
		},
		Data: []byte(`{"status":"updated"}`), // no email field
	}
	if _, err := store.Save(ctx, nullEntity); err != nil {
		t.Fatalf("all-null update: expected success, got %v", err)
	}
	// Old claim must be gone.
	if n := countClaims(); n != 0 {
		t.Errorf("expected 0 claim rows after all-null update, got %d", n)
	}

	// e2 must now be able to claim a@x.com (previously held by e1).
	if _, err := store.Save(ctx, ucEntity("e2", "a@x.com")); err != nil {
		t.Fatalf("Save e2 with freed value: %v", err)
	}
}

// TestUniqueClaims_SameTxReclaim_BothOrdersCommit: within one transaction,
// entity A (which holds a value) is deleted and entity B is created with that
// value. The commit judges only the final state — A gone, B holding the value
// — so it succeeds whichever of the two writes came first, and B's claim is
// enforced afterwards.
func TestUniqueClaims_SameTxReclaim_BothOrdersCommit(t *testing.T) {
	for _, order := range []string{"DeleteFirst", "SaveFirst"} {
		t.Run(order, func(t *testing.T) {
			fx := setupUC(t, emailKeys()...)
			if _, err := fx.store.Save(fx.ctx, ucEntity("a", "shared@x.com")); err != nil {
				t.Fatalf("pre-save A: %v", err)
			}

			txID, txCtx, err := fx.tm.Begin(fx.ctx)
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			deleteA := func() {
				if err := fx.store.Delete(txCtx, "a"); err != nil {
					t.Fatalf("tx Delete a: %v", err)
				}
			}
			saveB := func() {
				if _, err := fx.store.Save(txCtx, ucEntity("b", "shared@x.com")); err != nil {
					t.Fatalf("tx Save b: %v", err)
				}
			}
			if order == "DeleteFirst" {
				deleteA()
				saveB()
			} else {
				saveB()
				deleteA()
			}
			if err := fx.tm.Commit(txCtx, txID); err != nil {
				t.Fatalf("Commit: the final state is unique, got %v", err)
			}

			if exists, err := fx.store.Exists(fx.ctx, "a"); err != nil || exists {
				t.Errorf("a should not exist after tx delete: exists=%v err=%v", exists, err)
			}
			b, err := fx.store.Get(fx.ctx, "b")
			if err != nil {
				t.Fatalf("Get b: %v", err)
			}
			if string(b.Data) != `{"email":"shared@x.com"}` {
				t.Errorf("b.Data = %s, want {\"email\":\"shared@x.com\"}", b.Data)
			}
			if _, err := fx.store.Save(fx.ctx, ucEntity("c", "shared@x.com")); !errors.Is(err, spi.ErrUniqueViolation) {
				t.Errorf("c with b's value: expected ErrUniqueViolation, got %v", err)
			}
		})
	}
}

// A CompareAndSave inside a transaction writes to the same buffer Save does,
// and its entity is claimed at commit like a saved one: the value it moves
// the entity to is held afterwards and the value it moved off is free.
func TestUniqueClaims_CompareAndSaveInTxRecordsClaims(t *testing.T) {
	fx := setupUC(t, emailKeys()...)

	// CompareAndSave never creates, so the entity is seeded first.
	seedTxID := saveCommitted(t, fx.tm, fx.store, fx.ctx, ucEntity("e1", "seed@x.com"))

	txID, txCtx, err := fx.tm.Begin(fx.ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := fx.store.CompareAndSave(txCtx, ucEntity("e1", "a@x.com"), seedTxID); err != nil {
		t.Fatalf("CompareAndSave: %v", err)
	}
	if err := fx.tm.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// A second transaction claiming the same value for a different entity
	// must lose at commit, exactly as it would against a committed Save.
	txID2, txCtx2, err := fx.tm.Begin(fx.ctx)
	if err != nil {
		t.Fatalf("Begin (second): %v", err)
	}
	if _, err := fx.store.Save(txCtx2, ucEntity("e2", "a@x.com")); err != nil {
		t.Fatalf("buffered Save: %v", err)
	}
	if err := fx.tm.Commit(txCtx2, txID2); !errors.Is(err, spi.ErrUniqueViolation) {
		t.Fatalf("e2 with e1's value: expected ErrUniqueViolation at commit, got %v", err)
	}

	// The value the compare-and-save moved e1 off is free.
	if _, err := fx.store.Save(fx.ctx, ucEntity("e3", "seed@x.com")); err != nil {
		t.Fatalf("e3 with e1's former value: %v", err)
	}
}
