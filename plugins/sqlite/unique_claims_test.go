package sqlite_test

// unique_claims_test.go — integration tests for composite unique-key enforcement
// in the sqlite entity store. Mirrors postgres unique_claims_test.go (Task 5.2)
// but exercises the sqlite-specific buffered-flush architecture: keys must be
// captured at Save (buffer) time, not read from the flush context.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	sqlite3 "github.com/ncruces/go-sqlite3"

	"github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

// setupUCStore creates a fresh StoreFactory, returns an EntityStore bound to
// "uc-tenant", a base context for that tenant, and a helper that counts rows
// in unique_claims for "uc-tenant".
func setupUCStore(t *testing.T) (spi.EntityStore, context.Context, func() int) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "uc.db")
	factory, err := sqlite.NewStoreFactoryForTest(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("NewStoreFactoryForTest: %v", err)
	}
	t.Cleanup(func() { _ = factory.Close() })

	baseCtx := testCtx("uc-tenant")
	store, err := factory.EntityStore(baseCtx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}

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
		t.Fatalf("duplicate key: expected ErrUniqueViolation, got %v", err)
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

// TestUniqueClaims_MixedModelBatch is the critical capture-at-Save test.
//
// Two entities from different models are buffered in a single transaction;
// each Save call provides a DIFFERENT key context. The test verifies that
// each entity is enforced with ITS OWN keys — not the other entity's keys —
// demonstrating that keys are captured at buffer time (not read from the
// flush context, which would be the last item's keys).
//
// If the bug existed (B gets A's keyA context), B would have no claim row
// (since $.emailA is absent from B's data), and the duplicate-B assertion
// below would succeed instead of returning ErrUniqueViolation.
func TestUniqueClaims_MixedModelBatch(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "mixed.db")
	factory, err := sqlite.NewStoreFactoryForTest(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("NewStoreFactoryForTest: %v", err)
	}
	t.Cleanup(func() { _ = factory.Close() })

	baseCtx := testCtx("uc-tenant")
	store, err := factory.EntityStore(baseCtx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	tm, err := factory.TransactionManager(baseCtx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}

	// Two models with distinct key fields — the fields don't overlap.
	keyA := []spi.UniqueKey{{ID: "key-a", Fields: []string{"$.emailA"}}}
	keyB := []spi.UniqueKey{{ID: "key-b", Fields: []string{"$.emailB"}}}

	// --- Phase 1: commit a mixed-model batch ---
	txID, txCtx, err := tm.Begin(baseCtx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	// Save entity A (model M1) with keyA context.
	entA := &spi.Entity{
		Meta: spi.EntityMeta{ID: "a1", ModelRef: spi.ModelRef{EntityName: "M1", ModelVersion: "1"}},
		Data: []byte(`{"emailA":"a@m1.com"}`),
	}
	if _, err := store.Save(spi.WithUniqueKeys(txCtx, keyA), entA); err != nil {
		t.Fatalf("Save A: %v", err)
	}

	// Save entity B (model M2) with keyB context. B's data has no $.emailA field.
	entB := &spi.Entity{
		Meta: spi.EntityMeta{ID: "b1", ModelRef: spi.ModelRef{EntityName: "M2", ModelVersion: "1"}},
		Data: []byte(`{"emailB":"b@m2.com"}`),
	}
	if _, err := store.Save(spi.WithUniqueKeys(txCtx, keyB), entB); err != nil {
		t.Fatalf("Save B: %v", err)
	}

	if err := tm.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// --- Phase 2: verify A's claim is enforced (A was committed with keyA) ---
	dupA := &spi.Entity{
		Meta: spi.EntityMeta{ID: "a2", ModelRef: spi.ModelRef{EntityName: "M1", ModelVersion: "1"}},
		Data: []byte(`{"emailA":"a@m1.com"}`),
	}
	_, err = store.Save(spi.WithUniqueKeys(baseCtx, keyA), dupA)
	if !errors.Is(err, spi.ErrUniqueViolation) {
		t.Fatalf("A's key not enforced: expected ErrUniqueViolation, got %v", err)
	}

	// --- Phase 3: verify B's claim is enforced (B was committed with keyB) ---
	// If the bug existed, B would have gotten A's flush-ctx keys, and since
	// $.emailA is absent from B's data, B would have no claim row — and this
	// duplicate save would succeed instead of returning ErrUniqueViolation.
	dupB := &spi.Entity{
		Meta: spi.EntityMeta{ID: "b2", ModelRef: spi.ModelRef{EntityName: "M2", ModelVersion: "1"}},
		Data: []byte(`{"emailB":"b@m2.com"}`),
	}
	_, err = store.Save(spi.WithUniqueKeys(baseCtx, keyB), dupB)
	if !errors.Is(err, spi.ErrUniqueViolation) {
		t.Fatalf("B's key not enforced: expected ErrUniqueViolation, got %v", err)
	}
}

// TestUniqueClaims_NonScalarKeyPath verifies that a non-scalar value at a
// declared key path causes Save to return ErrPartialUniqueKey (not a 5xx).
func TestUniqueClaims_NonScalarKeyPath(t *testing.T) {
	store, baseCtx, _ := setupUCStore(t)
	ctx := spi.WithUniqueKeys(baseCtx, emailKeys())

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
