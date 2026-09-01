package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
)

// search_group_operator_not_test.go covers Task 12 of the NOT-node plan at
// the full HTTP stack: the search-condition surfaces (sync direct search,
// async submit, conditional delete, grouped stats) now accept a
// GroupCondition{Operator:"NOT"} with exactly one entry in `conditions`,
// and reject zero or two-or-more with 400 INVALID_CONDITION — the same
// classification search.StructuralConditionErrCode already gives every
// other structural condition failure on these surfaces (see
// search_unknown_operator_test.go and search_function_condition_test.go for
// the sibling coverage this file mirrors).
//
// notCondition/notArity build a NOT group wrapping the given inner
// condition JSON fragments.
func notCondition(inner string) string {
	return `{"type":"group","operator":"NOT","conditions":[` + inner + `]}`
}

const notInnerStatusInactive = `{"type":"simple","jsonPath":"$.status","operatorType":"EQUALS","value":"inactive"}`
const notInnerAmountOver1000 = `{"type":"simple","jsonPath":"$.amount","operatorType":"GREATER_THAN","value":1000}`

// --- Direct (sync) search ---

func TestSearch_Sync_GroupOperatorNOT_OneCondition_Accepted(t *testing.T) {
	const model = "e2e-search-not-sync-accept"
	setupSearchModel(t, model)
	createEntityE2E(t, model, 1, `{"name":"Alice","amount":100,"status":"active"}`)
	createEntityE2E(t, model, 1, `{"name":"Bob","amount":50,"status":"inactive"}`)

	status, results := directSearch(t, model, 1, notCondition(notInnerStatusInactive))
	if status != http.StatusOK {
		t.Fatalf("NOT(status==inactive): expected 200, got %d", status)
	}
	// Count alone does not discriminate NOT from the exact historical bug
	// this validator closes: a non-OR group operator silently mapped to
	// FilterAnd would answer AND(status==inactive) here, which ALSO returns
	// exactly one row — Bob, not Alice. Assert the identity, not just the
	// count.
	if len(results) != 1 {
		t.Fatalf("NOT(status==inactive): expected 1 result (Alice), got %d: %v", len(results), results)
	}
	if name := extractDataString(t, results[0], "name"); name != "Alice" {
		t.Errorf("NOT(status==inactive): expected Alice, got %q — a group operator silently folded to AND "+
			"would also return exactly 1 row here (Bob), so the count alone would not have caught it", name)
	}
}

func TestSearch_Sync_GroupOperatorNOT_ZeroConditions_Returns400(t *testing.T) {
	const model = "e2e-search-not-sync-zero"
	setupSearchModel(t, model)
	createEntityE2E(t, model, 1, `{"name":"Alice","amount":100,"status":"active"}`)

	status, _ := directSearch(t, model, 1, `{"type":"group","operator":"NOT","conditions":[]}`)
	if status != http.StatusBadRequest {
		t.Fatalf("NOT with zero conditions: expected 400, got %d", status)
	}
}

func TestSearch_Sync_GroupOperatorNOT_TwoConditions_Returns400(t *testing.T) {
	const model = "e2e-search-not-sync-two"
	setupSearchModel(t, model)
	createEntityE2E(t, model, 1, `{"name":"Alice","amount":100,"status":"active"}`)

	cond := notCondition(notInnerStatusInactive + "," + notInnerAmountOver1000)
	status, _ := directSearch(t, model, 1, cond)
	if status != http.StatusBadRequest {
		t.Fatalf("NOT with two conditions: expected 400, got %d", status)
	}
}

