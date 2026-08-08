package parity

import (
	"net/http"
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// search_path_key.go pins two properties that were backend-visible defects
// before PR #490, and which no single-backend test can guard: both are about
// which document a condition's field path is resolved against.

// RunSearchPrefixlessPathResolvesDeclaredType pins that `amount` and
// `$.amount` name the same field.
//
// FieldsMap keys always carry the "$." prefix, and pre-execution path
// validation normalises before checking — so a prefix-less path clears every
// gate as a known field. The declared-type lookup did not normalise, so it
// missed the map and the leaf came back with no declared type. The kernel is
// type-directed, so a comparison leaf with no declared type expands into
// nothing and never matches: `city EQUALS "Berlin"` answered 200 with an empty
// page on a model whose $.city holds Berlin.
//
// It is a parity scenario rather than a unit test because the two spellings
// have to agree on every backend at once — the pushdown translator, the
// in-memory evaluator, and the type-soundness validator each held their own
// copy of the defect, and each is exercised by a different backend/plan shape.
func RunSearchPrefixlessPathResolvesDeclaredType(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-search-prefixless-path"
	const modelVersion = 1
	setupSearchModel(t, c, modelName, modelVersion)

	aID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Alice","amount":100,"status":"active"}`)
	if err != nil {
		t.Fatalf("CreateEntity Alice: %v", err)
	}
	if _, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Bob","amount":5,"status":"active"}`); err != nil {
		t.Fatalf("CreateEntity Bob: %v", err)
	}

	// Both spellings of the same field must select the same entity. A string
	// equality and a numeric comparison, because the defect showed up through
	// the declared-type set and the two take different kernel branches.
	for _, tc := range []struct {
		label string
		cond  string
	}{
		{"prefixed string", `{"type":"simple","jsonPath":"$.name","operatorType":"EQUALS","value":"Alice"}`},
		{"prefixless string", `{"type":"simple","jsonPath":"name","operatorType":"EQUALS","value":"Alice"}`},
		{"prefixed numeric", `{"type":"simple","jsonPath":"$.amount","operatorType":"GREATER_THAN","value":50}`},
		{"prefixless numeric", `{"type":"simple","jsonPath":"amount","operatorType":"GREATER_THAN","value":50}`},
	} {
		results, err := c.SyncSearch(t, modelName, modelVersion, tc.cond)
		if err != nil {
			t.Fatalf("[%s] SyncSearch: %v", tc.label, err)
		}
		assertResultIDSet(t, tc.label, results, []string{aID.String()})
	}
}

// RunSearchPrefixlessPathTypeMismatch400 pins the other half: the
// type-soundness check indexes the same FieldsMap, so before the fix it
// skipped any prefix-less leaf entirely. An operand that must be rejected was
// accepted and answered with an empty page, while the identical condition
// written "$.amount" was rejected — two spellings of one query returning
// different HTTP statuses.
func RunSearchPrefixlessPathTypeMismatch400(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-search-prefixless-400"
	const modelVersion = 1
	setupSearchModel(t, c, modelName, modelVersion)

	if _, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Alice","amount":100,"status":"active"}`); err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	for _, tc := range []struct {
		label string
		path  string
	}{
		{"prefixed", "$.amount"},
		{"prefixless", "amount"},
	} {
		cond := `{"type":"simple","jsonPath":"` + tc.path + `","operatorType":"GREATER_THAN","value":"not-a-number"}`
		status, body, err := c.SyncSearchRaw(t, modelName, modelVersion, cond)
		if err != nil {
			t.Fatalf("[%s] SyncSearchRaw: %v", tc.label, err)
		}
		if status != http.StatusBadRequest {
			t.Fatalf("[%s] expected 400, got %d; body=%s", tc.label, status, body)
		}
		if !containsErrorCode(body, "CONDITION_TYPE_MISMATCH") {
			t.Errorf("[%s] expected errorCode CONDITION_TYPE_MISMATCH, body=%s", tc.label, body)
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
// "_meta.state" is rejected 400 INVALID_FIELD_PATH before any evaluator runs
// and the probe would pass vacuously on a leaking backend. Grouped stats runs
// no data-field path validation (cyoda-go#480), so the condition reaches the
// residual evaluator — which is the code this guards. Verified by reverting
// the fix: through /search the scenario passes either way; through grouped
// stats it fails.
//
// DELIBERATELY NOT COVERED: the groupBy / ORDER BY / aggregate arms, and the
// IS_NULL / NOT_NULL operators. Those are compiled straight to SQL against the
// merged document with no kernel re-check, so they still resolve _meta on
// PostgreSQL. That is cyoda-go#489, whose fix (nesting the domain data rather
// than merging it) removes the shared namespace outright and closes all of them
// at once. When #489 lands, extend this scenario and delete this note.
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
		GroupBy:   []string{"status"},
		Condition: &client.AggregationCond{"type": "simple", "jsonPath": "status", "operatorType": "EQUALS", "value": "active"},
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
	//   - A COMPARISON operator also pins nothing. "_meta.state" is not in the
	//     model, so it carries no declared type, and the type-directed kernel
	//     expands a comparison with no declared type into nothing — a non-match
	//     whichever document it was handed. String operators never consult
	//     declared types, so they are the operators that can see the difference.
	//
	// Verified by reverting the fix: with EQUALS the scenario passes either way;
	// with CONTAINS all four probes fail.
	for _, tc := range []struct {
		label string
		cond  client.AggregationCond
	}{
		{"_meta.state", client.AggregationCond{"type": "simple", "jsonPath": "_meta.state", "operatorType": "CONTAINS", "value": "CREATE"}},
		{"$._meta.state", client.AggregationCond{"type": "simple", "jsonPath": "$._meta.state", "operatorType": "CONTAINS", "value": "CREATE"}},
		{"_meta.tenant_id", client.AggregationCond{"type": "simple", "jsonPath": "_meta.tenant_id", "operatorType": "CONTAINS", "value": tenant.ID}},
		{"_meta.model_name", client.AggregationCond{"type": "simple", "jsonPath": "_meta.model_name", "operatorType": "CONTAINS", "value": modelName}},
	} {
		buckets, err := c.QueryGroupedStats(t, modelName, modelVersion, client.GroupedStatsRequest{
			GroupBy:   []string{"status"},
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
