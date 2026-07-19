package postgres_test

// searcher_tx_test.go — in-transaction Search behaviour for the PostgreSQL
// backend (issue #420, Task 10).
//
// DESIGN NOTE — postgres has no in-process tx buffer.
//
// Unlike the memory and sqlite backends (which stage a transaction's writes in
// spi.TransactionState.Buffer/.Deletes and must overlay that buffer onto a
// committed snapshot to serve read-your-own-writes), the postgres backend runs
// every transaction as a real pgx.Tx under REPEATABLE READ. Writes go straight
// to the DB inside that pgx.Tx via the context-resolving Querier, so a Search
// executed inside the tx already sees the tx's own uncommitted creates/updates/
// deletes — RYW is provided by the database, not an in-process merge. postgres's
// spi.TransactionState carries only ID+TenantID; Buffer/Deletes/OpMu are never
// populated. Consequently the only tx-specific behaviour Search must add over
// the committed pushdown is READ-SET RECORDING: when opts.TrackingRead is set,
// each returned committed entity's id+observed-version must enter the tx read-set
// so commit-time first-committer-wins validates it (matching Get/GetAll, which
// record unconditionally; Search records only when asked).
//
// These tests assert (a) RYW parity with GetAll+spi.MatchFilter for buffered
// create/update/delete and delete-then-save, and (b) the TrackingRead read-set
// contract (records returned ids ⇒ conflicting concurrent commit aborts;
// records nothing when false ⇒ concurrent commit does not abort).

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

var searchTxModel = spi.ModelRef{EntityName: "txperson", ModelVersion: "1"}

func searchTxOpts() spi.SearchOptions {
	return spi.SearchOptions{ModelName: searchTxModel.EntityName, ModelVersion: searchTxModel.ModelVersion}
}

// setupSearchTx wires a factory+TM (so transactions are available), seeds the
// committed rows, and returns a non-tx (committed) context on the seed tenant.
func setupSearchTx(t *testing.T, seed map[string]string) (*postgres.StoreFactory, *postgres.TransactionManager, context.Context) {
	t.Helper()
	factory, tm := setupFCWTest(t)
	ctx := ctxWithTenant("searchtx-tenant")
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore (seed): %v", err)
	}
	// Sort ids for deterministic seed order.
	ids := make([]string, 0, len(seed))
	for id := range seed {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if _, err := store.Save(ctx, &spi.Entity{
			Meta: spi.EntityMeta{ID: id, ModelRef: searchTxModel, State: "NEW"},
			Data: []byte(seed[id]),
		}); err != nil {
			t.Fatalf("seed Save %s: %v", id, err)
		}
	}
	return factory, tm, ctx
}

func idsOf(es []*spi.Entity) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.Meta.ID)
	}
	sort.Strings(out)
	return out
}

// assertSearchMatchesGetAll is the RYW oracle: Search inside the tx must return
// exactly the id set that GetAll + spi.MatchFilter produces for the same tx
// state. Both sides run on the same tx-scoped store/ctx.
func assertSearchMatchesGetAll(t *testing.T, store spi.EntityStore, ctx context.Context, filter spi.Filter, opts spi.SearchOptions) []string {
	t.Helper()
	sr, ok := store.(spi.Searcher)
	if !ok {
		t.Fatal("store does not implement spi.Searcher")
	}
	got, err := sr.Search(ctx, filter, opts)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	gotIDs := idsOf(got)

	all, err := store.GetAll(ctx, spi.ModelRef{EntityName: opts.ModelName, ModelVersion: opts.ModelVersion})
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	wantIDs := make([]string, 0, len(all))
	for _, e := range all {
		if spi.MatchFilter(filter, e.Data, e.Meta) {
			wantIDs = append(wantIDs, e.Meta.ID)
		}
	}
	sort.Strings(wantIDs)

	// Normalise nil vs empty for DeepEqual.
	if len(gotIDs) == 0 {
		gotIDs = []string{}
	}
	if len(wantIDs) == 0 {
		wantIDs = []string{}
	}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("RYW parity mismatch: Search=%v, GetAll+MatchFilter=%v", gotIDs, wantIDs)
	}
	return gotIDs
}

