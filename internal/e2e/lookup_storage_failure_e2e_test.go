package e2e_test

// lookup_storage_failure_e2e_test.go — a lookup that cannot reach storage must
// not answer "it does not exist".
//
// These are the running-backend half of the classification change. The fault is
// a genuine one: this harness's own PostgreSQL sessions are terminated out from
// under it, which is what an operator restart, a failover or a connection reaper
// does. What the caller must never see afterwards is a 404 — an answer that
// reads as a completed lookup and stops a client retrying.
//
// Isolation: each test runs on its own stack with its own pool and its own
// application_name, and the kill is scoped to that name. Nothing here locks a
// table or touches the shared TestMain stack, so a neighbouring test cannot see
// any of it.
//
// Why these assert "not 404 and no leak" rather than a specific 503: on
// PostgreSQL these six lookups have no reachable retryable shape. They issue
// their statements on the pool rather than inside a transaction, so they never
// pass the acquire deadline that mints the STORAGE_UNAVAILABLE marker, and a
// terminated session arrives as the server's FATAL 57P01 ErrorResponse, which
// the plugin deliberately leaves unmarked. The marked shape — a torn socket —
// needs an abrupt transport failure a shared container cannot be asked for. So
// the reachable, assertable contract on this backend is: never a substituted
// not-found, always a 5xx with a ticket and nothing of the driver's in it.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cyoda-platform/cyoda-go/app"
)

const lookupFailureSample = `{"name":"Alice","amount":1,"status":"new"}`

// lookupFailureCycles is how many times each test tears the connection and
// re-asks. One cycle is normally enough — the probe request follows the kill by
// microseconds, far inside the one-second idle window below which pgx hands the
// dead connection straight out. Three cycles exist so that a scheduling stall
// long enough for pgx to notice and transparently reconnect costs a retry
// rather than a failed run.
const lookupFailureCycles = 3

// newLookupFailureHarness stands up a stack with a pool of one connection, so
// the kill below has exactly one session to take and no spare to fall back on.
func newLookupFailureHarness(t *testing.T) *callbackHarness {
	t.Helper()
	return newTinyPoolHarnessConfigured(t, 1, func(cfg *app.Config) {
		// The scan loop would reconnect the pool between the kill and the probe
		// request, and log acquire failures unrelated to what is under test.
		cfg.Scheduler.Enabled = false
		cfg.IAM.TrustedKeyRegistrationEnabled = true
	})
}

// lookupFailureModel derives a per-test model name: these stacks share the
// package's Postgres container, so a fixed name would collide on the second
// import.
func lookupFailureModel(t *testing.T, prefix string) string {
	t.Helper()
	return storageCeilingModel(t, prefix)
}

// sessionKiller terminates the harness's own PostgreSQL backends. It holds its
// own pool under a distinct application_name so it can never kill itself.
type sessionKiller struct {
	admin   *pgxpool.Pool
	appName string
}

func newSessionKiller(t *testing.T) *sessionKiller {
	t.Helper()
	admin, err := pgxpool.New(context.Background(), withAppName(t, pgURLFromEnv(t), "lookup-failure-killer"))
	if err != nil {
		t.Fatalf("open killer pool: %v", err)
	}
	t.Cleanup(admin.Close)
	return &sessionKiller{admin: admin, appName: harnessAppName(t)}
}

// kill terminates every session belonging to the harness under test and returns
// how many it took.
func (k *sessionKiller) kill(t *testing.T) int {
	t.Helper()
	var n int
	err := k.admin.QueryRow(context.Background(),
		`SELECT count(*) FROM (
		   SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		   WHERE datname = current_database() AND application_name = $1
		 ) terminated`, k.appName).Scan(&n)
	if err != nil {
		t.Fatalf("terminate harness sessions: %v", err)
	}
	return n
}

// probeAfterKill runs the tear-and-ask cycle: warm the pool so there is a live
// session, take it, then issue the request whose store call now has a dead
// connection to run on. It returns every response status observed, and asserts
// on each body as it goes.
//
// warm and probe are separate because the warm request is the one that puts a
// session on the wire for the kill to take.
func probeAfterKill(t *testing.T, k *sessionKiller, warm, probe func() (int, string)) []int {
	t.Helper()
	var statuses []int
	for i := 0; i < lookupFailureCycles; i++ {
		if status, body := warm(); status >= 500 {
			t.Fatalf("cycle %d: the warm-up request itself failed: %d %s", i, status, body)
		}
		if killed := k.kill(t); killed == 0 {
			t.Fatalf("cycle %d: no harness session to terminate; the warm-up left none on the wire", i)
		}
		status, body := probe()
		statuses = append(statuses, status)
		assertNotSubstitutedNotFound(t, status, body)
		t.Logf("cycle %d: status=%d body=%s", i, status, body)
	}
	return statuses
}

