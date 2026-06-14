package sqlite_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

// gsModel is the canonical (entity, version) pair for grouped-stats tests.
var gsModel = spi.ModelRef{EntityName: "Item", ModelVersion: "1"}

// gsNewStore creates a fresh SQLite store factory + EntityStore for grouped-stats tests.
func gsNewStore(t *testing.T) (*sqlite.StoreFactory, spi.EntityStore, context.Context) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "grouped_stats_test.db")

	factory, err := sqlite.NewStoreFactoryForTest(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("create factory: %v", err)
	}
	t.Cleanup(func() { factory.Close() })

	ctx := testCtx("tenant-1")
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	return factory, store, ctx
}

// gsSave seeds an entity with explicit ID, state, and JSON data.
func gsSave(t *testing.T, ctx context.Context, store spi.EntityStore, id, state string, data map[string]any) {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	e := &spi.Entity{
		Meta: spi.EntityMeta{
			ID:       id,
			ModelRef: gsModel,
			State:    state,
		},
		Data: raw,
	}
	if _, err := store.Save(ctx, e); err != nil {
		t.Fatalf("save %s: %v", id, err)
	}
}

// ---------- Iterate ----------

func TestSqliteIterate_StreamsAllEntitiesForModel(t *testing.T) {
	_, store, ctx := gsNewStore(t)
	for i := 0; i < 5; i++ {
		gsSave(t, ctx, store, fmt.Sprintf("e-%d", i), "available", map[string]any{"x": i})
	}

	it, ok := store.(spi.Iterable)
	if !ok {
		t.Fatal("entityStore does not implement spi.Iterable")
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
		t.Fatalf("iter Err: %v", err)
	}
	if seen != 5 {
		t.Fatalf("got %d entities, want 5", seen)
	}
}

func TestSqliteIterate_FilterPushdown(t *testing.T) {
	_, store, ctx := gsNewStore(t)
	gsSave(t, ctx, store, "a", "available", map[string]any{"city": "Berlin"})
	gsSave(t, ctx, store, "b", "available", map[string]any{"city": "Munich"})
	gsSave(t, ctx, store, "c", "available", map[string]any{"city": "Berlin"})

	it := store.(spi.Iterable)
	filter := spi.Filter{
		Op:     spi.FilterEq,
		Source: spi.SourceData,
		Path:   "city",
		Value:  "Berlin",
	}
	iter, err := it.Iterate(ctx, gsModel, filter, spi.IterateOptions{})
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	defer iter.Close()

	var seen int
	for iter.Next() {
		seen++
	}
	if err := iter.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	if seen != 2 {
		t.Fatalf("got %d, want 2", seen)
	}
}

func TestSqliteIterate_ResidualApplied(t *testing.T) {
	_, store, ctx := gsNewStore(t)
	gsSave(t, ctx, store, "a", "available", map[string]any{"city": "Berlin", "tag": "x"})
	gsSave(t, ctx, store, "b", "available", map[string]any{"city": "Berlin", "tag": "y"})

	it := store.(spi.Iterable)
	// MatchesRegex is non-pushable in sqlite planner — forces residual evaluation.
	filter := spi.Filter{
		Op:       spi.FilterAnd,
		Children: []spi.Filter{
			{Op: spi.FilterEq, Source: spi.SourceData, Path: "city", Value: "Berlin"},
			{Op: spi.FilterMatchesRegex, Source: spi.SourceData, Path: "tag", Value: "^x$"},
		},
	}
	iter, err := it.Iterate(ctx, gsModel, filter, spi.IterateOptions{})
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	defer iter.Close()

	var seen int
	for iter.Next() {
		seen++
	}
	if err := iter.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	if seen != 1 {
		t.Fatalf("got %d, want 1 (only tag=x matches regex residual)", seen)
	}
}

func TestSqliteIterate_CtxCancellation(t *testing.T) {
	_, store, ctx := gsNewStore(t)
	for i := 0; i < 10; i++ {
		gsSave(t, ctx, store, fmt.Sprintf("e-%d", i), "available", map[string]any{"x": i})
	}

	cancelCtx, cancel := context.WithCancel(ctx)
	it := store.(spi.Iterable)
	iter, err := it.Iterate(cancelCtx, gsModel, spi.Filter{}, spi.IterateOptions{})
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	defer iter.Close()

	// Read one then cancel.
	if !iter.Next() {
		t.Fatal("expected first Next() to succeed")
	}
	cancel()

	// Drain — subsequent Next() should stop and Err() expose ctx err.
	for iter.Next() {
	}
	if err := iter.Err(); err == nil {
		t.Fatal("expected sticky Err() after cancel, got nil")
	}
}

