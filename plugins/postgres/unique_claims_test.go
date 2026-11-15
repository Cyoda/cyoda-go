package postgres_test

// unique_claims_test.go — integration tests for composite unique-key enforcement
// in the postgres entity store. The keys are declared on the stored model
// (spitest.EnsureLockedModel); the store reads them from there. They exercise
// the full stack: the model's keys → claims → unique_claims_uq UNIQUE index →
// classifyError → ErrUniqueViolation.

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/spitest"
	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

// ucModel is ucEntity's model.
var ucModel = spi.ModelRef{EntityName: "UCModel", ModelVersion: "1"}

// setupUCStore creates a fresh schema with ucModel LOCKED and declaring keys,
// and returns an EntityStore bound to "uc-tenant", a base context with that
// tenant, and a helper that counts rows in the unique_claims table for
// "uc-tenant".
func setupUCStore(t *testing.T, keys ...spi.UniqueKey) (spi.EntityStore, context.Context, func() int) {
	t.Helper()
	factory := setupEntityTest(t)
	baseCtx := ctxWithTenant("uc-tenant")
	spitest.EnsureLockedModel(t, factory, baseCtx, ucModel, keys...)
	store, err := factory.EntityStore(baseCtx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	pool := postgres.PoolForTest(factory)
	countClaims := func() int {
		var n int
		// Pool connection bypasses RLS (owner role), so we can count directly.
		if err := pool.QueryRow(context.Background(),
			"SELECT count(*) FROM unique_claims WHERE tenant_id = $1", "uc-tenant",
		).Scan(&n); err != nil {
			t.Fatalf("count unique_claims: %v", err)
		}
		return n
	}
	return store, baseCtx, countClaims
}

// emailKey is a single-field unique key over the $.email field.
var emailKey = spi.UniqueKey{ID: "email-key", Fields: []string{"$.email"}}

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
	store, ctx, _ := setupUCStore(t, emailKey)

	if _, err := store.Save(ctx, ucEntity("e1", "a@x.com")); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	_, err := store.Save(ctx, ucEntity("e2", "a@x.com"))
	if !errors.Is(err, spi.ErrUniqueViolation) {
		t.Fatalf("second Save with duplicate key: expected ErrUniqueViolation, got %v", err)
	}
}

