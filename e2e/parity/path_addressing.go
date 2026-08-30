package parity

import (
	"net/http"
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// path_addressing.go pins docs/cloud-parity/path-grammar.md sections 3, 4, 5
// and 8 end to end, on every backend at once: what a path addresses is
// decided by the field's DECLARED shape, never by the shape of one stored
// value, and an "array" clause is read as an AND of positional comparisons —
// with every rule in the document applying to it without exception.
//
// The centerpiece, RunArrayClausePositional, reproduces the original defect
// directly: before the fix, an "array" clause's positional leaf was addressed
// with a DOTTED index ("tags.0") rather than a BRACKET index ("tags[0]"). A
// dotted numeric segment is a field name, not an index (section 3, "a dot and
// a number is a field name, not an index") — so the in-memory evaluator
// (which resolves the bracket form) matched, while both SQL backends render
// "tags.0" as a literal object-key lookup against an ARRAY and get null.
// Same query, same data, three different answers: memory 1, sqlite 0,
// postgres 0. Only a scenario that runs unmodified against all three,
// asserting an EXACT count, can catch that asymmetry — a single-backend test
// would have passed on whichever backend it happened to run against.

// RunArrayClausePositional is the scenario that reproduces the original
// defect. See the file doc above.
func RunArrayClausePositional(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-array-clause-positional"
	const modelVersion = 1
	const sampleDoc = `{"name":"S","tags":["A","B"],"obj":{"0":"Z"}}`
	setupModelWithWorkflow(t, c, modelName, modelVersion, sampleDoc, searchWorkflowJSON)

	entityID, err := c.CreateEntity(t, modelName, modelVersion, sampleDoc)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	cases := []struct {
		name      string
		condition string
		wantCount int
	}{
		{"array clause position 0", `{"type":"array","jsonPath":"$.tags[*]","values":["A"]}`, 1},
		{"array clause position 1", `{"type":"array","jsonPath":"$.tags[*]","values":[null,"B"]}`, 1},
		{"array clause wrong value", `{"type":"array","jsonPath":"$.tags[*]","values":["Z"]}`, 0},
		{"positional simple", `{"type":"simple","jsonPath":"$.tags[0]","operatorType":"EQUALS","value":"A"}`, 1},
		{"wildcard simple", `{"type":"simple","jsonPath":"$.tags[*]","operatorType":"EQUALS","value":"B"}`, 1},
		// The case a naive "numbers are always array indexes" rewrite
		// breaks: "obj" is an OBJECT holding a field literally named "0",
		// not an array. This must keep matching on every backend.
		{"numeric field name", `{"type":"simple","jsonPath":"$.obj.0","operatorType":"EQUALS","value":"Z"}`, 1},
	}

	for _, tc := range cases {
		status, body, err := c.SyncSearchRaw(t, modelName, modelVersion, tc.condition)
		if err != nil {
			t.Fatalf("[%s] SyncSearchRaw: %v", tc.name, err)
		}
		if status != http.StatusOK {
			t.Fatalf("[%s] condition %s: expected 200, got %d; body=%s", tc.name, tc.condition, status, body)
		}
		results, err := c.SyncSearch(t, modelName, modelVersion, tc.condition)
		if err != nil {
			t.Fatalf("[%s] SyncSearch: %v", tc.name, err)
		}
		if len(results) != tc.wantCount {
			t.Errorf("[%s] condition %s: want %d result(s), got %d: %+v", tc.name, tc.condition, tc.wantCount, len(results), results)
			continue
		}
		if tc.wantCount == 1 && results[0].Meta.ID != entityID.String() {
			t.Errorf("[%s] matched entity %s, want %s", tc.name, results[0].Meta.ID, entityID.String())
		}
	}

	// The addressing rule: a bare path addresses the array itself, not its
	// elements. "tags" is declared ONLY as an array branch here (no scalar
	// observation), so it carries no scalar comparison at all (section 6) —
	// a SIMPLE clause's scalar comparison on the bare path is refused
	// 400 INVALID_FIELD_PATH, not silently answered as a non-match. This is
	// the addressing rule showing up as a rejection rather than an empty
	// page: an implementation that unwrapped the bare path into its
	// elements would accept this and match, which is exactly the SQL/JSON
	// `lax`-mode behaviour section 3 says cyoda-go does not have.
	status, body, err := c.SyncSearchRaw(t, modelName, modelVersion,
		`{"type":"simple","jsonPath":"$.tags","operatorType":"EQUALS","value":"A"}`)
	if err != nil {
		t.Fatalf("SyncSearchRaw bare path scalar comparison: %v", err)
	}
	if status != http.StatusBadRequest {
		t.Fatalf("bare $.tags EQUALS \"A\": expected 400, got %d; body=%s", status, body)
	}
	if !containsErrorCode(body, "INVALID_FIELD_PATH") {
		t.Errorf("bare $.tags EQUALS \"A\": expected INVALID_FIELD_PATH, body=%s", body)
	}

	// The array clause's own rejections (section 8): a bare path carries no
	// positional elements to test, so the clause is refused rather than
	// desugared against an out-of-range or nonexistent index.
	status, body, err = c.SyncSearchRaw(t, modelName, modelVersion,
		`{"type":"array","jsonPath":"$.tags","values":["A"]}`)
	if err != nil {
		t.Fatalf("SyncSearchRaw bare array clause: %v", err)
	}
	if status != http.StatusBadRequest {
		t.Fatalf("array clause on bare $.tags: expected 400, got %d; body=%s", status, body)
	}
	if !containsErrorCode(body, "INVALID_FIELD_PATH") {
		t.Errorf("array clause on bare $.tags: expected INVALID_FIELD_PATH, body=%s", body)
	}

	// An array position is not a grouping dimension (section 7): the
	// grouped-stats groupBy surface admits no subscript at all, so a dotted
	// numeric segment ("tags.0") is the only spelling that would even be
	// grammatical there, and a bracket position is refused outright.
	gStatus, gBody, gErr := c.QueryGroupedStatsRaw(t, modelName, modelVersion, client.GroupedStatsRequest{
		GroupBy: []string{"$.tags[0]"},
	})
	if gErr != nil {
		t.Fatalf("QueryGroupedStatsRaw: %v", gErr)
	}
	if gStatus != http.StatusBadRequest {
		t.Fatalf("groupBy $.tags[0]: expected 400, got %d; body=%s", gStatus, gBody)
	}
	if !containsErrorCode(gBody, "INVALID_GROUP_BY_PATH") {
		t.Errorf("groupBy $.tags[0]: expected INVALID_GROUP_BY_PATH, body=%s", gBody)
	}
}

// RunPathAddressingByDeclaredShape pins section 4's union rule: a path is
// valid when it is a valid statement for AT LEAST ONE branch a field is
// declared as, and per entity the predicate applies to the branch that
// entity's data actually is — a non-match, not an error, where the path does
// not fit that entity's branch.
//
// The model here declares field "a" as string | array-of-string, from a
// sample-data collection carrying one document of each shape. Two entities
// are created, one per branch, and each condition below is asserted against
// BOTH at once so the result set — not just "did it error" — proves which
// branch answered.
func RunPathAddressingByDeclaredShape(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-path-addressing-shape"
	const modelVersion = 1
	setupModelWithWorkflow(t, c, modelName, modelVersion,
		`[{"a":"A"},{"a":["A","B"]}]`, searchWorkflowJSON)

	strID, err := c.CreateEntity(t, modelName, modelVersion, `{"a":"A"}`)
	if err != nil {
		t.Fatalf("CreateEntity (string branch): %v", err)
	}
	arrID, err := c.CreateEntity(t, modelName, modelVersion, `{"a":["A","B"]}`)
	if err != nil {
		t.Fatalf("CreateEntity (array branch): %v", err)
	}

	for _, tc := range []struct {
		name string
		cond string
		want []string
	}{
		// "$.a EQUALS "A"": valid for the string branch, not a valid
		// statement for the array branch.
		{"bare EQUALS matches the string branch only", `{"type":"simple","jsonPath":"$.a","operatorType":"EQUALS","value":"A"}`, []string{strID.String()}},
		// "$.a[*] EQUALS "A"": the inverse — [*] is not a valid statement
		// for the string branch, and matches an element of the array branch.
		{"wildcard EQUALS matches the array branch only", `{"type":"simple","jsonPath":"$.a[*]","operatorType":"EQUALS","value":"A"}`, []string{arrID.String()}},
		// "$.a NOT_NULL": valid for both branches at once.
		{"bare NOT_NULL matches both branches", `{"type":"simple","jsonPath":"$.a","operatorType":"NOT_NULL","value":null}`, []string{strID.String(), arrID.String()}},
	} {
		status, body, err := c.SyncSearchRaw(t, modelName, modelVersion, tc.cond)
		if err != nil {
			t.Fatalf("[%s] SyncSearchRaw: %v", tc.name, err)
		}
		if status != http.StatusOK {
			t.Fatalf("[%s] expected 200, got %d; body=%s", tc.name, status, body)
		}
		results, err := c.SyncSearch(t, modelName, modelVersion, tc.cond)
		if err != nil {
			t.Fatalf("[%s] SyncSearch: %v", tc.name, err)
		}
		assertResultIDSet(t, tc.name, results, tc.want)
	}
}

// RunPathVacuity pins section 5's table exactly: for a field "a" declared
// array-of-string, presence and nullness answer differently for a bare path,
// a wildcard path, and a positional path, and none of the three collapse
// into another across an entity holding a populated array, an empty array,
// an explicit null, and the field absent entirely.
func RunPathVacuity(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-path-vacuity"
	const modelVersion = 1
	// The sample-data collection carries a null observation for "a" as well
	// as a populated one, so the model records "a" as nullable — without
	// that, an explicit null for a declared field is refused at ingest, and
	// the null row of the table below could never be constructed.
	setupModelWithWorkflow(t, c, modelName, modelVersion,
		`[{"name":"seed","a":["s"]},{"name":"seed2","a":null}]`, searchWorkflowJSON)

	hasID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"has","a":["A"]}`)
	if err != nil {
		t.Fatalf("CreateEntity (populated array): %v", err)
	}
	emptyID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"empty","a":[]}`)
	if err != nil {
		t.Fatalf("CreateEntity (empty array): %v", err)
	}
	nullID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"null","a":null}`)
	if err != nil {
		t.Fatalf("CreateEntity (explicit null): %v", err)
	}
	absentID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"absent"}`)
	if err != nil {
		t.Fatalf("CreateEntity (absent field): %v", err)
	}

	for _, tc := range []struct {
		name string
		cond string
		want []string
	}{
		// A bare path addresses the array itself, which exists when it is
		// empty — so NOT_NULL holds for [] too, and IS_NULL separates only
		// the two states with no array value at all (null, absent).
		{"$.a NOT_NULL", `{"type":"simple","jsonPath":"$.a","operatorType":"NOT_NULL","value":null}`,
			[]string{hasID.String(), emptyID.String()}},
		{"$.a IS_NULL", `{"type":"simple","jsonPath":"$.a","operatorType":"IS_NULL","value":null}`,
			[]string{nullID.String(), absentID.String()}},
		// A wildcard path addresses elements only. An empty array, an
		// explicit null and an absent field present no elements at all, so
		// NOT_NULL is false for all three, and — the table's sharpest
		// row — IS_NULL is false for EVERY state including the populated
		// array: IS_NULL and NOT_NULL are complements only where at least
		// one element exists.
		{"$.a[*] NOT_NULL", `{"type":"simple","jsonPath":"$.a[*]","operatorType":"NOT_NULL","value":null}`,
			[]string{hasID.String()}},
		{"$.a[*] IS_NULL", `{"type":"simple","jsonPath":"$.a[*]","operatorType":"IS_NULL","value":null}`,
			nil},
		// A positional path addresses exactly one position, which is
		// present only for the populated array — so IS_NULL holds for the
		// other three states, unlike the wildcard row above.
		{"$.a[0] IS_NULL", `{"type":"simple","jsonPath":"$.a[0]","operatorType":"IS_NULL","value":null}`,
			[]string{emptyID.String(), nullID.String(), absentID.String()}},
	} {
		status, body, err := c.SyncSearchRaw(t, modelName, modelVersion, tc.cond)
		if err != nil {
			t.Fatalf("[%s] SyncSearchRaw: %v", tc.name, err)
		}
		if status != http.StatusOK {
			t.Fatalf("[%s] expected 200, got %d; body=%s", tc.name, status, body)
		}
		results, err := c.SyncSearch(t, modelName, modelVersion, tc.cond)
		if err != nil {
			t.Fatalf("[%s] SyncSearch: %v", tc.name, err)
		}
		assertResultIDSet(t, tc.name, results, tc.want)
	}
}
