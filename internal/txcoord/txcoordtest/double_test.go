package txcoordtest_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	"github.com/cyoda-platform/cyoda-go/internal/txcoord/txcoordtest"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// TestDouble_CompletionIntoTheRootIsAllOrNothing: a completion whose writes
// the memory transaction refuses part-way leaves none of them in the
// transaction. The last write (by id) changes a stored entity's model, which
// the memory plugin refuses, after the others went in.
func TestDouble_CompletionIntoTheRootIsAllOrNothing(t *testing.T) {
	f := txcoordtest.NewFactory(memory.NewStoreFactory())
	t.Cleanup(func() { _ = f.Close() })
	co, err := f.(spi.TransactionCoordinationCapable).TransactionCoordinator(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx := spi.WithUserContext(context.Background(), &spi.UserContext{UserID: "u", Tenant: spi.Tenant{ID: "tenant-double"}})
	tm, err := f.TransactionManager(ctx)
	if err != nil {
		t.Fatal(err)
	}
	entity := func(id, model string) *spi.Entity {
		data, _ := json.Marshal(map[string]string{"id": id})
		return &spi.Entity{Meta: spi.EntityMeta{ID: id, ModelRef: spi.ModelRef{EntityName: model, ModelVersion: "1"}}, Data: data}
	}
	save := func(ctx context.Context, e *spi.Entity) {
		t.Helper()
		es, err := f.EntityStore(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := es.Save(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	const last = "ffffffff-ffff-ffff-ffff-ffffffffffff"
	seedID, seedCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	save(seedCtx, entity(last, "m-first"))
	if err := tm.Commit(seedCtx, seedID); err != nil {
		t.Fatal(err)
	}

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tm.Rollback(txCtx, txID) }()
	if err := co.BeginCallout(txCtx, txID, "c", nil); err != nil {
		t.Fatal(err)
	}
	if err := co.AdvanceCallout(txCtx, txID, "c", 1); err != nil {
		t.Fatal(err)
	}
	jctx, err := tm.Join(ctx, txID)
	if err != nil {
		t.Fatal(err)
	}
	sctx, err := co.OpenScope(jctx, txID, []spi.CalloutPair{{Callout: "c", Major: 1}})
	if err != nil {
		t.Fatal(err)
	}
	var fresh []string
	for i := range 5 {
		id := fmt.Sprintf("00000000-0000-0000-0000-%012d", i)
		fresh = append(fresh, id)
		save(sctx, entity(id, "m-first"))
	}
	save(sctx, entity(last, "m-second"))

	if err := co.CompleteScope(sctx); err == nil || errors.Is(err, spi.ErrScopeConflict) {
		t.Fatalf("CompleteScope = %v; want the memory plugin's refusal", err)
	}
	es, err := f.EntityStore(txCtx)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range fresh {
		if _, err := es.Get(txCtx, id); !errors.Is(err, spi.ErrNotFound) {
			t.Errorf("entity %s is in the transaction after a refused completion (err %v)", id, err)
		}
	}
}
