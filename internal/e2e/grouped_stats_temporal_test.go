package e2e_test

// grouped_stats_temporal_test.go — running-backend (postgres) e2e coverage
// for the grouped-stats temporal/lifecycle condition validation added
// alongside the /search type-soundness checks: a type-unsound or malformed
// lifecycle/temporal condition on the grouped-stats endpoint must be
// rejected with 400 before any store is touched, exactly like the sibling
// /search boundary, rather than silently degrading to an empty or
// over-inclusive result (see grouped_stats_service.go's
// ValidateConditionValueTypes / ValidateCondition calls). This mirrors the
// structure of grouped_stats_invalid_regex_test.go.

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
)

// TestGroupedStats_TemporalTypeMismatch_Returns400 verifies that CONTAINS —
// a string-shaped operator with no temporal semantics — against the
// temporal creationDate meta field is rejected with 400
// CONDITION_TYPE_MISMATCH, matching the equivalent /search request.
func TestGroupedStats_TemporalTypeMismatch_Returns400(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	const model = "e2e-grouped-stats-temporal-mismatch"
	setupStatsModel(t, model)
	createEntityE2E(t, model, 1, `{"variantId":"v1","price":10.0}`)

	reqBody := `{
		"groupBy": ["$.variantId"],
		"condition": {"type":"lifecycle","field":"creationDate","operatorType":"CONTAINS","value":"2021"}
	}`
	path := fmt.Sprintf("/api/entity/stats/%s/1/query", model)
	resp := doAuth(t, http.MethodPost, path, reqBody)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		body := readBody(t, resp)
		t.Fatalf("expected 400, got %d: %s", resp.StatusCode, body)
	}
	commontest.ExpectErrorCode(t, resp, "CONDITION_TYPE_MISMATCH")
}

// TestGroupedStats_TemporalBadOperand_Returns400 verifies that a
// non-RFC3339 operand against the temporal creationDate meta field is
// rejected with 400 CONDITION_TYPE_MISMATCH — a valid comparison operator
// (GREATER_THAN) doesn't rescue an unparsable timestamp operand.
func TestGroupedStats_TemporalBadOperand_Returns400(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	const model = "e2e-grouped-stats-temporal-badoperand"
	setupStatsModel(t, model)
	createEntityE2E(t, model, 1, `{"variantId":"v1","price":10.0}`)

	reqBody := `{
		"groupBy": ["$.variantId"],
		"condition": {"type":"lifecycle","field":"creationDate","operatorType":"GREATER_THAN","value":"not-a-date"}
	}`
	path := fmt.Sprintf("/api/entity/stats/%s/1/query", model)
	resp := doAuth(t, http.MethodPost, path, reqBody)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		body := readBody(t, resp)
		t.Fatalf("expected 400, got %d: %s", resp.StatusCode, body)
	}
	commontest.ExpectErrorCode(t, resp, "CONDITION_TYPE_MISMATCH")
}

// TestGroupedStats_UnknownMetaField_Returns400 verifies that a lifecycle
// condition against an unrecognized meta filter field is rejected with 400
// INVALID_FIELD_PATH, matching the equivalent /search request.
func TestGroupedStats_UnknownMetaField_Returns400(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	const model = "e2e-grouped-stats-unknown-field"
	setupStatsModel(t, model)
	createEntityE2E(t, model, 1, `{"variantId":"v1","price":10.0}`)

	reqBody := `{
		"groupBy": ["$.variantId"],
		"condition": {"type":"lifecycle","field":"bogus","operatorType":"EQUALS","value":"x"}
	}`
	path := fmt.Sprintf("/api/entity/stats/%s/1/query", model)
	resp := doAuth(t, http.MethodPost, path, reqBody)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		body := readBody(t, resp)
		t.Fatalf("expected 400, got %d: %s", resp.StatusCode, body)
	}
	commontest.ExpectErrorCode(t, resp, "INVALID_FIELD_PATH")
}

// TestGroupedStats_MalformedBetweenArity_Returns400 verifies that a BETWEEN
// condition whose value is a scalar (not the required 2-element [lo, hi]
// array) is rejected with 400 INVALID_CONDITION — the same structural
// arity check applies to lifecycle/temporal fields as to simple/data
// fields (validateBetweenArity in condition_type_validate.go is shared).
func TestGroupedStats_MalformedBetweenArity_Returns400(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	const model = "e2e-grouped-stats-malformed-between"
	setupStatsModel(t, model)
	createEntityE2E(t, model, 1, `{"variantId":"v1","price":10.0}`)

	reqBody := `{
		"groupBy": ["$.variantId"],
		"condition": {"type":"lifecycle","field":"creationDate","operatorType":"BETWEEN","value":"2021-01-01T00:00:00Z"}
	}`
	path := fmt.Sprintf("/api/entity/stats/%s/1/query", model)
	resp := doAuth(t, http.MethodPost, path, reqBody)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		body := readBody(t, resp)
		t.Fatalf("expected 400, got %d: %s", resp.StatusCode, body)
	}
	commontest.ExpectErrorCode(t, resp, "INVALID_CONDITION")
}

// TestGroupedStats_ValidTemporal_Returns200 is the accept-side control: a
// well-formed lifecycle/temporal condition (GREATER_THAN a clearly-past
// RFC3339 threshold, matching every entity) must still produce buckets
// normally — the new upstream validation must not introduce over-rejection.
func TestGroupedStats_ValidTemporal_Returns200(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	const model = "e2e-grouped-stats-valid-temporal"
	setupStatsModel(t, model)
	createEntityE2E(t, model, 1, `{"variantId":"v1","price":10.0}`)

	reqBody := `{
		"groupBy": ["$.variantId"],
		"condition": {"type":"lifecycle","field":"creationDate","operatorType":"GREATER_THAN","value":"2000-01-01T00:00:00Z"}
	}`
	path := fmt.Sprintf("/api/entity/stats/%s/1/query", model)
	resp := doAuth(t, http.MethodPost, path, reqBody)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	buckets := decodeBuckets(t, body)
	if len(buckets) != 1 {
		t.Fatalf("expected 1 bucket (variantId=v1), got %d: %s", len(buckets), body)
	}
	b := findBucket(buckets, "$.variantId", "v1")
	if b == nil {
		t.Fatalf("missing bucket for variantId=v1; buckets=%s", body)
	}
	if got, _ := b["count"].(float64); got != 1 {
		t.Errorf("count: got %v, want 1", got)
	}
}
