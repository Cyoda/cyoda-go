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
	spitest "github.com/cyoda-platform/cyoda-go-spi/spitest"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// ucModel is the model the tests save into; setupUCFactory stores it LOCKED
// with ucEmailKeys as its unique keys.
var ucModel = spi.ModelRef{EntityName: "UCModel", ModelVersion: "1"}

// setupUCFactory creates a fresh StoreFactory and a base context for
// "uc-tenant", with ucModel stored LOCKED and keyed on $.email.
func setupUCFactory(t *testing.T) (*memory.StoreFactory, spi.EntityStore) {
	t.Helper()
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { _ = factory.Close() })
	baseCtx := ctxWithTenant("uc-tenant")
	spitest.EnsureLockedModel(t, factory, baseCtx, ucModel, ucEmailKeys()...)
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

	if _, err := store.Save(baseCtx, ucEntity("e1", "a@x.com")); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	_, err := store.Save(baseCtx, ucEntity("e2", "a@x.com"))
	if !errors.Is(err, spi.ErrUniqueViolation) {
		t.Fatalf("duplicate key: expected ErrUniqueViolation, got %v", err)
	}
}

// TestUniqueClaims_SoftDeleteFreesValue verifies that soft-deleting the holder
// of a key value allows another entity to claim that same value.
func TestUniqueClaims_SoftDeleteFreesValue(t *testing.T) {
	_, store := setupUCFactory(t)
	baseCtx := ctxWithTenant("uc-tenant")

	if _, err := store.Save(baseCtx, ucEntity("e1", "a@x.com")); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	if err := store.Delete(baseCtx, "e1"); err != nil {
		t.Fatalf("Delete e1: %v", err)
	}
	if _, err := store.Save(baseCtx, ucEntity("e2", "a@x.com")); err != nil {
		t.Fatalf("Save e2 after delete: %v", err)
	}
}

// TestUniqueClaims_DeleteAllFreesValues verifies that DeleteAll releases all
// claim entries for the model, allowing previously-used values to be re-saved.
func TestUniqueClaims_DeleteAllFreesValues(t *testing.T) {
	_, store := setupUCFactory(t)
	baseCtx := ctxWithTenant("uc-tenant")
	modelRef := spi.ModelRef{EntityName: "UCModel", ModelVersion: "1"}

	if _, err := store.Save(baseCtx, ucEntity("e1", "a@x.com")); err != nil {
		t.Fatalf("Save e1: %v", err)
	}
	if _, err := store.Save(baseCtx, ucEntity("e2", "b@x.com")); err != nil {
		t.Fatalf("Save e2: %v", err)
	}
	if err := store.DeleteAll(baseCtx, modelRef); err != nil {
		t.Fatalf("DeleteAll: %v", err)
	}
	// Re-saving with the same values must succeed once claims are freed.
	if _, err := store.Save(baseCtx, ucEntity("e1", "a@x.com")); err != nil {
		t.Fatalf("re-Save after DeleteAll: %v", err)
	}
}

// TestUniqueClaims_UpdateMovesKey verifies that updating an entity to a new key
// value frees the old claim so another entity can take the old value.
func TestUniqueClaims_UpdateMovesKey(t *testing.T) {
	_, store := setupUCFactory(t)
	baseCtx := ctxWithTenant("uc-tenant")

	if _, err := store.Save(baseCtx, ucEntity("e1", "a@x.com")); err != nil {
		t.Fatalf("first Save e1: %v", err)
	}
	// Update e1 to a new value — old claim (a@x.com) must be released.
	if _, err := store.Save(baseCtx, ucEntity("e1", "b@x.com")); err != nil {
		t.Fatalf("update Save e1: %v", err)
	}
	// e2 may now claim the previously-held a@x.com.
	if _, err := store.Save(baseCtx, ucEntity("e2", "a@x.com")); err != nil {
		t.Fatalf("Save e2 with freed old value: %v", err)
	}
	// e3 must NOT steal b@x.com (still held by e1).
	_, err := store.Save(baseCtx, ucEntity("e3", "b@x.com"))
	if !errors.Is(err, spi.ErrUniqueViolation) {
		t.Fatalf("Save e3 with e1's active value: expected ErrUniqueViolation, got %v", err)
	}
}

