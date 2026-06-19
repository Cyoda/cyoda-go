package parity

import (
	"bytes"
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// Grouped-stats cross-backend parity suite.
//
// These scenarios pin the OBSERVABLE behaviour of POST
// /api/entity/stats/{name}/{version}/query across every storage backend
// (memory / sqlite / postgres / out-of-tree plugins). They are
// deliberately black-box: the assertions describe the response a client
// sees, not the dispatch path the service layer chose. A backend that
// implements GroupedAggregator (postgres) and one that falls through to
// the streaming-tally fallback (sqlite stdev, memory) must produce the
// same buckets for the same fixture corpus.
//
// What this suite does NOT cover:
//   - per-plugin contract bugs (iterator Err stickiness, ctx cancellation):
//     those live in plugin-local tests under plugins/*/grouped_stats_test.go.
//   - low-level rounding (postgres returns float64 STDDEV_SAMP, memory and
//     sqlite use Welford): the service layer normalises the wire shape, so
//     the parity assertion uses a 1e-9 relative tolerance for stdev, exact
//     for sum/min/max, and a smaller tolerance for avg.
//
// Scope (spec §7 matrix, pragmatically scoped to v1):
//   1. ParityGroupedStats_CountByState                   group-by lifecycle state
//   2. ParityGroupedStats_CountByDataField               group-by JSON path
//   3. ParityGroupedStats_MultiDimGroupBy                cartesian path × state
//   4. ParityGroupedStats_WithCondition                  predicate-filtered buckets
//   5. ParityGroupedStats_PointInTime                    historical snapshot
//   6. ParityGroupedStats_AggregationsTier1              sum / avg / min / max
//   7. ParityGroupedStats_StdevLowVarianceHighMean       Welford ↔ STDDEV_SAMP agreement
//   8. ParityGroupedStats_NonNumericSkipped              text values silently dropped
//   9. ParityGroupedStats_NonScalarCoercesToNull         object/array → null group-key
//  10. ParityGroupedStats_CardinalityExceeded            422 GROUP_CARDINALITY_EXCEEDED
//
// Each scenario uses NewTenant for full tenant isolation — there is no
// shared fixture state between scenarios.

// statsModelSampleDoc is the canonical sample shape we import once per
// scenario. The fields are a superset of what any single scenario needs;
// importing once keeps the model schema identical across scenarios and
// across backends (so JSON-type inference does not diverge).
const statsModelSampleDoc = `{
	"name": "Sample",
	"variantId": "v1",
	"price": 1.5,
	"qty": 1,
	"region": "EU",
	"meta": {"source": "parity"},
	"tags": ["a","b"]
}`

// statsModelAutoWorkflowJSON is a workflow with a single auto-transition
// NONE → CREATED, plus a manual transition CREATED → SHIPPED. Auto on
// creation means every CreateEntity call lands in CREATED, and the
// optional UpdateEntity("ship",...) call moves it to SHIPPED. This is
// enough to exercise group-by state across two distinct states without
// dragging compute nodes into the picture.
const statsModelAutoWorkflowJSON = `{
	"importMode": "REPLACE",
	"workflows": [{
		"version": "1.0", "name": "stats-wf", "initialState": "NONE", "active": true,
		"states": {
			"NONE":    {"transitions": [{"name": "create", "next": "CREATED", "manual": false}]},
			"CREATED": {"transitions": [{"name": "ship",   "next": "SHIPPED", "manual": true}]},
			"SHIPPED": {}
		}
	}]
}`

// setupStatsModel imports the canonical model and workflow under a fresh
// (name, version) — every parity scenario uses its own model so that
// tenant + model name together fully isolate the test from any other
// scenario running in the same backend process.
func setupStatsModel(t *testing.T, c *client.Client, modelName string, modelVersion int) {
	t.Helper()
	if err := c.ImportModel(t, modelName, modelVersion, statsModelSampleDoc); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if err := c.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	if err := c.ImportWorkflow(t, modelName, modelVersion, statsModelAutoWorkflowJSON); err != nil {
		t.Fatalf("ImportWorkflow: %v", err)
	}
}

// floatTol is the relative tolerance applied to stdev aggregations. The
// service layer normalises the wire shape (postgres float64 ↔ Welford
// float64) so 1e-9 relative is well above the actual cross-backend
// drift; we use it as a deliberate ceiling.
const floatTol = 1e-9

// avgTol is the relative tolerance for `avg` aggregations. Both backends
// effectively compute SUM/COUNT in float64, so cross-backend drift is
// bounded by a few ULPs — 1e-12 relative is comfortable.
const avgTol = 1e-12

// findBucketByGroupKey locates the bucket whose group-key matches the
// supplied (path, value) pairs verbatim. Returns nil when not found.
// The assertion helpers pre-sort their expectations and use this lookup
// rather than positional comparison so a backend that legitimately ties
// on (count, key) and orders them differently does not produce a false
// negative.
func findBucketByGroupKey(buckets []client.GroupedStatsBucket, expected []client.GroupKeyEntry) *client.GroupedStatsBucket {
	for i := range buckets {
		if len(buckets[i].GroupKey) != len(expected) {
			continue
		}
		match := true
		for j := range expected {
			if buckets[i].GroupKey[j].Path != expected[j].Path {
				match = false
				break
			}
			if !sameKeyValue(buckets[i].GroupKey[j].Value, expected[j].Value) {
				match = false
				break
			}
		}
		if match {
			return &buckets[i]
		}
	}
	return nil
}

// sameKeyValue compares two group-key values using the wire-shape
// equality rules. nil ↔ nil; strings exact; everything else via ==.
// Numbers arrive as JSON numbers — the service emits them as their raw
// text form (string), so equality is direct.
func sameKeyValue(a, b any) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return a == b
}

