package postgres_test

import (
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
	"github.com/google/uuid"
)

// TestGetSubmitTime_AnswersFromAnotherManager proves the submit time is not
// process-local: a second TransactionManager over the same database — which
// is what another cluster node is — resolves a transaction it never
// committed.
func TestGetSubmitTime_AnswersFromAnotherManager(t *testing.T) {
	factory, tmA := setupEntityTestWithTM(t)
	ctx := ctxWithTenant("submit-durable-tenant")
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}

	txID, txCtx, err := tmA.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := store.Save(txCtx, &spi.Entity{
		Meta: spi.EntityMeta{ID: uuid.NewString(), ModelRef: spi.ModelRef{EntityName: "submit-durable", ModelVersion: "1"}},
		Data: []byte(`{"n":1}`),
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := tmA.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	want, err := tmA.GetSubmitTime(ctx, txID)
	if err != nil {
		t.Fatalf("GetSubmitTime on the committing manager: %v", err)
	}

	// A second manager stands in for another node: same database, empty
	// in-process maps — it never saw Begin or Commit for this tx.
	tmB := postgres.NewTransactionManager(postgres.PoolForTest(factory), newTestUUIDGenerator())
	got, err := tmB.GetSubmitTime(ctx, txID)
	if err != nil {
		t.Fatalf("a node that did not commit the transaction must still resolve it: %v", err)
	}
	if !got.Equal(want) {
		t.Errorf("submit time differs across nodes: got %s, want %s", got, want)
	}
}

// TestGetSubmitTime_AnotherManagerRejectsCrossTenant proves the durable
// fallback applies the same tenant gate the in-process maps do. Both
// TransactionManager instances below are independent, over the same
// database, so tenant B's lookup can only be answered from the submit_times
// table — the fallback must recognise the row belongs to tenant A and
// report a mismatch, not silently treat it as unknown (which would turn a
// node-local restriction into a cross-tenant disclosure on the "not found"
// side, or a leak on the "succeeds" side).
func TestGetSubmitTime_AnotherManagerRejectsCrossTenant(t *testing.T) {
	factory, tmA := setupEntityTestWithTM(t)
	ctxA := ctxWithTenant("submit-durable-tenant-a")
	ctxB := ctxWithTenant("submit-durable-tenant-b")
	store, err := factory.EntityStore(ctxA)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}

	txID, txCtx, err := tmA.Begin(ctxA)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := store.Save(txCtx, &spi.Entity{
		Meta: spi.EntityMeta{ID: uuid.NewString(), ModelRef: spi.ModelRef{EntityName: "submit-durable-x", ModelVersion: "1"}},
		Data: []byte(`{"n":1}`),
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := tmA.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// tmB never saw Begin or Commit for this tx: its maps are empty, so any
	// answer can only come from the table.
	tmB := postgres.NewTransactionManager(postgres.PoolForTest(factory), newTestUUIDGenerator())
	_, err = tmB.GetSubmitTime(ctxB, txID)
	if err == nil {
		t.Fatal("expected an error for tenant B resolving tenant A's committed tx via another manager")
	}
	if !errors.Is(err, spi.ErrTxTenantMismatch) {
		t.Fatalf("cross-tenant GetSubmitTime via the durable fallback must wrap ErrTxTenantMismatch; got: %v", err)
	}
}