func TestSqliteIterate_CloseIdempotent(t *testing.T) {
	_, store, ctx := gsNewStore(t)
	gsSave(t, ctx, store, "a", "available", map[string]any{})

	it := store.(spi.Iterable)
	iter, err := it.Iterate(ctx, gsModel, spi.Filter{}, spi.IterateOptions{})
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	if err := iter.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := iter.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// ---------- GroupedAggregate ----------

func TestSqliteGroupedAggregate_PushesCountByState(t *testing.T) {
	_, store, ctx := gsNewStore(t)
	for i := 0; i < 5; i++ {
		gsSave(t, ctx, store, fmt.Sprintf("avail-%d", i), "available", map[string]any{"x": i})
	}
	for i := 0; i < 2; i++ {
		gsSave(t, ctx, store, fmt.Sprintf("alloc-%d", i), "allocated", map[string]any{"x": i})
	}

	ga, ok := store.(spi.GroupedAggregator)
	if !ok {
		t.Fatal("entityStore does not implement spi.GroupedAggregator")
	}
	res, err := ga.GroupedAggregate(ctx, gsModel,
		[]spi.GroupExpr{{Kind: spi.GroupExprState}},
		spi.Filter{},
		spi.GroupedAggregationsOptions{MaxBuckets: 10},
	)
	if err != nil {
		t.Fatalf("GroupedAggregate: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("got %d buckets, want 2", len(res))
	}
	counts := map[string]int64{}
	for _, b := range res {
		if len(b.GroupKey) != 1 {
			t.Fatalf("bucket has %d group-key entries, want 1", len(b.GroupKey))
		}
		k, _ := b.GroupKey[0].Value.(string)
		counts[k] = b.Count
	}
	if counts["available"] != 5 {
		t.Errorf("available count = %d, want 5", counts["available"])
	}
	if counts["allocated"] != 2 {
		t.Errorf("allocated count = %d, want 2", counts["allocated"])
	}
}

func TestSqliteGroupedAggregate_DeclinesStdev(t *testing.T) {
	_, store, ctx := gsNewStore(t)
	gsSave(t, ctx, store, "a", "available", map[string]any{"price": 1.0})

	ga := store.(spi.GroupedAggregator)
	_, err := ga.GroupedAggregate(ctx, gsModel,
		[]spi.GroupExpr{{Kind: spi.GroupExprState}},
		spi.Filter{},
		spi.GroupedAggregationsOptions{
			MaxBuckets: 10,
			Aggregations: []spi.AggregateExpr{
				{Op: spi.AggStdev, Field: "price", Alias: "stdev_price"},
			},
		},
	)
	if !errors.Is(err, spi.ErrAggregationNotPushdownable) {
		t.Fatalf("got %v, want ErrAggregationNotPushdownable", err)
	}
}

func TestSqliteGroupedAggregate_DeclinesOnResidualFilter(t *testing.T) {
	_, store, ctx := gsNewStore(t)
	gsSave(t, ctx, store, "a", "available", map[string]any{"tag": "x"})

	ga := store.(spi.GroupedAggregator)
	// MatchesRegex is not pushable → residual exists.
	_, err := ga.GroupedAggregate(ctx, gsModel,
		[]spi.GroupExpr{{Kind: spi.GroupExprState}},
		spi.Filter{Op: spi.FilterMatchesRegex, Source: spi.SourceData, Path: "tag", Value: "^x$"},
		spi.GroupedAggregationsOptions{MaxBuckets: 10},
	)
	if !errors.Is(err, spi.ErrAggregationNotPushdownable) {
		t.Fatalf("got %v, want ErrAggregationNotPushdownable", err)
	}
}

func TestSqliteGroupedAggregate_DeclinesOnPointInTime(t *testing.T) {
	_, store, ctx := gsNewStore(t)
	gsSave(t, ctx, store, "a", "available", map[string]any{"x": 1})

	pit := time.Now()
	ga := store.(spi.GroupedAggregator)
	_, err := ga.GroupedAggregate(ctx, gsModel,
		[]spi.GroupExpr{{Kind: spi.GroupExprState}},
		spi.Filter{},
		spi.GroupedAggregationsOptions{MaxBuckets: 10, PointInTime: &pit},
	)
	if !errors.Is(err, spi.ErrAggregationNotPushdownable) {
		t.Fatalf("got %v, want ErrAggregationNotPushdownable", err)
	}
}

func TestSqliteGroupedAggregate_Sum(t *testing.T) {
	_, store, ctx := gsNewStore(t)
	gsSave(t, ctx, store, "a", "available", map[string]any{"price": 10.0})
	gsSave(t, ctx, store, "b", "available", map[string]any{"price": 20.0})
	gsSave(t, ctx, store, "c", "allocated", map[string]any{"price": 5.0})

	ga := store.(spi.GroupedAggregator)
	res, err := ga.GroupedAggregate(ctx, gsModel,
		[]spi.GroupExpr{{Kind: spi.GroupExprState}},
		spi.Filter{},
		spi.GroupedAggregationsOptions{
			MaxBuckets: 10,
			Aggregations: []spi.AggregateExpr{
				{Op: spi.AggSum, Field: "price", Alias: "sum_price"},
			},
		},
	)
	if err != nil {
		t.Fatalf("GroupedAggregate: %v", err)
	}
	sums := map[string]float64{}
	for _, b := range res {
		k, _ := b.GroupKey[0].Value.(string)
		v, _ := b.Aggregations["sum_price"].(float64)
		sums[k] = v
	}
	if sums["available"] != 30.0 {
		t.Errorf("sum available = %v, want 30", sums["available"])
	}
	if sums["allocated"] != 5.0 {
		t.Errorf("sum allocated = %v, want 5", sums["allocated"])
	}
}

func TestSqliteGroupedAggregate_CardinalityExceeded(t *testing.T) {
	_, store, ctx := gsNewStore(t)
	for i := 0; i < 5; i++ {
		gsSave(t, ctx, store, fmt.Sprintf("e-%d", i), fmt.Sprintf("state-%d", i), map[string]any{})
	}

	ga := store.(spi.GroupedAggregator)
	_, err := ga.GroupedAggregate(ctx, gsModel,
		[]spi.GroupExpr{{Kind: spi.GroupExprState}},
		spi.Filter{},
		spi.GroupedAggregationsOptions{MaxBuckets: 3},
	)
	if !errors.Is(err, spi.ErrGroupCardinalityExceeded) {
		t.Fatalf("got %v, want ErrGroupCardinalityExceeded", err)
	}
}

func TestSqliteGroupedAggregate_DataPathGroupBy(t *testing.T) {
	_, store, ctx := gsNewStore(t)
	gsSave(t, ctx, store, "a", "available", map[string]any{"variantId": "v1"})
	gsSave(t, ctx, store, "b", "available", map[string]any{"variantId": "v1"})
	gsSave(t, ctx, store, "c", "available", map[string]any{"variantId": "v2"})

	ga := store.(spi.GroupedAggregator)
	res, err := ga.GroupedAggregate(ctx, gsModel,
		[]spi.GroupExpr{{Kind: spi.GroupExprDataPath, Path: "variantId"}},
		spi.Filter{},
		spi.GroupedAggregationsOptions{MaxBuckets: 10},
	)
	if err != nil {
		t.Fatalf("GroupedAggregate: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("got %d buckets, want 2", len(res))
	}
	counts := map[string]int64{}
	for _, b := range res {
		k, _ := b.GroupKey[0].Value.(string)
		counts[k] = b.Count
	}
	if counts["v1"] != 2 || counts["v2"] != 1 {
		t.Errorf("counts = %v, want v1=2 v2=1", counts)
	}
}

func TestSqliteGroupedAggregate_MissingStateBucketsAsNil(t *testing.T) {
	_, store, ctx := gsNewStore(t)
	gsSave(t, ctx, store, "a", "", map[string]any{"x": 1})
	gsSave(t, ctx, store, "b", "available", map[string]any{"x": 2})

	ga := store.(spi.GroupedAggregator)
	res, err := ga.GroupedAggregate(ctx, gsModel,
		[]spi.GroupExpr{{Kind: spi.GroupExprState}},
		spi.Filter{},
		spi.GroupedAggregationsOptions{MaxBuckets: 10},
	)
	if err != nil {
		t.Fatalf("GroupedAggregate: %v", err)
	}
	// Sort by key for determinism (nil first).
	sort.Slice(res, func(i, j int) bool {
		ai, aj := res[i].GroupKey[0].Value, res[j].GroupKey[0].Value
		if ai == nil {
			return true
		}
		if aj == nil {
			return false
		}
		return fmt.Sprint(ai) < fmt.Sprint(aj)
	})
	if len(res) != 2 {
		t.Fatalf("got %d buckets, want 2", len(res))
	}
	if res[0].GroupKey[0].Value != nil {
		t.Errorf("missing state bucket value = %v (%T), want nil", res[0].GroupKey[0].Value, res[0].GroupKey[0].Value)
	}
}

