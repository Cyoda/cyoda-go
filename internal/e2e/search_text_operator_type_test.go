package e2e_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
)

// search_text_operator_type_test.go: a string or pattern operator on a field
// with no text type can never match a stored value. Every surface that carries
// a condition refuses it with 400 CONDITION_TYPE_MISMATCH, instead of answering
// an empty result (or, for a negated operator, every entity).
//
// setupSearchModel's entities declare $.amount as a number and $.name as text.

const containsOnAmount = `{"type":"simple","jsonPath":"$.amount","operatorType":"ICONTAINS","value":"1"}`

// --- Direct (sync) search ---

func TestSearch_Sync_TextOperatorOnNumericField_ConditionTypeMismatch(t *testing.T) {
	const model = "e2e-search-textop-sync"
	setupSearchModel(t, model)
	createEntityE2E(t, model, 1, `{"name":"Alice","amount":100,"status":"active"}`)

	for _, cond := range []string{
		containsOnAmount,
		`{"type":"simple","jsonPath":"$.amount","operatorType":"NOT_CONTAINS","value":"7"}`,
		`{"type":"simple","jsonPath":"$.amount","operatorType":"LIKE","value":"1%"}`,
		`{"type":"simple","jsonPath":"$.amount","operatorType":"MATCHES_PATTERN","value":"1.*"}`,
	} {
		resp := doAuth(t, http.MethodPost, fmt.Sprintf("/api/search/direct/%s/1", model), cond)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: expected 400, got %d; body: %s", cond, resp.StatusCode, readBody(t, resp))
		}
		commontest.ExpectErrorCode(t, resp, "CONDITION_TYPE_MISMATCH")
		resp.Body.Close()
	}
}

// TestSearch_Sync_TextOperatorOnTextField_Accepted: the same operator on a
// text field still answers — the refusal is about the field's type only.
func TestSearch_Sync_TextOperatorOnTextField_Accepted(t *testing.T) {
	const model = "e2e-search-textop-sync-text"
	setupSearchModel(t, model)
	createEntityE2E(t, model, 1, `{"name":"Alice","amount":100,"status":"active"}`)
	createEntityE2E(t, model, 1, `{"name":"Bob","amount":50,"status":"active"}`)

	status, results := directSearch(t, model, 1,
		`{"type":"simple","jsonPath":"$.name","operatorType":"ICONTAINS","value":"ali"}`)
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	if len(results) != 1 || extractDataString(t, results[0], "name") != "Alice" {
		t.Fatalf("expected exactly Alice, got %v", results)
	}
}

// --- Async submit ---

func TestSearch_AsyncSubmit_TextOperatorOnNumericField_ConditionTypeMismatch_NoJobIssued(t *testing.T) {
	const model = "e2e-search-textop-async"
	setupSearchModel(t, model)
	createEntityE2E(t, model, 1, `{"name":"Alice","amount":100,"status":"active"}`)

	resp := doAuth(t, http.MethodPost, fmt.Sprintf("/api/search/async/%s/1", model), containsOnAmount)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d; body: %s", resp.StatusCode, readBody(t, resp))
	}
	commontest.ExpectErrorCode(t, resp, "CONDITION_TYPE_MISMATCH")

	count := queryDB(t, "test-tenant", "SELECT count(*) FROM search_jobs WHERE model_name = $1", model)
	if count != 0 {
		t.Errorf("rejected condition issued %d search job(s) for model %q", count, model)
	}
}

// --- Conditional delete ---

