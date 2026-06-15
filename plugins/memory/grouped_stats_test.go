package memory_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

// TestMemoryGroupedStats_ConcurrentReadersDoNotStarveWriters pins the
// snapshot-then-iterate contract from plugins/memory/grouped_stats.go:
// Iterate / GroupedAggregate take entityMu.RLock only during the snapshot
// capture phase, then release before iterating. Writers (Save) acquire
// entityMu.Lock and must make progress even while readers are mid-iterate.
//
// The bound on worst-case Save (500ms) is a sanity check, not an SLO —
// if a regression makes Save block for the full reader iteration phase
// (e.g. the read lock is held across the iterate loop instead of just
// the snapshot build), the worst-case Save explodes to multi-second
// values. If CI flakes at 500ms, bump to a still-meaningful value
// (e.g. 2000ms) rather than removing the assertion.
func TestMemoryGroupedStats_ConcurrentReadersDoNotStarveWriters(t *testing.T) {
	if testing.Short() {
		t.Skip("3s concurrency stress test; -short skips")
	}
	_, store, ctx := gsNewStore(t)

	// Seed 1000 baseline entities so snapshots have work to do.
	for i := 0; i < 1000; i++ {
		gsSave(t, ctx, store, fmt.Sprintf("seed-%d", i), "available", map[string]any{
			"variantId": fmt.Sprintf("v%d", i%50),
		})
	}

	var (
		readsDone   atomic.Int64
		writesDone  atomic.Int64
		writeWallNs atomic.Int64 // cumulative time spent in Save
		maxWriteNs  atomic.Int64 // worst-case Save wall-clock
		stop        atomic.Bool
		wg          sync.WaitGroup
	)

	// 8 concurrent reader goroutines doing GroupedAggregate.
	for r := 0; r < 8; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ga := store.(spi.GroupedAggregator)
			for !stop.Load() {
				_, err := ga.GroupedAggregate(ctx, gsModel,
					[]spi.GroupExpr{{Kind: spi.GroupExprDataPath, Path: "variantId"}},
					spi.Filter{},
					spi.GroupedAggregationsOptions{MaxBuckets: 10000},
				)
				if err != nil {
					t.Errorf("reader: %v", err)
					return
				}
				readsDone.Add(1)
			}
		}()
	}

	// 4 concurrent writer goroutines doing Save. Inline Save to avoid the
	// gsSave helper's t.Fatalf — t.Fatalf from a non-test goroutine is
	// documented-unsafe.
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := 0; !stop.Load(); i++ {
				id := fmt.Sprintf("w%d-i%d", workerID, i)
				// Bounded variantId domain (50 buckets) so the reader's
				// GroupedAggregate stays under MaxBuckets — we're measuring
				// reader/writer contention, not cardinality enforcement.
				data, err := json.Marshal(map[string]any{
					"variantId": fmt.Sprintf("v%d", i%50),
				})
				if err != nil {
					t.Errorf("writer marshal: %v", err)
					return
				}
				e := &spi.Entity{
					Meta: spi.EntityMeta{
						ID:       id,
						TenantID: "t1",
						ModelRef: gsModel,
						State:    "available",
					},
					Data: data,
				}
				start := time.Now()
				if _, err := store.Save(ctx, e); err != nil {
					t.Errorf("writer save: %v", err)
					return
				}
				elapsed := time.Since(start).Nanoseconds()
				writeWallNs.Add(elapsed)
				for {
					cur := maxWriteNs.Load()
					if elapsed <= cur || maxWriteNs.CompareAndSwap(cur, elapsed) {
						break
					}
				}
				writesDone.Add(1)
			}
		}(w)
	}

	time.Sleep(3 * time.Second)
	stop.Store(true)
	wg.Wait()

	reads := readsDone.Load()
	writes := writesDone.Load()
	if reads == 0 {
		t.Fatalf("no reads completed — readers may be blocked")
	}
	if writes == 0 {
		t.Fatalf("no writes completed — writers were starved by readers")
	}

	avgWriteUs := float64(writeWallNs.Load()) / float64(writes) / 1000.0
	maxWriteMs := float64(maxWriteNs.Load()) / 1e6

	t.Logf("reads=%d writes=%d avg_write=%.1fus max_write=%.1fms", reads, writes, avgWriteUs, maxWriteMs)

	// Sanity bound — see godoc above.
	if maxWriteMs > 500 {
		t.Errorf("worst-case write took %.1fms — writer may be blocked by reader iteration phase (snapshot-then-iterate contract broken)", maxWriteMs)
	}
}

