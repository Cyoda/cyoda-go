package e2e_test

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
)

// scheduled_fire_unique_keys_test.go — a scheduled fire's unique-key claims
// on PostgreSQL. The fire's commit computes them from its final entity and the
// stored model's keys, like any other write: a fire that changes the key field
// moves the claim, and a fire whose final entity duplicates a key is a failed
// run, retried until its timeoutMs has passed and then FAILED.

// setupKeyedSchedModel imports the sample model, gives it the unique key
// $.name, locks it and imports workflowJSON.
func setupKeyedSchedModel(t *testing.T, h *callbackHarness, model, workflowJSON string) {
	t.Helper()
	steps := []struct{ method, path, body string }{
		{http.MethodPost, fmt.Sprintf("/api/model/import/JSON/SAMPLE_DATA/%s/1", model), workflowSampleModel},
		{http.MethodPut, fmt.Sprintf("/api/model/%s/1/unique-keys", model), `{"uniqueKeys":[{"id":"name-key","fields":["$.name"]}]}`},
		{http.MethodPut, fmt.Sprintf("/api/model/%s/1/lock", model), ""},
		{http.MethodPost, fmt.Sprintf("/api/model/%s/1/workflow/import", model), workflowJSON},
	}
	for _, s := range steps {
		resp := h.DoAuth(t, s.method, s.path, s.body, "")
		if body := h.readBody(t, resp); resp.StatusCode != http.StatusOK {
			t.Fatalf("%s %s: %d %s", s.method, s.path, resp.StatusCode, body)
		}
	}
}

// namedOrder is the sample payload with name set.
func namedOrder(name string) string {
	return fmt.Sprintf(`{"name":%q,"amount":100,"status":"draft"}`, name)
}

// TestSchedFire_KeyChangeMovesClaim: the fire's processor changes $.name from
// A to B. After the fire, B is held (a create with B is 409 UNIQUE_VIOLATION)
// and A is free (a create with A succeeds).
func TestSchedFire_KeyChangeMovesClaim(t *testing.T) {
	h, _ := newSchedulerHarness(t, nil)
	model, tag := uniq("sf-uk-move"), uniq("sf-uk-move-tag")
	nameA, nameB := uniq("A"), uniq("B")
	// The first callout renames the fired entity to B; later ones (the probe
	// entity's own fire) return no data.
	var calls atomic.Int32
	h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{tag}, script: func(context.Context, receivedCallout, *reqCtx) cnodeReply {
		if calls.Add(1) == 1 {
			return answerData(map[string]any{"name": nameB, "amount": 100, "status": "draft"})
		}
		return answerOK()
	}})
	setupKeyedSchedModel(t, h, model, fireOpenToDone("sf-uk-move-wf", 100, 0, sProc("p", "SYNC", tag, true)))
	id := createOpen(t, h, model, namedOrder(nameA))

	awaitCallbackEntityState(t, h, id, "Done", scheduledFireTimeout)
	if got := h.GetEntityData(t, id)["name"]; got != nameB {
		t.Fatalf("fired entity name = %v; want %s", got, nameB)
	}

	_, status, body := h.CreateEntity(t, model, 1, namedOrder(nameB))
	if status != http.StatusConflict {
		t.Fatalf("create with the fired entity's new name: %d %s; want 409", status, body)
	}
	assertErrorCode(t, body, "UNIQUE_VIOLATION")
	if _, status, body := h.CreateEntity(t, model, 1, namedOrder(nameA)); status != http.StatusOK {
		t.Errorf("create with the fired entity's old name: %d %s; want 200 (the fire freed it)", status, body)
	}
}

// TestSchedFire_BreaksKey_FailedRun: the fire's processor renames the entity
// to a name another entity holds. Each run fails at commit and commits
// nothing; the processor is idempotent, so each failure is a counted attempt,
// and the task ends FAILED EXPIRED_AFTER_FAILED_ATTEMPTS once its timeoutMs
// has passed, with the UNIQUE_VIOLATION text a client write gets and the
// SCHEDULED_TRANSITION_FAIL event.
func TestSchedFire_BreaksKey_FailedRun(t *testing.T) {
	h, s := newSchedulerHarness(t, nil)
	model, tag := uniq("sf-uk-break"), uniq("sf-uk-break-tag")
	nameA, nameB := uniq("A"), uniq("B")
	h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{tag},
		script: scriptAlways(answerData(map[string]any{"name": nameB, "amount": 100, "status": "draft"}))})
	setupKeyedSchedModel(t, h, model, fireOpenToDone("sf-uk-break-wf", 100, 2000, sProc("p", "SYNC", tag, true)))

	// The holder of B: its own fire renames it to B again, and ends Done.
	holder := createOpen(t, h, model, namedOrder(nameB))
	awaitCallbackEntityState(t, h, holder, "Done", scheduledFireTimeout)

	id := createOpen(t, h, model, namedOrder(nameA))
	r := awaitFailed(t, s, id, "Fire")
	if r.FailureReason != "EXPIRED_AFTER_FAILED_ATTEMPTS" || r.Attempts < 1 || r.ClaimToken != "" || r.Marked {
		t.Errorf("failed task = %+v; want EXPIRED_AFTER_FAILED_ATTEMPTS after at least one counted attempt, no claim, no mark", r)
	}
	// The text a client write gets for the same commit, not a ticket.
	if want := "UNIQUE_VIOLATION: a composite unique key constraint was violated"; r.LastError != want {
		t.Errorf("lastError = %q; want %q", r.LastError, want)
	}
	if data := failEvent(t, h, id); data["reason"] != "EXPIRED_AFTER_FAILED_ATTEMPTS" {
		t.Errorf("SCHEDULED_TRANSITION_FAIL data = %v", data)
	}
	requireState(t, h, id, "Open")
	if got := h.GetEntityData(t, id)["name"]; got != nameA {
		t.Errorf("entity name = %v; want %s (no run committed)", got, nameA)
	}
	if got := h.GetEntityData(t, holder)["name"]; got != nameB {
		t.Errorf("holder name = %v; want %s", got, nameB)
	}
}
