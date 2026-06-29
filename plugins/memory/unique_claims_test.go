package memory_test

// unique_claims_test.go — tests for composite unique-key enforcement in the
// memory entity store. Mirrors sqlite/postgres unique_claims_test.go but also
// covers the concurrency winner/loser and intra-batch duplicate scenarios.
// Memory-plugin suite only — NOT in parity.

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// setupUCFactory creates a fresh StoreFactory and a base context for "uc-tenant".
func setupUCFactory(t *testing.T) (*memory.StoreFactory, spi.EntityStore) {
	t.Helper()
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { _ = factory.Close() })
	baseCtx := ctxWithTenant("uc-tenant")
	store, err := factory.EntityStore(baseCtx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	return factory, store
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

// emailKeys returns a single composite unique key over the $.email field.
func ucEmailKeys() []spi.UniqueKey {
	return []spi.UniqueKey{{ID: "email-key", Fields: []string{"$.email"}}}
}

// TestUniqueClaims_DuplicateSignature verifies that saving two distinct entities
// with the same key value returns spi.ErrUniqueViolation on the second save.
func TestUniqueClaims_DuplicateSignature(t *testing.T) {
	_, store := setupUCFactory(t)
	baseCtx := ctxWithTenant("uc-tenant")
	ctx := spi.WithUniqueKeys(baseCtx, ucEmailKeys())

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
	_, store := setupUCFactory(t)
	baseCtx := ctxWithTenant("uc-tenant")
	ctx := spi.WithUniqueKeys(baseCtx, ucEmailKeys())

	if _, err := store.Save(ctx, ucEntity("e1", "a@x.com")); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	if err := store.Delete(ctx, "e1"); err != nil {
		t.Fatalf("Delete e1: %v", err)
	}
	if _, err := store.Save(ctx, ucEntity("e2", "a@x.com")); err != nil {
		t.Fatalf("Save e2 after delete: %v", err)
	}
}

// TestUniqueClaims_DeleteAllFreesValues verifies that DeleteAll releases all
// claim entries for the model, allowing previously-used values to be re-saved.
func TestUniqueClaims_DeleteAllFreesValues(t *testing.T) {
	_, store := setupUCFactory(t)
	baseCtx := ctxWithTenant("uc-tenant")
	ctx := spi.WithUniqueKeys(baseCtx, ucEmailKeys())
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
	// Re-saving with the same values must succeed once claims are freed.
	if _, err := store.Save(ctx, ucEntity("e1", "a@x.com")); err != nil {
		t.Fatalf("re-Save after DeleteAll: %v", err)
	}
}

// TestUniqueClaims_UpdateMovesKey verifies that updating an entity to a new key
// value frees the old claim so another entity can take the old value.
func TestUniqueClaims_UpdateMovesKey(t *testing.T) {
	_, store := setupUCFactory(t)
	baseCtx := ctxWithTenant("uc-tenant")
	ctx := spi.WithUniqueKeys(baseCtx, ucEmailKeys())

	if _, err := store.Save(ctx, ucEntity("e1", "a@x.com")); err != nil {
		t.Fatalf("first Save e1: %v", err)
	}
	// Update e1 to a new value — old claim (a@x.com) must be released.
	if _, err := store.Save(ctx, ucEntity("e1", "b@x.com")); err != nil {
		t.Fatalf("update Save e1: %v", err)
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
// Two entities from different models are buffered in a single transaction, each
// Save carrying DIFFERENT key contexts. The test verifies each entity is enforced
// with ITS OWN keys — not the other entity's — demonstrating keys are captured at
// buffer time, not read from the flush context.
func TestUniqueClaims_MixedModelBatch(t *testing.T) {
	factory, store := setupUCFactory(t)
	baseCtx := ctxWithTenant("uc-tenant")
	tm, err := factory.TransactionManager(baseCtx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}

	keyA := []spi.UniqueKey{{ID: "key-a", Fields: []string{"$.emailA"}}}
	keyB := []spi.UniqueKey{{ID: "key-b", Fields: []string{"$.emailB"}}}

	txID, txCtx, err := tm.Begin(baseCtx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	entA := &spi.Entity{
		Meta: spi.EntityMeta{ID: "a1", ModelRef: spi.ModelRef{EntityName: "M1", ModelVersion: "1"}},
		Data: []byte(`{"emailA":"a@m1.com"}`),
	}
	if _, err := store.Save(spi.WithUniqueKeys(txCtx, keyA), entA); err != nil {
		t.Fatalf("Save A: %v", err)
	}

	// B's data has no $.emailA — if A's keys bled into B's context, B would have no claim.
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

	// A's claim must be enforced.
	dupA := &spi.Entity{
		Meta: spi.EntityMeta{ID: "a2", ModelRef: spi.ModelRef{EntityName: "M1", ModelVersion: "1"}},
		Data: []byte(`{"emailA":"a@m1.com"}`),
	}
	_, err = store.Save(spi.WithUniqueKeys(baseCtx, keyA), dupA)
	if !errors.Is(err, spi.ErrUniqueViolation) {
		t.Fatalf("A's key not enforced: expected ErrUniqueViolation, got %v", err)
	}

	// B's claim must be enforced. If the bug existed (B got A's context), B would
	// have no claim row and this duplicate save would succeed instead of failing.
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
// declared key path causes Save to return ErrPartialUniqueKey.
func TestUniqueClaims_NonScalarKeyPath(t *testing.T) {
	_, store := setupUCFactory(t)
	baseCtx := ctxWithTenant("uc-tenant")
	ctx := spi.WithUniqueKeys(baseCtx, ucEmailKeys())

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
// in the context, Save succeeds and no claim entry is maintained.
func TestUniqueClaims_NoContextKeys(t *testing.T) {
	_, store := setupUCFactory(t)
	baseCtx := ctxWithTenant("uc-tenant")
	// Intentionally no spi.WithUniqueKeys — baseline context only.

	if _, err := store.Save(baseCtx, ucEntity("e1", "a@x.com")); err != nil {
		t.Fatalf("Save without unique keys: %v", err)
	}
	// A second distinct entity with the same value must also succeed (no claim was made).
	if _, err := store.Save(baseCtx, ucEntity("e2", "a@x.com")); err != nil {
		t.Fatalf("second Save without unique keys: %v", err)
	}
}

// TestUniqueClaims_IntraBatchDuplicate verifies that two entities with the same
// key value buffered in a single transaction yield ErrUniqueViolation on Commit.
func TestUniqueClaims_IntraBatchDuplicate(t *testing.T) {
	factory, store := setupUCFactory(t)
	baseCtx := ctxWithTenant("uc-tenant")
	tm, err := factory.TransactionManager(baseCtx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}

	txID, txCtx, err := tm.Begin(baseCtx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	keyCtx := spi.WithUniqueKeys(txCtx, ucEmailKeys())

	if _, err := store.Save(keyCtx, ucEntity("e1", "a@x.com")); err != nil {
		t.Fatalf("Save e1: %v", err)
	}
	if _, err := store.Save(keyCtx, ucEntity("e2", "a@x.com")); err != nil {
		t.Fatalf("Save e2 (buffer): %v", err)
	}

	err = tm.Commit(txCtx, txID)
	if !errors.Is(err, spi.ErrUniqueViolation) {
		t.Fatalf("intra-batch duplicate: expected ErrUniqueViolation, got %v", err)
	}
}

// TestUniqueClaims_ConcurrentWinnerLoser verifies that when two goroutines each
// create a distinct entity with the same key value concurrently, exactly one
// commits and the other gets spi.ErrUniqueViolation; no torn write.
// Run with -race to confirm no data races.
func TestUniqueClaims_ConcurrentWinnerLoser(t *testing.T) {
	factory, store := setupUCFactory(t)
	baseCtx := ctxWithTenant("uc-tenant")
	tm, err := factory.TransactionManager(baseCtx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}

	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			txID, txCtx, err := tm.Begin(baseCtx)
			if err != nil {
				errs[idx] = fmt.Errorf("Begin: %w", err)
				return
			}
			saveCtx := spi.WithUniqueKeys(txCtx, ucEmailKeys())
			ent := &spi.Entity{
				Meta: spi.EntityMeta{
					ID:       fmt.Sprintf("entity-%d", idx),
					ModelRef: spi.ModelRef{EntityName: "UCModel", ModelVersion: "1"},
				},
				Data: []byte(`{"email":"shared@x.com"}`),
			}
			if _, err := store.Save(saveCtx, ent); err != nil {
				errs[idx] = fmt.Errorf("Save: %w", err)
				return
			}
			errs[idx] = tm.Commit(txCtx, txID)
		}(i)
	}
	wg.Wait()

	wins, violations := 0, 0
	for _, e := range errs {
		switch {
		case e == nil:
			wins++
		case errors.Is(e, spi.ErrUniqueViolation):
			violations++
		default:
			t.Errorf("unexpected error: %v", e)
		}
	}
	if wins != 1 || violations != 1 {
		t.Errorf("expected exactly 1 winner and 1 ErrUniqueViolation, got %d wins %d violations", wins, violations)
	}
}
