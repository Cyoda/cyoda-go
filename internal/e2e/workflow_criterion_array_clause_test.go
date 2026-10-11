package e2e_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
)

// arrayClauseCriterionWorkflow builds a workflow whose CREATED state has one
// automated transition to ADVANCED, gated by an ArrayCondition on jsonPath.
// Mirrors dataCriterionWorkflow (workflow_criterion_path_key_test.go) but for
// the "array" clause shape, which carries "values" (one operand per position)
// plus one "operatorType" instead of a single "value".
func arrayClauseCriterionWorkflow(wfName, jsonPath string) string {
	return fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": %q, "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {"transitions": [{"name": "advance", "next": "ADVANCED", "manual": false,
					"criterion": {"type":"array","jsonPath":%q,"operatorType":"EQUALS","values":["A"]}}]},
				"ADVANCED": {}
			}
		}]
	}`, wfName, jsonPath)
}

// TestCriterionArrayClause_BarePathRejectedAtImport is the sixth defect this
// batch closes: {"type":"array","jsonPath":"$.tags",...} is 400
// INVALID_FIELD_PATH on the search surface (path-grammar.md §8 requires a
// trailing "[*]" — a bare path addresses the container, not its elements),
// but workflow import called only search.ValidateConditionJSONPath, which is
// grammar-only, so the identical clause imported cleanly as a criterion — two
// doors, two answers for the same clause. Workflow import now enforces the
// same clause-shape rule via search.ValidateArrayClauseJSONPath, answering
// 400 VALIDATION_FAILED with detail naming the offending workflow and
// transition, matching this surface's existing convention.
func TestCriterionArrayClause_BarePathRejectedAtImport(t *testing.T) {
	const model = "e2e-crit-array-clause-bare-reject"
	importModelSampleE2E(t, model, 1, `{"name":"Sample","tags":["a"]}`)
	lockModelE2E(t, model, 1)

	path := fmt.Sprintf("/api/model/%s/1/workflow/import", model)
	resp := doAuth(t, http.MethodPost, path, arrayClauseCriterionWorkflow("crit-array-bare-wf", "$.tags"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		body := readBody(t, resp)
		t.Fatalf("expected 400, got %d: %s", resp.StatusCode, body)
	}
	commontest.ExpectErrorCode(t, resp, "VALIDATION_FAILED")
}

// TestCriterionArrayClause_PositionalOnlyPathRejectedAtImport covers the
// other bare-vs-positional distinction section 8 draws: a positional-only
// path ("$.tags[0]") names one element rather than testing by position
// across the whole array, so it is also refused for the array clause even
// though it is valid JSON Path.
func TestCriterionArrayClause_PositionalOnlyPathRejectedAtImport(t *testing.T) {
	const model = "e2e-crit-array-clause-positional-reject"
	importModelSampleE2E(t, model, 1, `{"name":"Sample","tags":["a"]}`)
	lockModelE2E(t, model, 1)

	path := fmt.Sprintf("/api/model/%s/1/workflow/import", model)
	resp := doAuth(t, http.MethodPost, path, arrayClauseCriterionWorkflow("crit-array-positional-wf", "$.tags[0]"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		body := readBody(t, resp)
		t.Fatalf("expected 400, got %d: %s", resp.StatusCode, body)
	}
	commontest.ExpectErrorCode(t, resp, "VALIDATION_FAILED")
}

// TestCriterionArrayClause_TrailingWildcardImportsAndFires is the positive
// control: an array clause whose path genuinely ends in "[*]" imports AND
// fires the transition for an entity whose array carries the matching
// element at position 0.
func TestCriterionArrayClause_TrailingWildcardImportsAndFires(t *testing.T) {
	const model = "e2e-crit-array-clause-accept"
	setupModelSampleWithWorkflow(t, model, `{"name":"Sample","tags":["a"]}`,
		arrayClauseCriterionWorkflow("crit-array-accept-wf", "$.tags[*]"))

	entityID := createEntityE2E(t, model, 1, `{"name":"X","tags":["A","B"]}`)
	if state := getEntityState(t, entityID); state != "ADVANCED" {
		t.Errorf(`array-clause criterion on "$.tags[*]" values=["A"] with tags=["A","B"]: expected ADVANCED, got %q`, state)
	}
}

func TestCriterionArrayClause_OperatorDecidesTheTransition(t *testing.T) {
	const model = "e2e-crit-array-op"
	wf := `{"importMode":"REPLACE","workflows":[{"version":"1.1","name":"crit-array-op-wf","initialState":"NONE","active":true,
		"states":{"NONE":{"transitions":[{"name":"init","next":"CREATED","manual":false}]},
		"CREATED":{"transitions":[{"name":"advance","next":"ADVANCED","manual":false,
			"criterion":{"type":"array","jsonPath":"$.scores[*]","operatorType":"GREATER_THAN","values":[50,null,70]}}]},
		"ADVANCED":{}}}]}`
	setupModelSampleWithWorkflow(t, model, `{"name":"S","scores":[1,2,3]}`, wf)
	if s := getEntityState(t, createEntityE2E(t, model, 1, `{"name":"A","scores":[90,40,75]}`)); s != "ADVANCED" {
		t.Errorf("A satisfies the criterion: want ADVANCED, got %q", s)
	}
	if s := getEntityState(t, createEntityE2E(t, model, 1, `{"name":"B","scores":[10,95,60]}`)); s != "CREATED" {
		t.Errorf("B does not: want CREATED, got %q", s)
	}
}

func TestCriterionArrayClause_RefusedAtImport(t *testing.T) {
	const model = "e2e-crit-array-refuse"
	importModelSampleE2E(t, model, 1, `{"name":"S","scores":[1]}`)
	lockModelE2E(t, model, 1)
	for _, crit := range []string{
		`{"type":"array","jsonPath":"$.scores[*]","values":[50]}`,
		`{"type":"array","jsonPath":"$.scores[*]","operatorType":"EQUALS","values":[null]}`,
		`{"type":"array","jsonPath":"$.o[*].scores[*]","operatorType":"EQUALS","values":[1]}`,
		`{"type":"array","jsonPath":"$.scores[*]","operatorType":"FROBNICATE","values":[50]}`,
		`{"type":"array","jsonPath":"$.scores[*]","operatorType":"EQUALS","values":[{"a":1}]}`,
		`{"type":"array","jsonPath":"$.scores[*]","operatorType":"BETWEEN","values":[5]}`,
		`{"type":"array","jsonPath":"$.scores[*]","operatorType":"EQUALS","values":"x"}`,
		`{"type":"array","jsonPath":"$.scores[*]","operatorType":"BETWEEN","values":[[null,5]]}`,
		`{"type":"array","jsonPath":"$.scores[*]","operatorType":"NOT_EQUAL","values":[[1,2]]}`,
		`{"type":"bogus"}`,
	} {
		wf := fmt.Sprintf(`{"importMode":"REPLACE","workflows":[{"version":"1.1","name":"w","initialState":"NONE","active":true,
			"states":{"NONE":{"transitions":[{"name":"go","next":"DONE","manual":false,"criterion":%s}]},"DONE":{}}}]}`, crit)
		resp := doAuth(t, http.MethodPost, fmt.Sprintf("/api/model/%s/1/workflow/import", model), wf)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: expected 400, got %d: %s", crit, resp.StatusCode, readBody(t, resp))
		}
		commontest.ExpectErrorCode(t, resp, "VALIDATION_FAILED")
		resp.Body.Close()
	}
}
