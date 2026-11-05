package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// TestE2E_AsyncSearch_Successful_HasNoFailureReason is the other half of the
// failureReason contract on getAsyncSearchStatus: the member is present only
// when the status is FAILED (TestE2E_AsyncSearch_CeilingExceeded_ServesSanitizedFailureReason
// and TestE2E_AsyncSearch_AttemptCap_Fails cover that side), and a job that
// finished SUCCESSFUL carries none.
func TestE2E_AsyncSearch_Successful_HasNoFailureReason(t *testing.T) {
	const model = "e2e-async-search-no-failure-reason"
	setupSearchModel(t, model)
	createEntityE2E(t, model, 1, `{"name":"Alice","amount":100,"status":"active"}`)

	jobID := submitAsyncSearch(t, model, 1, `{"type":"group","operator":"AND","conditions":[]}`)
	if st := waitForAsyncSearch(t, jobID, 10*time.Second); st != "SUCCESSFUL" {
		t.Fatalf("status = %q, want SUCCESSFUL", st)
	}

	resp := doAuth(t, http.MethodGet, fmt.Sprintf("/api/search/async/%s/status", jobID), "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("getAsyncSearchStatus: %d %s", resp.StatusCode, body)
	}
	var status map[string]any
	if err := json.Unmarshal([]byte(body), &status); err != nil {
		t.Fatalf("status body is not JSON: %v; body: %s", err, body)
	}
	if reason, ok := status["failureReason"]; ok {
		t.Errorf("SUCCESSFUL status carries failureReason %v; want it absent. body: %s", reason, body)
	}
}
