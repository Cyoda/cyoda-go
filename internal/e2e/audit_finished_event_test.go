package e2e_test

import (
	"net/http"
	"testing"
)

// TestGetStateMachineFinishedEvent_NonV1UUID_NotRejected is a proving test:
// it asserts that a well-formed non-v1 (v4) UUID in entityId/transactionId
// is NOT rejected with 400. The spec previously claimed both IDs "must be a
// valid time-based UUID (version 1)" and documented a 400 for that check, but
// the handler performs no version check. This test guards that the fictional
// 400 stays absent.
func TestGetStateMachineFinishedEvent_NonV1UUID_NotRejected(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	// v4 UUIDs — not time-based (version 1).
	resp := doAuth(t, http.MethodGet, "/api/audit/entity/00000000-0000-4000-8000-000000000000/workflow/00000000-0000-4000-8000-000000000001/finished", "")
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusBadRequest {
		t.Fatalf("non-v1 UUID rejected with 400; want 404/200 (handler does no version check)")
	}
}
