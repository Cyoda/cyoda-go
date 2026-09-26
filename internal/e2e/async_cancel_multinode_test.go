package e2e_test

// async_cancel_multinode_test.go — isolated single-backend (postgres) e2e
// coverage for cancel-mid-flight and cross-node cancel (design §9 row 7) and
// the deposed-executor reclaim (§9 row 8). See async_stream_test.go's
// top-of-file comment for the full §9 reconciliation.

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/app"
)

// ---------------------------------------------------------------------------
// (b) cancel mid-flight, single node
// ---------------------------------------------------------------------------

// TestE2E_AsyncSearch_CancelMidFlight submits a job over a backend whose
// Iterate blocks (so it can never settle on its own), cancels it
// immediately, and asserts the terminal status is CANCELLED and stays that
// way — the results endpoint answers 400 job-not-complete, and a further 2s
// poll never observes SUCCESSFUL.
func TestE2E_AsyncSearch_CancelMidFlight(t *testing.T) {
	backend, gate := newBlockingIterateBackend(t)
	h := newCallbackHarnessConfigured(t, func(cfg *app.Config) {
		cfg.StorageBackend = backend
	})

	const model = "cancel-midflight-e2e"
	h.setupModelSampleWithWorkflow(t, model, `{"name":"Alice","amount":1,"status":"new"}`, secondaryWorkflow)
	if _, status, body := h.CreateEntity(t, model, 1, `{"name":"Alice","amount":1,"status":"new"}`); status != http.StatusOK {
		t.Fatalf("seed: %d %s", status, body)
	}

	gate.Block()
	defer gate.Release()

	resp := h.DoAuth(t, http.MethodPost, "/api/search/async/"+model+"/1",
		`{"type":"group","operator":"AND","conditions":[]}`, "")
	body := h.readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("submit: %d %s", resp.StatusCode, body)
	}
	jobID := strings.Trim(strings.TrimSpace(body), `"`)

	select {
	case <-gate.Entered():
	case <-time.After(5 * time.Second):
		t.Fatal("worker never reached the blocked Iterate call")
	}

	cancelResp := h.DoAuth(t, http.MethodPut, "/api/search/async/"+jobID+"/cancel", "", "")
	cancelBody := h.readBody(t, cancelResp)
	if cancelResp.StatusCode != http.StatusOK {
		t.Fatalf("cancel: %d %s", cancelResp.StatusCode, cancelBody)
	}
	if !strings.Contains(cancelBody, `"cancelled":true`) {
		t.Errorf("cancel response = %s, want cancelled:true", cancelBody)
	}

	if status := h.waitForAsyncTerminal(t, jobID, 5*time.Second); status != "CANCELLED" {
		t.Fatalf("terminal status = %s, want CANCELLED", status)
	}

	// Results endpoint refuses a not-successfully-complete job.
	resultsResp := h.DoAuth(t, http.MethodGet, "/api/search/async/"+jobID, "", "")
	resultsBody := h.readBody(t, resultsResp)
	if resultsResp.StatusCode != http.StatusBadRequest {
		t.Errorf("results status = %d, want 400; body=%s", resultsResp.StatusCode, resultsBody)
	}

	// Never flips to SUCCESSFUL afterward — poll a further 2s.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		statusResp := h.DoAuth(t, http.MethodGet, "/api/search/async/"+jobID+"/status", "", "")
		statusBody := h.readBody(t, statusResp)
		if strings.Contains(statusBody, "SUCCESSFUL") {
			t.Fatalf("job flipped to SUCCESSFUL after a CANCELLED terminal write: %s", statusBody)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// (c) cross-node cancel
// ---------------------------------------------------------------------------

// TestE2E_AsyncSearch_CrossNodeCancel submits on node A (whose Iterate
// blocks), cancels via node B — a wholly separate app.App + SearchService
// sharing the same Postgres job table, so B's in-process cancel registry
// has no entry for the job — and asserts A's OWN executor observes the
// cross-node cancellation through its own heartbeat and actually unblocks
// its in-flight Iterate call, not merely that a status read reflects the
// shared row (any node would show that identically). The engine's cancel
// registry is process-local, so the cross-node path IS the store refusing
// the executor's heartbeat as already terminal; allow up to ~2x the
// heartbeat interval before asserting abort.
func TestE2E_AsyncSearch_CrossNodeCancel(t *testing.T) {
	const heartbeat = 300 * time.Millisecond
	backend, gate := newBlockingIterateBackend(t)

	hA := newCallbackHarnessConfigured(t, func(cfg *app.Config) {
		cfg.StorageBackend = backend
		cfg.SearchJobHeartbeatInterval = heartbeat
	})
	// B is a plain postgres node — it never runs this job, only writes the
	// cancel to the shared row.
	hB := newCallbackHarnessConfigured(t, func(cfg *app.Config) {
		cfg.SearchJobHeartbeatInterval = heartbeat
	})

	const model = "xnode-cancel-e2e"
	hA.setupModelSampleWithWorkflow(t, model, `{"name":"Alice","amount":1,"status":"new"}`, secondaryWorkflow)
	if _, status, body := hA.CreateEntity(t, model, 1, `{"name":"Alice","amount":1,"status":"new"}`); status != http.StatusOK {
		t.Fatalf("seed create on A: %d %s", status, body)
	}

	gate.Block()
	defer gate.Release()

	resp := hA.DoAuth(t, http.MethodPost, "/api/search/async/"+model+"/1",
		`{"type":"group","operator":"AND","conditions":[]}`, "")
	body := hA.readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("submit on A: %d %s", resp.StatusCode, body)
	}
	jobID := strings.Trim(strings.TrimSpace(body), `"`)

	select {
	case <-gate.Entered():
	case <-time.After(5 * time.Second):
		t.Fatal("A's executor never reached the blocked Iterate call")
	}
	resumed := gate.Resumed()

	// Cancel via B: writes CANCELLED to the shared row; B's own
	// CancelRunning finds nothing registered locally (returns false,
	// swallowed — the HTTP contract only reports the row's cancelled
	// state), so this exercises the store-only cross-node path.
	cancelResp := hB.DoAuth(t, http.MethodPut, "/api/search/async/"+jobID+"/cancel", "", "")
	cancelBody := hB.readBody(t, cancelResp)
	if cancelResp.StatusCode != http.StatusOK {
		t.Fatalf("cancel via B: %d %s", cancelResp.StatusCode, cancelBody)
	}

	// The store must refuse A's next heartbeat against the CANCELLED row,
	// which cancels the job's ctx and unblocks the gate — proof the abort
	// reached A's executor, not just the shared store.
	select {
	case <-resumed:
	case <-time.After(4 * heartbeat):
		t.Fatal("A's executor did not abort its blocked scan via its own heartbeat within the budget")
	}

	if status := hA.waitForAsyncTerminal(t, jobID, 5*time.Second); status != "CANCELLED" {
		t.Fatalf("terminal status observed from A = %s, want CANCELLED", status)
	}
}

