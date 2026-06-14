package memory_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// gsModel is the canonical (entity, version) pair for grouped-stats tests.
var gsModel = spi.ModelRef{EntityName: "Item", ModelVersion: "1"}

// gsNewStore returns a fresh factory and tenant-bound EntityStore ready for
// grouped-stats tests. Mirrors the bootstrap helper used in entity_store_test.go.
func gsNewStore(t *testing.T) (*memory.StoreFactory, spi.EntityStore, context.Context) {
	t.Helper()
	f := memory.NewStoreFactory()
	ctx := ctxWithTenant("t1")
	store, err := f.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore failed: %v", err)
	}
	return f, store, ctx
}

// gsSave is a convenience helper to seed an entity with an explicit ID,
// state, and JSON-encoded data.
func gsSave(t *testing.T, ctx context.Context, store spi.EntityStore, id, state string, data map[string]any) {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	e := &spi.Entity{
		Meta: spi.EntityMeta{
			ID:       id,
			TenantID: "t1",
			ModelRef: gsModel,
			State:    state,
		},
		Data: raw,
	}
	if _, err := store.Save(ctx, e); err != nil {
		t.Fatalf("save %s: %v", id, err)
	}
}

func TestMemoryIterate_BasicScan(t *testing.T) {
	_, store, ctx := gsNewStore(t)
	for i := 0; i < 3; i++ {
		gsSave(t, ctx, store, fmt.Sprintf("e-%d", i), "available", map[string]any{"x": i})
	}

	it, ok := store.(spi.Iterable)
	if !ok {
		t.Fatal("store does not implement spi.Iterable")
	}
	iter, err := it.Iterate(ctx, gsModel, spi.Filter{}, spi.IterateOptions{})
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	defer iter.Close()

	var seen int
	for iter.Next() {
		if iter.Entity() == nil {
			t.Fatal("Entity() returned nil after Next()==true")
		}
		seen++
	}
	if err := iter.Err(); err != nil {
		t.Fatalf("iter err: %v", err)
	}
	if seen != 3 {
		t.Fatalf("got %d, want 3", seen)
	}
}

func TestMemoryIterate_FilterAppliedInNext(t *testing.T) {
	_, store, ctx := gsNewStore(t)
	gsSave(t, ctx, store, "e-a", "available", map[string]any{"x": 1})
	gsSave(t, ctx, store, "e-b", "allocated", map[string]any{"x": 2})
	gsSave(t, ctx, store, "e-c", "available", map[string]any{"x": 3})

	it := store.(spi.Iterable)
	filter := spi.Filter{
		Op:     spi.FilterEq,
		Source: spi.SourceMeta,
		Path:   "state",
		Value:  "available",
	}
	iter, err := it.Iterate(ctx, gsModel, filter, spi.IterateOptions{})
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	defer iter.Close()

	seenIDs := map[string]bool{}
	for iter.Next() {
		seenIDs[iter.Entity().Meta.ID] = true
	}
	if err := iter.Err(); err != nil {
		t.Fatalf("iter err: %v", err)
	}
	if len(seenIDs) != 2 || !seenIDs["e-a"] || !seenIDs["e-c"] {
		t.Fatalf("filter-applied scan got %v; want {e-a,e-c}", seenIDs)
	}
}

func TestMemoryIterate_CtxCancellationObserved(t *testing.T) {
	_, store, ctx := gsNewStore(t)
	for i := 0; i < 5; i++ {
		gsSave(t, ctx, store, fmt.Sprintf("e-%d", i), "available", map[string]any{"x": i})
	}

	it := store.(spi.Iterable)
	cctx, cancel := context.WithCancel(ctx)
	iter, err := it.Iterate(cctx, gsModel, spi.Filter{}, spi.IterateOptions{})
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	defer iter.Close()

	cancel()
	if iter.Next() {
		t.Fatal("Next() should return false after ctx cancel")
	}
	if !errors.Is(iter.Err(), context.Canceled) {
		t.Fatalf("Err() = %v; want context.Canceled", iter.Err())
	}
	// Sticky: subsequent Next() still false.
	if iter.Next() {
		t.Fatal("Next() should remain false after Err() set")
	}
}

func TestMemoryIterate_CloseIdempotent(t *testing.T) {
	_, store, ctx := gsNewStore(t)
	gsSave(t, ctx, store, "e-1", "available", map[string]any{"x": 1})

	it := store.(spi.Iterable)
	iter, err := it.Iterate(ctx, gsModel, spi.Filter{}, spi.IterateOptions{})
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	if err := iter.Close(); err != nil {
		t.Fatalf("Close #1: %v", err)
	}
	if err := iter.Close(); err != nil {
		t.Fatalf("Close #2 should be a no-op, got: %v", err)
	}
	// Next() after Close MUST return false (the iterator is done).
	if iter.Next() {
		t.Fatal("Next() should return false after Close")
	}
}

