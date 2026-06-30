package postgres_test

// unique_claims_test.go — integration tests for composite unique-key enforcement
// in the postgres entity store. These tests require a real PostgreSQL instance
// (CYODA_TEST_DB_URL / docker-compose dev-up). They exercise the full stack:
// replaceClaims → unique_claims_uq UNIQUE index → classifyError → ErrUniqueViolation.

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

// setupUCStore creates a fresh schema and returns an EntityStore bound to
// "uc-tenant", a base context with that tenant, and a helper that counts
// rows in the unique_claims table for "uc-tenant".
func setupUCStore(t *testing.T) (spi.EntityStore, context.Context, func() int) {
	t.Helper()
	factory := setupEntityTest(t)
	baseCtx := ctxWithTenant("uc-tenant")
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

// emailKeys returns a single composite unique key over the $.email field.
func emailKeys() []spi.UniqueKey {
	return []spi.UniqueKey{{ID: "email-key", Fields: []string{"$.email"}}}
}

// ucEntity builds a minimal entity with the given id and email payload.
func ucEntity(id, email string) *spi.Entity {
	return &spi.Entity{
		Meta: spi.EntityMeta{
			ID:       id,
			ModelRef: spi.ModelRef{EntityName: "UCModel", ModelVersion: "1"},
		},
		Data: []byte(fmt.Sprintf(`{"email":%q}`, email)),
	}
}

// TestUniqueClaims_DuplicateSignature verifies that saving two distinct entities
// with the same key value returns spi.ErrUniqueViolation on the second save.
func TestUniqueClaims_DuplicateSignature(t *testing.T) {
	store, baseCtx, _ := setupUCStore(t)
	ctx := spi.WithUniqueKeys(baseCtx, emailKeys())

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
	store, baseCtx, _ := setupUCStore(t)
	ctx := spi.WithUniqueKeys(baseCtx, emailKeys())

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
	store, baseCtx, countClaims := setupUCStore(t)
	ctx := spi.WithUniqueKeys(baseCtx, emailKeys())
	modelRef := spi.ModelRef{EntityName: "UCModel", ModelVersion: "1"}

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
	store, baseCtx, _ := setupUCStore(t)
	ctx := spi.WithUniqueKeys(baseCtx, emailKeys())

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
	store, baseCtx, _ := setupUCStore(t)
	ctx := spi.WithUniqueKeys(baseCtx, emailKeys())

	// $.email is an object rather than a scalar string.
	ent := &spi.Entity{
		Meta: spi.EntityMeta{
			ID:       "e1",
			ModelRef: spi.ModelRef{EntityName: "UCModel", ModelVersion: "1"},
		},
		Data: []byte(`{"email":{"nested":"object"}}`),
	}
	_, err := store.Save(ctx, ent)
	if !errors.Is(err, spi.ErrPartialUniqueKey) {
		t.Fatalf("non-scalar key path: expected ErrPartialUniqueKey, got %v", err)
	}
}

// TestUniqueClaims_NoContextKeys verifies that when no unique keys are present
// in the context, Save succeeds and writes zero rows to unique_claims.
func TestUniqueClaims_NoContextKeys(t *testing.T) {
	store, baseCtx, countClaims := setupUCStore(t)
	// Intentionally no spi.WithUniqueKeys — baseline context only.

	if _, err := store.Save(baseCtx, ucEntity("e1", "a@x.com")); err != nil {
		t.Fatalf("Save without unique keys: %v", err)
	}
	if n := countClaims(); n != 0 {
		t.Errorf("expected 0 claim rows when context carries no unique keys, got %d", n)
	}
}

// TestUniqueClaims_UpdateToAllNullFreesValue is the regression test for B1:
// when an entity is updated so that ALL declared key fields go null/absent
// (the "all-null exempt" case), the old claim row must be deleted.
// A subsequent entity claiming the same value must succeed — not see a
// spurious 409.
func TestUniqueClaims_UpdateToAllNullFreesValue(t *testing.T) {
	store, baseCtx, countClaims := setupUCStore(t)
	ctx := spi.WithUniqueKeys(baseCtx, emailKeys())

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
			ModelRef: spi.ModelRef{EntityName: "UCModel", ModelVersion: "1"},
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

// TestUniqueClaims_SameTransactionDeleteAndReclaim verifies that within ONE
// transaction, deleting entity A (which holds a key value) and creating entity B
// with that same value commits successfully — A's delete frees the value for B.
// Mirrors plugins/memory and plugins/sqlite. Postgres enforces claims inline
// (no buffer), so the delete's claim-release must precede B's claim-insert within
// the tx — which the delete-first ordering here guarantees.
func TestUniqueClaims_SameTransactionDeleteAndReclaim(t *testing.T) {
	factory, tm := setupEntityTestWithTM(t)
	baseCtx := ctxWithTenant("uc-tenant")
	store, err := factory.EntityStore(baseCtx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}

	ctx := spi.WithUniqueKeys(baseCtx, emailKeys())

	// Pre-condition (non-tx): entity A holds "shared@x.com".
	if _, err := store.Save(ctx, ucEntity("a", "shared@x.com")); err != nil {
		t.Fatalf("pre-save A: %v", err)
	}

	txID, txCtx, err := tm.Begin(baseCtx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	// Same transaction: delete A, then create B with A's former value.
	if err := store.Delete(txCtx, "a"); err != nil {
		t.Fatalf("tx Delete a: %v", err)
	}
	if _, err := store.Save(spi.WithUniqueKeys(txCtx, emailKeys()), ucEntity("b", "shared@x.com")); err != nil {
		t.Fatalf("tx Save b: %v", err)
	}

	// Commit must succeed: A's claim is released by the delete, so B may hold it.
	if err := tm.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: expected success (delete-then-reclaim), got %v", err)
	}

	// Post-conditions: A is gone, B holds the value.
	if exists, err := store.Exists(baseCtx, "a"); err != nil || exists {
		t.Errorf("a should not exist after tx delete: exists=%v err=%v", exists, err)
	}
	b, err := store.Get(baseCtx, "b")
	if err != nil {
		t.Fatalf("Get b: %v", err)
	}
	if string(b.Data) != `{"email":"shared@x.com"}` {
		t.Errorf("b.Data = %s, want {\"email\":\"shared@x.com\"}", b.Data)
	}

	// B's claim is enforced: a third entity cannot steal the value.
	if _, err := store.Save(ctx, ucEntity("c", "shared@x.com")); !errors.Is(err, spi.ErrUniqueViolation) {
		t.Errorf("c with b's value: expected ErrUniqueViolation, got %v", err)
	}
}
