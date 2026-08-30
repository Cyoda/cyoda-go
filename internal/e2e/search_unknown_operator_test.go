package e2e_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
)

// TestSearch_Sync_UnknownOperator_Returns400_InvalidCondition pins
// operator-semantics.md §4: "An operator name outside this set is 400
// INVALID_CONDITION, on every surface that carries a condition." Before this
// fix, an unknown operatorType on the sync search path answered 400
// BAD_REQUEST while the identical condition on grouped stats
// (POST .../stats/query) answered 400 INVALID_CONDITION — one error class,
// two codes depending only on which endpoint served the request.
func TestSearch_Sync_UnknownOperator_Returns400_InvalidCondition(t *testing.T) {
	const model = "e2e-search-unknown-operator"
	setupSearchModel(t, model)
	createEntityE2E(t, model, 1, `{"name":"Alice","amount":100,"status":"active"}`)

	const badCondition = `{"type":"simple","jsonPath":"$.name","operatorType":"NOT_EQUALS","value":"Alice"}`
	path := fmt.Sprintf("/api/search/direct/%s/%d", model, 1)
	resp := doAuth(t, http.MethodPost, path, badCondition)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	commontest.ExpectErrorCode(t, resp, "INVALID_CONDITION")
	body := readBody(t, resp)
	if !strings.Contains(body, "NOT_EQUALS") {
		t.Errorf("expected response detail to name the offending operator; body: %s", body)
	}
}

// TestSearch_AsyncSubmit_UnknownOperator_Returns400_InvalidCondition mirrors
// the sync case for the async submit path: no job should ever be created
// for an unknown operatorType.
func TestSearch_AsyncSubmit_UnknownOperator_Returns400_InvalidCondition(t *testing.T) {
	const model = "e2e-search-unknown-operator-async"
	setupSearchModel(t, model)
	createEntityE2E(t, model, 1, `{"name":"Bob","amount":42,"status":"active"}`)

	const badCondition = `{"type":"simple","jsonPath":"$.name","operatorType":"NOT_EQUALS","value":"Bob"}`
	path := fmt.Sprintf("/api/search/async/%s/1", model)
	resp := doAuth(t, http.MethodPost, path, badCondition)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		body := readBody(t, resp)
		t.Fatalf("expected 400, got %d; body: %s", resp.StatusCode, body)
	}
	commontest.ExpectErrorCode(t, resp, "INVALID_CONDITION")
}

// TestDeleteEntitiesConditional_UnknownOperator_Returns400_InvalidCondition
// covers the conditional-delete surface, which shares the same
// search.ValidateCondition boundary via search.StructuralConditionErrCode.
func TestDeleteEntitiesConditional_UnknownOperator_Returns400_InvalidCondition(t *testing.T) {
	const model = "e2e-delete-unknown-operator"
	setupSearchModel(t, model)
	createEntityE2E(t, model, 1, `{"name":"Carol","amount":10,"status":"active"}`)

	const badCondition = `{"type":"simple","jsonPath":"$.name","operatorType":"NOT_EQUALS","value":"Carol"}`
	path := fmt.Sprintf("/api/entity/%s/%d", model, 1)
	resp := doAuth(t, http.MethodDelete, path, badCondition)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		body := readBody(t, resp)
		t.Fatalf("expected 400, got %d; body: %s", resp.StatusCode, body)
	}
	commontest.ExpectErrorCode(t, resp, "INVALID_CONDITION")
}