func TestMemoryIterate_InTxOverlay(t *testing.T) {
	f, store, ctx := gsNewStore(t)
	defer f.Close()

	// Seed two committed entities, one of which we'll later delete inside tx.
	gsSave(t, ctx, store, "e-keep", "available", map[string]any{"x": 1})
	gsSave(t, ctx, store, "e-delete", "available", map[string]any{"x": 2})

	tm := f.GetTransactionManager()
	if tm == nil {
		t.Fatal("no TransactionManager registered")
	}
	_, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	// In-tx: add a new entity (visible) and delete e-delete (hidden).
	gsSave(t, txCtx, store, "e-new", "available", map[string]any{"x": 99})
	if err := store.Delete(txCtx, "e-delete"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	it := store.(spi.Iterable)
	iter, err := it.Iterate(txCtx, gsModel, spi.Filter{}, spi.IterateOptions{})
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	defer iter.Close()

	seen := map[string]bool{}
	for iter.Next() {
		seen[iter.Entity().Meta.ID] = true
	}
	if err := iter.Err(); err != nil {
		t.Fatalf("iter err: %v", err)
	}
	if !seen["e-keep"] {
		t.Errorf("e-keep should be visible (committed, not deleted)")
	}
	if !seen["e-new"] {
		t.Errorf("e-new should be visible (tx.Buffer overlay)")
	}
	if seen["e-delete"] {
		t.Errorf("e-delete should be hidden (tx.Deletes)")
	}
}

func TestMemoryGroupedAggregate_CountByState(t *testing.T) {
	_, store, ctx := gsNewStore(t)
	for i := 0; i < 5; i++ {
		gsSave(t, ctx, store, fmt.Sprintf("a-%d", i), "available", map[string]any{})
	}
	for i := 0; i < 2; i++ {
		gsSave(t, ctx, store, fmt.Sprintf("b-%d", i), "allocated", map[string]any{})
	}

	ga := store.(spi.GroupedAggregator)
	res, err := ga.GroupedAggregate(ctx, gsModel,
		[]spi.GroupExpr{{Kind: spi.GroupExprState}},
		spi.Filter{},
		spi.GroupedAggregationsOptions{MaxBuckets: 100},
	)
	if err != nil {
		t.Fatalf("GroupedAggregate: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("buckets=%d, want 2; got %+v", len(res), res)
	}
	totals := map[string]int64{}
	for _, b := range res {
		v, ok := b.GroupKey[0].Value.(string)
		if !ok {
			t.Fatalf("group key value is not string: %T %v", b.GroupKey[0].Value, b.GroupKey[0].Value)
		}
		totals[v] = b.Count
	}
	if totals["available"] != 5 || totals["allocated"] != 2 {
		t.Fatalf("counts wrong: %v", totals)
	}
}

func TestMemoryGroupedAggregate_Sum(t *testing.T) {
	_, store, ctx := gsNewStore(t)
	gsSave(t, ctx, store, "e-1", "available", map[string]any{"amount": 10.0})
	gsSave(t, ctx, store, "e-2", "available", map[string]any{"amount": 20.0})
	gsSave(t, ctx, store, "e-3", "allocated", map[string]any{"amount": 5.0})

	ga := store.(spi.GroupedAggregator)
	res, err := ga.GroupedAggregate(ctx, gsModel,
		[]spi.GroupExpr{{Kind: spi.GroupExprState}},
		spi.Filter{},
		spi.GroupedAggregationsOptions{
			MaxBuckets: 100,
			Aggregations: []spi.AggregateExpr{
				{Op: spi.AggSum, Field: "amount", Alias: "sum_amount"},
			},
		},
	)
	if err != nil {
		t.Fatalf("GroupedAggregate: %v", err)
	}
	sums := map[string]float64{}
	for _, b := range res {
		v := b.GroupKey[0].Value.(string)
		f, ok := b.Aggregations["sum_amount"].(float64)
		if !ok {
			t.Fatalf("sum_amount missing or wrong type: %T", b.Aggregations["sum_amount"])
		}
		sums[v] = f
	}
	if sums["available"] != 30 || sums["allocated"] != 5 {
		t.Fatalf("sums wrong: %v", sums)
	}
}

// TestMemoryGroupedAggregate_StdevWelfordParity verifies that the
// plugin's online Welford stdev implementation matches an independent
// Welford reference within 1e-9 relative error, on a dataset chosen to
// make this the binding correctness check — high mean, low variance,
// the classical case where a naive (E[X^2]-E[X]^2) implementation
// would lose precision to catastrophic cancellation.
//
// Note on tolerance choice: the plugin walks the snapshot in Go map
// iteration order (non-deterministic) so the observation order to
// Welford shuffles across runs. Welford's variance is mathematically
// permutation-invariant but float rounding differs at the ULP level;
// the cross-permutation spread scales with the magnitude of the
// running mean. At mean ≈ 1e6 (this test), the empirical spread is
// ~3e-10 relative — well inside the 1e-9 bound prescribed by the
// implementation plan. Pushing the mean to 1e9 would inflate the
// spread to ~6e-7 and force a looser tolerance (or a sort the plugin
// deliberately doesn't do for D14 perf reasons).
func TestMemoryGroupedAggregate_StdevWelfordParity(t *testing.T) {
	_, store, ctx := gsNewStore(t)
	// High-mean, low-variance: values clustered tightly around 1e6.
	values := []float64{
		1_000_000.0,
		1_000_000.1,
		1_000_000.2,
		999_999.9,
		999_999.8,
	}
	for i, v := range values {
		gsSave(t, ctx, store, fmt.Sprintf("e-%d", i), "available", map[string]any{"x": v})
	}

	// Reference: independent Welford recurrence on the original order.
	// Mathematically equal across permutations; numerically differs at
	// the ULP level, which the tolerance below absorbs.
	var n int64
	var mean, m2 float64
	for _, x := range values {
		n++
		delta := x - mean
		mean += delta / float64(n)
		delta2 := x - mean
		m2 += delta * delta2
	}
	wantStdev := math.Sqrt(m2 / float64(n-1))

	ga := store.(spi.GroupedAggregator)
	res, err := ga.GroupedAggregate(ctx, gsModel,
		[]spi.GroupExpr{{Kind: spi.GroupExprState}},
		spi.Filter{},
		spi.GroupedAggregationsOptions{
			MaxBuckets: 10,
			Aggregations: []spi.AggregateExpr{
				{Op: spi.AggStdev, Field: "x", Alias: "stdev_x"},
			},
		},
	)
	if err != nil {
		t.Fatalf("GroupedAggregate: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("buckets=%d, want 1", len(res))
	}
	gotStdev, ok := res[0].Aggregations["stdev_x"].(float64)
	if !ok {
		t.Fatalf("stdev_x missing or wrong type: %T", res[0].Aggregations["stdev_x"])
	}
	relErr := math.Abs(gotStdev-wantStdev) / wantStdev
	if relErr > 1e-9 {
		t.Fatalf("stdev parity: got %g, want %g (relErr=%g)", gotStdev, wantStdev, relErr)
	}
}

func TestMemoryGroupedAggregate_CardinalityExceeded(t *testing.T) {
	_, store, ctx := gsNewStore(t)
	// Three distinct states; MaxBuckets=2 must trip the ceiling.
	for i, s := range []string{"s1", "s2", "s3"} {
		gsSave(t, ctx, store, fmt.Sprintf("e-%d", i), s, map[string]any{})
	}
	ga := store.(spi.GroupedAggregator)
	_, err := ga.GroupedAggregate(ctx, gsModel,
		[]spi.GroupExpr{{Kind: spi.GroupExprState}},
		spi.Filter{},
		spi.GroupedAggregationsOptions{MaxBuckets: 2},
	)
	if !errors.Is(err, spi.ErrGroupCardinalityExceeded) {
		t.Fatalf("err = %v; want ErrGroupCardinalityExceeded", err)
	}
}

func TestMemoryGroupedAggregate_DataPathGrouping(t *testing.T) {
	_, store, ctx := gsNewStore(t)
	// Group by data path "$.region" with mixed scalar types.
	gsSave(t, ctx, store, "e-1", "available", map[string]any{"region": "us-east"})
	gsSave(t, ctx, store, "e-2", "available", map[string]any{"region": "us-east"})
	gsSave(t, ctx, store, "e-3", "available", map[string]any{"region": "eu-west"})
	// One entity missing the field — falls into the null bucket.
	gsSave(t, ctx, store, "e-4", "available", map[string]any{})

	ga := store.(spi.GroupedAggregator)
	res, err := ga.GroupedAggregate(ctx, gsModel,
		[]spi.GroupExpr{{Kind: spi.GroupExprDataPath, Path: "$.region"}},
		spi.Filter{},
		spi.GroupedAggregationsOptions{MaxBuckets: 10},
	)
	if err != nil {
		t.Fatalf("GroupedAggregate: %v", err)
	}
	counts := map[any]int64{}
	for _, b := range res {
		counts[b.GroupKey[0].Value] = b.Count
		if b.GroupKey[0].Path != "$.region" {
			t.Errorf("group key path = %q; want $.region", b.GroupKey[0].Path)
		}
	}
	if counts["us-east"] != 2 {
		t.Errorf("us-east count = %d; want 2", counts["us-east"])
	}
	if counts["eu-west"] != 1 {
		t.Errorf("eu-west count = %d; want 1", counts["eu-west"])
	}
	if counts[nil] != 1 {
		t.Errorf("null-bucket count = %d; want 1", counts[nil])
	}
}