// ---------------------------------------------------------------------------
// (row 8) deposed-executor reclaim
// ---------------------------------------------------------------------------

// TestE2E_AsyncSearch_DeposedExecutorFenced covers the reclaim of a LIVE but
// slow-heartbeating executor. Node A holds the job over a blocking-Iterate
// backend and heartbeats on an 8s interval (its stale bound the 4x floor, 32s);
// node B runs a 2s stale bound. A is alive — it is simply not heartbeating fast
// enough for B's tighter bound — so between A's heartbeats B legitimately reads
// the job as stale, claims it (bumping the epoch), and re-executes it on B's
// live backend. When A's blocked Iterate finally resumes, its writes are at the
// superseded epoch and are fenced.
//
// Deliberately asserts the OUTCOME — the job is SUCCESSFUL exactly as B wrote
// it, with a single clean author (result_count == a direct search, so no torn
// or doubled write) — never which of A's specific calls (heartbeat, SaveResults,
// or the terminal write) lost the fencing race, which is timing-dependent.
func TestE2E_AsyncSearch_DeposedExecutorFenced(t *testing.T) {
	backend, gate := newBlockingIterateBackend(t)

	// A: live but slow. 8s heartbeat, 32s stale bound (the enforced 4x floor).
	hA := newCallbackHarnessConfigured(t, func(cfg *app.Config) {
		cfg.StorageBackend = backend
		cfg.SearchJobHeartbeatInterval = 8 * time.Second
		cfg.SearchJobStaleAfter = 32 * time.Second
	})
	// B: tight stale bound, live backend. 500ms heartbeat, 2s stale bound.
	hB := newCallbackHarnessConfigured(t, func(cfg *app.Config) {
		cfg.SearchJobHeartbeatInterval = 500 * time.Millisecond
		cfg.SearchJobStaleAfter = 2 * time.Second
	})

	const model = "deposed-executor-e2e"
	const seeded = 5
	hA.setupModelSampleWithWorkflow(t, model, `{"name":"Alice","amount":1,"status":"new"}`, secondaryWorkflow)
	for i := 0; i < seeded; i++ {
		if _, status, body := hA.CreateEntity(t, model, 1, fmt.Sprintf(`{"name":"e%d","amount":%d,"status":"new"}`, i, i)); status != http.StatusOK {
			t.Fatalf("seed %d on A: %d %s", i, status, body)
		}
	}

	gate.Block()
	defer gate.Release()

	resp := hA.DoAuth(t, http.MethodPost, "/api/search/async/"+model+"/1",
		`{"type":"group","operator":"AND","conditions":[]}`, "")
	body := hA.readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("submit on A: %d %s", resp.StatusCode, body)
	}
	jobID := strings.Trim(strings.TrimSpace(body), `"`)

	select {
	case <-gate.Entered():
	case <-time.After(5 * time.Second):
		t.Fatal("A's executor never reached the blocked Iterate call")
	}

	// B deposes A: it reads the job stale against its 2s bound while A is
	// between its 8s heartbeats, claims it, and completes it on B's live
	// backend. Budget covers the 2s stale window + reclaim + scan.
	if status := hB.waitForAsyncTerminal(t, jobID, 20*time.Second); status != "SUCCESSFUL" {
		t.Fatalf("B settled deposed job %s = %s, want SUCCESSFUL", jobID, status)
	}

	// Release A's gate: its blocked Iterate resumes and its writes land at the
	// superseded epoch, fenced. The job must STAY SUCCESSFUL as B wrote it.
	gate.Release()

	// The deposed node's resumed write must not overturn or duplicate B's
	// result. Poll a short while to catch a late fencing miss, then assert the
	// single-author result count.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if status := hB.asyncJobStatus(t, jobID); status != "SUCCESSFUL" {
			t.Fatalf("deposed job flipped to %s after A resumed — A's fenced write was not rejected", status)
		}
		time.Sleep(100 * time.Millisecond)
	}
	doAuthB := func(method, path, body string) *http.Response { return hB.DoAuth(t, method, path, body, "") }
	if got, want := persistedJobResultCount(t, jobID), directSearchCount(t, doAuthB, model); got != want {
		t.Fatalf("deposed-then-reclaimed job result_count = %d, want %d (single clean author)", got, want)
	}
}