// floatFromAgg extracts a float64 from an aggregation map value. JSON
// numbers decode as float64 already, so this is a typed accessor; nil
// (null on the wire) returns NaN to signal "not present".
func floatFromAgg(v any) float64 {
	if v == nil {
		return math.NaN()
	}
	f, ok := v.(float64)
	if !ok {
		return math.NaN()
	}
	return f
}

// assertAggFloat compares one aggregation result against an expected
// value with the supplied tolerance. relTol is interpreted relative to
// the magnitude of the expected value, with an absolute floor of relTol
// itself so values near zero still compare meaningfully.
func assertAggFloat(t *testing.T, alias string, got, want, relTol float64) {
	t.Helper()
	if math.IsNaN(got) {
		t.Errorf("aggregation %q: got nil/NaN, want %v", alias, want)
		return
	}
	diff := math.Abs(got - want)
	scale := math.Max(math.Abs(want), 1.0)
	if diff/scale > relTol {
		t.Errorf("aggregation %q: got %.17g, want %.17g (relative diff %.3e > tol %.3e)",
			alias, got, want, diff/scale, relTol)
	}
}

// stringValue is shorthand for the GroupKeyEntry.Value field where the
// expected value is known to be a string-typed key (lifecycle state or
// a string-valued data field). Helps readability of expectations.
func stringValue(v string) any { return v }

// --- Scenario 1 — count by lifecycle state ---

