package search

import (
	"testing"
	"time"
)

// TestBuildStatusResponse_FailureReason pins the HTTP status body:
// failureReason is present, with the recorded text, exactly when the job
// failed with a reason, and absent from every other status.
func TestBuildStatusResponse_FailureReason(t *testing.T) {
	created := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)

	failed := buildStatusResponse(SearchJobStatus{
		JobID: "j1", Status: "FAILED", CreateTime: created,
		FailureReason: "search failed unexpectedly",
	})
	if got, ok := failed["failureReason"]; !ok || got != "search failed unexpectedly" {
		t.Errorf("FAILED body failureReason = %v (present %v), want the recorded reason", got, ok)
	}

	for _, st := range []string{"RUNNING", "SUCCESSFUL", "CANCELLED", "NOT_FOUND"} {
		body := buildStatusResponse(SearchJobStatus{JobID: "j2", Status: st, CreateTime: created})
		if got, ok := body["failureReason"]; ok {
			t.Errorf("%s body carries failureReason %v; want it absent", st, got)
		}
	}
}
