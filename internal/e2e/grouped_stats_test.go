package e2e_test

// grouped_stats_test.go — ONE in-process e2e test for
// POST /api/entity/stats/{entityName}/{modelVersion}/query. The parity suite
// (e2e/parity/grouped_stats.go) exhaustively covers cross-backend behavioural
// agreement via subprocess-launched fixtures; this test narrowly verifies the
// HTTP wiring on the canonical backend (postgres testcontainer):
// handler → service → SPI → postgres GROUP BY → JSON response. It exercises
// the happy path, an aggregation request (sum + avg + stdev), and the
// validation-error path (empty groupBy → 400 MISSING_GROUP_BY surfaced via
// the common RFC 9457 problem+json shape with properties.errorCode).

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"testing"
)

// statsWorkflowJSON is the same canonical workflow used by the search tests:
// auto NONE → CREATED on create, manual CREATED → APPROVED. Entities created
// here land in CREATED, which makes the lifecycle-state condition path
// meaningful (state == "CREATED" matches all rows).
const statsWorkflowJSON = `{
	"importMode": "REPLACE",
	"workflows": [{
		"version": "1.0", "name": "stats-wf", "initialState": "NONE", "active": true,
		"states": {
			"NONE":     {"transitions": [{"name": "init",    "next": "CREATED",  "manual": false}]},
			"CREATED":  {"transitions": [{"name": "approve", "next": "APPROVED", "manual": true}]},
			"APPROVED": {}
		}
	}]
}`

// statsSampleDoc is the canonical sample document for SAMPLE_DATA schema
// inference. The default importModelE2E sample (`{name, amount, status}`)
// does not include variantId/price, and the resulting strict schema rejects
// extra fields on createEntity — so this test imports its own sample that
// covers every field used in the corpus.
const statsSampleDoc = `{"variantId":"v1","price":1.0}`

// setupStatsModel imports a model whose schema is inferred from
// statsSampleDoc, locks it, and imports statsWorkflowJSON. The combination
// gives every created entity an auto-init to CREATED so the lifecycle
// condition path is exercisable.
func setupStatsModel(t *testing.T, entityName string) {
	t.Helper()
	const modelVersion = 1
	importPath := fmt.Sprintf("/api/model/import/JSON/SAMPLE_DATA/%s/%d", entityName, modelVersion)
	resp := doAuth(t, http.MethodPost, importPath, statsSampleDoc)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("import stats model %s: expected 200, got %d: %s", entityName, resp.StatusCode, body)
	}
	lockModelE2E(t, entityName, modelVersion)
	status, lockBody := importWorkflowE2E(t, entityName, modelVersion, statsWorkflowJSON)
	if status != http.StatusOK {
		t.Fatalf("import stats workflow %s: expected 200, got %d: %s", entityName, status, lockBody)
	}
}

// decodeBuckets parses the response body as the wire-shape grouped-stats
// array. Keeping the decoder local mirrors the parity client but avoids
// pulling its dependencies into the in-process e2e package.
func decodeBuckets(t *testing.T, body string) []map[string]any {
	t.Helper()
	var buckets []map[string]any
	if err := json.Unmarshal([]byte(body), &buckets); err != nil {
		t.Fatalf("decode grouped-stats response: %v\nbody: %s", err, body)
	}
	return buckets
}

// findBucket locates the bucket whose groupKey contains a (path, value) pair.
// Returns nil if not found. The wire format is groupKey: [{path, value}, ...].
func findBucket(buckets []map[string]any, path string, value any) map[string]any {
	for _, b := range buckets {
		gk, _ := b["groupKey"].([]any)
		for _, e := range gk {
			em, _ := e.(map[string]any)
			if em["path"] == path && em["value"] == value {
				return b
			}
		}
	}
	return nil
}

// TestGroupedStats_E2E_HappyPath verifies HTTP wiring on postgres: import a
// model with a workflow, create entities with mixed variantIds, POST a
// grouped-stats request, and assert the response shape, bucket count, and
// per-bucket count values.
func TestGroupedStats_E2E_HappyPath(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	const model = "e2e-grouped-stats-happy"
	setupStatsModel(t, model)

	// 3 variants, 5 entities: v1=2, v2=2, v3=1. All land in CREATED.
	createEntityE2E(t, model, 1, `{"variantId":"v1","price":10.0}`)
	createEntityE2E(t, model, 1, `{"variantId":"v1","price":12.0}`)
	createEntityE2E(t, model, 1, `{"variantId":"v2","price":8.0}`)
	createEntityE2E(t, model, 1, `{"variantId":"v2","price":8.0}`)
	createEntityE2E(t, model, 1, `{"variantId":"v3","price":100.0}`)

	reqBody := `{
		"groupBy": ["$.variantId"]
	}`
	path := fmt.Sprintf("/api/entity/stats/%s/1/query", model)
	resp := doAuth(t, http.MethodPost, path, reqBody)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("grouped-stats happy: expected 200, got %d: %s", resp.StatusCode, body)
	}

	buckets := decodeBuckets(t, body)
	if len(buckets) != 3 {
		t.Fatalf("expected 3 buckets (v1, v2, v3), got %d: %s", len(buckets), body)
	}

	// Per-bucket counts. The wire `count` is JSON-decoded as float64 — the
	// service emits int64 but Go's encoding/json normalises numbers into a
	// single `any` type on decode.
	check := func(variant string, want float64) {
		b := findBucket(buckets, "$.variantId", variant)
		if b == nil {
			t.Errorf("missing bucket for variantId=%s; buckets=%s", variant, body)
			return
		}
		got, _ := b["count"].(float64)
		if got != want {
			t.Errorf("variantId=%s: count got %v, want %v", variant, got, want)
		}
		// No aggregations requested — the field must be omitted or empty.
		if aggs, ok := b["aggregations"].(map[string]any); ok && len(aggs) != 0 {
			t.Errorf("variantId=%s: aggregations should be omitted, got %v", variant, aggs)
		}
	}
	check("v1", 2)
	check("v2", 2)
	check("v3", 1)
}

