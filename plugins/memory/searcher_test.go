package memory_test

import (
	"context"
	"errors"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// searcher returns the store as a spi.Searcher, failing the test if the memory
// entity store does not implement the optional interface.
func asSearcher(t *testing.T, store spi.EntityStore) spi.Searcher {
	t.Helper()
	s, ok := store.(spi.Searcher)
	if !ok {
		t.Fatalf("memory EntityStore does not implement spi.Searcher")
	}
	return s
}

func idSet(entities []*spi.Entity) map[string]bool {
	ids := make(map[string]bool, len(entities))
	for _, e := range entities {
		ids[e.Meta.ID] = true
	}
	return ids
}

// activeFilter matches entities whose meta state == ACTIVE. The Declared type
// set is stamped (STRING) as the domain layer does from the model schema, so the
// type-directed kernel evaluates the comparison rather than degrading to
// non-match on an unstamped leaf.
var activeFilter = spi.Filter{Op: spi.FilterEq, Source: spi.SourceMeta, Path: "state", Value: "ACTIVE", Declared: []spi.DataType{spi.String}}

func mkEntity(id, state, data string, modelRef spi.ModelRef) *spi.Entity {
	return &spi.Entity{
		Meta: spi.EntityMeta{
			ID: id, TenantID: "tenant-A", ModelRef: modelRef, State: state,
		},
		Data: []byte(data),
	}
}

// TestMemorySearch_NonTx_ParityWithGetAllMatch asserts that a non-tx Search
// returns exactly the same id set as GetAll filtered by spi.Prepare(filter).Match.
func TestMemorySearch_NonTx_ParityWithGetAllMatch(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	ctx := ctxWithTenant("tenant-A")
	store, _ := factory.EntityStore(ctx)
	modelRef := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}
	searcher := asSearcher(t, store)

	store.Save(ctx, mkEntity("e-1", "ACTIVE", `{"n": 1}`, modelRef))
	store.Save(ctx, mkEntity("e-2", "INACTIVE", `{"n": 2}`, modelRef))
	store.Save(ctx, mkEntity("e-3", "ACTIVE", `{"n": 3}`, modelRef))
	store.Save(ctx, mkEntity("e-4", "ACTIVE", `{"n": 4}`, modelRef))

	// Reference: GetAll + spi.Prepare(filter).Match.
	all, err := store.GetAll(ctx, modelRef)
	if err != nil {
		t.Fatalf("GetAll failed: %v", err)
	}
	pf, err := spi.Prepare(activeFilter)
	if err != nil {
		t.Fatalf("spi.Prepare failed: %v", err)
	}
	want := make(map[string]bool)
	for _, e := range all {
		if pf.Match(e.Data, e.Meta) {
			want[e.Meta.ID] = true
		}
	}

	got, err := searcher.Search(ctx, activeFilter, spi.SearchOptions{
		ModelName: "Order", ModelVersion: "1", Limit: 10,
	})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	gotIDs := idSet(got)
	if len(gotIDs) != len(want) {
		t.Fatalf("id-set size mismatch: got %v, want %v", gotIDs, want)
	}
	for id := range want {
		if !gotIDs[id] {
			t.Errorf("expected id %s in Search result, missing", id)
		}
	}
}

// TestMemorySearch_NonTx_Order checks ordering on the non-tx path.
func TestMemorySearch_NonTx_Order(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	ctx := ctxWithTenant("tenant-A")
	store, _ := factory.EntityStore(ctx)
	modelRef := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}
	searcher := asSearcher(t, store)

	store.Save(ctx, mkEntity("e-c", "ACTIVE", `{"n": 3}`, modelRef))
	store.Save(ctx, mkEntity("e-a", "ACTIVE", `{"n": 1}`, modelRef))
	store.Save(ctx, mkEntity("e-b", "ACTIVE", `{"n": 2}`, modelRef))

	order := []spi.OrderSpec{{Path: "n", Source: spi.SourceData, Kind: spi.OrderNumeric}}
	got, err := searcher.Search(ctx, spi.Filter{}, spi.SearchOptions{
		ModelName: "Order", ModelVersion: "1", OrderBy: order, Limit: 10,
	})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 entities, got %d", len(got))
	}
	wantOrder := []string{"e-a", "e-b", "e-c"}
	for i, id := range wantOrder {
		if got[i].Meta.ID != id {
			t.Errorf("index %d: expected %s (n asc), got %s", i, id, got[i].Meta.ID)
		}
	}
}