// assertNotSubstitutedNotFound is the whole point of the change: whatever the
// answer is, it is not "it does not exist", and it carries nothing of the
// driver's.
func assertNotSubstitutedNotFound(t *testing.T, status int, body string) {
	t.Helper()
	if status == http.StatusNotFound {
		t.Errorf("a storage failure was reported as 404 — the caller is told the resource is gone and stops retrying; body: %s", body)
		return
	}
	if status < 400 {
		return // the pool reconnected before the probe landed; nothing to assert
	}
	var pd struct {
		Detail string         `json:"detail"`
		Ticket string         `json:"ticket"`
		Props  map[string]any `json:"properties"`
	}
	if err := json.Unmarshal([]byte(body), &pd); err != nil {
		t.Fatalf("problem detail is not JSON: %v; body: %s", err, body)
	}
	if status >= 500 && pd.Ticket == "" {
		t.Errorf("5xx carries no ticket to correlate the server-side log with: %s", body)
	}
	for _, leak := range []string{"postgres://", "password", "dbname=", "sqlstate", "57p01", "pg_terminate"} {
		if strings.Contains(strings.ToLower(body), leak) {
			t.Errorf("response leaked storage internals (%q): %s", leak, body)
		}
	}
}

// countFailures reports how many probes actually reached the dead connection.
// Every cycle normally does; the run is only meaningless if none did.
func countFailures(t *testing.T, statuses []int) {
	t.Helper()
	failed := 0
	for _, s := range statuses {
		if s >= 400 {
			failed++
		}
	}
	if failed == 0 {
		t.Fatalf("no probe reached the torn connection in %d cycles (statuses %v); the fault was never injected, so this test asserted nothing",
			lookupFailureCycles, statuses)
	}
}

// --- async search job lookup ------------------------------------------------

// settledJob submits an async search and waits for it to reach a terminal
// status, so that every lookup below is of a job that genuinely exists — which
// is what makes a 404 afterwards provably wrong rather than merely suspicious.
func settledJob(t *testing.T, h *callbackHarness, model string) string {
	t.Helper()
	resp := h.DoAuth(t, http.MethodPost, "/api/search/async/"+model+"/1",
		`{"type":"simple","jsonPath":"$.name","operatorType":"EQUALS","value":"Alice"}`, "")
	body := h.readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("submit async search: %d %s", resp.StatusCode, body)
	}
	jobID := strings.Trim(strings.TrimSpace(body), `"`)

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		r := h.DoAuth(t, http.MethodGet, "/api/search/async/"+jobID+"/status", "", "")
		b := h.readBody(t, r)
		if strings.Contains(b, "SUCCESSFUL") || strings.Contains(b, "FAILED") {
			return jobID
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("async search job never settled")
	return ""
}

// TestE2E_AsyncSearchLookup_StorageFailureIsNotNotFound covers
// getAsyncSearchStatus and getAsyncSearchResults on a running backend. Before
// the classification change both answered 404 SEARCH_JOB_NOT_FOUND here.
func TestE2E_AsyncSearchLookup_StorageFailureIsNotNotFound(t *testing.T) {
	h := newLookupFailureHarness(t)
	model := lookupFailureModel(t, "lookupfail")
	h.setupModelSampleWithWorkflow(t, model, lookupFailureSample, secondaryWorkflow)
	jobID := settledJob(t, h, model)
	k := newSessionKiller(t)

	get := func(path string) func() (int, string) {
		return func() (int, string) {
			resp := h.DoAuth(t, http.MethodGet, path, "", "")
			return resp.StatusCode, h.readBody(t, resp)
		}
	}
	statusPath := "/api/search/async/" + jobID + "/status"
	resultsPath := "/api/search/async/" + jobID

	t.Run("status", func(t *testing.T) {
		countFailures(t, probeAfterKill(t, k, get(statusPath), get(statusPath)))
	})
	t.Run("results", func(t *testing.T) {
		countFailures(t, probeAfterKill(t, k, get(resultsPath), get(resultsPath)))
	})

	// The other direction, on the same running backend: a job that genuinely is
	// not there is still 404 with its shipped error code.
	unknown := "00000000-0000-4000-8000-0000000000ff"
	for _, path := range []string{"/api/search/async/" + unknown + "/status", "/api/search/async/" + unknown} {
		resp := h.DoAuth(t, http.MethodGet, path, "", "")
		body := h.readBody(t, resp)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: status = %d, want 404; body: %s", path, resp.StatusCode, body)
		}
		if !strings.Contains(body, "SEARCH_JOB_NOT_FOUND") {
			t.Errorf("GET %s: want SEARCH_JOB_NOT_FOUND; body: %s", path, body)
		}
	}
}

