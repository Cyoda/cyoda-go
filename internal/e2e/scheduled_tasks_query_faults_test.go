package e2e_test

// The 500 and 503 cells of GET /scheduled-tasks, injected for real on
// PostgreSQL. Harnesses and their isolation: lookup_storage_failure_e2e_test.go
// (terminated session → unmarked 57P01 → 500 with a ticket) and
// torn_connection_e2e_test.go (torn socket → marked → retryable 503).

import (
	"net/http"
	"testing"
)

func TestScheduledTasks_StorageFailure_500(t *testing.T) {
	h := newLookupFailureHarness(t)
	k := newSessionKiller(t)
	list := func() (int, string) {
		resp := h.DoAuth(t, http.MethodGet, "/api/scheduled-tasks", "", "")
		return resp.StatusCode, h.readBody(t, resp)
	}
	saw500 := false
	for i := 0; i < lookupFailureCycles && !saw500; i++ {
		if status, body := list(); status != http.StatusOK {
			t.Fatalf("cycle %d: warm-up: %d %s", i, status, body)
		}
		if k.kill(t) == 0 {
			t.Fatalf("cycle %d: no harness session to terminate", i)
		}
		status, body := list()
		t.Logf("cycle %d: status=%d body=%s", i, status, body)
		assertNotSubstitutedNotFound(t, status, body) // ticket on a 5xx, no driver detail
		if status == http.StatusInternalServerError {
			if code := problemErrorCode(body); code != "SERVER_ERROR" {
				t.Errorf("errorCode = %q, want SERVER_ERROR; body: %s", code, body)
			}
			saw500 = true
		}
	}
	if !saw500 {
		t.Fatalf("no probe answered 500 in %d cycles; the fault was never injected", lookupFailureCycles)
	}
}

func TestScheduledTasks_TornConnection_503(t *testing.T) {
	h := newTornHarness(t)
	list := h.get("/api/scheduled-tasks")
	probeTorn(t, h, list, list) // asserts 503 STORAGE_UNAVAILABLE, retryable, no leak
}
