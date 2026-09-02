package sqlite_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

// idSetTx builds a set of entity IDs from a result slice.
func idSetTx(entities []*spi.Entity) map[string]bool {
	ids := make(map[string]bool, len(entities))
	for _, e := range entities {
		ids[e.Meta.ID] = true
	}
	return ids
}

// cityBerlin matches entities whose data.city == "Berlin" (a pushable predicate).
var cityBerlin = spi.Filter{Op: spi.FilterEq, Path: "city", Source: spi.SourceData, Value: "Berlin", Declared: []spi.DataType{spi.String}}

// beginTxSearcher sets up a factory seeded with the standard person set, begins a
// transaction, and returns the store, the transaction context, and the searcher.
func beginTxSearcher(t *testing.T) (spi.EntityStore, context.Context, spi.Searcher) {
	t.Helper()
	factory, ctx := setupSearcherTest(t)
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	tm, err := factory.TransactionManager(ctx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	_, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	searcher, ok := store.(spi.Searcher)
	if !ok {
		t.Fatal("entityStore does not implement spi.Searcher")
	}
	return store, txCtx, searcher
}

// mkPerson builds a person entity with the given id, city, and state.
func mkPerson(id, city, state string) *spi.Entity {
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}
	return &spi.Entity{
		Meta: spi.EntityMeta{ID: id, ModelRef: ref, State: state},
		Data: []byte(`{"name":"` + id + `","city":"` + city + `"}`),
	}
}

// assertSearchEqualsGetAllMatch asserts that Search returns exactly the same
// id-set (and per-id Data) as GetAll + spi.Prepare(filter).Match would for the
// same tx state — the canonical RYW parity contract.
func assertSearchEqualsGetAllMatch(t *testing.T, store spi.EntityStore, searcher spi.Searcher, txCtx context.Context, filter spi.Filter, opts spi.SearchOptions) []*spi.Entity {
	t.Helper()
	ref := spi.ModelRef{EntityName: opts.ModelName, ModelVersion: opts.ModelVersion}
	all, err := store.GetAll(txCtx, ref)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	wantIDs := make(map[string]bool)
	wantData := make(map[string]string)
	pf, err := spi.Prepare(filter)
	if err != nil {
		t.Fatalf("spi.Prepare: %v", err)
	}
	for _, e := range all {
		if pf.Match(e.Data, e.Meta) {
			wantIDs[e.Meta.ID] = true
			wantData[e.Meta.ID] = string(e.Data)
		}
	}

	got, err := searcher.Search(txCtx, filter, opts)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	gotIDs := idSetTx(got)
	if len(gotIDs) != len(wantIDs) {
		t.Fatalf("id-set size mismatch: got %v, want %v", gotIDs, wantIDs)
	}
	for id := range wantIDs {
		if !gotIDs[id] {
			t.Errorf("expected id %s present in Search, missing (want %v)", id, wantIDs)
		}
	}
	for _, e := range got {
		if wd, ok := wantData[e.Meta.ID]; ok && string(e.Data) != wd {
			t.Errorf("id %s data mismatch: Search=%s GetAll=%s", e.Meta.ID, e.Data, wd)
		}
	}
	return got
}

