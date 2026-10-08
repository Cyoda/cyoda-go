package sqlite_test

import (
	"encoding/json"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// An update after the instant does not move the aggregate at the instant: the
// bucket uses the older version of the entity, and agrees with what Iterate at
// the same instant yields.
func TestSqliteGroupedAggregate_PointInTimeUsesOlderVersion(t *testing.T) {
	_, store, ctx := gsNewStore(t)
	gsSave(t, ctx, store, "a", "available", map[string]any{"price": 10.0})
	gsSave(t, ctx, store, "b", "available", map[string]any{"price": 20.0})
	at := time.Now()
	time.Sleep(10 * time.Millisecond)
	gsSave(t, ctx, store, "a", "allocated", map[string]any{"price": 99.0})

	ga := store.(spi.GroupedAggregator)
	res, err := ga.GroupedAggregate(ctx, gsModel,
		[]spi.GroupExpr{{Kind: spi.GroupExprState}},
		spi.Filter{},
		spi.GroupedAggregationsOptions{
			MaxBuckets: 10, PointInTime: &at,
			Aggregations: []spi.AggregateExpr{{Op: spi.AggSum, Field: "price", Alias: "sum_price"}},
		},
	)
	if err != nil {
		t.Fatalf("GroupedAggregate: %v", err)
	}
	if len(res) != 1 || res[0].Count != 2 || res[0].Aggregations["sum_price"] != 30.0 {
		t.Fatalf("buckets at the instant = %+v, want one 'available' bucket of 2 summing 30", res)
	}

	iter, err := store.Iterate(ctx, gsModel, spi.Filter{}, spi.IterateOptions{PointInTime: &at})
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	defer iter.Close()
	var n int64
	var sum float64
	for iter.Next() {
		var d struct{ Price float64 }
		if err := json.Unmarshal(iter.Entity().Data, &d); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		n++
		sum += d.Price
	}
	if err := iter.Err(); err != nil {
		t.Fatalf("iter: %v", err)
	}
	if n != res[0].Count || sum != res[0].Aggregations["sum_price"] {
		t.Errorf("Iterate at the instant = %d/%v, grouped = %d/%v", n, sum, res[0].Count, res[0].Aggregations["sum_price"])
	}
}