// TestSearchTx_RYWParity_CreateUpdateDelete: buffered create + update + delete
// inside a tx must be reflected in Search identically to GetAll+MatchFilter.
func TestSearchTx_RYWParity_CreateUpdateDelete(t *testing.T) {
	factory, tm, _ := setupSearchTx(t, map[string]string{
		"e1": `{"city":"Berlin"}`,
		"e2": `{"city":"Munich"}`,
		"e3": `{"city":"Berlin"}`,
	})
	baseCtx := ctxWithTenant("searchtx-tenant")
	txID, txCtx, err := tm.Begin(baseCtx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tm.Rollback(txCtx, txID) }()
	store, err := factory.EntityStore(txCtx)
	if err != nil {
		t.Fatalf("EntityStore (tx): %v", err)
	}

	// create e4 (Berlin), update e2 (Munich → Berlin), delete e3.
	if _, err := store.Save(txCtx, &spi.Entity{
		Meta: spi.EntityMeta{ID: "e4", ModelRef: searchTxModel, State: "NEW"},
		Data: []byte(`{"city":"Berlin"}`),
	}); err != nil {
		t.Fatalf("tx create e4: %v", err)
	}
	if _, err := store.Save(txCtx, &spi.Entity{
		Meta: spi.EntityMeta{ID: "e2", ModelRef: searchTxModel, State: "NEW"},
		Data: []byte(`{"city":"Berlin"}`),
	}); err != nil {
		t.Fatalf("tx update e2: %v", err)
	}
	if err := store.Delete(txCtx, "e3"); err != nil {
		t.Fatalf("tx delete e3: %v", err)
	}

	berlin := spi.Filter{Op: spi.FilterEq, Path: "city", Source: spi.SourceData, Value: "Berlin"}
	got := assertSearchMatchesGetAll(t, store, txCtx, berlin, searchTxOpts())
	if !reflect.DeepEqual(got, []string{"e1", "e2", "e4"}) {
		t.Fatalf("city=Berlin in tx: got %v, want [e1 e2 e4]", got)
	}
	// match-all
	assertSearchMatchesGetAll(t, store, txCtx, spi.Filter{}, searchTxOpts())
}

// TestSearchTx_DeleteThenSavePresent: delete-then-save within one tx must leave
// the entity PRESENT in Search results (postgres Save's UPSERT clears the
// soft-delete), matching GetAll.
func TestSearchTx_DeleteThenSavePresent(t *testing.T) {
	factory, tm, _ := setupSearchTx(t, map[string]string{
		"e1": `{"city":"Berlin"}`,
	})
	baseCtx := ctxWithTenant("searchtx-tenant")
	txID, txCtx, err := tm.Begin(baseCtx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tm.Rollback(txCtx, txID) }()
	store, err := factory.EntityStore(txCtx)
	if err != nil {
		t.Fatalf("EntityStore (tx): %v", err)
	}

	if err := store.Delete(txCtx, "e1"); err != nil {
		t.Fatalf("tx delete e1: %v", err)
	}
	if _, err := store.Save(txCtx, &spi.Entity{
		Meta: spi.EntityMeta{ID: "e1", ModelRef: searchTxModel, State: "NEW"},
		Data: []byte(`{"city":"Hamburg"}`),
	}); err != nil {
		t.Fatalf("tx re-save e1: %v", err)
	}

	got := assertSearchMatchesGetAll(t, store, txCtx, spi.Filter{}, searchTxOpts())
	if !reflect.DeepEqual(got, []string{"e1"}) {
		t.Fatalf("delete-then-save: got %v, want [e1] present", got)
	}
	// The resurrected row must carry the new data.
	res, err := store.(spi.Searcher).Search(txCtx,
		spi.Filter{Op: spi.FilterEq, Path: "city", Source: spi.SourceData, Value: "Hamburg"}, searchTxOpts())
	if err != nil {
		t.Fatalf("Search Hamburg: %v", err)
	}
	if len(res) != 1 || res[0].Meta.ID != "e1" {
		t.Fatalf("delete-then-save data: want e1@Hamburg, got %v", idsOf(res))
	}
}