// TestMemorySearch_InTx_CreatedInTxMatchPresent: an entity created in the tx
// buffer must be visible to an in-tx Search matching it.
func TestMemorySearch_InTx_CreatedInTxMatchPresent(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	txMgr := factory.NewTransactionManager(newTestUUIDGenerator())
	ctx := ctxWithTenant("tenant-A")
	store, _ := factory.EntityStore(ctx)
	modelRef := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}
	searcher := asSearcher(t, store)

	_, txCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("begin failed: %v", err)
	}
	store.Save(txCtx, mkEntity("e-buf", "ACTIVE", `{"n": 1}`, modelRef))

	got, err := searcher.Search(txCtx, activeFilter, spi.SearchOptions{
		ModelName: "Order", ModelVersion: "1", Limit: 10,
	})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	if !idSet(got)["e-buf"] {
		t.Errorf("expected buffered e-buf present in in-tx Search, got %v", idSet(got))
	}
}

// TestMemorySearch_InTx_DeletedInTxAbsent: a committed entity deleted in the tx
// must be absent from an in-tx Search.
func TestMemorySearch_InTx_DeletedInTxAbsent(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	txMgr := factory.NewTransactionManager(newTestUUIDGenerator())
	ctx := ctxWithTenant("tenant-A")
	store, _ := factory.EntityStore(ctx)
	modelRef := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}
	searcher := asSearcher(t, store)

	store.Save(ctx, mkEntity("e-del", "ACTIVE", `{"n": 1}`, modelRef))
	store.Save(ctx, mkEntity("e-keep", "ACTIVE", `{"n": 2}`, modelRef))

	_, txCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("begin failed: %v", err)
	}
	if err := store.Delete(txCtx, "e-del"); err != nil {
		t.Fatalf("delete in tx failed: %v", err)
	}

	got, err := searcher.Search(txCtx, activeFilter, spi.SearchOptions{
		ModelName: "Order", ModelVersion: "1", Limit: 10,
	})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	ids := idSet(got)
	if ids["e-del"] {
		t.Errorf("deleted-in-tx e-del must be absent, got %v", ids)
	}
	if !ids["e-keep"] {
		t.Errorf("e-keep must remain present, got %v", ids)
	}
}

// TestMemorySearch_InTx_BufferedNoLongerMatchesAbsent: a committed entity that
// matched is overwritten in the tx buffer with a non-matching version; it must
// be absent (committed version suppressed, buffered version doesn't match).
func TestMemorySearch_InTx_BufferedNoLongerMatchesAbsent(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	txMgr := factory.NewTransactionManager(newTestUUIDGenerator())
	ctx := ctxWithTenant("tenant-A")
	store, _ := factory.EntityStore(ctx)
	modelRef := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}
	searcher := asSearcher(t, store)

	store.Save(ctx, mkEntity("e-flip", "ACTIVE", `{"n": 1}`, modelRef))

	_, txCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("begin failed: %v", err)
	}
	// Overwrite in buffer with a non-matching state.
	store.Save(txCtx, mkEntity("e-flip", "INACTIVE", `{"n": 1}`, modelRef))

	got, err := searcher.Search(txCtx, activeFilter, spi.SearchOptions{
		ModelName: "Order", ModelVersion: "1", Limit: 10,
	})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	if idSet(got)["e-flip"] {
		t.Errorf("buffered-no-longer-matches e-flip must be absent, got %v", idSet(got))
	}
}

