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
	"time"

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

// TestSearchTxPIT_CommittedOnlyMatchesGetAllAsAt is the RED driver for in-tx
// point-in-time (PIT) Search (Task 11, issue #420): a Search issued INSIDE a
// transaction with opts.PointInTime set must return the committed-as-at-PIT
// snapshot — identical to GetAllAsAt + spi.MatchFilter run through the same
// tx-scoped store/ctx — never a buffered/overlaid current-state view. The
// entity's status flip (active -> inactive) is pinned to a valid_time AFTER
// the pit, so it must not be visible: only the historical v1 (active) may
// come back.
func TestSearchTxPIT_CommittedOnlyMatchesGetAllAsAt(t *testing.T) {
	const (
		tenant = "searchtx-pit-tenant"
		baseTS = "2026-03-01 00:00:00+00"
		nextTS = "2026-03-02 00:00:00+00"
	)
	factory, tm := setupFCWTest(t)
	ctx := ctxWithTenant(tenant)
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}

	ent := &spi.Entity{
		Meta: spi.EntityMeta{ID: "pit-e1", ModelRef: searchTxModel, State: "NEW"},
		Data: []byte(`{"status":"active"}`),
	}
	if _, err := store.Save(ctx, ent); err != nil {
		t.Fatalf("save v1: %v", err)
	}
	ent.Data = []byte(`{"status":"inactive"}`)
	if _, err := store.Save(ctx, ent); err != nil {
		t.Fatalf("save v2 (later committed change): %v", err)
	}

	// Pin v1 before, v2 after the pit we will query at.
	pool := factory.Pool()
	if _, err := pool.Exec(ctx,
		`UPDATE entity_versions SET valid_time=$1, transaction_time=$1
		 WHERE tenant_id=$2 AND entity_id=$3 AND version=1`,
		baseTS, tenant, "pit-e1"); err != nil {
		t.Fatalf("pin v1: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE entity_versions SET valid_time=$1, transaction_time=$1
		 WHERE tenant_id=$2 AND entity_id=$3 AND version=2`,
		nextTS, tenant, "pit-e1"); err != nil {
		t.Fatalf("pin v2: %v", err)
	}

	pit, err := time.Parse(time.RFC3339, "2026-03-01T12:00:00Z") // between baseTS and nextTS
	if err != nil {
		t.Fatalf("parse pit: %v", err)
	}

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tm.Rollback(txCtx, txID) }()
	txStore, err := factory.EntityStore(txCtx)
	if err != nil {
		t.Fatalf("EntityStore (tx): %v", err)
	}

	opts := searchTxOpts()
	opts.PointInTime = &pit
	opts.TrackingRead = true // must NOT produce a read-set entry for PIT (asserted below)

	got, err := txStore.(spi.Searcher).Search(txCtx, spi.Filter{}, opts)
	if err != nil {
		t.Fatalf("in-tx PIT Search: %v", err)
	}
	if len(got) != 1 || got[0].Meta.ID != "pit-e1" {
		t.Fatalf("in-tx PIT Search: got %v, want [pit-e1]", idsOf(got))
	}
	if string(got[0].Data) != `{"status":"active"}` {
		t.Fatalf("in-tx PIT Search data: got %s, want v1 {\"status\":\"active\"} (v2 committed after pit)", got[0].Data)
	}

	// Oracle: GetAllAsAt + spi.MatchFilter, run through the SAME tx-scoped
	// store/ctx, must agree exactly with Search's committed-as-at-PIT result.
	all, err := txStore.GetAllAsAt(txCtx, searchTxModel, pit)
	if err != nil {
		t.Fatalf("GetAllAsAt: %v", err)
	}
	var wantIDs []string
	for _, e := range all {
		if spi.MatchFilter(spi.Filter{}, e.Data, e.Meta) {
			wantIDs = append(wantIDs, e.Meta.ID)
		}
	}
	sort.Strings(wantIDs)
	gotIDs := idsOf(got)
	if len(wantIDs) == 0 {
		wantIDs = []string{}
	}
	if len(gotIDs) == 0 {
		gotIDs = []string{}
	}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("PIT parity mismatch: Search=%v, GetAllAsAt+MatchFilter=%v", gotIDs, wantIDs)
	}

	// No read-set recorded for a PIT search, even with TrackingRead=true:
	// in-tx PIT is committed-only and does not participate in read-set tracking.
	state, ok := postgres.LookupTxStateForTest(tm, txID)
	if !ok {
		t.Fatal("txState not found")
	}
	if v := postgres.ReadSetVersionForTest(state, "pit-e1"); v != 0 {
		t.Fatalf("PIT search recorded a read-set entry (v=%d), want 0 (PIT is committed-only, no read tracking ⇒ RED)", v)
	}
}

// TestSearchTxPIT_TrackingReadNoConflictOnConcurrentWrite is the RED driver
// proving in-tx PIT search participates in NO read-set tracking end-to-end:
// even with TrackingRead=true, a concurrent committed write to an entity the
// PIT search returned must NOT abort this transaction's commit — because no
// read dependency on it was ever recorded. Contrast with
// TestSearchTx_TrackingReadRecordsReturnedIds, where the same concurrent-write
// scenario against a CURRENT-STATE (PointInTime==nil) tracked search DOES abort.
func TestSearchTxPIT_TrackingReadNoConflictOnConcurrentWrite(t *testing.T) {
	const (
		tenant = "searchtx-pit-conflict-tenant"
		baseTS = "2026-04-01 00:00:00+00"
	)
	factory, tm := setupFCWTest(t)
	baseCtx := ctxWithTenant(tenant)
	seedStore, err := factory.EntityStore(baseCtx)
	if err != nil {
		t.Fatalf("EntityStore (seed): %v", err)
	}
	ids := []string{"pc-e1", "pc-e2", "pc-e3"}
	for _, id := range ids {
		if _, err := seedStore.Save(baseCtx, &spi.Entity{
			Meta: spi.EntityMeta{ID: id, ModelRef: searchTxModel, State: "NEW"},
			Data: []byte(`{"city":"Berlin"}`),
		}); err != nil {
			t.Fatalf("seed Save %s: %v", id, err)
		}
	}
	pool := factory.Pool()
	for _, id := range ids {
		if _, err := pool.Exec(baseCtx,
			`UPDATE entity_versions SET valid_time=$1, transaction_time=$1
			 WHERE tenant_id=$2 AND entity_id=$3 AND version=1`,
			baseTS, tenant, id); err != nil {
			t.Fatalf("pin %s: %v", id, err)
		}
	}
	pit, err := time.Parse(time.RFC3339, "2026-04-01T00:00:00Z") // == baseTS: inclusive bound covers all three
	if err != nil {
		t.Fatalf("parse pit: %v", err)
	}

	// Tx A: PIT Search with TrackingRead=true.
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
	opts.PointInTime = &pit
	opts.TrackingRead = true
	got, err := storeA.(spi.Searcher).Search(txCtxA, spi.Filter{}, opts)
	if err != nil {
		t.Fatalf("Tx A PIT Search: %v", err)
	}
	if !reflect.DeepEqual(idsOf(got), []string{"pc-e1", "pc-e2", "pc-e3"}) {
		t.Fatalf("Tx A PIT Search: got %v, want [pc-e1 pc-e2 pc-e3]", idsOf(got))
	}

	// Direct read-set inspection: PIT must record nothing, despite TrackingRead=true.
	state, ok := postgres.LookupTxStateForTest(tm, txA)
	if !ok {
		t.Fatal("txState not found for Tx A")
	}
	for _, id := range ids {
		if v := postgres.ReadSetVersionForTest(state, id); v != 0 {
			t.Fatalf("PIT TrackingRead=true: read-set for %s = %d, want 0 (PIT must not record reads)", id, v)
		}
	}

	// Tx B: concurrently update pc-e2 (a PIT-returned id) and commit, bumping
	// its current-state version.
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
		Meta: spi.EntityMeta{ID: "pc-e2", ModelRef: searchTxModel, State: "CHANGED"},
		Data: []byte(`{"city":"Hamburg"}`),
	}); err != nil {
		_ = tm.Rollback(txCtxB, txB)
		t.Fatalf("Tx B Save: %v", err)
	}
	if err := tm.Commit(baseCtx, txB); err != nil {
		t.Fatalf("Tx B Commit: %v", err)
	}

	// Tx A commit must SUCCEED: the PIT search recorded no read dependency on
	// pc-e2, so first-committer-wins has nothing to invalidate.
	if err := tm.Commit(baseCtx, txA); err != nil {
		t.Fatalf("Tx A commit: want success (PIT records no read-set), got %v", err)
	}
}
