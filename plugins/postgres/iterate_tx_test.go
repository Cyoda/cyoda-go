package postgres_test

// iterate_tx_test.go — in-transaction Iterate TrackingRead behaviour for the
// PostgreSQL backend.
//
// searcher_tx_test.go's TestSearchTx_TrackingReadRecordsReturnedIds and
// TestSearchTx_NoTrackingReadRecordsNothing pin the same read-set contract
// for Search, but both drive it with spi.Filter{} (match-all): every seeded
// row is returned, so those tests cannot distinguish "record only what was
// yielded" from "record everything scanned" — the two coincide when nothing
// is filtered out. Search's engine-level Limit<=0 branch now delegates to
// Iterate (internal/domain/search/service.go's drainIterate) precisely for
// the "unbounded, still filtered" shape, so this file exercises Iterate with
// a filter that excludes some seeded rows — the shape that actually
// discriminates the bug this pins a regression test for: over-recording the
// read-set with entities the caller never asked about, purely because they
// were scanned, not because they were returned.
import (
	"reflect"
	"sort"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

// berlinFilter matches only rows whose data.city equals "Berlin" — a proper
// subset of the 3-row seed every test in this file uses, so "yielded" and
// "scanned" diverge.
var berlinFilter = spi.Filter{Op: spi.FilterEq, Source: spi.SourceData, Path: "city", Value: "Berlin", Declared: []spi.DataType{spi.String}}

// TestIterateTx_TrackingReadRecordsOnlyYieldedIds is the regression test for
// the over-recording bug (fixed at the engine-routing layer in
// internal/domain/search/service.go, verified here at the layer that
// actually performs the recording): with TrackingRead=true, Iterate must
// record ONLY the entities its filter yields — e1 and e3 (city=Berlin) —
// never e2 (city=Hamburg), which is scanned (it's in the same model) but not
// yielded. A concurrent commit to the UNRETURNED entity e2 must not conflict
// with this transaction's own commit.
func TestIterateTx_TrackingReadRecordsOnlyYieldedIds(t *testing.T) {
	factory, tm, _ := setupSearchTx(t, map[string]string{
		"e1": `{"city":"Berlin"}`,
		"e2": `{"city":"Hamburg"}`,
		"e3": `{"city":"Berlin"}`,
	})
	baseCtx := ctxWithTenant("searchtx-tenant")

	// Tx A: Iterate with TrackingRead=true, filtered to Berlin rows only.
	txA, txCtxA, err := tm.Begin(baseCtx)
	if err != nil {
		t.Fatalf("Tx A Begin: %v", err)
	}
	defer func() { _ = tm.Rollback(txCtxA, txA) }()
	storeA, err := factory.EntityStore(txCtxA)
	if err != nil {
		t.Fatalf("Tx A EntityStore: %v", err)
	}
	iterableA, ok := storeA.(spi.Iterable)
	if !ok {
		t.Fatal("store does not implement spi.Iterable")
	}
	it, err := iterableA.Iterate(txCtxA, searchTxModel, berlinFilter, spi.IterateOptions{TrackingRead: true})
	if err != nil {
		t.Fatalf("Tx A Iterate: %v", err)
	}
	var got []string
	for it.Next() {
		got = append(got, it.Entity().Meta.ID)
	}
	if closeErr := it.Close(); closeErr != nil {
		t.Fatalf("Tx A Iterate Close: %v", closeErr)
	}
	if err := it.Err(); err != nil {
		t.Fatalf("Tx A Iterate Err: %v", err)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, []string{"e1", "e3"}) {
		t.Fatalf("Tx A Iterate: got %v, want [e1 e3]", got)
	}

	// Direct read-set inspection: only the YIELDED ids may be recorded.
	state, ok := postgres.LookupTxStateForTest(tm, txA)
	if !ok {
		t.Fatal("txState not found for Tx A")
	}
	for _, id := range []string{"e1", "e3"} {
		if v := postgres.ReadSetVersionForTest(state, id); v != 1 {
			t.Fatalf("TrackingRead=true: read-set for yielded %s = %d, want 1 (not recorded ⇒ RED)", id, v)
		}
	}
	if v := postgres.ReadSetVersionForTest(state, "e2"); v != 0 {
		t.Fatalf("TrackingRead=true: read-set for UNYIELDED e2 = %d, want 0 (over-recording ⇒ the regression this test pins)", v)
	}

	// Tx B: concurrently update e2 (scanned by Tx A's Iterate, but never
	// yielded — the filter excluded it) and commit.
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
		Data: []byte(`{"city":"Munich"}`),
	}); err != nil {
		_ = tm.Rollback(txCtxB, txB)
		t.Fatalf("Tx B Save e2: %v", err)
	}
	if err := tm.Commit(baseCtx, txB); err != nil {
		t.Fatalf("Tx B Commit: %v", err)
	}

	// Tx A Commit must SUCCEED: e2 was scanned but never yielded, so it was
	// never tracked, so Tx B's write to it carries no conflict.
	if err := tm.Commit(baseCtx, txA); err != nil {
		t.Fatalf("Tx A commit: want success (e2 was scanned, not yielded, so not tracked), got %v", err)
	}
}

// TestIterateTx_NoTrackingReadRecordsNothing: with TrackingRead=false
// (default), Iterate records no reads at all, even for yielded entities — a
// concurrent commit to a YIELDED entity must not abort this tx.
func TestIterateTx_NoTrackingReadRecordsNothing(t *testing.T) {
	factory, tm, _ := setupSearchTx(t, map[string]string{
		"e1": `{"city":"Berlin"}`,
		"e2": `{"city":"Hamburg"}`,
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
	iterableA, ok := storeA.(spi.Iterable)
	if !ok {
		t.Fatal("store does not implement spi.Iterable")
	}
	// TrackingRead defaults to false.
	it, err := iterableA.Iterate(txCtxA, searchTxModel, berlinFilter, spi.IterateOptions{})
	if err != nil {
		t.Fatalf("Tx A Iterate: %v", err)
	}
	var got []string
	for it.Next() {
		got = append(got, it.Entity().Meta.ID)
	}
	if closeErr := it.Close(); closeErr != nil {
		t.Fatalf("Tx A Iterate Close: %v", closeErr)
	}
	if err := it.Err(); err != nil {
		t.Fatalf("Tx A Iterate Err: %v", err)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, []string{"e1", "e3"}) {
		t.Fatalf("Tx A Iterate: got %v, want [e1 e3]", got)
	}

	state, ok := postgres.LookupTxStateForTest(tm, txA)
	if !ok {
		t.Fatal("txState not found for Tx A")
	}
	for _, id := range []string{"e1", "e2", "e3"} {
		if v := postgres.ReadSetVersionForTest(state, id); v != 0 {
			t.Fatalf("TrackingRead=false: read-set for %s = %d, want 0 (recorded ⇒ over-tracking)", id, v)
		}
	}

	// Tx B updates e1 (yielded by Tx A's Iterate) and commits.
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
		Meta: spi.EntityMeta{ID: "e1", ModelRef: searchTxModel, State: "CHANGED"},
		Data: []byte(`{"city":"Hamburg"}`),
	}); err != nil {
		_ = tm.Rollback(txCtxB, txB)
		t.Fatalf("Tx B Save e1: %v", err)
	}
	if err := tm.Commit(baseCtx, txB); err != nil {
		t.Fatalf("Tx B Commit: %v", err)
	}

	// Tx A Commit must SUCCEED (no read-set recorded).
	if err := tm.Commit(baseCtx, txA); err != nil {
		t.Fatalf("Tx A commit: want success (no read-set), got %v", err)
	}
}