// TestSearch_Sync_GroupOperatorNOT_BadArity_ErrorCodeIsInvalidCondition pins
// the error code directly (directSearch discards the body on non-200), the
// way search_unknown_operator_test.go does for the unknown-operator case.
func TestSearch_Sync_GroupOperatorNOT_BadArity_ErrorCodeIsInvalidCondition(t *testing.T) {
	const model = "e2e-search-not-sync-code"
	setupSearchModel(t, model)
	createEntityE2E(t, model, 1, `{"name":"Alice","amount":100,"status":"active"}`)

	path := fmt.Sprintf("/api/search/direct/%s/1", model)
	resp := doAuth(t, http.MethodPost, path, `{"type":"group","operator":"NOT","conditions":[]}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d; body: %s", resp.StatusCode, readBody(t, resp))
	}
	commontest.ExpectErrorCode(t, resp, "INVALID_CONDITION")
}

// --- Async submit ---

func TestSearch_AsyncSubmit_GroupOperatorNOT_OneCondition_Accepted(t *testing.T) {
	const model = "e2e-search-not-async-accept"
	setupSearchModel(t, model)
	createEntityE2E(t, model, 1, `{"name":"Alice","amount":100,"status":"active"}`)
	createEntityE2E(t, model, 1, `{"name":"Bob","amount":50,"status":"inactive"}`)

	jobID := submitAsyncSearch(t, model, 1, notCondition(notInnerStatusInactive))
	finalStatus := waitForAsyncSearch(t, jobID, 10*time.Second)
	if finalStatus != "SUCCESSFUL" {
		t.Fatalf("expected SUCCESSFUL, got %q", finalStatus)
	}

	path := fmt.Sprintf("/api/search/async/%s?pageSize=10&pageNumber=0", jobID)
	resp := doAuth(t, http.MethodGet, path, "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("getAsyncSearchResults: expected 200, got %d: %s", resp.StatusCode, body)
	}
	var page map[string]any
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatalf("parse results page: %v; body: %s", err, body)
	}
	content, _ := page["content"].([]any)
	// Same identity check as the sync test above: a group operator silently
	// folded to AND would also answer exactly 1 row here (Bob), so the count
	// alone would not distinguish NOT from that historical bug.
	if len(content) != 1 {
		t.Fatalf("NOT(status==inactive) async: expected 1 result (Alice), got %d: %s", len(content), body)
	}
	row, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("content[0] is not a map[string]any: %T", content[0])
	}
	if name := extractDataString(t, row, "name"); name != "Alice" {
		t.Errorf("NOT(status==inactive) async: expected Alice, got %q", name)
	}
}

// TestSearch_AsyncSubmit_GroupOperatorNOT_ZeroConditions_Returns400_NoJobIssued
// asserts the async submit path's strongest guarantee: a rejected condition
// must never create a job record at all, not merely answer 400 while a job
// silently starts (or gets left behind) in the background. Queries
// search_jobs directly rather than relying on a subsequent 404/empty status
// lookup, which would not distinguish "no job created" from "job created but
// not found by that lookup".
func TestSearch_AsyncSubmit_GroupOperatorNOT_ZeroConditions_Returns400_NoJobIssued(t *testing.T) {
	const model = "e2e-search-not-async-zero"
	setupSearchModel(t, model)
	createEntityE2E(t, model, 1, `{"name":"Alice","amount":100,"status":"active"}`)

	path := fmt.Sprintf("/api/search/async/%s/1", model)
	resp := doAuth(t, http.MethodPost, path, `{"type":"group","operator":"NOT","conditions":[]}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d; body: %s", resp.StatusCode, readBody(t, resp))
	}
	commontest.ExpectErrorCode(t, resp, "INVALID_CONDITION")

	count := queryDB(t, "test-tenant", "SELECT count(*) FROM search_jobs WHERE model_name = $1", model)
	if count != 0 {
		t.Errorf("NOT with zero conditions rejected, but %d search_jobs row(s) exist for model %q — "+
			"a rejected condition must never issue a job", count, model)
	}
}

// TestSearch_AsyncSubmit_GroupOperatorNOT_TwoConditions_Returns400_NoJobIssued
// is the sibling arity failure for the same no-job-issued guarantee.
func TestSearch_AsyncSubmit_GroupOperatorNOT_TwoConditions_Returns400_NoJobIssued(t *testing.T) {
	const model = "e2e-search-not-async-two"
	setupSearchModel(t, model)
	createEntityE2E(t, model, 1, `{"name":"Alice","amount":100,"status":"active"}`)

	cond := notCondition(notInnerStatusInactive + "," + notInnerAmountOver1000)
	path := fmt.Sprintf("/api/search/async/%s/1", model)
	resp := doAuth(t, http.MethodPost, path, cond)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d; body: %s", resp.StatusCode, readBody(t, resp))
	}
	commontest.ExpectErrorCode(t, resp, "INVALID_CONDITION")

	count := queryDB(t, "test-tenant", "SELECT count(*) FROM search_jobs WHERE model_name = $1", model)
	if count != 0 {
		t.Errorf("NOT with two conditions rejected, but %d search_jobs row(s) exist for model %q — "+
			"a rejected condition must never issue a job", count, model)
	}
}

// --- Conditional delete ---

func TestDeleteConditional_GroupOperatorNOT_OneCondition_Accepted(t *testing.T) {
	const model = "e2e-delete-not-accept"
	setupSearchModel(t, model)
	keepID := createEntityE2E(t, model, 1, `{"name":"Alice","amount":100,"status":"active"}`)
	dropID := createEntityE2E(t, model, 1, `{"name":"Bob","amount":50,"status":"inactive"}`)

	// Delete everything that is NOT status==active, i.e. drop the inactive one.
	cond := notCondition(`{"type":"simple","jsonPath":"$.status","operatorType":"EQUALS","value":"active"}`)
	path := fmt.Sprintf("/api/entity/%s/1", model)
	resp := doAuth(t, http.MethodDelete, path, cond)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("NOT(status==active) delete: expected 200, got %d: %s", resp.StatusCode, body)
	}

	if r := doAuth(t, http.MethodGet, "/api/entity/"+keepID, ""); r.StatusCode != http.StatusOK {
		t.Errorf("kept entity (Alice, status==active) should survive, got %d", r.StatusCode)
	}
	if r := doAuth(t, http.MethodGet, "/api/entity/"+dropID, ""); r.StatusCode != http.StatusNotFound {
		t.Errorf("dropped entity (Bob, status==inactive) should be gone, got %d", r.StatusCode)
	}
}