// TestMemorySearch_InTx_PIT_CommittedOnly: in-tx point-in-time search reads
// committed data only; the tx's own uncommitted buffer must not appear.
func TestMemorySearch_InTx_PIT_CommittedOnly(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	txMgr := factory.NewTransactionManager(newTestUUIDGenerator())
	ctx := ctxWithTenant("tenant-A")
	store, _ := factory.EntityStore(ctx)
	modelRef := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}
	searcher := asSearcher(t, store)

	store.Save(ctx, mkEntity("e-committed", "ACTIVE", `{"n": 1}`, modelRef))
	time.Sleep(2 * time.Millisecond)
	pit := time.Now()
	time.Sleep(2 * time.Millisecond)

	_, txCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("begin failed: %v", err)
	}
	// Buffer a new matching entity — must NOT appear under PIT.
	store.Save(txCtx, mkEntity("e-buffered", "ACTIVE", `{"n": 2}`, modelRef))

	got, err := searcher.Search(txCtx, activeFilter, spi.SearchOptions{
		ModelName: "Order", ModelVersion: "1", PointInTime: &pit, Limit: 10,
	})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	ids := idSet(got)
	if !ids["e-committed"] {
		t.Errorf("PIT search must see committed e-committed, got %v", ids)
	}
	if ids["e-buffered"] {
		t.Errorf("PIT search must NOT see uncommitted buffer e-buffered, got %v", ids)
	}

	// PIT read records nothing into the read-set.
	tx := spi.GetTransaction(txCtx)
	if len(tx.ReadSet) != 0 {
		t.Errorf("PIT search must not populate read-set, got %v", tx.ReadSet)
	}
}

// TestMemorySearch_TrackingRead_RecordsReturnedOnly: TrackingRead=true records
// only returned committed ids into tx.ReadSet; TrackingRead=false records none.
func TestMemorySearch_TrackingRead_RecordsReturnedOnly(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	txMgr := factory.NewTransactionManager(newTestUUIDGenerator())
	ctx := ctxWithTenant("tenant-A")
	store, _ := factory.EntityStore(ctx)
	modelRef := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}
	searcher := asSearcher(t, store)

	store.Save(ctx, mkEntity("e-match", "ACTIVE", `{"n": 1}`, modelRef))
	store.Save(ctx, mkEntity("e-nomatch", "INACTIVE", `{"n": 2}`, modelRef))

	// TrackingRead=true → only the returned match is recorded.
	_, txCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("begin failed: %v", err)
	}
	_, err = searcher.Search(txCtx, activeFilter, spi.SearchOptions{
		ModelName: "Order", ModelVersion: "1", TrackingRead: true, Limit: 10,
	})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	tx := spi.GetTransaction(txCtx)
	if len(tx.ReadSet) != 1 || !tx.ReadSet["e-match"] {
		t.Errorf("TrackingRead=true must record only e-match, got %v", tx.ReadSet)
	}

	// TrackingRead=false → nothing recorded.
	_, txCtx2, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("begin failed: %v", err)
	}
	_, err = searcher.Search(txCtx2, activeFilter, spi.SearchOptions{
		ModelName: "Order", ModelVersion: "1", TrackingRead: false, Limit: 10,
	})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	tx2 := spi.GetTransaction(txCtx2)
	if len(tx2.ReadSet) != 0 {
		t.Errorf("TrackingRead=false must record nothing, got %v", tx2.ReadSet)
	}
}

