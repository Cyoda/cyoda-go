package e2e_test

// entity_read_storage_failure_e2e_test.go — the entity half of
// "a lookup that cannot reach storage must not answer 404".
//
// Four operations load the entity before doing their work: delete, the single
// update (and so PATCH, which shares updateEntityCore), the collection update,
// and the transitions listing. Each collapsed any store error into
// ENTITY_NOT_FOUND, so an outage told the caller its entity was gone.
//
// Same harness, fault and contract as lookup_storage_failure_e2e_test.go — read
// its header for why these assert "not 404, 5xx with a ticket, nothing of the
// driver's" rather than a specific 503. The reason applies here too: a
// terminated session arrives as the server's FATAL 57P01, which the plugin
// deliberately leaves unmarked, and the marked shape needs a transport failure a
// shared container cannot be asked for.
//
// Every test also asserts the other direction on the same running backend: an
// entity id that genuinely is not there still answers 404 ENTITY_NOT_FOUND.

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// unknownEntityID is a well-formed UUID no test creates, so a 404 for it is the
// genuine miss rather than a substituted one.
const unknownEntityID = "00000000-0000-4000-8000-0000000000fe"

// entityReadFailureFixture stands up a one-connection stack with a model and one
// entity, and returns the harness, the model name and the entity id.
func entityReadFailureFixture(t *testing.T) (*callbackHarness, string, string) {
	t.Helper()
	h := newLookupFailureHarness(t)
	model := lookupFailureModel(t, "entityreadfail")
	h.setupModelSampleWithWorkflow(t, model, lookupFailureSample, secondaryWorkflow)
	id, status, body := h.CreateEntity(t, model, 1, lookupFailureSample)
	if status != http.StatusOK {
		t.Fatalf("create entity: %d %s", status, body)
	}
	return h, model, id
}

// warmRead is the request that puts a session on the wire for the kill to take.
// A plain entity read: cheap, idempotent, and it mutates nothing, so a probe
// that succeeds because the pool reconnected does not change what the next cycle
// starts from.
func warmRead(t *testing.T, h *callbackHarness, entityID string) func() (int, string) {
	return func() (int, string) {
		resp := h.DoAuth(t, http.MethodGet, "/api/entity/"+entityID, "", "")
		return resp.StatusCode, h.readBody(t, resp)
	}
}

// assertGenuineMissStill404 is the other direction, run against the same live
// backend the outage probes ran against.
func assertGenuineMissStill404(t *testing.T, h *callbackHarness, method, path, body string) {
	t.Helper()
	resp := h.DoAuth(t, method, path, body, "")
	got := h.readBody(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("%s %s: status = %d, want 404; body: %s", method, path, resp.StatusCode, got)
		return
	}
	if !strings.Contains(got, "ENTITY_NOT_FOUND") {
		t.Errorf("%s %s: want ENTITY_NOT_FOUND; body: %s", method, path, got)
	}
}

// TestE2E_GetEntityTransitions_StorageFailureIsNotNotFound covers
// getEntityTransitions. It is the sharpest of the four: its documented alias
// fetchEntityTransitions reads through a classifier that has always been
// correct, so before the fix one storage outage produced two different answers
// depending on which door the caller used.
func TestE2E_GetEntityTransitions_StorageFailureIsNotNotFound(t *testing.T) {
	h, _, entityID := entityReadFailureFixture(t)
	k := newSessionKiller(t)

	probe := func() (int, string) {
		resp := h.DoAuth(t, http.MethodGet, "/api/entity/"+entityID+"/transitions", "", "")
		return resp.StatusCode, h.readBody(t, resp)
	}
	countFailures(t, probeAfterKill(t, k, warmRead(t, h, entityID), probe))

	assertGenuineMissStill404(t, h, http.MethodGet, "/api/entity/"+unknownEntityID+"/transitions", "")
}

// TestE2E_UpdateSingleEntity_StorageFailureIsNotNotFound covers the loopback
// update, which runs updateEntityCore — the same read PATCH and the named
// transition go through.
func TestE2E_UpdateSingleEntity_StorageFailureIsNotNotFound(t *testing.T) {
	h, _, entityID := entityReadFailureFixture(t)
	k := newSessionKiller(t)

	probe := func() (int, string) {
		resp := h.DoAuth(t, http.MethodPut, "/api/entity/JSON/"+entityID, lookupFailureSample, "")
		return resp.StatusCode, h.readBody(t, resp)
	}
	countFailures(t, probeAfterKill(t, k, warmRead(t, h, entityID), probe))

	assertGenuineMissStill404(t, h, http.MethodPut, "/api/entity/JSON/"+unknownEntityID, lookupFailureSample)
}

// TestE2E_UpdateEntityCollection_StorageFailureIsNotNotFound covers the
// per-item read inside the collection update, which aborts the chunk either way
// — what changes is whether the caller is told to stop retrying.
func TestE2E_UpdateEntityCollection_StorageFailureIsNotNotFound(t *testing.T) {
	h, _, entityID := entityReadFailureFixture(t)
	k := newSessionKiller(t)

	collectionBody := func(id string) string {
		return fmt.Sprintf(`[{"id":"%s","payload":"{\"name\":\"Alice\",\"amount\":1,\"status\":\"new\"}"}]`, id)
	}
	probe := func() (int, string) {
		resp := h.DoAuth(t, http.MethodPut, "/api/entity/JSON", collectionBody(entityID), "")
		return resp.StatusCode, h.readBody(t, resp)
	}
	countFailures(t, probeAfterKill(t, k, warmRead(t, h, entityID), probe))

	assertGenuineMissStill404(t, h, http.MethodPut, "/api/entity/JSON", collectionBody(unknownEntityID))
}

// TestE2E_DeleteSingleEntity_StorageFailureIsNotNotFound covers deleteSingleEntity.
// Unlike the other three this one is not repeatable — a delete that succeeds
// makes every later attempt a genuine 404 — so it runs a single cycle on its own
// entity, the same shape the trusted-key delete uses.
func TestE2E_DeleteSingleEntity_StorageFailureIsNotNotFound(t *testing.T) {
	h, model, warmID := entityReadFailureFixture(t)
	victimID, status, body := h.CreateEntity(t, model, 1, lookupFailureSample)
	if status != http.StatusOK {
		t.Fatalf("create victim entity: %d %s", status, body)
	}
	k := newSessionKiller(t)

	if s, b := warmRead(t, h, warmID)(); s >= 500 {
		t.Fatalf("warm-up read failed: %d %s", s, b)
	}
	if killed := k.kill(t); killed == 0 {
		t.Fatal("no harness session to terminate; the warm-up left none on the wire")
	}
	resp := h.DoAuth(t, http.MethodDelete, "/api/entity/"+victimID, "", "")
	got := h.readBody(t, resp)
	t.Logf("delete after kill: status=%d body=%s", resp.StatusCode, got)
	assertNotSubstitutedNotFound(t, resp.StatusCode, got)

	assertGenuineMissStill404(t, h, http.MethodDelete, "/api/entity/"+unknownEntityID, "")
}