// TestSearchTx_RYWParity_CreateUpdateDelete: buffered create, an update that
// changes a matching entity to no longer match, and a delete must all be
// reflected in Search exactly as GetAll + spi.Prepare(filter).Match sees them.
func TestSearchTx_RYWParity_CreateUpdateDelete(t *testing.T) {
	store, txCtx, searcher := beginTxSearcher(t)
	// Committed baseline (from setup): e1=Berlin, e3=Berlin match cityBerlin.
	// Buffered create: e6 Berlin (should appear).
	if _, err := store.Save(txCtx, mkPerson("e6", "Berlin", "NEW")); err != nil {
		t.Fatalf("Save e6: %v", err)
	}
	// Buffered update: e1 → Munich (no longer matches Berlin).
	if _, err := store.Save(txCtx, mkPerson("e1", "Munich", "NEW")); err != nil {
		t.Fatalf("Save e1: %v", err)
	}
	// Buffered delete: e3 (drops out).
	if err := store.Delete(txCtx, "e3"); err != nil {
		t.Fatalf("Delete e3: %v", err)
	}

	opts := spi.SearchOptions{ModelName: "person", ModelVersion: "1", Limit: 20}
	got := assertSearchEqualsGetAllMatch(t, store, searcher, txCtx, cityBerlin, opts)

	ids := idSetTx(got)
	if !ids["e6"] {
		t.Errorf("buffered create e6 must be present, got %v", ids)
	}
	if ids["e1"] {
		t.Errorf("updated-away e1 must be absent, got %v", ids)
	}
	if ids["e3"] {
		t.Errorf("deleted e3 must be absent, got %v", ids)
	}
}

