package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// importRejection imports payload and asserts 400 VALIDATION_FAILED whose
// detail contains every string in wantInDetail.
func importRejection(t *testing.T, entity string, payload string, wantInDetail ...string) {
	t.Helper()
	status, respBody := importWorkflowE2E(t, entity, 1, payload)
	if status != http.StatusBadRequest {
		t.Fatalf("import status = %d; want 400; body: %s", status, respBody)
	}
	var errBody struct {
		Detail     string `json:"detail"`
		Properties struct {
			ErrorCode string `json:"errorCode"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(respBody), &errBody); err != nil {
		t.Fatalf("decode error body: %v; raw: %s", err, respBody)
	}
	if errBody.Properties.ErrorCode != "VALIDATION_FAILED" {
		t.Fatalf("errorCode = %q; want VALIDATION_FAILED; body: %s", errBody.Properties.ErrorCode, respBody)
	}
	for _, want := range wantInDetail {
		if !strings.Contains(errBody.Detail, want) {
			t.Errorf("detail %q must contain %q", errBody.Detail, want)
		}
	}
}

// calloutWorkflow builds a one-transition workflow. criterion and schedule
// are raw JSON members (or ""), processorConfig is the processor's config
// object (or "" for no processor).
func calloutWorkflow(name, criterion, schedule, processorConfig string) string {
	var members []string
	members = append(members, `"name": "t"`, `"next": "Done"`)
	if schedule == "" {
		members = append(members, `"manual": true`)
	} else {
		members = append(members, `"schedule": `+schedule)
	}
	if criterion != "" {
		members = append(members, `"criterion": `+criterion)
	}
	if processorConfig != "" {
		members = append(members, fmt.Sprintf(
			`"processors": [{"type":"externalized","name":"p","executionMode":"SYNC","config": %s}]`, processorConfig))
	}
	return fmt.Sprintf(`{
	  "importMode": "REPLACE",
	  "workflows": [{
	    "version": "1.1", "name": %q, "initialState": "S", "active": true,
	    "states": { "S": { "transitions": [ { %s } ] }, "Done": {} }
	  }]
	}`, name, strings.Join(members, ", "))
}

func TestWorkflowImport_CriterionRetryPolicyInvalid_400(t *testing.T) {
	const entity = "wf-callout-crit-retry"
	importModelE2E(t, entity, 1)
	crit := `{"type":"function","function":{"name":"min-amount","config":{"calculationNodesTags":"pricing","retryPolicy":"LINEAR_BACKOFF"}}}`
	importRejection(t, entity, calloutWorkflow("crit-retry-wf", crit, "", ""),
		"crit-retry-wf", `state "S"`, `transition "t"`, "unknown retryPolicy", "LINEAR_BACKOFF")
}

func TestWorkflowImport_FunctionRetryPolicyInvalid_400(t *testing.T) {
	const entity = "wf-callout-fn-retry"
	importModelE2E(t, entity, 1)
	sched := `{"function":{"name":"computeFire","resultKind":"Schedule","calculationNodesTags":"scheduler","retryPolicy":"EXPONENTIAL"}}`
	importRejection(t, entity, calloutWorkflow("fn-retry-wf", "", sched, ""),
		"fn-retry-wf", `state "S"`, `transition "t"`, "schedule.function", "unknown retryPolicy", "EXPONENTIAL")
}

func TestWorkflowImport_CalloutRetryPolicyValid_200(t *testing.T) {
	const entity = "wf-callout-retry-ok"
	importModelE2E(t, entity, 1)
	crit := `{"type":"function","function":{"name":"min-amount","config":{"calculationNodesTags":"pricing","retryPolicy":"NONE"}}}`
	if status, body := importWorkflowE2E(t, entity, 1, calloutWorkflow("crit-ok-wf", crit, "", "")); status != http.StatusOK {
		t.Fatalf("criterion retryPolicy NONE: expected 200, got %d: %s", status, body)
	}
	sched := `{"function":{"name":"computeFire","resultKind":"Schedule","calculationNodesTags":"scheduler","retryPolicy":"FIXED"}}`
	if status, body := importWorkflowE2E(t, entity, 1, calloutWorkflow("fn-ok-wf", "", sched, "")); status != http.StatusOK {
		t.Fatalf("function retryPolicy FIXED: expected 200, got %d: %s", status, body)
	}
}