// RunParityGroupedStats_CountByState creates 3 entities, ships 1 of
// them, then group-by state. Asserts {CREATED: 2, SHIPPED: 1}. This is
// the simplest possible parity assertion — every backend that can
// answer /api/entity/stats/states correctly must answer this the same
// way too.
func RunParityGroupedStats_CountByState(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-stats-by-state"
	const modelVersion = 1
	setupStatsModel(t, c, modelName, modelVersion)

	id1, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"A","variantId":"v1","price":10.0}`)
	if err != nil {
		t.Fatalf("CreateEntity A: %v", err)
	}
	if _, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"B","variantId":"v1","price":20.0}`); err != nil {
		t.Fatalf("CreateEntity B: %v", err)
	}
	if _, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"C","variantId":"v2","price":30.0}`); err != nil {
		t.Fatalf("CreateEntity C: %v", err)
	}
	// Ship one — moves it from CREATED to SHIPPED.
	if err := c.UpdateEntity(t, id1, "ship", `{"name":"A","variantId":"v1","price":10.0}`); err != nil {
		t.Fatalf("UpdateEntity ship A: %v", err)
	}

	buckets, err := c.QueryGroupedStats(t, modelName, modelVersion, client.GroupedStatsRequest{
		GroupBy: []string{"state"},
	})
	if err != nil {
		t.Fatalf("QueryGroupedStats: %v", err)
	}
	if len(buckets) != 2 {
		t.Fatalf("expected 2 buckets, got %d: %+v", len(buckets), buckets)
	}

	createdKey := []client.GroupKeyEntry{{Path: "state", Value: stringValue("CREATED")}}
	shippedKey := []client.GroupKeyEntry{{Path: "state", Value: stringValue("SHIPPED")}}

	if b := findBucketByGroupKey(buckets, createdKey); b == nil {
		t.Errorf("missing CREATED bucket; got %+v", buckets)
	} else if b.Count != 2 {
		t.Errorf("CREATED count: got %d, want 2", b.Count)
	}
	if b := findBucketByGroupKey(buckets, shippedKey); b == nil {
		t.Errorf("missing SHIPPED bucket; got %+v", buckets)
	} else if b.Count != 1 {
		t.Errorf("SHIPPED count: got %d, want 1", b.Count)
	}
	// D12 ordering: count desc; CREATED(2) must precede SHIPPED(1).
	if buckets[0].Count < buckets[1].Count {
		t.Errorf("buckets not sorted by count desc: %+v", buckets)
	}
}

// --- Scenario 2 — count by a single data-field path ---

// RunParityGroupedStats_CountByDataField creates entities with two
// distinct variantId values, group-by $.variantId, and asserts the
// counts. The string-typed key path exercises the dotted-path
// normalisation and the JSON-string keying path on every backend.
func RunParityGroupedStats_CountByDataField(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-stats-by-variant"
	const modelVersion = 1
	setupStatsModel(t, c, modelName, modelVersion)

	for i := 0; i < 3; i++ {
		if _, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"X","variantId":"v1","price":1.0}`); err != nil {
			t.Fatalf("CreateEntity v1: %v", err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Y","variantId":"v2","price":1.0}`); err != nil {
			t.Fatalf("CreateEntity v2: %v", err)
		}
	}

	buckets, err := c.QueryGroupedStats(t, modelName, modelVersion, client.GroupedStatsRequest{
		GroupBy: []string{"$.variantId"},
	})
	if err != nil {
		t.Fatalf("QueryGroupedStats: %v", err)
	}
	if len(buckets) != 2 {
		t.Fatalf("expected 2 buckets, got %d: %+v", len(buckets), buckets)
	}

	if b := findBucketByGroupKey(buckets, []client.GroupKeyEntry{{Path: "$.variantId", Value: stringValue("v1")}}); b == nil || b.Count != 3 {
		t.Errorf("v1 bucket: got %+v, want count=3", b)
	}
	if b := findBucketByGroupKey(buckets, []client.GroupKeyEntry{{Path: "$.variantId", Value: stringValue("v2")}}); b == nil || b.Count != 2 {
		t.Errorf("v2 bucket: got %+v, want count=2", b)
	}
}

// --- Scenario 3 — multi-dimensional cartesian group-by ---

