package e2e_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
)

// TestSearch_Sync_MalformedRegex_Returns400_InvalidCondition verifies that a
// MATCHES_PATTERN condition carrying an unparsable regex ("(" — unterminated
// group) is rejected with 400 INVALID_CONDITION on the sync search path,
// before any store is touched. This closes the fail-open regression left by
// Task 6's delegation to the error-free spi.MatchFilter: a malformed pattern
// must never reach the matcher on any backend (memory/sqlite/postgres) — the
// domain-layer validation runs identically for all of them.
func TestSearch_Sync_MalformedRegex_Returns400_InvalidCondition(t *testing.T) {
	const model = "e2e-search-regex-invalid"
	setupSearchModel(t, model)
	createEntityE2E(t, model, 1, `{"name":"Alice","amount":100,"status":"active"}`)

	const badCondition = `{"type":"simple","jsonPath":"$.name","operatorType":"MATCHES_PATTERN","value":"("}`
	path := fmt.Sprintf("/api/search/direct/%s/%d", model, 1)
	resp := doAuth(t, http.MethodPost, path, badCondition)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	// ExpectErrorCode re-buffers the body, so it's safe to read again after.
	commontest.ExpectErrorCode(t, resp, "INVALID_CONDITION")
	body := readBody(t, resp)
	if !strings.Contains(body, "regex") {
		t.Errorf("expected response detail to mention the regex problem; body: %s", body)
	}
}

// TestSearch_Sync_ValidRegex_Returns200 is the accept-side counterpart:
// a well-formed pattern must still search normally (no accept/reject skew
// introduced by the new upstream validation).
func TestSearch_Sync_ValidRegex_Returns200(t *testing.T) {
	const model = "e2e-search-regex-valid"
	setupSearchModel(t, model)
	createEntityE2E(t, model, 1, `{"name":"Alice","amount":100,"status":"active"}`)
	createEntityE2E(t, model, 1, `{"name":"Bob","amount":50,"status":"active"}`)

	const goodCondition = `{"type":"simple","jsonPath":"$.name","operatorType":"MATCHES_PATTERN","value":"^A.*e$"}`
	status, results := directSearch(t, model, 1, goodCondition)
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	if len(results) != 1 {
		t.Errorf("expected 1 match for ^A.*e$, got %d", len(results))
	}
}

// TestSearch_AsyncSubmit_MalformedRegex_Returns400_InvalidCondition mirrors
// the sync case for the async submit path: no job should ever be created
// for a malformed pattern (the pre-execution validation contract already
// established for field paths — issue #77 — extends to regex patterns).
func TestSearch_AsyncSubmit_MalformedRegex_Returns400_InvalidCondition(t *testing.T) {
	const model = "e2e-search-regex-invalid-async"
	setupSearchModel(t, model)
	createEntityE2E(t, model, 1, `{"name":"Bob","amount":42,"status":"active"}`)

	const badCondition = `{"type":"simple","jsonPath":"$.name","operatorType":"MATCHES_PATTERN","value":"("}`
	path := fmt.Sprintf("/api/search/async/%s/1", model)
	resp := doAuth(t, http.MethodPost, path, badCondition)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		body := readBody(t, resp)
		t.Fatalf("expected 400, got %d; body: %s", resp.StatusCode, body)
	}
	commontest.ExpectErrorCode(t, resp, "INVALID_CONDITION")
}

// TestSearch_Sync_MalformedRegex_NestedInGroup_Returns400_InvalidCondition
// verifies the whole condition tree is walked: a malformed pattern nested
// inside an AND/OR group must be found, not just a top-level clause.
func TestSearch_Sync_MalformedRegex_NestedInGroup_Returns400_InvalidCondition(t *testing.T) {
	const model = "e2e-search-regex-invalid-nested"
	setupSearchModel(t, model)
	createEntityE2E(t, model, 1, `{"name":"Alice","amount":100,"status":"active"}`)

	const badCondition = `{
		"type": "group",
		"operator": "AND",
		"conditions": [
			{"type":"simple","jsonPath":"$.amount","operatorType":"GREATER_THAN","value":10},
			{
				"type": "group",
				"operator": "OR",
				"conditions": [
					{"type":"simple","jsonPath":"$.name","operatorType":"MATCHES_PATTERN","value":"("},
					{"type":"simple","jsonPath":"$.name","operatorType":"EQUALS","value":"Alice"}
				]
			}
		]
	}`
	path := fmt.Sprintf("/api/search/direct/%s/%d", model, 1)
	resp := doAuth(t, http.MethodPost, path, badCondition)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		body := readBody(t, resp)
		t.Fatalf("expected 400, got %d; body: %s", resp.StatusCode, body)
	}
	commontest.ExpectErrorCode(t, resp, "INVALID_CONDITION")
}
