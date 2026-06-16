package e2e_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestE2E_AsyncResultTrueRejectedAtImport exercises the full HTTP stack:
// POST a workflow import body whose processor sets asyncResult=true,
// expect 400 VALIDATION_FAILED with a problem+json error response
// identifying the processor by location.
//
// Mirrors the rejection pattern from
// TestE2E_ExplicitFireOfScheduledTransition_ReturnsTransitionNotFound,
// differing only in the validator rule under test.
func TestE2E_AsyncResultTrueRejectedAtImport(t *testing.T) {
	const model = "e2e-async-result-true-reject"
	importModelE2E(t, model, 1)
	lockModelE2E(t, model, 1)

	body := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version":"1","name":"async-reject-wf","initialState":"S1","active":true,
			"states":{
				"S1":{"transitions":[{
					"name":"t","next":"S2","manual":false,
					"processors":[{"type":"externalized","name":"p","executionMode":"SYNC",
						"config":{"asyncResult":true}}]
				}]},
				"S2":{}
			}
		}]
	}`

	status, respBody := importWorkflowE2E(t, model, 1, body)
	if status != http.StatusBadRequest {
		t.Fatalf("expected 400 VALIDATION_FAILED; got %d: %s", status, respBody)
	}

	var pd struct {
		Status     int            `json:"status"`
		Detail     string         `json:"detail"`
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal([]byte(respBody), &pd); err != nil {
		t.Fatalf("decode problem detail: %v\nbody: %s", err, respBody)
	}
	if pd.Status != http.StatusBadRequest {
		t.Errorf("ProblemDetail.status: got %d, want 400", pd.Status)
	}
	if code, _ := pd.Properties["errorCode"].(string); code != "VALIDATION_FAILED" {
		t.Errorf("errorCode: got %q, want VALIDATION_FAILED; body=%s", code, respBody)
	}
	// Body must carry the rule-naming substring and the processor location.
	// Assert against raw body so we're robust to where common.WriteError
	// surfaces the wrapped error string (detail vs title vs another field).
	for _, want := range []string{
		`processor \"p\"`,
		"asyncResult=true is not supported",
	} {
		if !strings.Contains(respBody, want) {
			t.Errorf("response body must contain %q; got: %s", want, respBody)
		}
	}
}

// TestE2E_CrossoverToAsyncMsRejectedAtImport exercises the orphan
// crossoverToAsyncMs rejection path: asyncResult is absent, but
// crossoverToAsyncMs is set. Validator rejects with 400 VALIDATION_FAILED.
func TestE2E_CrossoverToAsyncMsRejectedAtImport(t *testing.T) {
	const model = "e2e-crossover-orphan-reject"
	importModelE2E(t, model, 1)
	lockModelE2E(t, model, 1)

	body := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version":"1","name":"crossover-orphan-wf","initialState":"S1","active":true,
			"states":{
				"S1":{"transitions":[{
					"name":"t","next":"S2","manual":false,
					"processors":[{"type":"externalized","name":"p","executionMode":"SYNC",
						"config":{"crossoverToAsyncMs":5000}}]
				}]},
				"S2":{}
			}
		}]
	}`

	status, respBody := importWorkflowE2E(t, model, 1, body)
	if status != http.StatusBadRequest {
		t.Fatalf("expected 400 VALIDATION_FAILED; got %d: %s", status, respBody)
	}

	var pd struct {
		Status     int            `json:"status"`
		Detail     string         `json:"detail"`
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal([]byte(respBody), &pd); err != nil {
		t.Fatalf("decode problem detail: %v\nbody: %s", err, respBody)
	}
	if code, _ := pd.Properties["errorCode"].(string); code != "VALIDATION_FAILED" {
		t.Errorf("errorCode: got %q, want VALIDATION_FAILED; body=%s", code, respBody)
	}
	for _, want := range []string{
		`processor \"p\"`,
		"crossoverToAsyncMs is not supported",
	} {
		if !strings.Contains(respBody, want) {
			t.Errorf("response body must contain %q; got: %s", want, respBody)
		}
	}
}