// TestSearchTx_PositiveSupersession: an in-tx update must supersede the
// committed snapshot — the entity moves out of its old predicate bucket and
// into the new one, identically to GetAll+MatchFilter.
func TestSearchTx_PositiveSupersession(t *testing.T) {
	factory, tm, _ := setupSearchTx(t, map[string]string{
		"e2": `{"city":"Munich"}`,
	})
	baseCtx := ctxWithTenant("searchtx-tenant")
	txID, txCtx, err := tm.Begin(baseCtx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tm.Rollback(txCtx, txID) }()
	store, err := factory.EntityStore(txCtx)
	if err != nil {
		t.Fatalf("EntityStore (tx): %v", err)
	}

	if _, err := store.Save(txCtx, &spi.Entity{
		Meta: spi.EntityMeta{ID: "e2", ModelRef: searchTxModel, State: "NEW"},
		Data: []byte(`{"city":"Berlin"}`),
	}); err != nil {
		t.Fatalf("tx update e2: %v", err)
	}

	munich := spi.Filter{Op: spi.FilterEq, Path: "city", Source: spi.SourceData, Value: "Munich"}
	berlin := spi.Filter{Op: spi.FilterEq, Path: "city", Source: spi.SourceData, Value: "Berlin"}
	if got := assertSearchMatchesGetAll(t, store, txCtx, munich, searchTxOpts()); len(got) != 0 {
		t.Fatalf("supersession: city=Munich should be empty, got %v", got)
	}
	if got := assertSearchMatchesGetAll(t, store, txCtx, berlin, searchTxOpts()); !reflect.DeepEqual(got, []string{"e2"}) {
		t.Fatalf("supersession: city=Berlin should be [e2], got %v", got)
	}
}

// TestSearchTx_TrackingReadRecordsReturnedIds is the RED driver: with
// TrackingRead=true, each returned committed entity must enter the tx read-set
// at its observed version, so a concurrent conflicting commit aborts this tx.
func TestSearchTx_TrackingReadRecordsReturnedIds(t *testing.T) {
	factory, tm, _ := setupSearchTx(t, map[string]string{
		"e1": `{"city":"Berlin"}`,
		"e2": `{"city":"Berlin"}`,
		"e3": `{"city":"Berlin"}`,
	})
	baseCtx := ctxWithTenant("searchtx-tenant")

	// Tx A: Search with TrackingRead=true.
	txA, txCtxA, err := tm.Begin(baseCtx)
	if err != nil {
		t.Fatalf("Tx A Begin: %v", err)
	}
	defer func() { _ = tm.Rollback(txCtxA, txA) }()
	storeA, err := factory.EntityStore(txCtxA)
	if err != nil {
		t.Fatalf("Tx A EntityStore: %v", err)
	}
	opts := searchTxOpts()
	opts.TrackingRead = true
	got, err := storeA.(spi.Searcher).Search(txCtxA, spi.Filter{}, opts)
	if err != nil {
		t.Fatalf("Tx A Search: %v", err)
	}
	if !reflect.DeepEqual(idsOf(got), []string{"e1", "e2", "e3"}) {
		t.Fatalf("Tx A Search: got %v, want [e1 e2 e3]", idsOf(got))
	}

	// Direct read-set inspection: each returned id must be recorded @ v1.
	state, ok := postgres.LookupTxStateForTest(tm, txA)
	if !ok {
		t.Fatal("txState not found for Tx A")
	}
	for _, id := range []string{"e1", "e2", "e3"} {
		if v := postgres.ReadSetVersionForTest(state, id); v != 1 {
			t.Fatalf("TrackingRead=true: read-set for %s = %d, want 1 (not recorded ⇒ RED)", id, v)
		}
	}

	// Tx B: concurrently update e2 (a returned id) and commit → bumps e2 to v2.
	txB, txCtxB, err := tm.Begin(baseCtx)
	if err != nil {
		t.Fatalf("Tx B Begin: %v", err)
	}
	storeB, err := factory.EntityStore(txCtxB)
	if err != nil {
		_ = tm.Rollback(txCtxB, txB)
		t.Fatalf("Tx B EntityStore: %v", err)
	}
	if _, err := storeB.Save(txCtxB, &spi.Entity{
		Meta: spi.EntityMeta{ID: "e2", ModelRef: searchTxModel, State: "CHANGED"},
		Data: []byte(`{"city":"Hamburg"}`),
	}); err != nil {
		_ = tm.Rollback(txCtxB, txB)
		t.Fatalf("Tx B Save e2: %v", err)
	}
	if err := tm.Commit(baseCtx, txB); err != nil {
		t.Fatalf("Tx B Commit: %v", err)
	}

	// Tx A Commit must fail with ErrConflict (read-set staleness on e2).
	if err := tm.Commit(baseCtx, txA); err == nil {
		t.Fatal("expected ErrConflict on Tx A commit (Search recorded read of e2), got nil")
	} else if !errors.Is(err, spi.ErrConflict) {
		t.Fatalf("Tx A commit: want ErrConflict, got %v", err)
	}
}

