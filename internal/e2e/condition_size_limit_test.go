package e2e_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
)

// arrayClauseWithEntries is an array clause with n entries in `values`: one
// tested position and n-1 nulls. A null entry counts towards the limit.
func arrayClauseWithEntries(n int) string {
	return `{"type":"array","jsonPath":"$.tags[*]","operatorType":"EQUALS","values":["a"` +
		strings.Repeat(",null", n-1) + `]}`
}

// A condition that tests more than 10,000 entries is refused before it is
// expanded: 400 INVALID_CONDITION, naming the limit.
func TestSearch_ConditionOverSizeLimit_IsInvalidCondition(t *testing.T) {
	const model = "e2e-cond-size-limit"
	setupScoresModel(t, model)
	resp := doAuth(t, http.MethodPost, fmt.Sprintf("/api/search/direct/%s/1", model), arrayClauseWithEntries(10_001))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d; body: %s", resp.StatusCode, readBody(t, resp))
	}
	commontest.ExpectErrorCode(t, resp, "INVALID_CONDITION")
	if body := readBody(t, resp); !strings.Contains(body, "10000") {
		t.Errorf("detail must name the limit, got %s", body)
	}

	at := doAuth(t, http.MethodPost, fmt.Sprintf("/api/search/direct/%s/1", model), arrayClauseWithEntries(10_000))
	if at.StatusCode != http.StatusOK {
		t.Errorf("at the limit: expected 200, got %d; body: %s", at.StatusCode, readBody(t, at))
	}
	at.Body.Close()
}

// Workflow import refuses a criterion over the limit, so the engine never
// evaluates one.
func TestWorkflowImport_CriterionOverSizeLimit_IsValidationFailed(t *testing.T) {
	const model = "e2e-crit-size-limit"
	importModelSampleE2E(t, model, 1, `{"name":"S","tags":["t"]}`)
	lockModelE2E(t, model, 1)
	wf := fmt.Sprintf(`{"importMode":"REPLACE","workflows":[{"version":"1.1","name":"w","initialState":"NONE","active":true,
		"states":{"NONE":{"transitions":[{"name":"go","next":"DONE","manual":false,"criterion":%s}]},"DONE":{}}}]}`,
		arrayClauseWithEntries(10_001))
	resp := doAuth(t, http.MethodPost, fmt.Sprintf("/api/model/%s/1/workflow/import", model), wf)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", resp.StatusCode, readBody(t, resp))
	}
	commontest.ExpectErrorCode(t, resp, "VALIDATION_FAILED")
	if body := readBody(t, resp); !strings.Contains(body, "10000") {
		t.Errorf("detail must name the limit, got %s", body)
	}
}