// TestUniqueClaims_MixedModelBatch buffers two entities of different models,
// each keyed on a different field, in one transaction. The commit enforces
// each entity with its own model's keys, not the other's.
func TestUniqueClaims_MixedModelBatch(t *testing.T) {
	factory, store := setupUCFactory(t)
	baseCtx := ctxWithTenant("uc-tenant")
	tm, err := factory.TransactionManager(baseCtx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}

	keyA := spi.UniqueKey{ID: "key-a", Fields: []string{"$.emailA"}}
	keyB := spi.UniqueKey{ID: "key-b", Fields: []string{"$.emailB"}}
	spitest.EnsureLockedModel(t, factory, baseCtx, spi.ModelRef{EntityName: "M1", ModelVersion: "1"}, keyA)
	spitest.EnsureLockedModel(t, factory, baseCtx, spi.ModelRef{EntityName: "M2", ModelVersion: "1"}, keyB)

	txID, txCtx, err := tm.Begin(baseCtx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	entA := &spi.Entity{
		Meta: spi.EntityMeta{ID: "a1", ModelRef: spi.ModelRef{EntityName: "M1", ModelVersion: "1"}},
		Data: []byte(`{"emailA":"a@m1.com"}`),
	}
	if _, err := store.Save(txCtx, entA); err != nil {
		t.Fatalf("Save A: %v", err)
	}

	// B's data has no $.emailA — enforced with M1's keys, B would have no claim.
	entB := &spi.Entity{
		Meta: spi.EntityMeta{ID: "b1", ModelRef: spi.ModelRef{EntityName: "M2", ModelVersion: "1"}},
		Data: []byte(`{"emailB":"b@m2.com"}`),
	}
	if _, err := store.Save(txCtx, entB); err != nil {
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
	_, err = store.Save(baseCtx, dupA)
	if !errors.Is(err, spi.ErrUniqueViolation) {
		t.Fatalf("A's key not enforced: expected ErrUniqueViolation, got %v", err)
	}

	// B's claim must be enforced. Had B been enforced with M1's keys, it would
	// have no claim and this duplicate save would succeed instead of failing.
	dupB := &spi.Entity{
		Meta: spi.EntityMeta{ID: "b2", ModelRef: spi.ModelRef{EntityName: "M2", ModelVersion: "1"}},
		Data: []byte(`{"emailB":"b@m2.com"}`),
	}
	_, err = store.Save(baseCtx, dupB)
	if !errors.Is(err, spi.ErrUniqueViolation) {
		t.Fatalf("B's key not enforced: expected ErrUniqueViolation, got %v", err)
	}
}

// TestUniqueClaims_NonScalarKeyPath verifies that a non-scalar value at a
// declared key path causes Save to return ErrPartialUniqueKey.
func TestUniqueClaims_NonScalarKeyPath(t *testing.T) {
	_, store := setupUCFactory(t)
	baseCtx := ctxWithTenant("uc-tenant")

	ent := &spi.Entity{
		Meta: spi.EntityMeta{
			ID:       "e1",
			ModelRef: spi.ModelRef{EntityName: "UCModel", ModelVersion: "1"},
		},
		Data: []byte(`{"email":{"nested":"object"}}`),
	}
	_, err := store.Save(baseCtx, ent)
	if !errors.Is(err, spi.ErrPartialUniqueKey) {
		t.Fatalf("non-scalar key path: expected ErrPartialUniqueKey, got %v", err)
	}
}

// TestUniqueClaims_KeylessModel verifies that a model with no unique keys
// claims nothing: two entities with the same value both save.
func TestUniqueClaims_KeylessModel(t *testing.T) {
	factory, store := setupUCFactory(t)
	baseCtx := ctxWithTenant("uc-tenant")
	keyless := spi.ModelRef{EntityName: "UCKeyless", ModelVersion: "1"}
	spitest.EnsureLockedModel(t, factory, baseCtx, keyless)

	e1, e2 := ucEntity("e1", "a@x.com"), ucEntity("e2", "a@x.com")
	e1.Meta.ModelRef, e2.Meta.ModelRef = keyless, keyless
	if _, err := store.Save(baseCtx, e1); err != nil {
		t.Fatalf("Save into a keyless model: %v", err)
	}
	// A second distinct entity with the same value must also succeed (no claim was made).
	if _, err := store.Save(baseCtx, e2); err != nil {
		t.Fatalf("second Save into a keyless model: %v", err)
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
	if _, err := store.Save(txCtx, ucEntity("e1", "a@x.com")); err != nil {
		t.Fatalf("Save e1: %v", err)
	}
	if _, err := store.Save(txCtx, ucEntity("e2", "a@x.com")); err != nil {
		t.Fatalf("Save e2 (buffer): %v", err)
	}

	err = tm.Commit(txCtx, txID)
	if !errors.Is(err, spi.ErrUniqueViolation) {
		t.Fatalf("intra-batch duplicate: expected ErrUniqueViolation, got %v", err)
	}
}

// TestUniqueClaims_TxDeleteReleasesClaimForNextTx verifies that a standalone
// tx-mode Delete releases unique-key claims so a subsequent non-tx Save with the
// same value succeeds (the commit releases the claims of every entity it deletes).
func TestUniqueClaims_TxDeleteReleasesClaimForNextTx(t *testing.T) {
	factory, store := setupUCFactory(t)
	baseCtx := ctxWithTenant("uc-tenant")

	// Non-tx save: A holds "v@x.com".
	if _, err := store.Save(baseCtx, ucEntity("a", "v@x.com")); err != nil {
		t.Fatalf("non-tx Save a: %v", err)
	}

	tm, err := factory.TransactionManager(baseCtx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}

	// Tx: delete A (only Delete, no new entity).
	txID, txCtx, err := tm.Begin(baseCtx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := store.Delete(txCtx, "a"); err != nil {
		t.Fatalf("tx Delete a: %v", err)
	}
	if err := tm.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// After commit, A's claim must be released so B can claim the same value.
	if _, err := store.Save(baseCtx, ucEntity("b", "v@x.com")); err != nil {
		t.Fatalf("non-tx Save b with freed value: %v", err)
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
			ent := &spi.Entity{
				Meta: spi.EntityMeta{
					ID:       fmt.Sprintf("entity-%d", idx),
					ModelRef: spi.ModelRef{EntityName: "UCModel", ModelVersion: "1"},
				},
				Data: []byte(`{"email":"shared@x.com"}`),
			}
			if _, err := store.Save(txCtx, ent); err != nil {
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

// TestUniqueClaims_SameTxReclaim_BothOrdersCommit deletes entity A, which
// holds value V, and creates entity B with V in one transaction, in either
// order. The commit judges the transaction's final entities, in which B alone
// holds V, so both orders commit and B's claim is enforced afterwards.
func TestUniqueClaims_SameTxReclaim_BothOrdersCommit(t *testing.T) {
	for _, tc := range []struct {
		name        string
		deleteFirst bool
	}{
		{"DeleteThenSave", true},
		{"SaveThenDelete", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			factory, store := setupUCFactory(t)
			baseCtx := ctxWithTenant("uc-tenant")

			if _, err := store.Save(baseCtx, ucEntity("a", "shared@x.com")); err != nil {
				t.Fatalf("pre-save A: %v", err)
			}
			tm, err := factory.TransactionManager(baseCtx)
			if err != nil {
				t.Fatalf("TransactionManager: %v", err)
			}
			txID, txCtx, err := tm.Begin(baseCtx)
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			deleteA := func() {
				if err := store.Delete(txCtx, "a"); err != nil {
					t.Fatalf("tx Delete a: %v", err)
				}
			}
			if tc.deleteFirst {
				deleteA()
			}
			if _, err := store.Save(txCtx, ucEntity("b", "shared@x.com")); err != nil {
				t.Fatalf("tx Save b: %v", err)
			}
			if !tc.deleteFirst {
				deleteA()
			}
			if err := tm.Commit(txCtx, txID); err != nil {
				t.Fatalf("Commit: expected success, got %v", err)
			}

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
			// B's claim must be enforced: a third entity cannot take the value.
			if _, err := store.Save(baseCtx, ucEntity("c", "shared@x.com")); !errors.Is(err, spi.ErrUniqueViolation) {
				t.Errorf("c with b's value: expected ErrUniqueViolation, got %v", err)
			}
		})
	}
}

// A CompareAndSave inside a transaction writes to the same buffer Save does,
// so its entity is one of the transaction's final entities and the commit
// computes its claims like any other. A commit that missed it would leave the
// value it should hold free for anyone else.
func TestUniqueClaims_CompareAndSaveInTxRecordsClaims(t *testing.T) {
	factory, store := setupUCFactory(t)
	baseCtx := ctxWithTenant("uc-tenant")
	tm, err := factory.TransactionManager(baseCtx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}

	// CompareAndSave never creates, so the entity is seeded first, with a
	// different value: the claim on a@x.com asserted below can only have come
	// from the compare-and-save.
	seedTxID := saveCommitted(t, tm, store, baseCtx, ucEntity("e1", "seed@x.com"))

	txID, txCtx, err := tm.Begin(baseCtx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := store.CompareAndSave(
		txCtx, ucEntity("e1", "a@x.com"), seedTxID); err != nil {
		t.Fatalf("CompareAndSave: %v", err)
	}
	if err := tm.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// A second transaction claiming the same value for a different entity
	// must lose at commit, exactly as it would against a committed Save.
	txID2, txCtx2, err := tm.Begin(baseCtx)
	if err != nil {
		t.Fatalf("Begin (second): %v", err)
	}
	if _, err := store.Save(txCtx2, ucEntity("e2", "a@x.com")); err != nil {
		t.Fatalf("buffered Save: %v", err)
	}
	if err := tm.Commit(txCtx2, txID2); !errors.Is(err, spi.ErrUniqueViolation) {
		t.Fatalf("e2 with e1's value: expected ErrUniqueViolation at commit, got %v", err)
	}
}

// TestUniqueClaims_ModelMismatchAnswersBeforeKeyClash: a write outside a
// transaction that names an existing entity under another model, and whose key
// value another entity of that model already holds, is refused for the model
// mismatch, as on postgres and sqlite. An entity's model is fixed at creation,
// so the write never reaches the key check.
func TestUniqueClaims_ModelMismatchAnswersBeforeKeyClash(t *testing.T) {
	factory, store := setupUCFactory(t)
	baseCtx := ctxWithTenant("uc-tenant")
	other := spi.ModelRef{EntityName: "UCOther", ModelVersion: "1"}
	spitest.EnsureLockedModel(t, factory, baseCtx, other, ucEmailKeys()...)

	if _, err := store.Save(baseCtx, ucEntity("holder", "a@x.com")); err != nil {
		t.Fatalf("Save holder: %v", err)
	}
	elsewhere := ucEntity("moved", "b@x.com")
	elsewhere.Meta.ModelRef = other
	if _, err := store.Save(baseCtx, elsewhere); err != nil {
		t.Fatalf("Save moved under %s: %v", other.EntityName, err)
	}

	_, err := store.Save(baseCtx, ucEntity("moved", "a@x.com"))
	if !errors.Is(err, spi.ErrEntityModelMismatch) {
		t.Fatalf("Save of an entity under another model, clashing on a key: got %v, want ErrEntityModelMismatch", err)
	}
}

// TestUniqueClaims_PartialKeyAnswersBeforeDuplicate — a commit whose final
// entities include both a duplicate key value and a partly filled composite
// key answers ErrPartialUniqueKey, whatever the entity ids' order, as on
// postgres and sqlite, which compute every entity's claims before they check
// any of them. The duplicate's id sorts first here.
func TestUniqueClaims_PartialKeyAnswersBeforeDuplicate(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { _ = factory.Close() })
	baseCtx := ctxWithTenant("uc-tenant")
	pair := spi.ModelRef{EntityName: "UCPair", ModelVersion: "1"}
	spitest.EnsureLockedModel(t, factory, baseCtx, pair,
		spi.UniqueKey{ID: "ab", Fields: []string{"$.a", "$.b"}})
	store, err := factory.EntityStore(baseCtx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	entity := func(id, data string) *spi.Entity {
		return &spi.Entity{Meta: spi.EntityMeta{ID: id, ModelRef: pair}, Data: []byte(data)}
	}
	if _, err := store.Save(baseCtx, entity("holder", `{"a":1,"b":1}`)); err != nil {
		t.Fatalf("Save holder: %v", err)
	}

	tm, err := factory.TransactionManager(baseCtx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	txID, txCtx, err := tm.Begin(baseCtx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := store.Save(txCtx, entity("a-duplicate", `{"a":1,"b":1}`)); err != nil {
		t.Fatalf("Save a-duplicate: %v", err)
	}
	if _, err := store.Save(txCtx, entity("b-partial", `{"a":2}`)); err != nil {
		t.Fatalf("Save b-partial: %v", err)
	}

	err = tm.Commit(txCtx, txID)
	if !errors.Is(err, spi.ErrPartialUniqueKey) {
		t.Fatalf("commit: expected ErrPartialUniqueKey, got %v", err)
	}
}
