package parity

import (
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// RunAsyncStatusNoFailureReasonUnlessFailed pins, on every backend, the half of
// the async status's failureReason contract a client can reach on any backend:
// a job that finished SUCCESSFUL serves no failureReason. The member is
// present only when the status is FAILED.
//
// The FAILED half is not a cross-backend scenario: no request through the
// public API makes an async search fail on every backend. Submission checks
// the condition, the sort keys and the limits first, so what remains are
// server-side failures and backend-specific limits. It is covered where it can
// be caused: internal/e2e (PostgreSQL search ceiling, attempt cap) and the
// unit tests in internal/domain/search and internal/grpc. Each backend's
// round trip of the recorded message is pinned by spitest's async search
// conformance suite.
func RunAsyncStatusNoFailureReasonUnlessFailed(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-async-failure-reason"
	const modelVersion = 1
	setupSortModel(t, c, modelName, modelVersion)
	if _, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"a","amount":1,"status":"new"}`); err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	jobID, err := c.SubmitAsyncSearch(t, modelName, modelVersion, `{"type":"group","operator":"AND","conditions":[]}`)
	if err != nil {
		t.Fatalf("SubmitAsyncSearch: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second) // realtime: test poll deadline
	for {
		st, err := c.GetAsyncSearchStatusBody(t, jobID)
		if err != nil {
			t.Fatalf("GetAsyncSearchStatusBody: %v", err)
		}
		if st.SearchJobStatus != "RUNNING" {
			if st.SearchJobStatus != "SUCCESSFUL" {
				t.Fatalf("status = %q, want SUCCESSFUL", st.SearchJobStatus)
			}
			if st.FailureReason != nil {
				t.Errorf("SUCCESSFUL status carries failureReason %q; want it absent", *st.FailureReason)
			}
			return
		}
		if time.Now().After(deadline) { // realtime: test poll deadline
			t.Fatalf("job %s still RUNNING after 30s", jobID)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