// TestE2E_StateMachineFinishedEvent_StorageFailureIsNotNotFound covers
// getStateMachineFinishedEvent. Before the change a failed audit read answered
// 404 "no events found" — indistinguishable from a workflow that never ran.
func TestE2E_StateMachineFinishedEvent_StorageFailureIsNotNotFound(t *testing.T) {
	h := newLookupFailureHarness(t)
	model := lookupFailureModel(t, "lookupfail")
	h.setupModelSampleWithWorkflow(t, model, lookupFailureSample, secondaryWorkflow)

	entityID, status, body := h.CreateEntity(t, model, 1, lookupFailureSample)
	if status != http.StatusOK {
		t.Fatalf("create entity: %d %s", status, body)
	}
	var txID string
	for _, ev := range h.GetSMAuditEvents(t, entityID) {
		if id, _ := ev["transactionId"].(string); id != "" {
			txID = id
			break
		}
	}
	if txID == "" {
		t.Fatal("no state-machine event carried a transaction id")
	}
	path := fmt.Sprintf("/api/audit/entity/%s/workflow/%s/finished", entityID, txID)
	call := func() (int, string) {
		resp := h.DoAuth(t, http.MethodGet, path, "", "")
		return resp.StatusCode, h.readBody(t, resp)
	}

	k := newSessionKiller(t)
	countFailures(t, probeAfterKill(t, k, call, call))

	// The other direction: a transaction with no events recorded against it is
	// still 404 on the same running backend.
	unknownTx := fmt.Sprintf("/api/audit/entity/%s/workflow/%s/finished", entityID, "00000000-0000-4000-8000-0000000000ff")
	resp := h.DoAuth(t, http.MethodGet, unknownTx, "", "")
	if b := h.readBody(t, resp); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown transaction: status = %d, want 404; body: %s", resp.StatusCode, b)
	}
}

// TestE2E_TrustedKeyMutations_StorageFailureIsNotNotFound covers
// deleteTrustedKey, invalidateTrustedKey and reactivateTrustedKey. Before the
// change all three answered 404 TRUSTED_KEY_NOT_FOUND when the KV write failed,
// which told an admin the key was gone while it was still live.
func TestE2E_TrustedKeyMutations_StorageFailureIsNotNotFound(t *testing.T) {
	h := newLookupFailureHarness(t)
	const kid = "lookupfail-trusted-key"
	registerTrustedTestKey(t, h, kid)
	k := newSessionKiller(t)

	post := func(path, body string) func() (int, string) {
		return func() (int, string) {
			resp := h.DoAuth(t, http.MethodPost, path, body, "")
			return resp.StatusCode, h.readBody(t, resp)
		}
	}
	base := "/api/oauth/keys/trusted/" + kid
	validTo := time.Now().Add(24 * time.Hour).Format(time.RFC3339)
	reactivateBody := `{"validTo":"` + validTo + `"}`

	// Invalidate and reactivate both leave the key registered whether they
	// succeed or fail, so each cycle finds it and reaches the KV write.
	t.Run("invalidate", func(t *testing.T) {
		countFailures(t, probeAfterKill(t, k, post(base+"/invalidate", ""), post(base+"/invalidate", "")))
	})
	t.Run("reactivate", func(t *testing.T) {
		countFailures(t, probeAfterKill(t, k, post(base+"/reactivate", reactivateBody), post(base+"/reactivate", reactivateBody)))
	})

	// Delete is not repeatable — a successful one removes the key and every
	// later attempt is a genuine 404 — so it runs once, last, on its own key.
	const deleteKid = "lookupfail-trusted-delete"
	registerTrustedTestKey(t, h, deleteKid)
	warm := func() (int, string) {
		resp := h.DoAuth(t, http.MethodGet, "/api/oauth/keys/trusted", "", "")
		return resp.StatusCode, h.readBody(t, resp)
	}
	if status, body := warm(); status >= 500 {
		t.Fatalf("warm-up listing failed: %d %s", status, body)
	}
	if killed := k.kill(t); killed == 0 {
		t.Fatal("no harness session to terminate before the delete")
	}
	resp := h.DoAuth(t, http.MethodDelete, "/api/oauth/keys/trusted/"+deleteKid, "", "")
	body := h.readBody(t, resp)
	t.Logf("delete after kill: status=%d body=%s", resp.StatusCode, body)
	assertNotSubstitutedNotFound(t, resp.StatusCode, body)

	// The other direction, on the same running backend: a keyId that was never
	// registered is still 404 TRUSTED_KEY_NOT_FOUND.
	resp = h.DoAuth(t, http.MethodDelete, "/api/oauth/keys/trusted/never-registered", "", "")
	body = h.readBody(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown keyId: status = %d, want 404; body: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "TRUSTED_KEY_NOT_FOUND") {
		t.Errorf("unknown keyId: want TRUSTED_KEY_NOT_FOUND; body: %s", body)
	}
}

// registerTrustedTestKey registers an RSA trusted key on the harness stack.
func registerTrustedTestKey(t *testing.T, h *callbackHarness, kid string) {
	t.Helper()
	jwk := rsaJWK(t, kid)
	body, err := json.Marshal(map[string]any{"keyId": kid, "jwk": jwk, "audience": "human"})
	if err != nil {
		t.Fatalf("marshal register body: %v", err)
	}
	resp := h.DoAuth(t, http.MethodPost, "/api/oauth/keys/trusted", string(body), "")
	if b := h.readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("register trusted key %s: %d %s", kid, resp.StatusCode, b)
	}
}
