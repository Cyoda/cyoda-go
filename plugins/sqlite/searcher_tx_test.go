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
var cityBerlin = spi.Filter{Op: spi.FilterEq, Path: "city", Source: spi.SourceData, Value: "Berlin"}

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
// id-set (and per-id Data) as GetAll + spi.MatchFilter would for the same tx
// state — the canonical RYW parity contract.
func assertSearchEqualsGetAllMatch(t *testing.T, store spi.EntityStore, searcher spi.Searcher, txCtx context.Context, filter spi.Filter, opts spi.SearchOptions) []*spi.Entity {
	t.Helper()
	ref := spi.ModelRef{EntityName: opts.ModelName, ModelVersion: opts.ModelVersion}
	all, err := store.GetAll(txCtx, ref)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	wantIDs := make(map[string]bool)
	wantData := make(map[string]string)
	for _, e := range all {
		if spi.MatchFilter(filter, e.Data, e.Meta) {
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
// reflected in Search exactly as GetAll + MatchFilter sees them.
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

	opts := spi.SearchOptions{ModelName: "person", ModelVersion: "1"}
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

// TestSearchTx_DeletedInTxAbsent: a committed entity deleted in the tx is absent;
// a sibling remains present.
func TestSearchTx_DeletedInTxAbsent(t *testing.T) {
	store, txCtx, searcher := beginTxSearcher(t)
	if err := store.Delete(txCtx, "e1"); err != nil {
		t.Fatalf("Delete e1: %v", err)
	}
	opts := spi.SearchOptions{ModelName: "person", ModelVersion: "1"}
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

	opts := spi.SearchOptions{ModelName: "person", ModelVersion: "1"}
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

	opts := spi.SearchOptions{ModelName: "person", ModelVersion: "1"}
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

// TestSearchTx_OrderAndPage_Overlay: ordering + offset/limit must apply across
// the merged (committed + buffer) result set.
func TestSearchTx_OrderAndPage_Overlay(t *testing.T) {
	store, txCtx, searcher := beginTxSearcher(t)
	// Buffer a new Berlin person that sorts between the committed ones by id.
	if _, err := store.Save(txCtx, mkPerson("e2b", "Berlin", "NEW")); err != nil {
		t.Fatalf("Save e2b: %v", err)
	}
	// Committed Berlin: e1, e3. Buffered Berlin: e2b. id-asc order: e1, e2b, e3.
	order := []spi.OrderSpec{{Path: "id", Source: spi.SourceMeta, Kind: spi.OrderText}}
	got, err := searcher.Search(txCtx, cityBerlin, spi.SearchOptions{
		ModelName: "person", ModelVersion: "1", OrderBy: order, Offset: 1, Limit: 1,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 result (offset 1 limit 1), got %d", len(got))
	}
	if got[0].Meta.ID != "e2b" {
		t.Errorf("expected e2b at offset 1 (id asc: e1,e2b,e3), got %s", got[0].Meta.ID)
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
		ModelName: "person", ModelVersion: "1", TrackingRead: true,
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

// TestSearchTx_TrackingRead_PagedWindowOnly: an in-tx TrackingRead=true search
// with a non-trivial Offset/Limit must record into tx.ReadSet only the
// committed ids that fall inside the RETURNED page window — matching
// committed rows that were paged OUT (before the offset or beyond the limit)
// must NOT be recorded, even though they satisfied the filter.
func TestSearchTx_TrackingRead_PagedWindowOnly(t *testing.T) {
	factory, ctx := setupSearcherTest(t)
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}

	// Widen the committed Berlin set beyond the standard e1,e3 so a 5-row
	// window with offset/limit has rows on both sides of the returned page.
	for _, id := range []string{"e6", "e7", "e8"} {
		if _, err := store.Save(ctx, mkPerson(id, "Berlin", "NEW")); err != nil {
			t.Fatalf("Save %s: %v", id, err)
		}
	}
	// Committed Berlin set, id-asc order: e1, e3, e6, e7, e8 (5 rows).

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

	// Offset=1, Limit=2 over id-asc order [e1,e3,e6,e7,e8] returns exactly
	// the page [e3,e6] — e1 is paged out by Offset, e7/e8 are paged out by Limit.
	order := []spi.OrderSpec{{Path: "id", Source: spi.SourceMeta, Kind: spi.OrderText}}
	got, err := searcher.Search(txCtx, cityBerlin, spi.SearchOptions{
		ModelName: "person", ModelVersion: "1", OrderBy: order,
		Offset: 1, Limit: 2, TrackingRead: true,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	gotIDs := idSetTx(got)
	wantPage := map[string]bool{"e3": true, "e6": true}
	if len(gotIDs) != len(wantPage) {
		t.Fatalf("page id-set mismatch: got %v, want %v", gotIDs, wantPage)
	}
	for id := range wantPage {
		if !gotIDs[id] {
			t.Errorf("expected %s in returned page, got %v", id, gotIDs)
		}
	}

	tx := spi.GetTransaction(txCtx)
	for id := range wantPage {
		if !tx.ReadSet[id] {
			t.Errorf("returned page id %s must be in read-set, got %v", id, tx.ReadSet)
		}
	}
	// Matched but paged-out: e1 (before offset), e7 and e8 (beyond limit).
	for _, id := range []string{"e1", "e7", "e8"} {
		if tx.ReadSet[id] {
			t.Errorf("paged-out matching id %s must NOT be in read-set, got %v", id, tx.ReadSet)
		}
	}
	if len(tx.ReadSet) != len(wantPage) {
		t.Errorf("read-set must contain exactly the returned page, got %v", tx.ReadSet)
	}
}

// TestSearchTx_TrackingReadFalse_RecordsNothing: TrackingRead=false records no ids.
func TestSearchTx_TrackingReadFalse_RecordsNothing(t *testing.T) {
	_, txCtx, searcher := beginTxSearcher(t)
	_, err := searcher.Search(txCtx, cityBerlin, spi.SearchOptions{
		ModelName: "person", ModelVersion: "1", TrackingRead: false,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	tx := spi.GetTransaction(txCtx)
	if len(tx.ReadSet) != 0 {
		t.Errorf("TrackingRead=false must record nothing, got %v", tx.ReadSet)
	}
}

// TestSearchTx_NarrowPredicateStaysWithinScanBudget: a mixed filter whose
// pushable part narrows the committed candidate set must NOT exhaust the scan
// budget over a large model (no full scan), while a broad post-filter over the
// same model still errors spi.ErrScanBudgetExhausted.
func TestSearchTx_NarrowPredicateStaysWithinScanBudget(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "tx_budget.db")
	// Scan budget of 5: far below the 50-row model size.
	factory, err := sqlite.NewStoreFactoryForTestWithScanLimit(context.Background(), dbPath, 5)
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

	// Mixed filter: pushable eq(city=Berlin) narrows to 2 rows; residual regex
	// on name then post-filters those 2 — scanned (2) <= budget (5). If the
	// pushdown were dropped (full scan), 50 > 5 would trip the budget.
	mixed := spi.Filter{Op: spi.FilterAnd, Children: []spi.Filter{
		cityBerlin,
		{Op: spi.FilterMatchesRegex, Path: "name", Source: spi.SourceData, Value: ".*"},
	}}
	got, err := searcher.Search(txCtx, mixed, spi.SearchOptions{ModelName: "person", ModelVersion: "1"})
	if err != nil {
		t.Fatalf("narrow in-tx search must stay within budget, got: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 Berlin matches, got %d", len(got))
	}

	// Broad residual over the whole model still exhausts the budget.
	broad := spi.Filter{Op: spi.FilterMatchesRegex, Path: "name", Source: spi.SourceData, Value: ".*"}
	_, err = searcher.Search(txCtx, broad, spi.SearchOptions{ModelName: "person", ModelVersion: "1"})
	if !errors.Is(err, spi.ErrScanBudgetExhausted) {
		t.Fatalf("broad in-tx post-filter must exhaust budget, got: %v", err)
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
// GetAllAsAt(pit) + spi.MatchFilter exactly. It must also record NOTHING in
// tx.ReadSet even with TrackingRead:true (PIT does not participate in RYW
// read-set tracking; it mirrors GetAllAsAt, which always reads committed data).
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
		ModelName: "person", ModelVersion: "1", PointInTime: &pit, TrackingRead: true,
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

	// Must equal GetAllAsAt(pit) + MatchFilter exactly (the committed-pushdown
	// contract; no overlay dimension participates).
	wantAll, err := store.GetAllAsAt(ctx, ref, pit)
	if err != nil {
		t.Fatalf("GetAllAsAt: %v", err)
	}
	wantIDs := map[string]bool{}
	for _, e := range wantAll {
		if spi.MatchFilter(cityBerlin, e.Data, e.Meta) {
			wantIDs[e.Meta.ID] = true
		}
	}
	if len(gotIDs) != len(wantIDs) {
		t.Fatalf("Search(PIT) id-set %v != GetAllAsAt+MatchFilter %v", gotIDs, wantIDs)
	}
	for id := range wantIDs {
		if !gotIDs[id] {
			t.Errorf("expected id %s from GetAllAsAt+MatchFilter, missing from Search(PIT) %v", id, gotIDs)
		}
	}

	// Committed-only PIT must record NOTHING in tx.ReadSet, even though
	// TrackingRead was true.
	tx := spi.GetTransaction(txCtx)
	if len(tx.ReadSet) != 0 {
		t.Errorf("in-tx PIT search must record no read-set entries, got %v", tx.ReadSet)
	}
}

// TestSearchTxPIT_NarrowPredicateStaysWithinScanBudget: an in-tx PIT search
// with a narrow pushable predicate over a large model must stay within
// SearchScanLimit (the pushdown, not a GetAllAsAt-style full scan), while a
// broad residual over the same model still exhausts the budget.
func TestSearchTxPIT_NarrowPredicateStaysWithinScanBudget(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "tx_pit_budget.db")
	clock := sqlite.NewTestClockAt(pitBase)
	// Scan budget of 5: far below the 50-row model size.
	factory, err := sqlite.NewStoreFactoryForTestWithScanLimit(context.Background(), dbPath, 5, sqlite.WithClock(clock))
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
	pit := clock.Now() // snapshot after all 50 committed rows
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

	// Mixed filter: pushable eq(city=Berlin) narrows to 2 rows; residual regex
	// on name then post-filters those 2 — scanned (2) <= budget (5). If the
	// pushdown were dropped (full scan), 50 > 5 would trip the budget.
	mixed := spi.Filter{Op: spi.FilterAnd, Children: []spi.Filter{
		cityBerlin,
		{Op: spi.FilterMatchesRegex, Path: "name", Source: spi.SourceData, Value: ".*"},
	}}
	got, err := searcher.Search(txCtx, mixed, spi.SearchOptions{
		ModelName: "person", ModelVersion: "1", PointInTime: &pit,
	})
	if err != nil {
		t.Fatalf("narrow in-tx PIT search must stay within budget, got: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 Berlin matches, got %d", len(got))
	}

	// Broad residual over the whole model still exhausts the budget.
	broad := spi.Filter{Op: spi.FilterMatchesRegex, Path: "name", Source: spi.SourceData, Value: ".*"}
	_, err = searcher.Search(txCtx, broad, spi.SearchOptions{
		ModelName: "person", ModelVersion: "1", PointInTime: &pit,
	})
	if !errors.Is(err, spi.ErrScanBudgetExhausted) {
		t.Fatalf("broad in-tx PIT post-filter must exhaust budget, got: %v", err)
	}
}
