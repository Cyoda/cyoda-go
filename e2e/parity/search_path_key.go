package parity

import (
	"net/http"
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// search_path_key.go pins how a condition's field path is spelled and which
// document it is resolved against — both backend-visible properties that no
// single-backend test can guard.

// RunSearchPathRequiresJSONPathLeader pins that a condition's jsonPath is JSON
// Path nomenclature: the "$." leader is required, and a bare "amount" is
// rejected rather than read as "$.amount".
//
// It is a parity scenario rather than a unit test because the rejection has to
// hold on every backend at once, and a bare path is exactly the input that
// used to differ per backend. The translator refuses it, but every engine call
// site treats a translate failure as "fall back to in-memory evaluation" — and
// the in-memory evaluator resolves a bare path happily. So the request
// silently left the pushdown plan and answered from a full scan, with results
// that looked right; on a backend or query shape that took a different plan it
// answered differently. Rejecting at the boundary makes all backends agree.
//
// The prefixed spellings are asserted too, as the positive control: a
// tightening that breaks valid callers is worse than the bug it fixes. A
// string equality and a numeric comparison, because they take different
// kernel branches — the numeric one only matches when the leaf carries its
// declared type, which is looked up under the "$."-prefixed FieldsMap key.
func RunSearchPathRequiresJSONPathLeader(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-search-path-leader"
	const modelVersion = 1
	setupSearchModel(t, c, modelName, modelVersion)

	aID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Alice","amount":100,"status":"active"}`)
	if err != nil {
		t.Fatalf("CreateEntity Alice: %v", err)
	}
	if _, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Bob","amount":5,"status":"active"}`); err != nil {
		t.Fatalf("CreateEntity Bob: %v", err)
	}

	// Positive control: the JSON Path spellings select the entity.
	for _, tc := range []struct {
		label string
		cond  string
	}{
		{"string", `{"type":"simple","jsonPath":"$.name","operatorType":"EQUALS","value":"Alice"}`},
		{"numeric", `{"type":"simple","jsonPath":"$.amount","operatorType":"GREATER_THAN","value":50}`},
	} {
		results, err := c.SyncSearch(t, modelName, modelVersion, tc.cond)
		if err != nil {
			t.Fatalf("[%s] SyncSearch: %v", tc.label, err)
		}
		assertResultIDSet(t, tc.label, results, []string{aID.String()})
	}

	// Rejected: not JSON Path nomenclature. Each of these addresses a field
	// that genuinely exists — the point is that the SPELLING is invalid, so
	// "the field is there" is not a reason to accept it.
	for _, tc := range []struct {
		label string
		path  string
	}{
		{"bare identifier", "name"},
		{"bare numeric field", "amount"},
		{"leader only", "$."},
		{"bracket quoted", "$['name']"},
		{"bracket quoted after leader", "$.['name']"},
		{"trailing dot", "$.name."},
		{"empty segment", "$..name"},
	} {
		cond := `{"type":"simple","jsonPath":"` + tc.path + `","operatorType":"EQUALS","value":"Alice"}`
		status, body, err := c.SyncSearchRaw(t, modelName, modelVersion, cond)
		if err != nil {
			t.Fatalf("[%s] SyncSearchRaw: %v", tc.label, err)
		}
		if status != http.StatusBadRequest {
			t.Fatalf("[%s] jsonPath %q: expected 400, got %d; body=%s", tc.label, tc.path, status, body)
		}
		if !containsErrorCode(body, "INVALID_FIELD_PATH") {
			t.Errorf("[%s] jsonPath %q: expected errorCode INVALID_FIELD_PATH, body=%s", tc.label, tc.path, body)
		}
	}
}

