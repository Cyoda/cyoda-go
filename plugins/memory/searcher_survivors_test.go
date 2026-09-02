package memory_test

import (
	"fmt"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func searcher(t *testing.T, store spi.EntityStore) spi.Searcher {
	t.Helper()
	s, ok := store.(spi.Searcher)
	if !ok {
		t.Fatal("store is not spi.Searcher")
	}
	return s
}

// Returned entities are copies: mutating a result must not change what the
// store returns next. Covers all three branches (non-tx, in-tx PIT, in-tx RYW).
func TestSearch_ResultsDoNotAliasStore(t *testing.T) {
	f, tm := newTxManager(t)
	ctx := tenantCtx("tenant-srch")
	ref := spi.ModelRef{EntityName: "m-alias", ModelVersion: "1"}
	store, _ := f.EntityStore(ctx)
	for i := 0; i < 3; i++ {
		if _, err := store.Save(ctx, &spi.Entity{Meta: spi.EntityMeta{ID: fmt.Sprintf("e%d", i), TenantID: "tenant-srch", ModelRef: ref, State: "open"}, Data: []byte(`{"v":1}`)}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	opts := spi.SearchOptions{ModelName: ref.EntityName, ModelVersion: ref.ModelVersion, Limit: 10}
	filter := spi.Filter{Op: spi.FilterEq, Path: "state", Source: spi.SourceMeta, Value: "open", Declared: []spi.DataType{spi.String}}

	mutateAndRecheck := func(name string, c contextT, o spi.SearchOptions) {
		t.Helper()
		first, err := searcher(t, store).Search(c, filter, o)
		if err != nil || len(first) != 3 {
			t.Fatalf("%s: Search = %d entities, err=%v", name, len(first), err)
		}
		first[0].Data[5] = '9' // {"v":9}
		first[0].Meta.State = "mutated"
		again, err := searcher(t, store).Search(c, filter, o)
		if err != nil || len(again) != 3 {
			t.Fatalf("%s: second Search = %d entities, err=%v (a mutated result leaked into the store)", name, len(again), err)
		}
		for _, e := range again {
			if string(e.Data) != `{"v":1}` || e.Meta.State != "open" {
				t.Fatalf("%s: store state changed through a returned entity: %s %s", name, e.Meta.ID, e.Data)
			}
		}
	}

	mutateAndRecheck("non-tx", ctx, opts)

	txID, txCtx, _ := tm.Begin(ctx)
	defer func() { _ = tm.Rollback(txCtx, txID) }()
	_, _ = store.Save(txCtx, &spi.Entity{Meta: spi.EntityMeta{ID: "e1", TenantID: "tenant-srch", ModelRef: ref, State: "open"}, Data: []byte(`{"v":1}`)}) // buffered update
	mutateAndRecheck("in-tx RYW", txCtx, opts)

	pit := spi.GetTransaction(txCtx).SnapshotTime
	pitOpts := opts
	pitOpts.PointInTime = &pit
	mutateAndRecheck("in-tx PIT", txCtx, pitOpts)
}