// TestMemorySearch_InTx_DeleteThenSave_AllViewsAgree is a regression test for
// the memory Save-after-Delete bug: Save must clear tx.Deletes so the id is not
// left in BOTH tx.Buffer and tx.Deletes. With the bug, GetAll reported the
// entity present, Search reported it absent, and commit deleted it — three
// disagreeing views. After the fix all three agree: last-write-wins → present.
func TestMemorySearch_InTx_DeleteThenSave_AllViewsAgree(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	txMgr := factory.NewTransactionManager(newTestUUIDGenerator())
	ctx := ctxWithTenant("tenant-A")
	store, _ := factory.EntityStore(ctx)
	modelRef := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}
	searcher := asSearcher(t, store)

	// Committed baseline.
	store.Save(ctx, mkEntity("e-dts", "ACTIVE", `{"gen": "committed"}`, modelRef))

	txID, txCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("begin failed: %v", err)
	}
	// Delete then re-Save (still matching) in the same tx.
	if err := store.Delete(txCtx, "e-dts"); err != nil {
		t.Fatalf("delete in tx failed: %v", err)
	}
	store.Save(txCtx, mkEntity("e-dts", "ACTIVE", `{"gen": "buffered"}`, modelRef))

	// Invariant: id must not be in tx.Deletes after Save-after-Delete.
	tx := spi.GetTransaction(txCtx)
	if tx.Deletes["e-dts"] {
		t.Errorf("Save-after-Delete must clear tx.Deletes; e-dts still marked deleted")
	}

	// GetAll and Search must AGREE: both contain e-dts, as the buffered version.
	all, err := store.GetAll(txCtx, modelRef)
	if err != nil {
		t.Fatalf("GetAll failed: %v", err)
	}
	if !idSet(all)["e-dts"] {
		t.Errorf("GetAll must contain e-dts after Save-after-Delete, got %v", idSet(all))
	}
	got, err := searcher.Search(txCtx, activeFilter, spi.SearchOptions{
		ModelName: "Order", ModelVersion: "1", Limit: 10,
	})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	var searched *spi.Entity
	for _, e := range got {
		if e.Meta.ID == "e-dts" {
			searched = e
		}
	}
	if searched == nil {
		t.Fatalf("Search must contain e-dts after Save-after-Delete, got %v", idSet(got))
	}
	if string(searched.Data) != `{"gen": "buffered"}` {
		t.Errorf("Search must return the buffered version, got %s", searched.Data)
	}

	// After commit, all three views agree: e-dts still present, buffered version.
	if err := txMgr.Commit(ctx, txID); err != nil {
		t.Fatalf("commit failed: %v", err)
	}
	committed, err := store.Get(ctx, "e-dts")
	if err != nil {
		t.Fatalf("expected e-dts present after commit, got error: %v", err)
	}
	if string(committed.Data) != `{"gen": "buffered"}` {
		t.Errorf("committed value must be the buffered version, got %s", committed.Data)
	}
}

// TestMemorySearch_InTx_BufferedSupersedesCommitted: a committed entity that
// matches is re-Saved in the buffer (still matching, changed non-filtered
// field). Search must return it EXACTLY ONCE, as the buffered version — the
// positive counterpart to _BufferedNoLongerMatchesAbsent.
func TestMemorySearch_InTx_BufferedSupersedesCommitted(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	txMgr := factory.NewTransactionManager(newTestUUIDGenerator())
	ctx := ctxWithTenant("tenant-A")
	store, _ := factory.EntityStore(ctx)
	modelRef := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}
	searcher := asSearcher(t, store)

	store.Save(ctx, mkEntity("e-sup", "ACTIVE", `{"note": "committed"}`, modelRef))

	_, txCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("begin failed: %v", err)
	}
	// Re-save with a changed non-filtered field; still matches (state ACTIVE).
	store.Save(txCtx, mkEntity("e-sup", "ACTIVE", `{"note": "buffered"}`, modelRef))

	got, err := searcher.Search(txCtx, activeFilter, spi.SearchOptions{
		ModelName: "Order", ModelVersion: "1", Limit: 10,
	})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	count := 0
	var found *spi.Entity
	for _, e := range got {
		if e.Meta.ID == "e-sup" {
			count++
			found = e
		}
	}
	if count != 1 {
		t.Fatalf("e-sup must appear exactly once, got %d occurrences in %v", count, got)
	}
	if string(found.Data) != `{"note": "buffered"}` {
		t.Errorf("Search must return the buffered version, got %s", found.Data)
	}
}