// TestSearchTx_RejectsUnevaluableFilter pins the propagation of
// spi.Prepare's error through the in-transaction Search overlay
// (searchTxOverlay): a leaf spi.Prepare genuinely cannot evaluate must fail
// the search outright, not silently degrade to an empty page. This is the
// same acceptance criterion as the non-tx Searcher tests, but exercised on
// the read-your-own-writes overlay path, which plans AND prepares the
// filter independently of the non-tx committed-pushdown path.
func TestSearchTx_RejectsUnevaluableFilter(t *testing.T) {
	store, txCtx, searcher := beginTxSearcher(t)
	if _, err := store.Save(txCtx, mkPerson("e6", "Berlin", "NEW")); err != nil {
		t.Fatalf("Save e6: %v", err)
	}

	_, err := searcher.Search(txCtx, spi.Filter{
		Op: spi.FilterLike, Source: spi.SourceData, Path: "name",
		Value: `a\`, Declared: []spi.DataType{spi.String},
	}, spi.SearchOptions{ModelName: "person", ModelVersion: "1", Limit: 20})
	if err == nil {
		t.Fatal("in-tx Search must fail on an unevaluable filter, not return an empty page")
	}
	if !errors.Is(err, spi.ErrUnevaluableLeaf) {
		t.Errorf("err = %v, want errors.Is(err, spi.ErrUnevaluableLeaf)", err)
	}
}

// TestSearchTx_DeletedInTxAbsent: a committed entity deleted in the tx is absent;
// a sibling remains present.
func TestSearchTx_DeletedInTxAbsent(t *testing.T) {
	store, txCtx, searcher := beginTxSearcher(t)
	if err := store.Delete(txCtx, "e1"); err != nil {
		t.Fatalf("Delete e1: %v", err)
	}
	opts := spi.SearchOptions{ModelName: "person", ModelVersion: "1", Limit: 20}
	got := assertSearchEqualsGetAllMatch(t, store, searcher, txCtx, cityBerlin, opts)
	ids := idSetTx(got)
	if ids["e1"] {
		t.Errorf("deleted-in-tx e1 must be absent, got %v", ids)
	}
	if !ids["e3"] {
		t.Errorf("e3 must remain present, got %v", ids)
	}
}

// TestSearchTx_DeleteThenSave_ReturnedOnceAsBuffered is the Save-after-Delete
// regression: Delete then re-Save the same id in one tx must leave it present
// exactly once, as the buffered version — and Search must agree with GetAll.
func TestSearchTx_DeleteThenSave_ReturnedOnceAsBuffered(t *testing.T) {
	store, txCtx, searcher := beginTxSearcher(t)
	// e1 is a committed Berlin match.
	if err := store.Delete(txCtx, "e1"); err != nil {
		t.Fatalf("Delete e1: %v", err)
	}
	// Re-Save e1 (still Berlin) with distinctive data.
	rebuf := &spi.Entity{
		Meta: spi.EntityMeta{ID: "e1", ModelRef: spi.ModelRef{EntityName: "person", ModelVersion: "1"}, State: "NEW"},
		Data: []byte(`{"name":"Alice","city":"Berlin","gen":"buffered"}`),
	}
	if _, err := store.Save(txCtx, rebuf); err != nil {
		t.Fatalf("Save e1: %v", err)
	}

	// Invariant: id must not remain in tx.Deletes after Save-after-Delete.
	tx := spi.GetTransaction(txCtx)
	if tx.Deletes["e1"] {
		t.Errorf("Save-after-Delete must clear tx.Deletes for e1")
	}

	opts := spi.SearchOptions{ModelName: "person", ModelVersion: "1", Limit: 20}
	got := assertSearchEqualsGetAllMatch(t, store, searcher, txCtx, cityBerlin, opts)

	count := 0
	var found *spi.Entity
	for _, e := range got {
		if e.Meta.ID == "e1" {
			count++
			found = e
		}
	}
	if count != 1 {
		t.Fatalf("e1 must appear exactly once, got %d in %v", count, idSetTx(got))
	}
	if string(found.Data) != string(rebuf.Data) {
		t.Errorf("Search must return the buffered e1, got %s", found.Data)
	}
}

// TestSearchTx_BufferedSupersedesCommitted: a committed match re-Saved in the
// buffer (still matching, changed non-filtered field) is returned exactly once,
// as the buffered version.
func TestSearchTx_BufferedSupersedesCommitted(t *testing.T) {
	store, txCtx, searcher := beginTxSearcher(t)
	sup := &spi.Entity{
		Meta: spi.EntityMeta{ID: "e1", ModelRef: spi.ModelRef{EntityName: "person", ModelVersion: "1"}, State: "NEW"},
		Data: []byte(`{"name":"Alice","city":"Berlin","note":"buffered"}`),
	}
	if _, err := store.Save(txCtx, sup); err != nil {
		t.Fatalf("Save e1: %v", err)
	}

	opts := spi.SearchOptions{ModelName: "person", ModelVersion: "1", Limit: 20}
	got := assertSearchEqualsGetAllMatch(t, store, searcher, txCtx, cityBerlin, opts)

	count := 0
	var found *spi.Entity
	for _, e := range got {
		if e.Meta.ID == "e1" {
			count++
			found = e
		}
	}
	if count != 1 {
		t.Fatalf("e1 must appear exactly once, got %d in %v", count, idSetTx(got))
	}
	if string(found.Data) != string(sup.Data) {
		t.Errorf("Search must return the buffered version, got %s", found.Data)
	}
}

// TestSearchTx_OrderAcrossOverlay: ordering must apply across the merged
// (committed + buffer) result set.
func TestSearchTx_OrderAcrossOverlay(t *testing.T) {
	store, txCtx, searcher := beginTxSearcher(t)
	// Buffer a new Berlin person that sorts between the committed ones by id.
	if _, err := store.Save(txCtx, mkPerson("e2b", "Berlin", "NEW")); err != nil {
		t.Fatalf("Save e2b: %v", err)
	}
	// Committed Berlin: e1, e3. Buffered Berlin: e2b. id-asc order: e1, e2b, e3.
	order := []spi.OrderSpec{{Path: "id", Source: spi.SourceMeta, Kind: spi.OrderText}}
	got, err := searcher.Search(txCtx, cityBerlin, spi.SearchOptions{
		ModelName: "person", ModelVersion: "1", OrderBy: order, Limit: 20,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	wantOrder := []string{"e1", "e2b", "e3"}
	if len(got) != len(wantOrder) {
		t.Fatalf("expected %d results, got %d", len(wantOrder), len(got))
	}
	for i, id := range wantOrder {
		if got[i].Meta.ID != id {
			t.Errorf("index %d: expected %s (id asc: e1,e2b,e3), got %s", i, id, got[i].Meta.ID)
		}
	}
}

// TestSearchTx_OverlayOverLimitFails: the in-tx RYW overlay must apply the
// same bound as the non-tx path — committed rows plus the transaction's own
// buffered writes together must fit within Limit.
func TestSearchTx_OverlayOverLimitFails(t *testing.T) {
	store, txCtx, searcher := beginTxSearcher(t)
	// Committed Berlin matches from setup: e1, e3 (2). Buffered own-write
	// Berlin match: e6. 3 survivors total.
	if _, err := store.Save(txCtx, mkPerson("e6", "Berlin", "NEW")); err != nil {
		t.Fatalf("Save e6: %v", err)
	}
	_, err := searcher.Search(txCtx, cityBerlin, spi.SearchOptions{
		ModelName: "person", ModelVersion: "1", Limit: 2,
	})
	if !errors.Is(err, spi.ErrSearchResultLimitExceeded) {
		t.Fatalf("overlay, 3 survivors over limit 2: got err %v, want ErrSearchResultLimitExceeded", err)
	}
}

// TestSearchTx_TrackingRead_RecordsReturnedCommittedOnly: TrackingRead=true
// records only returned committed ids into tx.ReadSet (buffered own-writes are
// excluded); TrackingRead=false records nothing.
func TestSearchTx_TrackingRead_RecordsReturnedCommittedOnly(t *testing.T) {
	store, txCtx, searcher := beginTxSearcher(t)
	// Buffer an own-write Berlin match.
	if _, err := store.Save(txCtx, mkPerson("e6", "Berlin", "NEW")); err != nil {
		t.Fatalf("Save e6: %v", err)
	}

	// TrackingRead=true: committed Berlin (e1, e3) recorded; buffered e6 NOT.
	got, err := searcher.Search(txCtx, cityBerlin, spi.SearchOptions{
		ModelName: "person", ModelVersion: "1", TrackingRead: true, Limit: 20,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !idSetTx(got)["e6"] {
		t.Fatalf("buffered e6 must be in result, got %v", idSetTx(got))
	}
	tx := spi.GetTransaction(txCtx)
	if !tx.ReadSet["e1"] || !tx.ReadSet["e3"] {
		t.Errorf("committed matches e1,e3 must be in read-set, got %v", tx.ReadSet)
	}
	if tx.ReadSet["e6"] {
		t.Errorf("buffered own-write e6 must NOT be in read-set, got %v", tx.ReadSet)
	}
	// Non-matching committed (e2,e4,e5) must not be recorded.
	for _, id := range []string{"e2", "e4", "e5"} {
		if tx.ReadSet[id] {
			t.Errorf("non-returned committed %s must not be in read-set, got %v", id, tx.ReadSet)
		}
	}
}

// TestSearchTx_TrackingRead_RecordsMatchedSet: under bounded-or-fail there is
// no page smaller than the matched set, so an in-tx TrackingRead=true search
// records into tx.ReadSet exactly the matched COMMITTED set — the whole of
// it, not a window — and nothing this transaction itself buffered, even when
// the buffered write is present in the same transaction. This supersedes the
// pre-bounded-or-fail invariant (paged-out matches stayed out of the
// read-set): there is no smaller page anymore, so the read-set widens to the
// full matched set, which is what first-committer-wins validates at commit.
func TestSearchTx_TrackingRead_RecordsMatchedSet(t *testing.T) {
	factory, ctx := setupSearcherTest(t)
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}

	// One more committed Berlin match beyond the standard e1,e3 → 3 committed
	// matches, id-asc order: e1, e3, e6.
	if _, err := store.Save(ctx, mkPerson("e6", "Berlin", "NEW")); err != nil {
		t.Fatalf("Save e6: %v", err)
	}

	tm, err := factory.TransactionManager(ctx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	_, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	searcher, ok := store.(spi.Searcher)
	if !ok {
		t.Fatal("entityStore does not implement spi.Searcher")
	}

	// A buffered own-write that DOES match cityBerlin — it is part of the
	// returned matched set (RYW), so it must actually reach tx.ReadSet's
	// exclusion check, not merely be absent because it never matched.
	if _, err := store.Save(txCtx, mkPerson("e9", "Berlin", "NEW")); err != nil {
		t.Fatalf("Save e9: %v", err)
	}

	// Limit exactly at the total matched-set size (3 committed + 1 buffered =
	// 4) succeeds and returns all of it.
	got, err := searcher.Search(txCtx, cityBerlin, spi.SearchOptions{
		ModelName: "person", ModelVersion: "1", Limit: 4, TrackingRead: true,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	gotIDs := idSetTx(got)
	wantReturned := map[string]bool{"e1": true, "e3": true, "e6": true, "e9": true}
	wantMatched := map[string]bool{"e1": true, "e3": true, "e6": true}
	if len(gotIDs) != len(wantReturned) {
		t.Fatalf("returned-set mismatch: got %v, want %v", gotIDs, wantReturned)
	}
	for id := range wantReturned {
		if !gotIDs[id] {
			t.Errorf("expected %s in returned set, got %v", id, gotIDs)
		}
	}

	tx := spi.GetTransaction(txCtx)
	for id := range wantMatched {
		if !tx.ReadSet[id] {
			t.Errorf("committed match %s must be in read-set, got %v", id, tx.ReadSet)
		}
	}
	if tx.ReadSet["e9"] {
		t.Errorf("buffered own-write e9 must NOT be in read-set even though it matched and was returned, got %v", tx.ReadSet)
	}
	if len(tx.ReadSet) != len(wantMatched) {
		t.Errorf("read-set must contain exactly the matched committed set, got %v", tx.ReadSet)
	}
}

// TestSearchTx_TrackingReadFalse_RecordsNothing: TrackingRead=false records no ids.
func TestSearchTx_TrackingReadFalse_RecordsNothing(t *testing.T) {
	_, txCtx, searcher := beginTxSearcher(t)
	_, err := searcher.Search(txCtx, cityBerlin, spi.SearchOptions{
		ModelName: "person", ModelVersion: "1", TrackingRead: false, Limit: 20,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	tx := spi.GetTransaction(txCtx)
	if len(tx.ReadSet) != 0 {
		t.Errorf("TrackingRead=false must record nothing, got %v", tx.ReadSet)
	}
}

// TestSearchTx_MixedFilterOverLargeModel: in-tx, a mixed filter (pushable
// eq(city) AND a non-pushable regex) returns exactly the matching rows, and a
// broad residual over the whole model runs to completion.
//
// The broad half is the in-tx regression guard for the removed scan budget: 50
// rows all reached through a residual post-filter used to fail the search
// outright, and must now return all 50.
//
// Note what this does NOT assert: that the pushable half actually narrows the
// SQL candidate set. It cannot — the residual re-checks the FULL original
// filter, so a full scan returns the same two rows as a narrowed one. Rows
// examined was observable only through the scan budget, which is gone. The
// narrowing is pinned at the planner instead, by
// TestPlanFor_MixedFilterPushesTheEqLeaf (plan_for_test.go).
func TestSearchTx_MixedFilterOverLargeModel(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "tx_broad_residual.db")
	factory, err := sqlite.NewStoreFactoryForTest(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("create factory: %v", err)
	}
	defer factory.Close()

	ctx := testCtx("tenant-1")
	store, _ := factory.EntityStore(ctx)
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}

	// 50 committed persons; only e-berlin-0 and e-berlin-1 live in Berlin.
	for i := 0; i < 48; i++ {
		if _, err := store.Save(ctx, mkPerson2(ref, "e-other-"+itoa(i), "Munich", "target")); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := store.Save(ctx, mkPerson2(ref, "e-berlin-"+itoa(i), "Berlin", "target")); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}

	tm, _ := factory.TransactionManager(ctx)
	_, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	searcher := store.(spi.Searcher)

	// Mixed filter: pushable eq(city=Berlin) narrows to 2 rows; the residual
	// regex on name then post-filters those 2.
	mixed := spi.Filter{Op: spi.FilterAnd, Children: []spi.Filter{
		cityBerlin,
		{Op: spi.FilterMatchesRegex, Path: "name", Source: spi.SourceData, Value: ".*"},
	}}
	got, err := searcher.Search(txCtx, mixed, spi.SearchOptions{ModelName: "person", ModelVersion: "1", Limit: 10})
	if err != nil {
		t.Fatalf("narrow in-tx search: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 Berlin matches, got %d", len(got))
	}

	// Broad residual over the whole model: unmetered, so all 50 come back.
	broad := spi.Filter{Op: spi.FilterMatchesRegex, Path: "name", Source: spi.SourceData, Value: ".*"}
	all, err := searcher.Search(txCtx, broad, spi.SearchOptions{ModelName: "person", ModelVersion: "1", Limit: 50})
	if err != nil {
		t.Fatalf("broad in-tx residual must not be metered: %v", err)
	}
	if len(all) != 50 {
		t.Fatalf("expected all 50 rows through the residual, got %d", len(all))
	}
}

func mkPerson2(ref spi.ModelRef, id, city, name string) *spi.Entity {
	return &spi.Entity{
		Meta: spi.EntityMeta{ID: id, ModelRef: ref, State: "NEW"},
		Data: []byte(`{"name":"` + name + `","city":"` + city + `"}`),
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// TestSearchTxPIT_CommittedOnly_ExcludesBufferedWrite: an in-tx Search with
// PointInTime set to before a buffered write must be committed-only — the
// buffered write is excluded (no overlay) — and must equal
// GetAllAsAt(pit) + spi.Prepare(filter).Match exactly. It must also record
// NOTHING in tx.ReadSet even with TrackingRead:true (PIT does not participate
// in RYW read-set tracking; it mirrors GetAllAsAt, which always reads
// committed data).
func TestSearchTxPIT_CommittedOnly_ExcludesBufferedWrite(t *testing.T) {
	dir := t.TempDir()
	clock := sqlite.NewTestClockAt(pitBase)
	factory, err := sqlite.NewStoreFactoryForTest(context.Background(), filepath.Join(dir, "pit_tx.db"), sqlite.WithClock(clock))
	if err != nil {
		t.Fatalf("create factory: %v", err)
	}
	t.Cleanup(func() { factory.Close() })

	ctx := testCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}

	// Committed Berlin match at pitBase.
	if _, err := store.Save(ctx, mkPerson2(ref, "e1", "Berlin", "committed")); err != nil {
		t.Fatalf("Save e1: %v", err)
	}
	pit := pitBase
	clock.Advance(time.Millisecond)

	tm, err := factory.TransactionManager(ctx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	_, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	searcher := store.(spi.Searcher)

	// Buffered write, AFTER pit, inside the tx: must be excluded from a
	// committed-only PIT search at pit (which predates it).
	if _, err := store.Save(txCtx, mkPerson2(ref, "e2", "Berlin", "buffered")); err != nil {
		t.Fatalf("Save e2 (buffered): %v", err)
	}

	got, err := searcher.Search(txCtx, cityBerlin, spi.SearchOptions{
		ModelName: "person", ModelVersion: "1", PointInTime: &pit, TrackingRead: true, Limit: 20,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	gotIDs := idSetTx(got)
	if gotIDs["e2"] {
		t.Errorf("buffered write postdating pit must be excluded from committed-only PIT search, got %v", gotIDs)
	}
	if !gotIDs["e1"] {
		t.Errorf("committed e1 must be present, got %v", gotIDs)
	}

	// Must equal GetAllAsAt(pit) + spi.Prepare(filter).Match exactly (the
	// committed-pushdown contract; no overlay dimension participates).
	wantAll, err := store.GetAllAsAt(ctx, ref, pit)
	if err != nil {
		t.Fatalf("GetAllAsAt: %v", err)
	}
	wantIDs := map[string]bool{}
	pfBerlin, err := spi.Prepare(cityBerlin)
	if err != nil {
		t.Fatalf("spi.Prepare: %v", err)
	}
	for _, e := range wantAll {
		if pfBerlin.Match(e.Data, e.Meta) {
			wantIDs[e.Meta.ID] = true
		}
	}
	if len(gotIDs) != len(wantIDs) {
		t.Fatalf("Search(PIT) id-set %v != GetAllAsAt+Prepare/Match %v", gotIDs, wantIDs)
	}
	for id := range wantIDs {
		if !gotIDs[id] {
			t.Errorf("expected id %s from GetAllAsAt+Prepare/Match, missing from Search(PIT) %v", id, gotIDs)
		}
	}

	// Committed-only PIT must record NOTHING in tx.ReadSet, even though
	// TrackingRead was true.
	tx := spi.GetTransaction(txCtx)
	if len(tx.ReadSet) != 0 {
		t.Errorf("in-tx PIT search must record no read-set entries, got %v", tx.ReadSet)
	}
}

// TestSearchTxPIT_MixedFilterOverLargeModel is the point-in-time counterpart of
// TestSearchTx_MixedFilterOverLargeModel: the mixed filter returns exactly the
// matching rows at the snapshot, and a broad residual over the whole model runs
// to completion unmetered. The same caveat applies — it does not assert that
// the pushable half narrows; see the sibling's note.
func TestSearchTxPIT_MixedFilterOverLargeModel(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "tx_pit_broad_residual.db")
	clock := sqlite.NewTestClockAt(pitBase)
	factory, err := sqlite.NewStoreFactoryForTest(context.Background(), dbPath, sqlite.WithClock(clock))
	if err != nil {
		t.Fatalf("create factory: %v", err)
	}
	defer factory.Close()

	ctx := testCtx("tenant-1")
	store, _ := factory.EntityStore(ctx)
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}

	// 50 committed persons; only e-berlin-0 and e-berlin-1 live in Berlin.
	for i := 0; i < 48; i++ {
		if _, err := store.Save(ctx, mkPerson2(ref, "e-other-"+itoa(i), "Munich", "target")); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := store.Save(ctx, mkPerson2(ref, "e-berlin-"+itoa(i), "Berlin", "target")); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}
	// Advance before capturing the snapshot: submit times are stamped under a
	// monotonic floor, so 50 writes inside one frozen-clock tick land at
	// successive microseconds ABOVE the clock. A point in time read off the
	// clock itself would sit below all but the first.
	clock.Advance(time.Millisecond)
	pit := clock.Now() // snapshot after all 50 committed rows

	tm, err := factory.TransactionManager(ctx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	_, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	searcher := store.(spi.Searcher)

	// Mixed filter: pushable eq(city=Berlin) narrows to 2 rows; the residual
	// regex on name then post-filters those 2.
	mixed := spi.Filter{Op: spi.FilterAnd, Children: []spi.Filter{
		cityBerlin,
		{Op: spi.FilterMatchesRegex, Path: "name", Source: spi.SourceData, Value: ".*"},
	}}
	got, err := searcher.Search(txCtx, mixed, spi.SearchOptions{
		ModelName: "person", ModelVersion: "1", PointInTime: &pit, Limit: 10,
	})
	if err != nil {
		t.Fatalf("narrow in-tx PIT search: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 Berlin matches, got %d", len(got))
	}

	// Broad residual over the whole model: unmetered, so all 50 come back.
	broad := spi.Filter{Op: spi.FilterMatchesRegex, Path: "name", Source: spi.SourceData, Value: ".*"}
	all, err := searcher.Search(txCtx, broad, spi.SearchOptions{
		ModelName: "person", ModelVersion: "1", PointInTime: &pit, Limit: 50,
	})
	if err != nil {
		t.Fatalf("broad in-tx PIT residual must not be metered: %v", err)
	}
	if len(all) != 50 {
		t.Fatalf("expected all 50 rows through the residual, got %d", len(all))
	}
}
