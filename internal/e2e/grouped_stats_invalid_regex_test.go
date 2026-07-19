package e2e_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
)

// TestGroupedStats_MalformedRegex_Returns400_InvalidCondition verifies that a
// MATCHES_PATTERN condition carrying an unparsable regex ("[" — unterminated
// character class) is rejected with 400 INVALID_CONDITION on the grouped-stats
// endpoint, before any store is touched. This closes a fail-open regression:
// the plugin residual filter evaluators (sqlite's evaluateFilter, postgres's
// evalPostFilter) delegate to the error-free spi.MatchFilter, which returns
// false (non-match) rather than erroring on a malformed pattern — so without
// upstream validation a bad regex here would silently under-include buckets
// (HTTP 200) instead of being rejected. The grouped-stats path now runs the
// same domain-layer regex validation the search path already has, so every
// backend (memory/sqlite/postgres) rejects identically.
func TestGroupedStats_MalformedRegex_Returns400_InvalidCondition(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	const model = "e2e-grouped-stats-regex-invalid"
	setupStatsModel(t, model)
	createEntityE2E(t, model, 1, `{"variantId":"v1","price":10.0}`)

	reqBody := `{
		"groupBy": ["$.variantId"],
		"condition": {"type":"simple","jsonPath":"$.variantId","operatorType":"MATCHES_PATTERN","value":"["}
	}`
	path := fmt.Sprintf("/api/entity/stats/%s/1/query", model)
	resp := doAuth(t, http.MethodPost, path, reqBody)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		body := readBody(t, resp)
		t.Fatalf("expected 400, got %d: %s", resp.StatusCode, body)
	}
	// ExpectErrorCode re-buffers the body, so it's safe to read again after.
	commontest.ExpectErrorCode(t, resp, "INVALID_CONDITION")
	body := readBody(t, resp)
	if !strings.Contains(body, "regex") {
		t.Errorf("expected response detail to mention the regex problem; body: %s", body)
	}
}

// TestGroupedStats_ValidRegex_Returns200 is the accept-side counterpart: a
// well-formed MATCHES_PATTERN condition must still produce buckets normally
// (no accept/reject skew introduced by the new upstream validation).
func TestGroupedStats_ValidRegex_Returns200(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	const model = "e2e-grouped-stats-regex-valid"
	setupStatsModel(t, model)
	createEntityE2E(t, model, 1, `{"variantId":"v1","price":10.0}`)
	createEntityE2E(t, model, 1, `{"variantId":"v2","price":20.0}`)

	reqBody := `{
		"groupBy": ["$.variantId"],
		"condition": {"type":"simple","jsonPath":"$.variantId","operatorType":"MATCHES_PATTERN","value":"^v1$"}
	}`
	path := fmt.Sprintf("/api/entity/stats/%s/1/query", model)
	resp := doAuth(t, http.MethodPost, path, reqBody)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	buckets := decodeBuckets(t, body)
	if len(buckets) != 1 {
		t.Fatalf("expected 1 bucket (only v1 matches ^v1$), got %d: %s", len(buckets), body)
	}
	b := findBucket(buckets, "$.variantId", "v1")
	if b == nil {
		t.Fatalf("missing bucket for variantId=v1; buckets=%s", body)
	}
	if got, _ := b["count"].(float64); got != 1 {
		t.Errorf("count: got %v, want 1", got)
	}
}