// TestUniqueClaims_SoftDeleteFreesValue verifies that soft-deleting the holder
// of a key value allows another entity to claim that same value.
func TestUniqueClaims_SoftDeleteFreesValue(t *testing.T) {
	store, ctx, _ := setupUCStore(t, emailKey)

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
	store, ctx, countClaims := setupUCStore(t, emailKey)
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
	store, ctx, _ := setupUCStore(t, emailKey)

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

// TestUniqueClaims_ClassifyError_Discrimination verifies that classifyError maps
// 23505 to ErrUniqueViolation ONLY when the constraint name is "unique_claims_uq".
// A 23505 on any other constraint (e.g. the entities PK) must pass through
// unchanged — not ErrUniqueViolation, not ErrConflict.
func TestUniqueClaims_ClassifyError_Discrimination(t *testing.T) {
	// unique_claims_uq → must be ErrUniqueViolation, must NOT be ErrConflict.
	claimErr := &pgconn.PgError{
		Code:           pgerrcode.UniqueViolation,
		ConstraintName: "unique_claims_uq",
	}
	result := postgres.ClassifyErrorForTest(claimErr)
	if !errors.Is(result, spi.ErrUniqueViolation) {
		t.Errorf("unique_claims_uq: expected ErrUniqueViolation, got %v", result)
	}
	if errors.Is(result, spi.ErrConflict) {
		t.Errorf("unique_claims_uq: must NOT also be ErrConflict, got %v", result)
	}

	// entities PK → must NOT be ErrUniqueViolation, must NOT be ErrConflict.
	pkErr := &pgconn.PgError{
		Code:           pgerrcode.UniqueViolation,
		ConstraintName: "entities_pkey",
	}
	result2 := postgres.ClassifyErrorForTest(pkErr)
	if errors.Is(result2, spi.ErrUniqueViolation) {
		t.Errorf("entities_pkey: must NOT be ErrUniqueViolation, got %v", result2)
	}
	if errors.Is(result2, spi.ErrConflict) {
		t.Errorf("entities_pkey: must NOT be ErrConflict, got %v", result2)
	}
}

// TestUniqueClaims_NonScalarKeyPath verifies that a non-scalar value at a
// declared key path causes Save to return ErrPartialUniqueKey (not a 5xx).
func TestUniqueClaims_NonScalarKeyPath(t *testing.T) {
	store, ctx, _ := setupUCStore(t, emailKey)

	// $.email is an object rather than a scalar string.
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

// TestUniqueClaims_KeylessModel verifies that a model declaring no unique
// keys claims nothing: Save succeeds and writes zero rows to unique_claims.
func TestUniqueClaims_KeylessModel(t *testing.T) {
	store, ctx, countClaims := setupUCStore(t)

	if _, err := store.Save(ctx, ucEntity("e1", "a@x.com")); err != nil {
		t.Fatalf("Save into a keyless model: %v", err)
	}
	if _, err := store.Save(ctx, ucEntity("e2", "a@x.com")); err != nil {
		t.Fatalf("Save of an equal value into a keyless model: %v", err)
	}
	if n := countClaims(); n != 0 {
		t.Errorf("expected 0 claim rows for a keyless model, got %d", n)
	}
}

// TestUniqueClaims_UpdateToAllNullFreesValue is the regression test for B1:
// when an entity is updated so that ALL declared key fields go null/absent
// (the "all-null exempt" case), the old claim row must be deleted.
// A subsequent entity claiming the same value must succeed — not see a
// spurious 409.
func TestUniqueClaims_UpdateToAllNullFreesValue(t *testing.T) {
	store, ctx, countClaims := setupUCStore(t, emailKey)

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

// ucTxSetup is setupUCStore with a transaction manager: a fresh schema with
// ucModel LOCKED declaring emailKey, a non-transactional store on the
// returned context, and entity "a" committed holding "shared@x.com".
func ucTxSetup(t *testing.T) (spi.EntityStore, *postgres.TransactionManager, context.Context) {
	t.Helper()
	factory, tm := setupEntityTestWithTM(t)
	ctx := ctxWithTenant("uc-tenant")
	spitest.EnsureLockedModel(t, factory, ctx, ucModel, emailKey)
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	if _, err := store.Save(ctx, ucEntity("a", "shared@x.com")); err != nil {
		t.Fatalf("pre-save A: %v", err)
	}
	return store, tm, ctx
}

// ucRequireReclaimed asserts that "a" is gone, "b" holds "shared@x.com", and
// the value is claimed: a third entity cannot take it.
func ucRequireReclaimed(t *testing.T, store spi.EntityStore, ctx context.Context) {
	t.Helper()
	if exists, err := store.Exists(ctx, "a"); err != nil || exists {
		t.Errorf("a should not exist after the tx delete: exists=%v err=%v", exists, err)
	}
	b, err := store.Get(ctx, "b")
	if err != nil {
		t.Fatalf("Get b: %v", err)
	}
	if string(b.Data) != `{"email":"shared@x.com"}` {
		t.Errorf("b.Data = %s, want {\"email\":\"shared@x.com\"}", b.Data)
	}
	if _, err := store.Save(ctx, ucEntity("c", "shared@x.com")); !errors.Is(err, spi.ErrUniqueViolation) {
		t.Errorf("c with b's value: expected ErrUniqueViolation, got %v", err)
	}
}

// TestUniqueClaims_SameTxReclaimAfterDelete_Commits verifies that within ONE
// transaction, deleting entity A (which holds a key value) and then creating
// entity B with that same value commits: the claims are computed at commit,
// from the transaction's final entities, in which only B holds the value.
func TestUniqueClaims_SameTxReclaimAfterDelete_Commits(t *testing.T) {
	store, tm, ctx := ucTxSetup(t)

	txID, txCtx := beginGuarded(t, tm, ctx)
	if err := store.Delete(txCtx, "a"); err != nil {
		t.Fatalf("tx Delete a: %v", err)
	}
	if _, err := store.Save(txCtx, ucEntity("b", "shared@x.com")); err != nil {
		t.Fatalf("tx Save b: %v", err)
	}
	if err := tm.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit (delete, then reclaim): %v", err)
	}
	ucRequireReclaimed(t, store, ctx)
}

// TestUniqueClaims_SameTxReclaimBeforeDelete_Commits verifies the other
// order: within ONE transaction, B claims A's value first and A is deleted
// after. A save inside a transaction claims nothing; the commit judges the
// final entities, in which only B holds the value, so it commits.
func TestUniqueClaims_SameTxReclaimBeforeDelete_Commits(t *testing.T) {
	store, tm, ctx := ucTxSetup(t)

	txID, txCtx := beginGuarded(t, tm, ctx)
	if _, err := store.Save(txCtx, ucEntity("b", "shared@x.com")); err != nil {
		t.Fatalf("tx Save b: a save inside a transaction claims nothing, got %v", err)
	}
	if err := store.Delete(txCtx, "a"); err != nil {
		t.Fatalf("tx Delete a: %v", err)
	}
	if err := tm.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit (reclaim, then delete): %v", err)
	}
	ucRequireReclaimed(t, store, ctx)
}
