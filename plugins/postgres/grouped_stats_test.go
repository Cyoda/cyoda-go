package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

// gsModel is the canonical (entity, version) pair for grouped-stats tests.
var gsModel = spi.ModelRef{EntityName: "Item", ModelVersion: "1"}

// gsNewStore creates a fresh postgres-backed EntityStore for grouped-stats tests.
// Skips if CYODA_TEST_DB_URL is not set (Docker required).
func gsNewStore(t *testing.T) (*postgres.StoreFactory, spi.EntityStore, context.Context) {
	t.Helper()
	factory := setupEntityTest(t)
	ctx := ctxWithTenant("gs-tenant")
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

func TestPostgresIterate_StreamsAllEntitiesForModel(t *testing.T) {
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

func TestPostgresIterate_FilterPushdown(t *testing.T) {
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

func TestPostgresIterate_ResidualApplied(t *testing.T) {
	_, store, ctx := gsNewStore(t)
	gsSave(t, ctx, store, "a", "available", map[string]any{"city": "Berlin", "tag": "x"})
	gsSave(t, ctx, store, "b", "available", map[string]any{"city": "Berlin", "tag": "y"})

	it := store.(spi.Iterable)
	// MatchesRegex is non-pushable in postgres planner — forces residual evaluation.
	filter := spi.Filter{
		Op: spi.FilterAnd,
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

func TestPostgresIterate_PointInTime(t *testing.T) {
	_, store, ctx := gsNewStore(t)

	// Seed two entities. We'll delete one after a snapshot instant; PIT
	// before the delete must show both, PIT after must show one.
	gsSave(t, ctx, store, "a", "available", map[string]any{"x": 1})
	gsSave(t, ctx, store, "b", "available", map[string]any{"x": 2})

	// Snapshot before the delete.
	beforeDelete := time.Now()
	time.Sleep(10 * time.Millisecond)

	if err := store.Delete(ctx, "b"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	time.Sleep(10 * time.Millisecond)
	afterDelete := time.Now()

	it := store.(spi.Iterable)

	// PIT before delete → 2 entities (deletion-marker version not yet present).
	iterBefore, err := it.Iterate(ctx, gsModel, spi.Filter{}, spi.IterateOptions{PointInTime: &beforeDelete})
	if err != nil {
		t.Fatalf("Iterate before: %v", err)
	}
	var seenBefore int
	for iterBefore.Next() {
		seenBefore++
	}
	if err := iterBefore.Err(); err != nil {
		t.Fatalf("iterBefore Err: %v", err)
	}
	iterBefore.Close()
	if seenBefore != 2 {
		t.Errorf("PIT before delete: got %d, want 2", seenBefore)
	}

	// PIT after delete → 1 entity (deletion-marker version excluded by predicate).
	iterAfter, err := it.Iterate(ctx, gsModel, spi.Filter{}, spi.IterateOptions{PointInTime: &afterDelete})
	if err != nil {
		t.Fatalf("Iterate after: %v", err)
	}
	var seenAfter int
	for iterAfter.Next() {
		seenAfter++
	}
	if err := iterAfter.Err(); err != nil {
		t.Fatalf("iterAfter Err: %v", err)
	}
	iterAfter.Close()
	if seenAfter != 1 {
		t.Errorf("PIT after delete: got %d, want 1 (deletion-marker excluded)", seenAfter)
	}
}

func TestPostgresIterate_CtxCancellation(t *testing.T) {
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

func TestPostgresIterate_CloseIdempotent(t *testing.T) {
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

func TestPostgresGroupedAggregate_PushesCountByState(t *testing.T) {
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

func TestPostgresGroupedAggregate_Sum(t *testing.T) {
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

func TestPostgresGroupedAggregate_StdevPushed(t *testing.T) {
	_, store, ctx := gsNewStore(t)
	// Three samples in one state — STDDEV_SAMP defined; one sample in another — STDDEV_SAMP undefined (NULL).
	gsSave(t, ctx, store, "a", "available", map[string]any{"price": 2.0})
	gsSave(t, ctx, store, "b", "available", map[string]any{"price": 4.0})
	gsSave(t, ctx, store, "c", "available", map[string]any{"price": 4.0})
	gsSave(t, ctx, store, "d", "allocated", map[string]any{"price": 7.0})

	ga := store.(spi.GroupedAggregator)
	res, err := ga.GroupedAggregate(ctx, gsModel,
		[]spi.GroupExpr{{Kind: spi.GroupExprState}},
		spi.Filter{},
		spi.GroupedAggregationsOptions{
			MaxBuckets: 10,
			Aggregations: []spi.AggregateExpr{
				{Op: spi.AggStdev, Field: "price", Alias: "stdev_price"},
			},
		},
	)
	if err != nil {
		t.Fatalf("GroupedAggregate: %v", err)
	}
	stdevs := map[string]any{}
	for _, b := range res {
		k, _ := b.GroupKey[0].Value.(string)
		stdevs[k] = b.Aggregations["stdev_price"]
	}
	// Sample stdev of {2, 4, 4} = sqrt(((2-10/3)^2 + (4-10/3)^2 + (4-10/3)^2)/2) ≈ 1.1547
	got, ok := stdevs["available"].(float64)
	if !ok {
		t.Fatalf("available stdev not a float64: %T = %v", stdevs["available"], stdevs["available"])
	}
	want := 1.1547005383792515
	if got < want-1e-9 || got > want+1e-9 {
		t.Errorf("available stdev = %v, want ~%v", got, want)
	}
	// n=1 → STDDEV_SAMP NULL → mapped to nil.
	if v := stdevs["allocated"]; v != nil {
		t.Errorf("allocated stdev = %v, want nil (n=1)", v)
	}
}

func TestPostgresGroupedAggregate_DeclinesOnResidualFilter(t *testing.T) {
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

func TestPostgresGroupedAggregate_DeclinesOnPointInTime(t *testing.T) {
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

func TestPostgresGroupedAggregate_CardinalityExceeded(t *testing.T) {
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

func TestPostgresGroupedAggregate_NonNumericValuesSilentlySkipped(t *testing.T) {
	_, store, ctx := gsNewStore(t)
	gsSave(t, ctx, store, "a", "available", map[string]any{"price": 10.0})
	gsSave(t, ctx, store, "b", "available", map[string]any{"price": "abc"}) // non-numeric → cyoda_try_float8 returns NULL
	gsSave(t, ctx, store, "c", "available", map[string]any{"price": 20.0})

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
	if len(res) != 1 {
		t.Fatalf("got %d buckets, want 1", len(res))
	}
	v, _ := res[0].Aggregations["sum_price"].(float64)
	if v != 30.0 {
		t.Errorf("sum = %v, want 30 (non-numeric silently skipped)", v)
	}
	// Count must reflect all rows including the non-numeric one.
	if res[0].Count != 3 {
		t.Errorf("count = %d, want 3 (non-numeric still counted)", res[0].Count)
	}
}

func TestPostgresGroupedAggregate_DataPathGroupBy(t *testing.T) {
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

func TestPostgresGroupedAggregate_MissingStateBucketsAsNil(t *testing.T) {
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
	// Sort by key for determinism. The empty-state row will be either nil or "".
	// Save with empty state stores "" in the JSONB doc — doc->'_meta'->>'state' returns "".
	sort.Slice(res, func(i, j int) bool {
		return fmt.Sprint(res[i].GroupKey[0].Value) < fmt.Sprint(res[j].GroupKey[0].Value)
	})
	if len(res) != 2 {
		t.Fatalf("got %d buckets, want 2: %+v", len(res), res)
	}
}

func TestPostgresGroupedAggregate_StateIdxUsed(t *testing.T) {
	factory, store, ctx := gsNewStore(t)
	for i := 0; i < 50; i++ {
		state := "available"
		if i%5 == 0 {
			state = "allocated"
		}
		gsSave(t, ctx, store, fmt.Sprintf("e-%d", i), state, map[string]any{"x": i})
	}

	// Use ANALYZE + EXPLAIN to verify the planner picks entities_state_idx.
	// We hit the raw pool directly (not via the store) since EXPLAIN is a
	// diagnostic, not a typed store method.
	pool := postgres.PoolForTest(factory)
	if pool == nil {
		t.Skip("PoolForTest not available")
	}
	if _, err := pool.Exec(ctx, "ANALYZE entities"); err != nil {
		t.Fatalf("ANALYZE: %v", err)
	}
	// Force the planner to prefer the index over a seq scan so we can verify
	// the index is selectable for the state predicate. (On a 50-row table
	// the planner would otherwise pick Seq Scan because the cost crossover
	// hasn't been reached yet.) enable_seqscan=off is a session GUC; we
	// acquire a dedicated connection to scope it cleanly.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SET enable_seqscan = off"); err != nil {
		t.Fatalf("SET enable_seqscan: %v", err)
	}
	rows, err := conn.Query(ctx, `
		EXPLAIN (FORMAT JSON)
		SELECT count(*) FROM entities
		WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3
		  AND NOT deleted
		  AND (doc->'_meta'->>'state') = 'allocated'`,
		"gs-tenant", gsModel.EntityName, gsModel.ModelVersion)
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	defer rows.Close()
	var plan string
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			t.Fatalf("scan: %v", err)
		}
		plan += string(b)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	// With seqscan disabled, the planner must pick an index; entities_state_idx
	// is the only one matching all of (tenant_id, model_name, model_version,
	// state) with the partial NOT deleted predicate, so it must appear in the
	// plan. This confirms the index is usable for the state predicate.
	if !strings.Contains(plan, "entities_state_idx") {
		t.Errorf("EXPLAIN plan did not select entities_state_idx with seqscan disabled. Plan: %s", plan)
	}
}

// ---------- Unit tests (no DB) ----------

func TestPostgresGroupedStats_PathValidation(t *testing.T) {
	// Inputs that MUST be rejected by the SQL-boundary validator. These are
	// the characters that could break out of the single-quoted JSONB key
	// literal, plus empty / leading-trailing-dot / double-dot grammar
	// violations.
	bad := []string{
		"",
		".",
		"foo.",
		".bar",
		"a..b",
		"a b",        // whitespace
		"a'b",        // single quote (literal terminator)
		"a\"b",       // double quote
		"a;b",        // semicolon
		"a[0]",       // brackets
		"foo\x00bar", // NUL byte
	}
	for _, p := range bad {
		if err := postgres.ValidateJSONPathForTest(p); err == nil {
			t.Errorf("ValidateJSONPathForTest(%q) = nil, want error", p)
		}
	}

	// Inputs that should be accepted. Hyphens are deliberately permitted —
	// '--' is harmless INSIDE a single-quoted literal (SQL comments only
	// have meaning outside string context).
	good := []string{
		"variantId",
		"a.b.c",
		"foo_bar",
		"foo-bar",
		"a--b", // hyphens allowed; safe inside the quoted literal
		"a123.b456",
	}
	for _, p := range good {
		if err := postgres.ValidateJSONPathForTest(p); err != nil {
			t.Errorf("ValidateJSONPathForTest(%q) = %v, want nil", p, err)
		}
	}
}

func TestPostgresGroupedStats_DeclinesInvalidGroupPath(t *testing.T) {
	_, store, ctx := gsNewStore(t)
	gsSave(t, ctx, store, "a", "available", map[string]any{"x": 1})

	ga := store.(spi.GroupedAggregator)
	// Inject a malformed path that the SQL boundary must reject (belt-and-braces).
	_, err := ga.GroupedAggregate(ctx, gsModel,
		[]spi.GroupExpr{{Kind: spi.GroupExprDataPath, Path: "bad'path"}},
		spi.Filter{},
		spi.GroupedAggregationsOptions{MaxBuckets: 10},
	)
	if err == nil {
		t.Fatalf("expected error for malformed group path, got nil")
	}
}