func TestDeleteConditional_GroupOperatorNOT_ZeroConditions_Returns400(t *testing.T) {
	const model = "e2e-delete-not-zero"
	setupSearchModel(t, model)
	entityID := createEntityE2E(t, model, 1, `{"name":"Alice","amount":100,"status":"active"}`)

	path := fmt.Sprintf("/api/entity/%s/1", model)
	resp := doAuth(t, http.MethodDelete, path, `{"type":"group","operator":"NOT","conditions":[]}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("NOT with zero conditions: expected 400, got %d: %s", resp.StatusCode, readBody(t, resp))
	}
	commontest.ExpectErrorCode(t, resp, "INVALID_CONDITION")

	if r := doAuth(t, http.MethodGet, "/api/entity/"+entityID, ""); r.StatusCode != http.StatusOK {
		t.Errorf("a rejected delete condition must delete nothing; entity lookup got %d", r.StatusCode)
	}
}

func TestDeleteConditional_GroupOperatorNOT_TwoConditions_Returns400(t *testing.T) {
	const model = "e2e-delete-not-two"
	setupSearchModel(t, model)
	entityID := createEntityE2E(t, model, 1, `{"name":"Alice","amount":100,"status":"active"}`)

	cond := notCondition(notInnerStatusInactive + "," + notInnerAmountOver1000)
	path := fmt.Sprintf("/api/entity/%s/1", model)
	resp := doAuth(t, http.MethodDelete, path, cond)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("NOT with two conditions: expected 400, got %d: %s", resp.StatusCode, readBody(t, resp))
	}
	commontest.ExpectErrorCode(t, resp, "INVALID_CONDITION")

	if r := doAuth(t, http.MethodGet, "/api/entity/"+entityID, ""); r.StatusCode != http.StatusOK {
		t.Errorf("a rejected delete condition must delete nothing; entity lookup got %d", r.StatusCode)
	}
}

// --- Grouped stats ---

func TestGroupedStats_GroupOperatorNOT_OneCondition_Accepted(t *testing.T) {
	const model = "e2e-stats-not-accept"
	setupStatsModel(t, model)
	createEntityE2E(t, model, 1, `{"variantId":"v1","price":10.0}`)
	createEntityE2E(t, model, 1, `{"variantId":"v2","price":8.0}`)

	// Exclude the v2 rows from the grouping via NOT.
	reqBody := `{"groupBy":["$.variantId"],"condition":` +
		notCondition(`{"type":"simple","jsonPath":"$.variantId","operatorType":"EQUALS","value":"v2"}`) + `}`
	path := fmt.Sprintf("/api/entity/stats/%s/1/query", model)
	resp := doAuth(t, http.MethodPost, path, reqBody)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("grouped-stats NOT: expected 200, got %d: %s", resp.StatusCode, body)
	}
	buckets := decodeBuckets(t, body)
	if len(buckets) != 1 {
		t.Fatalf("expected 1 bucket (v1 only), got %d: %s", len(buckets), body)
	}
	if findBucket(buckets, "$.variantId", "v1") == nil {
		t.Errorf("expected the surviving bucket to be v1; buckets=%s", body)
	}
}

func TestGroupedStats_GroupOperatorNOT_ZeroConditions_Returns400(t *testing.T) {
	const model = "e2e-stats-not-zero"
	setupStatsModel(t, model)
	createEntityE2E(t, model, 1, `{"variantId":"v1","price":10.0}`)

	reqBody := `{"groupBy":["$.variantId"],"condition":{"type":"group","operator":"NOT","conditions":[]}}`
	path := fmt.Sprintf("/api/entity/stats/%s/1/query", model)
	resp := doAuth(t, http.MethodPost, path, reqBody)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("NOT with zero conditions: expected 400, got %d: %s", resp.StatusCode, readBody(t, resp))
	}
	commontest.ExpectErrorCode(t, resp, "INVALID_CONDITION")
}

func TestGroupedStats_GroupOperatorNOT_TwoConditions_Returns400(t *testing.T) {
	const model = "e2e-stats-not-two"
	setupStatsModel(t, model)
	createEntityE2E(t, model, 1, `{"variantId":"v1","price":10.0}`)

	cond := notCondition(
		`{"type":"simple","jsonPath":"$.variantId","operatorType":"EQUALS","value":"v2"}` + "," +
			`{"type":"simple","jsonPath":"$.price","operatorType":"GREATER_THAN","value":1000}`)
	reqBody := `{"groupBy":["$.variantId"],"condition":` + cond + `}`
	path := fmt.Sprintf("/api/entity/stats/%s/1/query", model)
	resp := doAuth(t, http.MethodPost, path, reqBody)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("NOT with two conditions: expected 400, got %d: %s", resp.StatusCode, readBody(t, resp))
	}
	commontest.ExpectErrorCode(t, resp, "INVALID_CONDITION")
}
