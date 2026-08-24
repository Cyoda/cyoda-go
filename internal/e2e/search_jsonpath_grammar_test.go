package e2e_test

// search_jsonpath_grammar_test.go — running-backend (postgres) e2e coverage
// for the condition `jsonPath` grammar enforced at the API boundary.
//
// A condition's jsonPath is JSON Path nomenclature, so the "$." leader is
// required. A bare "amount" is not a path and is now rejected 400
// INVALID_FIELD_PATH.
//
// Why this needs a running backend rather than a unit test: the rejection was
// INERT before it moved to the boundary. spi.ConditionToFilter already refused
// a bare path, but every engine call site treats a translate failure as "not
// pushdownable, fall back to in-memory evaluation" — and the in-memory
// evaluator resolves a bare path happily. So over real HTTP the request
// returned correct-looking results, having silently abandoned the pushdown
// plan for a full scan. Only an end-to-end assertion can show the difference,
// because both the old and new code paths are "reachable" in a unit test.
//
// Three HTTP entry points reach a condition->filter translation and are all
// covered here: sync search, async search, and conditional delete.

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
)

// nonJSONPathSpellings are rejected on every condition surface. Each addresses
// a field that genuinely exists in the model below — the point is that the
// SPELLING is not JSON Path, so "the field is there" is not a reason to accept
// it.
var nonJSONPathSpellings = []struct {
	name string
	path string
}{
	{"bare identifier", "amount"},
	{"bare dotted", "nested.inner"},
	{"leader only", "$."},
	{"bare dollar", "$"},
	{"bracket quoted", "$['amount']"},
	{"bracket quoted after leader", "$.['amount']"},
	{"trailing dot", "$.amount."},
	{"empty segment", "$..amount"},
	{"space", "$.am ount"},
	{"sql tail", "$.amount'; --"},
}

// setupGrammarModel imports a model with a numeric "amount", a nested object,
// and a string array, then locks it and attaches a trivial workflow.
func setupGrammarModel(t *testing.T, model string) {
	t.Helper()
	importPath := fmt.Sprintf("/api/model/import/JSON/SAMPLE_DATA/%s/1", model)
	resp := doAuth(t, http.MethodPost, importPath,
		`{"name":"Sample","amount":0,"nested":{"inner":"x"},"tags":["a"]}`)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("import model %s: expected 200, got %d: %s", model, resp.StatusCode, body)
	}
	lockModelE2E(t, model, 1)
	status, body := importWorkflowE2E(t, model, 1, `{
		"importMode": "REPLACE",
		"workflows": [{"version": "1.1", "name": "grammar-wf", "initialState": "NONE", "active": true,
			"states": {"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
			           "CREATED": {}}}]
	}`)
	if status != http.StatusOK {
		t.Fatalf("workflow import for %s: expected 200, got %d: %s", model, status, body)
	}
}

// TestSearch_NonJSONPathCondition_Returns400 is the core regression proof for
// the sync-search surface, and it asserts the before/after on the SAME data in
// the SAME test: the "$."-prefixed spelling selects the entity (200, one
// result), and the bare spelling of the identical query — which used to return
// exactly that same result via the in-memory fallback — is now 400
// INVALID_FIELD_PATH.
func TestSearch_NonJSONPathCondition_Returns400(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	const model = "e2e-search-jsonpath-grammar"
	setupGrammarModel(t, model)
	createEntityE2E(t, model, 1, `{"name":"Alice","amount":100,"nested":{"inner":"yes"},"tags":["x"]}`)
	createEntityE2E(t, model, 1, `{"name":"Bob","amount":5,"nested":{"inner":"no"},"tags":["y"]}`)

	// Before/after control: the JSON Path spelling still works.
	const goodCond = `{"type":"simple","jsonPath":"$.amount","operatorType":"GREATER_THAN","value":50}`
	status, results := directSearch(t, model, 1, goodCond)
	if status != http.StatusOK {
		t.Fatalf(`"$.amount" spelling: expected 200, got %d`, status)
	}
	if len(results) != 1 {
		t.Fatalf(`"$.amount" spelling: expected 1 result, got %d`, len(results))
	}

	// The bare spelling of that same query previously returned the same single
	// result — the translate failure fell back to in-memory evaluation, which
	// resolves "amount" happily. It must now be refused.
	for _, tc := range nonJSONPathSpellings {
		t.Run(tc.name, func(t *testing.T) {
			cond := fmt.Sprintf(
				`{"type":"simple","jsonPath":%q,"operatorType":"GREATER_THAN","value":50}`, tc.path)
			path := fmt.Sprintf("/api/search/direct/%s/1", model)
			resp := doAuth(t, http.MethodPost, path, cond)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("jsonPath %q: expected 400, got %d: %s",
					tc.path, resp.StatusCode, readBody(t, resp))
			}
			commontest.ExpectErrorCode(t, resp, "INVALID_FIELD_PATH")
		})
	}
}