// A negated text operator is the dangerous case on delete: before the
// refusal, NOT_CONTAINS on a numeric field matched every entity holding a
// number there, so the delete removed them all.
func TestDeleteConditional_TextOperatorOnNumericField_ConditionTypeMismatch_NothingDeleted(t *testing.T) {
	const model = "e2e-delete-textop"
	setupSearchModel(t, model)
	entityID := createEntityE2E(t, model, 1, `{"name":"Alice","amount":100,"status":"active"}`)

	resp := doAuth(t, http.MethodDelete, fmt.Sprintf("/api/entity/%s/1", model),
		`{"type":"simple","jsonPath":"$.amount","operatorType":"NOT_CONTAINS","value":"7"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d; body: %s", resp.StatusCode, readBody(t, resp))
	}
	commontest.ExpectErrorCode(t, resp, "CONDITION_TYPE_MISMATCH")

	if r := doAuth(t, http.MethodGet, "/api/entity/"+entityID, ""); r.StatusCode != http.StatusOK {
		t.Errorf("entity %s must survive a rejected delete, got %d", entityID, r.StatusCode)
	}
}

// --- Grouped stats ---

func TestGroupedStats_TextOperatorOnNumericField_ConditionTypeMismatch(t *testing.T) {
	const model = "e2e-stats-textop"
	setupStatsModel(t, model)
	createEntityE2E(t, model, 1, `{"variantId":"v1","price":10.0}`)

	reqBody := `{"groupBy":["$.variantId"],"condition":` +
		`{"type":"simple","jsonPath":"$.price","operatorType":"STARTS_WITH","value":"1"}}`
	resp := doAuth(t, http.MethodPost, fmt.Sprintf("/api/entity/stats/%s/1/query", model), reqBody)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d; body: %s", resp.StatusCode, readBody(t, resp))
	}
	commontest.ExpectErrorCode(t, resp, "CONDITION_TYPE_MISMATCH")
}

// --- Workflow criterion ---

// A criterion is checked against the model when it is evaluated, not at
// import. A text operator on a numeric field used to read as "not satisfied";
// it now fails the evaluation, so the save that triggered it is aborted and
// rolled back.
func TestWorkflowCriterion_TextOperatorOnNumericField_AbortsCreationAndRollsBack(t *testing.T) {
	const model = "e2e-crit-textop"
	setupModelWithWorkflow(t, model, `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "textop-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false,
					"criterion": {"type":"simple","jsonPath":"$.amount","operatorType":"NOT_CONTAINS","value":"7"}
				}]},
				"CREATED": {}
			}
		}]
	}`)

	resp := doAuth(t, http.MethodPost, fmt.Sprintf("/api/entity/JSON/%s/1", model), `{"name":"Test","amount":10,"status":"new"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", resp.StatusCode, readBody(t, resp))
	}
	commontest.ExpectErrorCode(t, resp, "WORKFLOW_FAILED")
	readBody(t, resp)

	if count := queryDB(t, "test-tenant", "SELECT count(*) FROM entities WHERE model_name = $1", model); count != 0 {
		t.Errorf("expected 0 entities after the criterion aborted the save, got %d", count)
	}
}

// --- Ordering operator on an unordered field ---

// A boolean has no order the kernel compares by: GREATER_THAN "false" used to
// answer no entity at all, even for one holding true.
func TestSearch_Sync_OrderingOperatorOnBooleanField_ConditionTypeMismatch(t *testing.T) {
	const model = "e2e-search-ordop-bool"
	setupModelSampleWithWorkflow(t, model, `{"name":"Test","amount":1,"status":"new","active":true}`, `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "ordop-wf", "initialState": "NONE", "active": true,
			"states": {"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]}, "CREATED": {}}
		}]
	}`)
	createEntityE2E(t, model, 1, `{"name":"Alice","amount":100,"status":"active","active":true}`)

	for _, cond := range []string{
		`{"type":"simple","jsonPath":"$.active","operatorType":"GREATER_THAN","value":"false"}`,
		`{"type":"simple","jsonPath":"$.active","operatorType":"BETWEEN_INCLUSIVE","value":[false,true]}`,
	} {
		resp := doAuth(t, http.MethodPost, fmt.Sprintf("/api/search/direct/%s/1", model), cond)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: expected 400, got %d; body: %s", cond, resp.StatusCode, readBody(t, resp))
		}
		commontest.ExpectErrorCode(t, resp, "CONDITION_TYPE_MISMATCH")
		resp.Body.Close()
	}

	// Equality needs no order and still answers.
	status, results := directSearch(t, model, 1, `{"type":"simple","jsonPath":"$.active","operatorType":"EQUALS","value":true}`)
	if status != http.StatusOK || len(results) != 1 {
		t.Fatalf("EQUALS true: expected 200 with 1 result, got %d with %d", status, len(results))
	}
}