// TestMemorySearch_NonTx_PIT_CommittedAsAt: a non-tx Search with PointInTime
// returns the committed-as-at result (mirrors GetAllAsAt) and records no
// read-set (there is no transaction).
func TestMemorySearch_NonTx_PIT_CommittedAsAt(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	ctx := ctxWithTenant("tenant-A")
	store, _ := factory.EntityStore(ctx)
	modelRef := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}
	searcher := asSearcher(t, store)

	// Only e-early exists as-at pit.
	store.Save(ctx, mkEntity("e-early", "ACTIVE", `{"n": 1}`, modelRef))
	time.Sleep(2 * time.Millisecond)
	pit := time.Now()
	time.Sleep(2 * time.Millisecond)
	store.Save(ctx, mkEntity("e-late", "ACTIVE", `{"n": 2}`, modelRef))

	got, err := searcher.Search(ctx, activeFilter, spi.SearchOptions{
		ModelName: "Order", ModelVersion: "1", PointInTime: &pit, Limit: 10,
	})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	ids := idSet(got)
	if !ids["e-early"] {
		t.Errorf("non-tx PIT search must see e-early (committed as-at), got %v", ids)
	}
	if ids["e-late"] {
		t.Errorf("non-tx PIT search must NOT see e-late (saved after pit), got %v", ids)
	}

	// Parity with GetAllAsAt (same as-at instant).
	asAt, err := store.GetAllAsAt(ctx, modelRef, pit)
	if err != nil {
		t.Fatalf("GetAllAsAt failed: %v", err)
	}
	pf, err := spi.Prepare(activeFilter)
	if err != nil {
		t.Fatalf("spi.Prepare failed: %v", err)
	}
	want := make(map[string]bool)
	for _, e := range asAt {
		if pf.Match(e.Data, e.Meta) {
			want[e.Meta.ID] = true
		}
	}
	if len(ids) != len(want) {
		t.Fatalf("non-tx PIT search must equal GetAllAsAt+match: got %v, want %v", ids, want)
	}
	for id := range want {
		if !ids[id] {
			t.Errorf("expected id %s from PIT search, missing", id)
		}
	}
}

// TestMemorySearch_TrackingRead_BufferedNotInReadSet: an own-write (buffered)
// entity returned by a TrackingRead search must NOT be added to the read-set
// (it is already in the write-set).
func TestMemorySearch_TrackingRead_BufferedNotInReadSet(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	txMgr := factory.NewTransactionManager(newTestUUIDGenerator())
	ctx := ctxWithTenant("tenant-A")
	store, _ := factory.EntityStore(ctx)
	modelRef := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}
	searcher := asSearcher(t, store)

	store.Save(ctx, mkEntity("e-committed", "ACTIVE", `{"n": 1}`, modelRef))

	_, txCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("begin failed: %v", err)
	}
	store.Save(txCtx, mkEntity("e-own", "ACTIVE", `{"n": 2}`, modelRef))

	_, err = searcher.Search(txCtx, activeFilter, spi.SearchOptions{
		ModelName: "Order", ModelVersion: "1", TrackingRead: true, Limit: 10,
	})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	tx := spi.GetTransaction(txCtx)
	if tx.ReadSet["e-own"] {
		t.Errorf("own-write e-own must not be in read-set, got %v", tx.ReadSet)
	}
	if !tx.ReadSet["e-committed"] {
		t.Errorf("committed e-committed must be in read-set, got %v", tx.ReadSet)
	}
}

// TestMemorySearch_OverLimitFails: a matched set larger than Limit must fail
// with spi.ErrSearchResultLimitExceeded rather than returning a truncated
// prefix.
func TestMemorySearch_OverLimitFails(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	ctx := ctxWithTenant("tenant-A")
	store, _ := factory.EntityStore(ctx)
	modelRef := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}
	searcher := asSearcher(t, store)

	store.Save(ctx, mkEntity("e-1", "ACTIVE", `{"n": 1}`, modelRef))
	store.Save(ctx, mkEntity("e-2", "ACTIVE", `{"n": 2}`, modelRef))
	store.Save(ctx, mkEntity("e-3", "ACTIVE", `{"n": 3}`, modelRef))

	_, err := searcher.Search(ctx, spi.Filter{}, spi.SearchOptions{
		ModelName: "Order", ModelVersion: "1", Limit: 2,
	})
	if !errors.Is(err, spi.ErrSearchResultLimitExceeded) {
		t.Fatalf("3 matches over limit 2: got err %v, want ErrSearchResultLimitExceeded", err)
	}
}

// TestMemorySearch_AtLimitSucceeds: a matched set exactly equal to Limit
// succeeds and returns the whole set.
func TestMemorySearch_AtLimitSucceeds(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	ctx := ctxWithTenant("tenant-A")
	store, _ := factory.EntityStore(ctx)
	modelRef := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}
	searcher := asSearcher(t, store)

	store.Save(ctx, mkEntity("e-1", "ACTIVE", `{"n": 1}`, modelRef))
	store.Save(ctx, mkEntity("e-2", "ACTIVE", `{"n": 2}`, modelRef))

	got, err := searcher.Search(ctx, spi.Filter{}, spi.SearchOptions{
		ModelName: "Order", ModelVersion: "1", Limit: 2,
	})
	if err != nil {
		t.Fatalf("2 matches at limit 2: unexpected err %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d, want 2", len(got))
	}
}