// RunSearchArraySubscriptPathStillServed is the other side of the leader rule
// and the reason it cannot be enforced by rejecting every translate failure.
//
// "$.tags[*]" is valid JSON Path that no pushdown filter can express. The
// translator refuses it, and that refusal MUST remain the "fall back to
// in-memory evaluation" signal rather than a 400 — otherwise the tightening
// turns working queries into client errors. Backend-agnostic: the fallback is
// the engine's, but whether pushdown was even attempted is per-backend, so
// every backend has to answer the same.
func RunSearchArraySubscriptPathStillServed(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-search-path-subscript"
	const modelVersion = 1
	// The shared search model has no array field, so declare one: the whole
	// point of the scenario is an array-subscripted path.
	setupModelWithWorkflow(t, c, modelName, modelVersion,
		`{"name":"Test","amount":10,"status":"new","tags":[""]}`, searchWorkflowJSON)

	aID, err := c.CreateEntity(t, modelName, modelVersion,
		`{"name":"Alice","amount":100,"status":"active","tags":["red","blue"]}`)
	if err != nil {
		t.Fatalf("CreateEntity Alice: %v", err)
	}
	bID, err := c.CreateEntity(t, modelName, modelVersion,
		`{"name":"Bob","amount":5,"status":"active","tags":["green"]}`)
	if err != nil {
		t.Fatalf("CreateEntity Bob: %v", err)
	}

	// NOT_NULL and IS_NULL on the same subscripted path. The pair is what
	// makes this evidence rather than a shrug: if the path had failed to
	// resolve, both would answer the same way. NOT_NULL selecting everything
	// while IS_NULL selects nothing means the evaluator actually reached the
	// array.
	for _, tc := range []struct {
		label string
		op    string
		want  []string
	}{
		{"NOT_NULL", "NOT_NULL", []string{aID.String(), bID.String()}},
		{"IS_NULL", "IS_NULL", nil},
	} {
		cond := `{"type":"simple","jsonPath":"$.tags[*]","operatorType":"` + tc.op + `","value":null}`
		status, body, err := c.SyncSearchRaw(t, modelName, modelVersion, cond)
		if err != nil {
			t.Fatalf("[%s] SyncSearchRaw: %v", tc.label, err)
		}
		if status != http.StatusOK {
			t.Fatalf("[%s] array-subscript path answered %d, want 200 — it is valid JSON Path and must reach the in-memory fallback; body=%s",
				tc.label, status, body)
		}
		results, err := c.SyncSearch(t, modelName, modelVersion, cond)
		if err != nil {
			t.Fatalf("[%s] SyncSearch: %v", tc.label, err)
		}
		assertResultIDSet(t, tc.label, results, tc.want)
	}
}

// RunSearchPathTypeMismatch400 pins that a well-formed path with an operand
// that cannot parse into the field's declared type is rejected as
// CONDITION_TYPE_MISMATCH — the type check indexes the FieldsMap under the
// "$."-prefixed key, so it must find the leaf and constrain it.
//
// The malformed spelling is asserted alongside it to pin the CLASSIFICATION
// boundary: both are 400, but they are different failures and must not
// collapse into one code. A bare path is not a type mismatch — the request
// never named a field.
func RunSearchPathTypeMismatch400(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-search-path-400"
	const modelVersion = 1
	setupSearchModel(t, c, modelName, modelVersion)

	if _, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Alice","amount":100,"status":"active"}`); err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	for _, tc := range []struct {
		label string
		path  string
		want  string
	}{
		{"json path", "$.amount", "CONDITION_TYPE_MISMATCH"},
		{"bare path", "amount", "INVALID_FIELD_PATH"},
	} {
		cond := `{"type":"simple","jsonPath":"` + tc.path + `","operatorType":"GREATER_THAN","value":"not-a-number"}`
		status, body, err := c.SyncSearchRaw(t, modelName, modelVersion, cond)
		if err != nil {
			t.Fatalf("[%s] SyncSearchRaw: %v", tc.label, err)
		}
		if status != http.StatusBadRequest {
			t.Fatalf("[%s] expected 400, got %d; body=%s", tc.label, status, body)
		}
		if !containsErrorCode(body, tc.want) {
			t.Errorf("[%s] expected errorCode %s, body=%s", tc.label, tc.want, body)
		}
	}
}