// TestMemoryGroupedAggregate_NearCardinalityCeiling exercises bucket counts
// just below the default CYODA_STATS_GROUP_MAX (10000). 50k entities × 8k
// distinct variantIds → 8000 buckets, well under the 10000 ceiling. The
// existing CardinalityExceeded test pins the trip behaviour at 3 states;
// this one demonstrates correctness at SIOMS-scale just under the limit.
func TestMemoryGroupedAggregate_NearCardinalityCeiling(t *testing.T) {
	if testing.Short() {
		t.Skip("scale test; -short skips")
	}
	_, store, ctx := gsNewStore(t)

	const (
		entities = 50_000
		buckets  = 8_000
	)
	for i := 0; i < entities; i++ {
		gsSave(t, ctx, store, fmt.Sprintf("e-%d", i), "available", map[string]any{
			"variantId": fmt.Sprintf("v%d", i%buckets),
			"price":     float64(100 + i%100),
		})
	}

	ga := store.(spi.GroupedAggregator)
	res, err := ga.GroupedAggregate(ctx, gsModel,
		[]spi.GroupExpr{{Kind: spi.GroupExprDataPath, Path: "variantId"}},
		spi.Filter{},
		spi.GroupedAggregationsOptions{
			MaxBuckets: 10_000,
			Aggregations: []spi.AggregateExpr{
				{Op: spi.AggSum, Field: "price", Alias: "totalPrice"},
				{Op: spi.AggAvg, Field: "price", Alias: "avgPrice"},
			},
		},
	)
	if err != nil {
		t.Fatalf("GroupedAggregate: %v", err)
	}
	if len(res) != buckets {
		t.Fatalf("buckets=%d want %d", len(res), buckets)
	}

	// Distribution is exact: entities % buckets == 0 here, so every bucket
	// has exactly entities/buckets entries. Tolerance ±1 to absorb any
	// future-skew shifts in the modulo distribution.
	expectedPerBucket := entities / buckets
	var totalCount int64
	for _, b := range res {
		totalCount += b.Count
		if b.Count < int64(expectedPerBucket) || b.Count > int64(expectedPerBucket+1) {
			t.Fatalf("bucket %v count=%d not in [%d, %d]",
				b.GroupKey, b.Count, expectedPerBucket, expectedPerBucket+1)
		}
	}
	if totalCount != int64(entities) {
		t.Fatalf("totalCount=%d want %d (some entities lost)", totalCount, entities)
	}
	t.Logf("entities=%d buckets=%d totalCount=%d", entities, len(res), totalCount)
}

// TestMemoryGroupedAggregate_CardinalityCeilingExceeded_AtScale verifies
// the cardinality ceiling fires correctly at scale: 50k entities × 20k
// distinct values, MaxBuckets=10000 must return ErrGroupCardinalityExceeded.
// Complements the existing 3-state CardinalityExceeded test by exercising
// the bucket map at the boundary under realistic input volume.
func TestMemoryGroupedAggregate_CardinalityCeilingExceeded_AtScale(t *testing.T) {
	if testing.Short() {
		t.Skip("scale test; -short skips")
	}
	_, store, ctx := gsNewStore(t)

	for i := 0; i < 50_000; i++ {
		gsSave(t, ctx, store, fmt.Sprintf("e-%d", i), "available", map[string]any{
			"variantId": fmt.Sprintf("v%d", i%20_000),
		})
	}

	ga := store.(spi.GroupedAggregator)
	_, err := ga.GroupedAggregate(ctx, gsModel,
		[]spi.GroupExpr{{Kind: spi.GroupExprDataPath, Path: "variantId"}},
		spi.Filter{},
		spi.GroupedAggregationsOptions{MaxBuckets: 10_000},
	)
	if !errors.Is(err, spi.ErrGroupCardinalityExceeded) {
		t.Fatalf("err=%v, want ErrGroupCardinalityExceeded", err)
	}
}