// TestMemorySearch_ZeroLimitRejected: Limit <= 0 is a contract violation —
// the implementation must reject it rather than treating it as "unbounded"
// or substituting a default of its own.
func TestMemorySearch_ZeroLimitRejected(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	ctx := ctxWithTenant("tenant-A")
	store, _ := factory.EntityStore(ctx)
	modelRef := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}
	searcher := asSearcher(t, store)

	store.Save(ctx, mkEntity("e-1", "ACTIVE", `{"n": 1}`, modelRef))

	got, err := searcher.Search(ctx, spi.Filter{}, spi.SearchOptions{
		ModelName: "Order", ModelVersion: "1", Limit: 0,
	})
	if err == nil {
		t.Fatalf("Limit=0 must be rejected as a contract violation, got %d results", len(got))
	}
	if len(got) != 0 {
		t.Errorf("Limit=0 rejection must not also return a partial result, got %d", len(got))
	}
}

// TestMemorySearch_RejectsUnevaluableFilter pins the propagation of
// spi.Prepare's error through Search: a leaf spi.Prepare genuinely cannot
// evaluate (a LIKE pattern with a trailing unpaired escape, which will not
// compile) must fail the search outright, not silently degrade to an empty
// page. See .claude/rules/correctness-over-availability.md. Both malformed
// operands the skipped spitest Pattern/MalformedLike conformance case
// covered (a trailing escape after a literal, and a bare trailing escape)
// are exercised here so this replacement is not narrower than what it
// replaces.
func TestMemorySearch_RejectsUnevaluableFilter(t *testing.T) {
	for _, operand := range []string{`a\`, `\`} {
		t.Run(operand, func(t *testing.T) {
			factory := memory.NewStoreFactory()
			defer factory.Close()
			ctx := ctxWithTenant("tenant-A")
			store, _ := factory.EntityStore(ctx)
			modelRef := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}
			searcher := asSearcher(t, store)

			store.Save(ctx, mkEntity("e-1", "ACTIVE", `{"name": "a"}`, modelRef))

			_, err := searcher.Search(ctx, spi.Filter{
				Op: spi.FilterLike, Source: spi.SourceData, Path: "name",
				Value: operand, Declared: []spi.DataType{spi.String},
			}, spi.SearchOptions{ModelName: "Order", ModelVersion: "1", Limit: 100})
			if err == nil {
				t.Fatal("Search must fail on an unevaluable filter, not return an empty page")
			}
			if !errors.Is(err, spi.ErrUnevaluableLeaf) {
				t.Errorf("err = %v, want errors.Is(err, spi.ErrUnevaluableLeaf)", err)
			}
		})
	}
}

// TestMemorySearch_TxOverlayOverLimitFails: the in-tx RYW overlay must apply
// the same bound as the non-tx path — committed rows plus the transaction's
// own buffered writes together must fit within Limit.
func TestMemorySearch_TxOverlayOverLimitFails(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	txMgr := factory.NewTransactionManager(newTestUUIDGenerator())
	ctx := ctxWithTenant("tenant-A")
	store, _ := factory.EntityStore(ctx)
	modelRef := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}
	searcher := asSearcher(t, store)

	store.Save(ctx, mkEntity("e-1", "ACTIVE", `{"n": 1}`, modelRef))
	store.Save(ctx, mkEntity("e-2", "ACTIVE", `{"n": 2}`, modelRef))

	_, txCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("begin failed: %v", err)
	}
	store.Save(txCtx, mkEntity("e-3", "ACTIVE", `{"n": 3}`, modelRef))

	_, err = searcher.Search(txCtx, spi.Filter{}, spi.SearchOptions{
		ModelName: "Order", ModelVersion: "1", Limit: 2,
	})
	if !errors.Is(err, spi.ErrSearchResultLimitExceeded) {
		t.Fatalf("2 committed + 1 buffered over limit 2: got err %v, want ErrSearchResultLimitExceeded", err)
	}
}

// expiredCtx returns a context derived from parent that is already past its
// deadline (context.WithTimeout(parent, 0), waited on Done()). Used to prove
// the memory backend's real search path (spi.Searcher) — and the GetAll scan
// it and other consumers share — observes ctx cancellation instead of running
// an already-expired request to completion.
func expiredCtx(t *testing.T, parent context.Context) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 0)
	t.Cleanup(cancel)
	<-ctx.Done()
	return ctx
}

// TestSearch_PreExpiredCtxAborts: a pre-expired ctx must abort the non-tx
// scan on both entry points that walk the committed store — spi.Searcher's
// Search and EntityStore.GetAll — rather than returning a full result set
// computed past the deadline. This is spec D5: the memory plugin IS
// spi.Searcher, so this is the only real search path on this backend.
func TestSearch_PreExpiredCtxAborts(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	ctx := ctxWithTenant("tenant-A")
	store, _ := factory.EntityStore(ctx)
	modelRef := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}
	searcher := asSearcher(t, store)

	for i := 0; i < 10; i++ {
		id := "e-" + string(rune('0'+i))
		store.Save(ctx, mkEntity(id, "ACTIVE", `{"n": 1}`, modelRef))
	}

	deadCtx := expiredCtx(t, ctx)

	got, err := searcher.Search(deadCtx, spi.Filter{}, spi.SearchOptions{
		ModelName: "Order", ModelVersion: "1", Limit: 100,
	})
	if err == nil {
		t.Fatalf("Search with pre-expired ctx: expected error, got %d results", len(got))
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Search with pre-expired ctx: err = %v, want chain containing context.DeadlineExceeded", err)
	}
	if len(got) != 0 {
		t.Errorf("Search with pre-expired ctx: got %d results, want 0", len(got))
	}

	all, err := store.GetAll(deadCtx, modelRef)
	if err == nil {
		t.Fatalf("GetAll with pre-expired ctx: expected error, got %d results", len(all))
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("GetAll with pre-expired ctx: err = %v, want chain containing context.DeadlineExceeded", err)
	}
	if len(all) != 0 {
		t.Errorf("GetAll with pre-expired ctx: got %d results, want 0", len(all))
	}
}

// TestSearch_PreExpiredCtxAborts_InTx: the read-your-own-writes overlay path
// (in-tx, no PointInTime) walks the committed snapshot and the tx buffer via
// the same amortized-check loops. A pre-expired ctx presented to an
// otherwise-live transaction must abort Search and GetAll the same way the
// non-tx path does — the deadline belongs to the request, not the
// transaction.
func TestSearch_PreExpiredCtxAborts_InTx(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	txMgr := factory.NewTransactionManager(newTestUUIDGenerator())
	ctx := ctxWithTenant("tenant-A")
	store, _ := factory.EntityStore(ctx)
	modelRef := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}
	searcher := asSearcher(t, store)

	for i := 0; i < 10; i++ {
		id := "e-" + string(rune('0'+i))
		store.Save(ctx, mkEntity(id, "ACTIVE", `{"n": 1}`, modelRef))
	}

	_, txCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("begin failed: %v", err)
	}
	store.Save(txCtx, mkEntity("e-buf", "ACTIVE", `{"n": 99}`, modelRef))

	tx := spi.GetTransaction(txCtx)
	deadTxCtx := spi.WithTransaction(expiredCtx(t, ctx), tx)

	got, err := searcher.Search(deadTxCtx, spi.Filter{}, spi.SearchOptions{
		ModelName: "Order", ModelVersion: "1", Limit: 100,
	})
	if err == nil {
		t.Fatalf("in-tx Search with pre-expired ctx: expected error, got %d results", len(got))
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("in-tx Search with pre-expired ctx: err = %v, want chain containing context.DeadlineExceeded", err)
	}
	if len(got) != 0 {
		t.Errorf("in-tx Search with pre-expired ctx: got %d results, want 0", len(got))
	}

	all, err := store.GetAll(deadTxCtx, modelRef)
	if err == nil {
		t.Fatalf("in-tx GetAll with pre-expired ctx: expected error, got %d results", len(all))
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("in-tx GetAll with pre-expired ctx: err = %v, want chain containing context.DeadlineExceeded", err)
	}
	if len(all) != 0 {
		t.Errorf("in-tx GetAll with pre-expired ctx: got %d results, want 0", len(all))
	}
}