// TestSearchTx_NoTrackingReadRecordsNothing: with TrackingRead=false (default),
// Search records no reads, so a concurrent commit to a returned entity does NOT
// abort this tx.
func TestSearchTx_NoTrackingReadRecordsNothing(t *testing.T) {
	factory, tm, _ := setupSearchTx(t, map[string]string{
		"e1": `{"city":"Berlin"}`,
		"e2": `{"city":"Berlin"}`,
		"e3": `{"city":"Berlin"}`,
	})
	baseCtx := ctxWithTenant("searchtx-tenant")

	txA, txCtxA, err := tm.Begin(baseCtx)
	if err != nil {
		t.Fatalf("Tx A Begin: %v", err)
	}
	defer func() { _ = tm.Rollback(txCtxA, txA) }()
	storeA, err := factory.EntityStore(txCtxA)
	if err != nil {
		t.Fatalf("Tx A EntityStore: %v", err)
	}
	// TrackingRead defaults to false.
	got, err := storeA.(spi.Searcher).Search(txCtxA, spi.Filter{}, searchTxOpts())
	if err != nil {
		t.Fatalf("Tx A Search: %v", err)
	}
	if !reflect.DeepEqual(idsOf(got), []string{"e1", "e2", "e3"}) {
		t.Fatalf("Tx A Search: got %v, want [e1 e2 e3]", idsOf(got))
	}

	// Nothing recorded.
	state, ok := postgres.LookupTxStateForTest(tm, txA)
	if !ok {
		t.Fatal("txState not found for Tx A")
	}
	for _, id := range []string{"e1", "e2", "e3"} {
		if v := postgres.ReadSetVersionForTest(state, id); v != 0 {
			t.Fatalf("TrackingRead=false: read-set for %s = %d, want 0 (recorded ⇒ over-tracking)", id, v)
		}
	}

	// Tx B updates e2 and commits.
	txB, txCtxB, err := tm.Begin(baseCtx)
	if err != nil {
		t.Fatalf("Tx B Begin: %v", err)
	}
	storeB, err := factory.EntityStore(txCtxB)
	if err != nil {
		_ = tm.Rollback(txCtxB, txB)
		t.Fatalf("Tx B EntityStore: %v", err)
	}
	if _, err := storeB.Save(txCtxB, &spi.Entity{
		Meta: spi.EntityMeta{ID: "e2", ModelRef: searchTxModel, State: "CHANGED"},
		Data: []byte(`{"city":"Hamburg"}`),
	}); err != nil {
		_ = tm.Rollback(txCtxB, txB)
		t.Fatalf("Tx B Save e2: %v", err)
	}
	if err := tm.Commit(baseCtx, txB); err != nil {
		t.Fatalf("Tx B Commit: %v", err)
	}

	// Tx A Commit must SUCCEED (no read recorded).
	if err := tm.Commit(baseCtx, txA); err != nil {
		t.Fatalf("Tx A commit: want success (no read-set), got %v", err)
	}
}