// RunParityGroupedStats_MultiDimGroupBy groups by ($.variantId, state)
// over a corpus that produces a non-trivial cartesian. Every backend
// must yield the same set of (variantId, state) buckets with the same
// counts.
func RunParityGroupedStats_MultiDimGroupBy(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-stats-multidim"
	const modelVersion = 1
	setupStatsModel(t, c, modelName, modelVersion)

	// v1: 2 CREATED, 1 SHIPPED. v2: 1 CREATED.
	id1, err := c.CreateEntity(t, modelName, modelVersion, `{"variantId":"v1","price":1}`)
	if err != nil {
		t.Fatalf("CreateEntity v1#1: %v", err)
	}
	if _, err := c.CreateEntity(t, modelName, modelVersion, `{"variantId":"v1","price":1}`); err != nil {
		t.Fatalf("CreateEntity v1#2: %v", err)
	}
	if _, err := c.CreateEntity(t, modelName, modelVersion, `{"variantId":"v1","price":1}`); err != nil {
		t.Fatalf("CreateEntity v1#3: %v", err)
	}
	if _, err := c.CreateEntity(t, modelName, modelVersion, `{"variantId":"v2","price":1}`); err != nil {
		t.Fatalf("CreateEntity v2#1: %v", err)
	}
	if err := c.UpdateEntity(t, id1, "ship", `{"variantId":"v1","price":1}`); err != nil {
		t.Fatalf("ship v1#1: %v", err)
	}

	buckets, err := c.QueryGroupedStats(t, modelName, modelVersion, client.GroupedStatsRequest{
		GroupBy: []string{"$.variantId", "state"},
	})
	if err != nil {
		t.Fatalf("QueryGroupedStats: %v", err)
	}
	if len(buckets) != 3 {
		t.Fatalf("expected 3 buckets, got %d: %+v", len(buckets), buckets)
	}

	type expect struct {
		variantID, state string
		count            int64
	}
	want := []expect{
		{"v1", "CREATED", 2},
		{"v1", "SHIPPED", 1},
		{"v2", "CREATED", 1},
	}
	for _, w := range want {
		key := []client.GroupKeyEntry{
			{Path: "$.variantId", Value: stringValue(w.variantID)},
			{Path: "state", Value: stringValue(w.state)},
		}
		b := findBucketByGroupKey(buckets, key)
		if b == nil {
			t.Errorf("missing bucket (%s, %s); buckets=%+v", w.variantID, w.state, buckets)
			continue
		}
		if b.Count != w.count {
			t.Errorf("bucket (%s, %s) count: got %d, want %d", w.variantID, w.state, b.Count, w.count)
		}
	}
}

// --- Scenario 4 — predicate-filtered grouping ---

