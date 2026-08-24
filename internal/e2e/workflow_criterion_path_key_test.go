package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
)

// ---------------------------------------------------------------------------
// Workflow-criterion field-path coverage, through the full HTTP stack.
//
// A criterion's jsonPath is JSON Path nomenclature, the same model syntax a
// search condition uses. Search rejects a path outside the grammar at its API
// boundary; a criterion evaluates through the in-process predicate evaluator
// and never through the pushdown translator, so nothing rejected one — a bare
// "amount" imported cleanly and fired transitions, and the two spellings of one
// syntax disagreed on which paths exist.
//
// Import is now the boundary that refuses it: 400 VALIDATION_FAILED, the same
// code and shape every other import-time criterion rejection uses. Import is
// the only boundary a criterion crosses; failing at evaluation time instead
// would fail a save, repeatedly, long after the workflow was accepted.
//
// Array subscripts stay valid — criteria evaluate in memory, which resolves
// them — so the accept control below covers "$.tags[*].name" and "$.arr[0]".
// ---------------------------------------------------------------------------

// dataCriterionWorkflow builds a workflow whose CREATED state has one automated
// transition to ADVANCED, gated by a SimpleCondition comparing a data field.
// NONE -> CREATED is unconditioned and automated, so creating an entity
// cascades through CREATED and evaluates the guarded transition in the same
// request. jsonPath is supplied verbatim so a test can vary the spelling.
func dataCriterionWorkflow(t *testing.T, wfName, jsonPath string) string {
	t.Helper()
	criterion, err := json.Marshal(map[string]any{
		"type":         "simple",
		"jsonPath":     jsonPath,
		"operatorType": "GREATER_THAN",
		"value":        50,
	})
	if err != nil {
		t.Fatalf("marshal simple criterion: %v", err)
	}
	return fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": %q, "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {"transitions": [{"name": "advance", "next": "ADVANCED", "manual": false,
					"criterion": %s}]},
				"ADVANCED": {}
			}
		}]
	}`, wfName, string(criterion))
}

// setupCriterionPathModel imports and locks a model carrying the fields the
// criterion spellings below address, without importing a workflow — the tests
// import their own.
func setupCriterionPathModel(t *testing.T, model string) {
	t.Helper()
	importModelSampleE2E(t, model, 1,
		`{"name":"Sample","amount":0,"nested":{"inner":"x"},"tags":["a"],"arr":[1,2]}`)
	lockModelE2E(t, model, 1)
}

// TestCriterionPath_NonJSONPathRejectedAtImport is the core proof: a criterion
// path outside the grammar is refused when the workflow is imported, with the
// status and error code the import endpoint documents for a validation
// failure. Each spelling addresses a field that genuinely exists in the model —
// the point is that the SPELLING is not JSON Path, so "the field is there" is
// not a reason to accept it.
func TestCriterionPath_NonJSONPathRejectedAtImport(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	const model = "e2e-crit-path-reject"
	setupCriterionPathModel(t, model)

	for _, tc := range nonJSONPathSpellings {
		t.Run(tc.name, func(t *testing.T) {
			wf := dataCriterionWorkflow(t, "crit-path-reject-wf", tc.path)
			path := fmt.Sprintf("/api/model/%s/1/workflow/import", model)
			resp := doAuth(t, http.MethodPost, path, wf)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("criterion jsonPath %q: expected 400, got %d: %s",
					tc.path, resp.StatusCode, readBody(t, resp))
			}
			commontest.ExpectErrorCode(t, resp, "VALIDATION_FAILED")
		})
	}
}

// TestCriterionPath_ValidPathImportsAndFiresTransition is the positive control
// that keeps the tightening honest, and preserves what the pre-tightening test
// pinned: a criterion on a well-formed path imports AND fires the transition
// for an entity that satisfies it.
func TestCriterionPath_ValidPathImportsAndFiresTransition(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	const model = "e2e-crit-path-prefixed"

	setupModelWithWorkflow(t, model, dataCriterionWorkflow(t, "crit-path-prefixed-wf", "$.amount"))
	entityID := createEntityE2E(t, model, 1, `{"name":"X","amount":100}`)

	if state := getEntityState(t, entityID); state != "ADVANCED" {
		t.Errorf("criterion on \"$.amount\" with amount=100 > 50: expected ADVANCED, got %q", state)
	}
}

// And the criterion must still be able to evaluate FALSE. Without this, a
// change that made every leaf match unconditionally would pass the guard above.
func TestCriterionPath_ValidPathDoesNotFireBelowThreshold(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	const model = "e2e-crit-path-prefixed-false"

	setupModelWithWorkflow(t, model, dataCriterionWorkflow(t, "crit-path-prefixed-false-wf", "$.amount"))
	entityID := createEntityE2E(t, model, 1, `{"name":"X","amount":10}`)

	if state := getEntityState(t, entityID); state != "CREATED" {
		t.Errorf("criterion on \"$.amount\" with amount=10 < 50: expected CREATED (criterion false, transition does not fire), got %q", state)
	}
}

// TestCriterionPath_ArraySubscriptPathAcceptedAtImport pins that the
// tightening uses the CONDITION grammar, not the scalar one. An array
// subscript is valid JSON Path that no pushdown filter expresses — and a
// criterion is only ever served by the in-process evaluator, which resolves
// it — so rejecting it here would break working workflows.
func TestCriterionPath_ArraySubscriptPathAcceptedAtImport(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	const model = "e2e-crit-path-subscript"
	setupCriterionPathModel(t, model)

	for _, path := range []string{"$.tags[*]", "$.arr[0]", "$.nested.inner"} {
		t.Run(path, func(t *testing.T) {
			status, body := importWorkflowE2E(t, model, 1,
				dataCriterionWorkflow(t, "crit-path-subscript-wf", path))
			if status != http.StatusOK {
				t.Fatalf("criterion jsonPath %q: expected 200, got %d: %s", path, status, body)
			}
		})
	}
}

// TestCriterionPath_NumericSubscriptFiresTransition closes the loop with the
// in-memory evaluator: a numeric subscript is accepted at import AND resolves
// to the value the entity actually holds, so the transition fires. It used to
// import and then silently never fire — the evaluator handed gjson a path it
// has no syntax for ("arr[0]" rather than "arr.0") and resolved the declared
// type against a FieldsMap key that does not exist ("$.arr[0]" rather than
// "$.arr[*]"), so the leaf was false for every entity.
func TestCriterionPath_NumericSubscriptFiresTransition(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	const model = "e2e-crit-path-subscript-fires"
	importModelSampleE2E(t, model, 1, `{"name":"Sample","amount":0,"arr":[0,0]}`)
	lockModelE2E(t, model, 1)

	status, body := importWorkflowE2E(t, model, 1,
		dataCriterionWorkflow(t, "crit-path-subscript-fires-wf", "$.arr[1]"))
	if status != http.StatusOK {
		t.Fatalf("workflow import: expected 200, got %d: %s", status, body)
	}

	// arr[1] is 100, comfortably over the threshold of 50; arr[0] is 1, under
	// it — so a rewrite that addressed the wrong element would not pass either.
	entityID := createEntityE2E(t, model, 1, `{"name":"X","amount":0,"arr":[1,100]}`)
	if state := getEntityState(t, entityID); state != "ADVANCED" {
		t.Errorf(`criterion on "$.arr[1]" with arr=[1,100] and threshold 50: expected ADVANCED, got %q`, state)
	}

	below := createEntityE2E(t, model, 1, `{"name":"Y","amount":0,"arr":[100,1]}`)
	if state := getEntityState(t, below); state != "CREATED" {
		t.Errorf(`criterion on "$.arr[1]" with arr=[100,1]: expected CREATED (element 1 is 1, under the threshold), got %q`, state)
	}
}

// TestCriterionPath_TrailingWildcardFiresTransition is the criterion half of the
// trailing-wildcard defect. "$.arr[*]" addresses the array's ELEMENTS, so the
// criterion asks "does SOME element exceed the threshold". It resolved to the
// array's COUNT instead — a two-element array compared 2 against the threshold —
// so the transition silently never fired, whatever the entity held. A criterion
// has no result page to look wrong: the entity simply sits in the state before
// the guard, which is why this needs its own end-to-end assertion.
func TestCriterionPath_TrailingWildcardFiresTransition(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	const model = "e2e-crit-path-trailing-wildcard"
	// The sample declares a 60-wide array so the "no element matches" entity
	// below can be 60 elements long without widening a locked model.
	sixty := "[" + strings.TrimSuffix(strings.Repeat("1,", 60), ",") + "]"
	importModelSampleE2E(t, model, 1,
		fmt.Sprintf(`{"name":"Sample","amount":0,"arr":%s}`, sixty))
	lockModelE2E(t, model, 1)

	status, body := importWorkflowE2E(t, model, 1,
		dataCriterionWorkflow(t, "crit-path-trailing-wildcard-wf", "$.arr[*]"))
	if status != http.StatusOK {
		t.Fatalf("workflow import: expected 200, got %d: %s", status, body)
	}

	// One element is over the threshold of 50, so the criterion is true. The
	// array's LENGTH is 2 — under the threshold — so a count-valued leaf leaves
	// the entity in CREATED.
	fires := createEntityE2E(t, model, 1, `{"name":"X","amount":0,"arr":[1,100]}`)
	if state := getEntityState(t, fires); state != "ADVANCED" {
		t.Errorf(`criterion on "$.arr[*]" with arr=[1,100] and threshold 50: expected ADVANCED, got %q`, state)
	}

	// And it must still be able to evaluate FALSE: no element exceeds 50.
	// Sixty elements make the LENGTH exceed the threshold, so a count-valued
	// leaf fires here — the inverse of the case above, which is what makes the
	// pair evidence rather than a single-sided guard.
	quiet := createEntityE2E(t, model, 1,
		fmt.Sprintf(`{"name":"Y","amount":0,"arr":%s}`, sixty))
	if state := getEntityState(t, quiet); state != "CREATED" {
		t.Errorf(`criterion on "$.arr[*]" with 60 elements all equal to 1: expected CREATED (no element exceeds 50), got %q`, state)
	}
}