// TestGroupedStats_E2E_Aggregations verifies the aggregation path end-to-end:
// sum + avg + stdev over $.price grouped by $.variantId, with a lifecycle
// condition `state == "CREATED"`. The condition matches all entities (they
// all auto-land in CREATED) but exercises the condition-parsing wire path.
func TestGroupedStats_E2E_Aggregations(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	const model = "e2e-grouped-stats-agg"
	setupStatsModel(t, model)

	// Four prices in one group: 10, 20, 30, 40 → sum=100, avg=25, stdev=sqrt(166.6...)
	// Using the sample-stdev formula: variance = sum((x-mean)^2)/(n-1) = 500/3,
	// stdev = sqrt(500/3) ≈ 12.909944487358056.
	prices := []float64{10, 20, 30, 40}
	for _, p := range prices {
		body := fmt.Sprintf(`{"variantId":"v1","price":%v}`, p)
		createEntityE2E(t, model, 1, body)
	}

	reqBody := `{
		"groupBy": ["$.variantId"],
		"condition": {"type":"lifecycle","field":"state","operatorType":"EQUALS","value":"CREATED"},
		"aggregations": [
			{"op":"sum",   "field":"$.price", "as":"total"},
			{"op":"avg",   "field":"$.price", "as":"mean"},
			{"op":"stdev", "field":"$.price", "as":"s"}
		]
	}`
	path := fmt.Sprintf("/api/entity/stats/%s/1/query", model)
	resp := doAuth(t, http.MethodPost, path, reqBody)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("grouped-stats agg: expected 200, got %d: %s", resp.StatusCode, body)
	}

	buckets := decodeBuckets(t, body)
	if len(buckets) != 1 {
		t.Fatalf("expected 1 bucket, got %d: %s", len(buckets), body)
	}
	b := buckets[0]
	if got, _ := b["count"].(float64); got != 4 {
		t.Errorf("count: got %v, want 4", got)
	}

	aggs, ok := b["aggregations"].(map[string]any)
	if !ok {
		t.Fatalf("missing aggregations map; bucket=%v", b)
	}
	// sum is exact (integer-valued float64 sums).
	if got, _ := aggs["total"].(float64); got != 100 {
		t.Errorf("sum total: got %v, want 100", got)
	}
	// avg is exact for this corpus (100/4 = 25 — bit-exact in float64).
	if got, _ := aggs["mean"].(float64); got != 25 {
		t.Errorf("avg mean: got %v, want 25", got)
	}
	// stdev: sample stdev of {10,20,30,40} = sqrt(500/3). Allow 1e-9 relative
	// tolerance (matches the parity-suite ceiling for Welford ↔ STDDEV_SAMP).
	wantStdev := math.Sqrt(500.0 / 3.0)
	gotStdev, _ := aggs["s"].(float64)
	if math.Abs(gotStdev-wantStdev)/wantStdev > 1e-9 {
		t.Errorf("stdev s: got %.17g, want %.17g (rel diff %.3e)",
			gotStdev, wantStdev, math.Abs(gotStdev-wantStdev)/wantStdev)
	}
}

// TestGroupedStats_E2E_ValidationError verifies the validation-error wire
// path: empty groupBy returns 400 with properties.errorCode = MISSING_GROUP_BY
// in the RFC 9457 problem+json shape (per common.WriteError).
func TestGroupedStats_E2E_ValidationError(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	const model = "e2e-grouped-stats-valid"
	setupStatsModel(t, model)

	reqBody := `{"groupBy": []}`
	path := fmt.Sprintf("/api/entity/stats/%s/1/query", model)
	resp := doAuth(t, http.MethodPost, path, reqBody)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", resp.StatusCode, body)
	}

	var pd struct {
		Status     int            `json:"status"`
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal([]byte(body), &pd); err != nil {
		t.Fatalf("decode problem detail: %v\nbody: %s", err, body)
	}
	if pd.Status != http.StatusBadRequest {
		t.Errorf("ProblemDetail.status: got %d, want 400", pd.Status)
	}
	if code, _ := pd.Properties["errorCode"].(string); code != "MISSING_GROUP_BY" {
		t.Errorf("properties.errorCode: got %q, want MISSING_GROUP_BY; body=%s", code, body)
	}
}