// RunSearchMetaBlockNotMatchableAsDataPath pins that the storage layer's own
// bookkeeping is not addressable as entity data.
//
// PostgreSQL stores an entity as one JSON document with the domain data and a
// storage-level "_meta" block side by side; memory and sqlite keep the two
// apart. The residual evaluator was handed the un-stripped document, so a
// SourceData condition naming a path under _meta matched on PostgreSQL and on
// no other backend — a divergence, and storage internals exposed as a
// queryable surface.
//
// It probes through grouped stats, NOT /search, and that choice is the whole
// point: /search validates every data-field path against the model first, so
// "$._meta.state" is rejected 400 INVALID_FIELD_PATH before any evaluator runs
// and the probe would pass vacuously on a leaking backend. Grouped stats runs
// no data-field path validation, so the condition reaches the residual
// evaluator — which is the code this guards. Verified by reverting the fix:
// through /search the scenario passes either way; through grouped stats it
// fails.
//
// Every path here carries the "$." leader. That is not cosmetic: without it
// the request is rejected as malformed at the boundary and the probe would
// again pass vacuously, testing the path grammar instead of the meta block.
//
// DELIBERATELY NOT COVERED: the groupBy / ORDER BY / aggregate arms, and the
// IS_NULL / NOT_NULL operators. Those are compiled straight to SQL against the
// merged document with no kernel re-check, so they still resolve _meta on
// PostgreSQL. The fix for that (nesting the domain data rather than merging
// it) removes the shared namespace outright and closes all of them at once.
// When it lands, extend this scenario and delete this note.
func RunSearchMetaBlockNotMatchableAsDataPath(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-search-meta-not-data"
	const modelVersion = 1
	setupSearchModel(t, c, modelName, modelVersion)

	if _, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Alice","amount":100,"status":"active"}`); err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	// Sanity: the same query shape over a real data field does select the
	// entity, so a zero-bucket result below means "did not match", not
	// "grouped stats is broken".
	control, err := c.QueryGroupedStats(t, modelName, modelVersion, client.GroupedStatsRequest{
		GroupBy:   []string{"$.status"},
		Condition: &client.AggregationCond{"type": "simple", "jsonPath": "$.status", "operatorType": "EQUALS", "value": "active"},
	})
	if err != nil {
		t.Fatalf("control grouped stats: %v", err)
	}
	if len(control) != 1 || control[0].Count != 1 {
		t.Fatalf("control returned %d buckets, want 1 with count 1: %+v", len(control), control)
	}

	// String operators, and each operand is a value the entity's metadata
	// ACTUALLY holds. Both choices are load-bearing, and I got both wrong first:
	//
	//   - An operand nothing holds passes on a leaking backend and pins nothing.
	//   - A COMPARISON operator also pins nothing. "$._meta.state" is not in the
	//     model, so it carries no declared type, and the type-directed kernel
	//     expands a comparison with no declared type into nothing — a non-match
	//     whichever document it was handed. String operators never consult
	//     declared types, so they are the operators that can see the difference.
	//
	// Verified by reverting the fix: with EQUALS the scenario passes either way;
	// with CONTAINS all three probes fail.
	for _, tc := range []struct {
		label string
		cond  client.AggregationCond
	}{
		{"$._meta.state", client.AggregationCond{"type": "simple", "jsonPath": "$._meta.state", "operatorType": "CONTAINS", "value": "CREATE"}},
		{"$._meta.tenant_id", client.AggregationCond{"type": "simple", "jsonPath": "$._meta.tenant_id", "operatorType": "CONTAINS", "value": tenant.ID}},
		{"$._meta.model_name", client.AggregationCond{"type": "simple", "jsonPath": "$._meta.model_name", "operatorType": "CONTAINS", "value": modelName}},
	} {
		buckets, err := c.QueryGroupedStats(t, modelName, modelVersion, client.GroupedStatsRequest{
			GroupBy:   []string{"$.status"},
			Condition: &tc.cond,
		})
		if err != nil {
			t.Fatalf("[%s] QueryGroupedStats: %v", tc.label, err)
		}
		total := int64(0)
		for _, b := range buckets {
			total += b.Count
		}
		if total != 0 {
			t.Errorf("[%s] matched %d entities: the storage meta block is addressable as entity data on this backend", tc.label, total)
		}
	}

	// Meta stays searchable the supported way — a lifecycle condition reads
	// the entity's metadata and is unaffected. Asserted so the guard cannot be
	// satisfied by starving the evaluator of the document entirely.
	results, err := c.SyncSearch(t, modelName, modelVersion, `{"type":"lifecycle","field":"state","operatorType":"EQUALS","value":"CREATED"}`)
	if err != nil {
		t.Fatalf("lifecycle state search: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("lifecycle condition on state returned %d entities, want 1 — meta must stay searchable the supported way", len(results))
	}
}

// RunGroupedStatsPathRequiresJSONPathLeader pins the same rule on the OTHER
// path surface a grouped-stats request carries: groupBy entries and
// aggregation fields. Accepting a bare path there while rejecting it in
// `condition` would answer one request inconsistently across two of its own
// fields.
//
// Backend-agnostic input validation, so it belongs in parity: the group path
// reaches either the plugin's own validator (pushdown) or gjson (streaming
// tally), and which one depends on the backend and the query shape.
func RunGroupedStatsPathRequiresJSONPathLeader(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-stats-path-leader"
	const modelVersion = 1
	setupSearchModel(t, c, modelName, modelVersion)

	if _, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Alice","amount":100,"status":"active"}`); err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	// Positive control: the JSON Path spelling works, and so does the reserved
	// "state" token, which names the lifecycle state rather than a data path
	// and is therefore exempt from the leader rule.
	for _, groupBy := range [][]string{{"$.status"}, {"state"}} {
		buckets, err := c.QueryGroupedStats(t, modelName, modelVersion, client.GroupedStatsRequest{
			GroupBy: groupBy,
		})
		if err != nil {
			t.Fatalf("groupBy %v: %v", groupBy, err)
		}
		if len(buckets) != 1 || buckets[0].Count != 1 {
			t.Fatalf("groupBy %v returned %d buckets, want 1 with count 1: %+v", groupBy, len(buckets), buckets)
		}
	}

	for _, tc := range []struct {
		label string
		req   client.GroupedStatsRequest
		want  string
	}{
		{"bare groupBy", client.GroupedStatsRequest{GroupBy: []string{"status"}}, "INVALID_GROUP_BY_PATH"},
		{"bracket-quoted groupBy", client.GroupedStatsRequest{GroupBy: []string{"$['status']"}}, "INVALID_GROUP_BY_PATH"},
		{"bare aggregation field", client.GroupedStatsRequest{
			GroupBy:      []string{"$.status"},
			Aggregations: []client.AggregationExpr{{Op: "sum", Field: "amount"}},
		}, "INVALID_AGGREGATION_FIELD"},
		{"bare condition path", client.GroupedStatsRequest{
			GroupBy:   []string{"$.status"},
			Condition: &client.AggregationCond{"type": "simple", "jsonPath": "status", "operatorType": "EQUALS", "value": "active"},
		}, "INVALID_FIELD_PATH"},
	} {
		status, body, err := c.QueryGroupedStatsRaw(t, modelName, modelVersion, tc.req)
		if err != nil {
			t.Fatalf("[%s] QueryGroupedStatsRaw: %v", tc.label, err)
		}
		if status != http.StatusBadRequest {
			t.Fatalf("[%s] expected 400, got %d; body=%s", tc.label, status, body)
		}
		if !containsErrorCode(body, tc.want) {
			t.Errorf("[%s] expected errorCode %s, body=%s", tc.label, tc.want, body)
		}
	}
}