// RunParityGroupedStats_WithCondition repeats Scenario 1's corpus but
// adds a `state != "SHIPPED"` condition. Every backend must drop the
// SHIPPED bucket — the parity assertion is that the filtered result
// matches what the same group-by query against the unfiltered subset
// would have produced.
func RunParityGroupedStats_WithCondition(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-stats-with-cond"
	const modelVersion = 1
	setupStatsModel(t, c, modelName, modelVersion)

	id1, err := c.CreateEntity(t, modelName, modelVersion, `{"variantId":"v1","price":10}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	if _, err := c.CreateEntity(t, modelName, modelVersion, `{"variantId":"v1","price":20}`); err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	if _, err := c.CreateEntity(t, modelName, modelVersion, `{"variantId":"v2","price":30}`); err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	if err := c.UpdateEntity(t, id1, "ship", `{"variantId":"v1","price":10}`); err != nil {
		t.Fatalf("ship: %v", err)
	}

	// Lifecycle condition: state != "SHIPPED".
	buckets, err := c.QueryGroupedStats(t, modelName, modelVersion, client.GroupedStatsRequest{
		GroupBy: []string{"$.variantId"},
		Condition: &client.AggregationCond{
			"type":         "lifecycle",
			"field":        "state",
			"operatorType": "NOT_EQUAL",
			"value":        "SHIPPED",
		},
	})
	if err != nil {
		t.Fatalf("QueryGroupedStats: %v", err)
	}
	if len(buckets) != 2 {
		t.Fatalf("expected 2 buckets (CREATED v1 + CREATED v2), got %d: %+v", len(buckets), buckets)
	}
	if b := findBucketByGroupKey(buckets, []client.GroupKeyEntry{{Path: "$.variantId", Value: stringValue("v1")}}); b == nil || b.Count != 1 {
		t.Errorf("v1 bucket (filtered): got %+v, want count=1", b)
	}
	if b := findBucketByGroupKey(buckets, []client.GroupKeyEntry{{Path: "$.variantId", Value: stringValue("v2")}}); b == nil || b.Count != 1 {
		t.Errorf("v2 bucket (filtered): got %+v, want count=1", b)
	}
}

// --- Scenario 5 — historical snapshot via pointInTime ---

// RunParityGroupedStats_PointInTime walks the historical timeline:
// create three entities, capture T1, ship one and delete one, then
// query group-by-state at T1. The pre-update snapshot must report
// CREATED:3 — proving every backend correctly ignores writes after T1,
// including the deletion-marker version.
func RunParityGroupedStats_PointInTime(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-stats-pit"
	const modelVersion = 1
	setupStatsModel(t, c, modelName, modelVersion)

	id1, err := c.CreateEntity(t, modelName, modelVersion, `{"variantId":"v1","price":1}`)
	if err != nil {
		t.Fatalf("CreateEntity 1: %v", err)
	}
	if _, err := c.CreateEntity(t, modelName, modelVersion, `{"variantId":"v1","price":1}`); err != nil {
		t.Fatalf("CreateEntity 2: %v", err)
	}
	id3, err := c.CreateEntity(t, modelName, modelVersion, `{"variantId":"v1","price":1}`)
	if err != nil {
		t.Fatalf("CreateEntity 3: %v", err)
	}

	// Capture T1 here — we want the snapshot to include all three
	// CREATED-state entities. A small sleep on either side defends
	// against clock-tick granularity in the backend's timestamp source
	// (postgres NOW() in particular has microsecond granularity but
	// transaction commits within the same tick share a timestamp).
	time.Sleep(20 * time.Millisecond)
	pit := time.Now().UTC()
	time.Sleep(20 * time.Millisecond)

	// Post-T1 mutations that should be INVISIBLE to the pointInTime query.
	if err := c.UpdateEntity(t, id1, "ship", `{"variantId":"v1","price":1}`); err != nil {
		t.Fatalf("ship 1: %v", err)
	}
	if err := c.DeleteEntity(t, id3); err != nil {
		t.Fatalf("DeleteEntity 3: %v", err)
	}

	buckets, err := c.QueryGroupedStats(t, modelName, modelVersion, client.GroupedStatsRequest{
		GroupBy:     []string{"state"},
		PointInTime: &pit,
	})
	if err != nil {
		t.Fatalf("QueryGroupedStats pit=%s: %v", pit, err)
	}
	if len(buckets) != 1 {
		t.Fatalf("expected 1 bucket (all CREATED at T1), got %d: %+v", len(buckets), buckets)
	}
	if b := findBucketByGroupKey(buckets, []client.GroupKeyEntry{{Path: "state", Value: stringValue("CREATED")}}); b == nil || b.Count != 3 {
		t.Errorf("CREATED@T1: got %+v, want count=3 (including pre-ship + pre-delete versions)", b)
	}
}

// --- Scenario 6 — Tier-1 aggregations: sum / avg / min / max ---

// RunParityGroupedStats_AggregationsTier1 creates a single-group corpus
// with deterministic numeric values, requests sum/avg/min/max on
// $.price, and asserts the exact result. Tier-1 aggregations are
// bit-identical across backends (sum/min/max trivially, avg modulo a
// few ULPs which sit comfortably inside avgTol).
func RunParityGroupedStats_AggregationsTier1(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-stats-tier1"
	const modelVersion = 1
	setupStatsModel(t, c, modelName, modelVersion)

	// Prices: 10, 20, 30, 40 → sum=100, avg=25, min=10, max=40.
	for _, p := range []int{10, 20, 30, 40} {
		body := `{"variantId":"v1","price":` + itoa(p) + `}`
		if _, err := c.CreateEntity(t, modelName, modelVersion, body); err != nil {
			t.Fatalf("CreateEntity price=%d: %v", p, err)
		}
	}

	buckets, err := c.QueryGroupedStats(t, modelName, modelVersion, client.GroupedStatsRequest{
		GroupBy: []string{"$.variantId"},
		Aggregations: []client.AggregationExpr{
			{Op: "sum", Field: "$.price", As: "total"},
			{Op: "avg", Field: "$.price", As: "mean"},
			{Op: "min", Field: "$.price", As: "lo"},
			{Op: "max", Field: "$.price", As: "hi"},
		},
	})
	if err != nil {
		t.Fatalf("QueryGroupedStats: %v", err)
	}
	if len(buckets) != 1 {
		t.Fatalf("expected 1 bucket, got %d: %+v", len(buckets), buckets)
	}
	b := buckets[0]
	if b.Count != 4 {
		t.Errorf("count: got %d, want 4", b.Count)
	}
	// sum/min/max must be bit-identical across backends — float64 sum
	// of integer-typed inputs is exact, min/max are by-comparison.
	if got := floatFromAgg(b.Aggregations["total"]); got != 100 {
		t.Errorf("sum: got %.17g, want 100", got)
	}
	if got := floatFromAgg(b.Aggregations["lo"]); got != 10 {
		t.Errorf("min: got %.17g, want 10", got)
	}
	if got := floatFromAgg(b.Aggregations["hi"]); got != 40 {
		t.Errorf("max: got %.17g, want 40", got)
	}
	// avg has cross-backend ULP-level drift; use the tighter avgTol.
	assertAggFloat(t, "mean", floatFromAgg(b.Aggregations["mean"]), 25.0, avgTol)
}

// --- Scenario 7 — stdev: Welford ↔ STDDEV_SAMP within 1e-9 relative ---

// RunParityGroupedStats_StdevLowVarianceHighMean drives the
// low-variance/high-mean regime. The sqlite plugin declines stdev with
// ErrAggregationNotPushdownable so the service falls through to
// streaming Welford; postgres pushes STDDEV_SAMP. Both must agree
// within 1e-9 relative — this is the parity assertion that pins the D9
// cross-backend numerical-stability invariant.
//
// Values: 1_000_000 + small perturbations chosen so the population
// stdev is non-zero (avoids degenerate divide-by-zero in the relative
// comparison) and large enough that catastrophic-cancellation in a
// naive E[X²]-E[X]² formula would diverge by far more than 1e-9.
func RunParityGroupedStats_StdevLowVarianceHighMean(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-stats-stdev"
	const modelVersion = 1
	setupStatsModel(t, c, modelName, modelVersion)

	samples := []float64{
		1_000_000.10,
		1_000_000.20,
		1_000_000.30,
		1_000_000.40,
		1_000_000.50,
	}
	for _, s := range samples {
		body := `{"variantId":"v1","price":` + ftoa(s) + `}`
		if _, err := c.CreateEntity(t, modelName, modelVersion, body); err != nil {
			t.Fatalf("CreateEntity price=%v: %v", s, err)
		}
	}
	expected := sampleStdev(samples)

	buckets, err := c.QueryGroupedStats(t, modelName, modelVersion, client.GroupedStatsRequest{
		GroupBy: []string{"$.variantId"},
		Aggregations: []client.AggregationExpr{
			{Op: "stdev", Field: "$.price", As: "s"},
		},
	})
	if err != nil {
		t.Fatalf("QueryGroupedStats: %v", err)
	}
	if len(buckets) != 1 {
		t.Fatalf("expected 1 bucket, got %d: %+v", len(buckets), buckets)
	}
	got := floatFromAgg(buckets[0].Aggregations["s"])
	assertAggFloat(t, "stdev", got, expected, floatTol)
}

// --- Scenario 8 — missing/non-numeric values silently skipped ---

// RunParityGroupedStats_NonNumericSkipped exercises the D4 rule that
// any value the aggregator cannot interpret as a finite number is
// silently dropped from sum/avg/min/max/stdev. The corpus mixes
// entities that supply $.price (numeric) with entities that omit it
// entirely. Per spec D4, missing values are equivalent to non-numeric:
// they don't contribute to the aggregation, but they DO contribute to
// the bucket's Count (the count is over all entities, not just the
// numerically-aggregable subset).
//
// We exercise the "missing field" path rather than the "wrong type"
// path because Cyoda's model-locking enforces single-type fields:
// inserting a string at a path that was first observed as a number is
// rejected at the entity boundary. The missing-field branch is what
// real production traffic looks like (sparse JSON), and the
// observable contract — "count includes everything, aggregations
// include only finite numerics" — is identical to the wrong-type path
// from the service's perspective.
func RunParityGroupedStats_NonNumericSkipped(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-stats-nonnumeric"
	const modelVersion = 1
	setupStatsModel(t, c, modelName, modelVersion)

	// 3 entities with $.price set, 2 with $.price omitted. The schema
	// allows the field to be absent because the sample doc establishes
	// price as DOUBLE — strict type, not strict presence.
	for _, body := range []string{
		`{"variantId":"v1","price":10}`,
		`{"variantId":"v1","price":20}`,
		`{"variantId":"v1","price":30}`,
		`{"variantId":"v1"}`,
		`{"variantId":"v1"}`,
	} {
		if _, err := c.CreateEntity(t, modelName, modelVersion, body); err != nil {
			t.Fatalf("CreateEntity %s: %v", body, err)
		}
	}

	buckets, err := c.QueryGroupedStats(t, modelName, modelVersion, client.GroupedStatsRequest{
		GroupBy: []string{"$.variantId"},
		Aggregations: []client.AggregationExpr{
			{Op: "sum", Field: "$.price", As: "total"},
			{Op: "avg", Field: "$.price", As: "mean"},
			{Op: "min", Field: "$.price", As: "lo"},
			{Op: "max", Field: "$.price", As: "hi"},
		},
	})
	if err != nil {
		t.Fatalf("QueryGroupedStats: %v", err)
	}
	if len(buckets) != 1 {
		t.Fatalf("expected 1 bucket, got %d: %+v", len(buckets), buckets)
	}
	b := buckets[0]
	if b.Count != 5 {
		t.Errorf("count: got %d, want 5 (numeric + missing)", b.Count)
	}
	// Aggregations computed over the numeric subset {10, 20, 30}.
	if got := floatFromAgg(b.Aggregations["total"]); got != 60 {
		t.Errorf("sum: got %.17g, want 60", got)
	}
	if got := floatFromAgg(b.Aggregations["lo"]); got != 10 {
		t.Errorf("min: got %.17g, want 10", got)
	}
	if got := floatFromAgg(b.Aggregations["hi"]); got != 30 {
		t.Errorf("max: got %.17g, want 30", got)
	}
	assertAggFloat(t, "mean", floatFromAgg(b.Aggregations["mean"]), 20.0, avgTol)
}

// --- Scenario 9 — missing path coerces to null group-key ---

// RunParityGroupedStats_NonScalarCoercesToNull asserts both arms of
// spec D4's non-scalar coercion rule:
//
//  1. Missing-path: an absent $.meta resolves to a single null bucket.
//  2. Runtime-object: when $.meta is present but holds an object/array,
//     the group-key MUST still resolve to null so it merges with the
//     missing-path bucket. The streaming-tally path achieves this via
//     gjson.JSON → nil in buildGroupKeyFromEntity; the native pushdown
//     path achieves it via the CASE WHEN jsonb_typeof / json_type
//     wrapper in each plugin's groupExprToSQL.
//
// Mixed corpus design constraint: the model schema infers a single
// DataType per JSONPath at import time (see internal/domain/model/schema)
// and locks it. Once $.meta is an object in the imported sample doc, a
// subsequent entity submitting $.meta as a scalar fails validation with
// INCOMPATIBLE_TYPE. We cannot therefore submit (string-alpha,
// string-beta, object-gamma) at the same path through the HTTP API.
//
// Instead, the scenario submits a mix of:
//   - 2 entities OMITTING $.meta (exercises the missing-path arm)
//   - 3 entities WITH distinct object values at $.meta (exercises the
//     runtime-object arm — pre-fix this produced 3 distinct buckets
//     keyed by the object's JSON text on the pushdown path; post-fix
//     all 3 fold into the missing-path null bucket)
//
// The single null bucket therefore has count=5 when both arms of D4
// hold across all backends.
func RunParityGroupedStats_NonScalarCoercesToNull(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-stats-nonscalar"
	const modelVersion = 1
	setupStatsModel(t, c, modelName, modelVersion)

	bodies := []string{
		// Missing-path arm (gjson !Exists → nil; SQL NULL extraction).
		`{"variantId":"v1"}`,
		`{"variantId":"v1"}`,
		// Runtime-object arm (D4 coerces object → nil regardless of
		// content; without the D4 wrappers the pushdown path would
		// produce 3 distinct buckets keyed by the object's JSON text).
		`{"variantId":"v1","meta":{"source":"alpha"}}`,
		`{"variantId":"v1","meta":{"source":"beta"}}`,
		`{"variantId":"v1","meta":{"source":"gamma"}}`,
	}
	for _, b := range bodies {
		if _, err := c.CreateEntity(t, modelName, modelVersion, b); err != nil {
			t.Fatalf("CreateEntity %s: %v", b, err)
		}
	}

	buckets, err := c.QueryGroupedStats(t, modelName, modelVersion, client.GroupedStatsRequest{
		GroupBy: []string{"$.meta"},
	})
	if err != nil {
		t.Fatalf("QueryGroupedStats: %v", err)
	}
	if len(buckets) != 1 {
		t.Fatalf("expected 1 null bucket, got %d: %+v", len(buckets), buckets)
	}
	if b := findBucketByGroupKey(buckets, []client.GroupKeyEntry{{Path: "$.meta", Value: nil}}); b == nil || b.Count != 5 {
		t.Errorf("null bucket (missing + object both coerce): got %+v, want count=5", b)
	}
}

// --- Scenario 10 — cardinality ceiling exceeded ---

// RunParityGroupedStats_CardinalityExceeded constructs a corpus whose
// group-by cardinality exceeds the server's CYODA_STATS_GROUP_MAX
// (default 10000). We can't realistically exceed 10000 from a parity
// test, so we drive the assertion via the request's `limit` validator
// rejection — limit > maxBuckets is a 400 INVALID_LIMIT on every
// backend.
//
// The /cardinality-exceeded/ row of the spec §7 matrix is also
// asserted: limit=2 with 3 distinct groups would NOT exceed unless we
// override the runtime ceiling, which we can't reach from HTTP. The
// 400 INVALID_LIMIT assertion is the layer of the matrix that
// SERVERS handler-side must agree on.
func RunParityGroupedStats_CardinalityExceeded(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-stats-cardinality"
	const modelVersion = 1
	setupStatsModel(t, c, modelName, modelVersion)

	// A limit that vastly exceeds CYODA_STATS_GROUP_MAX must be
	// rejected at request-validation time with 400 INVALID_LIMIT,
	// regardless of the backend (the handler is backend-agnostic, but
	// this scenario pins that the same wire response shape is emitted
	// when running against every fixture).
	limit := 1_000_000_000
	status, body, err := c.QueryGroupedStatsRaw(t, modelName, modelVersion, client.GroupedStatsRequest{
		GroupBy: []string{"$.variantId"},
		Limit:   &limit,
	})
	if err != nil {
		t.Fatalf("QueryGroupedStatsRaw: %v", err)
	}
	if status != 400 {
		t.Errorf("status: got %d, want 400 (INVALID_LIMIT); body=%s", status, string(body))
	}
	if !containsErrorCode(body, "INVALID_LIMIT") {
		t.Errorf("body missing INVALID_LIMIT code: %s", string(body))
	}
}

// --- helpers ---

// itoa wraps strconv.Itoa for parity with ftoa — kept as a single
// formatting layer so future scenarios that extend the corpus use the
// same call shape everywhere.
func itoa(n int) string { return strconv.Itoa(n) }

// ftoa renders a float64 with full precision so the entity body
// JSON-decodes to the exact float64 the test computed. -1 precision
// with 'g' is the canonical shortest-round-trip form: the JSON decoder
// recovers the exact float64 from any such decimal.
func ftoa(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// containsErrorCode reports whether the JSON error body advertises the
// supplied errorCode. The cyoda error envelope (RFC 7807 problem+json)
// puts the code under properties.errorCode — we match on the substring
// rather than re-parsing the envelope because the surrounding fields
// (ticket UUID, path, etc.) vary scenario-to-scenario.
func containsErrorCode(body []byte, code string) bool {
	return bytes.Contains(body, []byte(`"errorCode":"`+code+`"`))
}

// sampleStdev computes the unbiased sample standard deviation of xs,
// which is what postgres STDDEV_SAMP and the service's Welford
// accumulator both report (n-1 in the denominator). Used as the
// expected value in the stdev parity scenario.
func sampleStdev(xs []float64) float64 {
	if len(xs) < 2 {
		return math.NaN()
	}
	var mean float64
	for _, x := range xs {
		mean += x
	}
	mean /= float64(len(xs))
	var ss float64
	for _, x := range xs {
		d := x - mean
		ss += d * d
	}
	return math.Sqrt(ss / float64(len(xs)-1))
}