// TestSearch_NonJSONPathCondition_NestedInGroup_Returns400 pins that the
// boundary walks the whole tree. The translator short-circuits on the FIRST
// child that fails, so a preceding non-pushdownable sibling ("$.tags[*]",
// which is valid JSON Path) would mask a malformed path behind it if the
// engine were relying on the translate error alone.
func TestSearch_NonJSONPathCondition_NestedInGroup_Returns400(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	const model = "e2e-search-jsonpath-nested"
	setupGrammarModel(t, model)
	createEntityE2E(t, model, 1, `{"name":"Alice","amount":100,"nested":{"inner":"yes"},"tags":["x"]}`)

	cond := `{"type":"group","operator":"AND","conditions":[
		{"type":"simple","jsonPath":"$.tags[*]","operatorType":"NOT_NULL","value":null},
		{"type":"simple","jsonPath":"amount","operatorType":"GREATER_THAN","value":50}
	]}`
	path := fmt.Sprintf("/api/search/direct/%s/1", model)
	resp := doAuth(t, http.MethodPost, path, cond)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", resp.StatusCode, readBody(t, resp))
	}
	commontest.ExpectErrorCode(t, resp, "INVALID_FIELD_PATH")
}

// TestSearch_ArraySubscriptPath_Still200 is the positive control that keeps
// the tightening honest. "$.tags[*]" is valid JSON Path that no pushdown
// filter can express; spi.ConditionToFilter refuses it with a PLAIN error
// (not ErrInvalidFilterPath) precisely so the engine falls back to in-memory
// evaluation. Rejecting every translate failure would have turned this working
// query into a 400 — the exact regression this test exists to catch.
func TestSearch_ArraySubscriptPath_Still200(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	const model = "e2e-search-jsonpath-subscript"
	setupGrammarModel(t, model)
	createEntityE2E(t, model, 1, `{"name":"Alice","amount":100,"nested":{"inner":"yes"},"tags":["x"]}`)
	createEntityE2E(t, model, 1, `{"name":"Bob","amount":5,"nested":{"inner":"no"},"tags":["y"]}`)

	// NOT_NULL and IS_NULL on the same subscripted path. The pair is what makes
	// this evidence: had the path failed to resolve, both would answer alike.
	for _, tc := range []struct {
		op   string
		want int
	}{
		{"NOT_NULL", 2},
		{"IS_NULL", 0},
	} {
		t.Run(tc.op, func(t *testing.T) {
			cond := fmt.Sprintf(
				`{"type":"simple","jsonPath":"$.tags[*]","operatorType":%q,"value":null}`, tc.op)
			status, results := directSearch(t, model, 1, cond)
			if status != http.StatusOK {
				t.Fatalf("array-subscript path answered %d, want 200 — it is valid JSON Path and must reach the in-memory fallback", status)
			}
			if len(results) != tc.want {
				t.Fatalf("%s on $.tags[*]: got %d results, want %d", tc.op, len(results), tc.want)
			}
		})
	}
}

// TestSearchAsync_NonJSONPathCondition_Returns400 covers the async submit
// surface. It matters on its own: SubmitAsync creates a job and answers 200
// with a job id, so an unvalidated bad path would be accepted here and fail
// (or silently fall back) in the background, out of the caller's sight.
func TestSearchAsync_NonJSONPathCondition_Returns400(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	const model = "e2e-search-jsonpath-async"
	setupGrammarModel(t, model)
	createEntityE2E(t, model, 1, `{"name":"Alice","amount":100,"nested":{"inner":"yes"},"tags":["x"]}`)

	path := fmt.Sprintf("/api/search/async/%s/1", model)
	for _, tc := range nonJSONPathSpellings {
		t.Run(tc.name, func(t *testing.T) {
			cond := fmt.Sprintf(
				`{"type":"simple","jsonPath":%q,"operatorType":"GREATER_THAN","value":50}`, tc.path)
			resp := doAuth(t, http.MethodPost, path, cond)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("jsonPath %q: expected 400, got %d: %s",
					tc.path, resp.StatusCode, readBody(t, resp))
			}
			commontest.ExpectErrorCode(t, resp, "INVALID_FIELD_PATH")
		})
	}
}

// TestDeleteEntities_NonJSONPathCondition_Returns400 covers the fourth
// translate site: conditional delete selects entities through its own
// spi.Iterable drain rather than through Search, so it replicates Search's
// pre-execution validation instead of inheriting it. This is the surface where
// a silent fallback is most dangerous — a bare path that quietly widened the
// selection would delete the wrong rows.
func TestDeleteEntities_NonJSONPathCondition_Returns400(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	const model = "e2e-delete-jsonpath-grammar"
	setupGrammarModel(t, model)
	createEntityE2E(t, model, 1, `{"name":"Alice","amount":100,"nested":{"inner":"yes"},"tags":["x"]}`)

	path := fmt.Sprintf("/api/entity/%s/1", model)
	for _, tc := range nonJSONPathSpellings {
		t.Run(tc.name, func(t *testing.T) {
			cond := fmt.Sprintf(
				`{"type":"simple","jsonPath":%q,"operatorType":"GREATER_THAN","value":50}`, tc.path)
			resp := doAuth(t, http.MethodDelete, path, cond)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("jsonPath %q: expected 400, got %d: %s",
					tc.path, resp.StatusCode, readBody(t, resp))
			}
			commontest.ExpectErrorCode(t, resp, "INVALID_FIELD_PATH")
		})
	}

	// The entity is still there — a rejected condition must not have deleted
	// anything.
	status, results := directSearch(t, model, 1,
		`{"type":"simple","jsonPath":"$.amount","operatorType":"GREATER_THAN","value":50}`)
	if status != http.StatusOK || len(results) != 1 {
		t.Fatalf("after rejected deletes: status %d, %d entities remain, want 200 and 1", status, len(results))
	}
}
