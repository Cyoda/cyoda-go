package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
)

// An operator that takes an operand refuses a missing or null `value`. Only
// IS_NULL and NOT_NULL take none. Left through, the operand compared as the
// empty string, so CONTAINS matched every string — on a conditional delete,
// every entity with a string at the path.

func TestSearch_Sync_MissingOperandRefused(t *testing.T) {
	const model = "e2e-missing-operand-search"
	importModelSampleE2E(t, model, 1, `{"name":"Sample","amount":0}`)
	lockModelE2E(t, model, 1)
	createEntityE2E(t, model, 1, `{"name":"","amount":1}`)
	for name, cond := range map[string]string{
		"EQUALS null":           `{"type":"simple","jsonPath":"$.name","operatorType":"EQUALS","value":null}`,
		"EQUALS missing":        `{"type":"simple","jsonPath":"$.name","operatorType":"EQUALS"}`,
		"CONTAINS missing":      `{"type":"simple","jsonPath":"$.name","operatorType":"CONTAINS"}`,
		"lifecycle EQUALS null": `{"type":"lifecycle","field":"state","operatorType":"EQUALS","value":null}`,
	} {
		t.Run(name, func(t *testing.T) {
			resp := doAuth(t, http.MethodPost, fmt.Sprintf("/api/search/direct/%s/1", model), cond)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", resp.StatusCode, readBody(t, resp))
			}
			commontest.ExpectErrorCode(t, resp, "INVALID_CONDITION")
			if body := readBody(t, resp); !strings.Contains(body, "takes an operand") {
				t.Errorf("detail must say the operator takes an operand; body: %s", body)
			}
		})
	}
}

func TestDeleteEntities_MissingOperandRefusedAndNothingDeleted(t *testing.T) {
	const model = "e2e-missing-operand-delete"
	importModelSampleE2E(t, model, 1, `{"name":"Sample","amount":0}`)
	lockModelE2E(t, model, 1)
	id := createEntityE2E(t, model, 1, `{"name":"keep","amount":1}`)

	cond := `{"type":"simple","jsonPath":"$.name","operatorType":"CONTAINS"}`
	resp := doAuth(t, http.MethodDelete, fmt.Sprintf("/api/entity/%s/1", model), cond)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", resp.StatusCode, readBody(t, resp))
	}
	commontest.ExpectErrorCode(t, resp, "INVALID_CONDITION")
	resp.Body.Close()

	if r := doAuth(t, http.MethodGet, "/api/entity/"+id, ""); r.StatusCode != http.StatusOK {
		t.Errorf("entity must survive a refused delete, got %d", r.StatusCode)
	}
}

func TestCriterionImport_MissingOperandRefused(t *testing.T) {
	rows := []struct{ name, criterion, want string }{
		{"criterion", `{"type":"simple","jsonPath":"$.amount","operatorType":"GREATER_THAN"}`,
			`operator "GREATER_THAN" takes an operand`},
		{"criterion null", `{"type":"lifecycle","field":"state","operatorType":"EQUALS","value":null}`,
			`operator "EQUALS" takes an operand`},
		{"pre-check", cfFn("f", `{"type":"simple","jsonPath":"$.amount","operatorType":"EQUALS"}`),
			`condition at /function/criterion: invalid condition: operator "EQUALS" takes an operand`},
	}
	for i, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			model := fmt.Sprintf("e2e-missing-operand-import-%d", i)
			status, body := cfImportInto(t, model, cfWorkflow("missing-operand", tc.criterion))
			if status != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", status, body)
			}
			assertErrorCode(t, body, "VALIDATION_FAILED")
			var pd struct {
				Detail string `json:"detail"`
			}
			if err := json.Unmarshal([]byte(body), &pd); err != nil {
				t.Fatalf("decode problem detail: %v\nbody: %s", err, body)
			}
			if !strings.Contains(pd.Detail, tc.want) {
				t.Errorf("detail does not contain %q:\n%s", tc.want, pd.Detail)
			}
		})
	}
}
