package e2e_test

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// callout_success_default_test.go: a compute member's `success` is optional
// with the default `true` (docs/cyoda/schema/common/BaseEvent.json). A member
// that omits it has answered success, and the operation behind the callout
// completes — the published schema and the running server agree.
//
// The default belongs to an absent key alone: an explicit `"success": null` is
// not a boolean, and the operation behind such an answer fails rather than
// reading a verdict out of it.

// TestCalloutOmittedSuccess_ProcessorSucceeds: a processor answers with the
// entity's new data and no `success` key. The transition runs, the data is
// applied and the client gets its 200 — where the same bytes used to end the
// operation as `400 WORKFLOW_FAILED`.
func TestCalloutOmittedSuccess_ProcessorSucceeds(t *testing.T) {
	h := newCalloutHarness(t, nil)
	sfx := randSuffix(t) // repeated runs (go test -count=N) share this package's Postgres testcontainer
	model, wf, proc, tag := "omitsuccess-"+sfx, "omitsuccess-wf-"+sfx, "omitsuccess-proc-"+sfx, "omitsuccess-tag-"+sfx

	h.AttachCnode(t, cnodeSpec{name: "omitsuccess", tags: []string{tag},
		script: scriptAlways(answerDataNoSuccessKey(map[string]any{
			"name": "Test Order", "amount": 100, "status": "processed",
		}))})
	h.SetupModelWithWorkflow(t, model, procWorkflowJSON(wf, proc, "SYNC",
		map[string]any{"calculationNodesTags": tag}))

	id, status, body := h.CreateEntity(t, model, 1, workflowSampleModel)
	if status != http.StatusOK {
		t.Fatalf("create behind a processor that omitted `success`: %d %s", status, body)
	}
	if got := h.GetEntityData(t, id)["status"]; got != "processed" {
		t.Errorf("status = %v; want processed: the processor's data was not applied", got)
	}
	if state, st := h.GetEntityState(t, id); state != "DONE" {
		t.Errorf("state = %q (GET %d); want DONE", state, st)
	}
}

// TestCalloutNullSuccess_CriterionIsUnreadable: a criterion answers
// `{"success":null,"matches":true}`. The verdict is there to be read and
// reading it would let a member whose answer says nothing about success decide
// a transition — so the running server refuses the whole answer instead: 400
// WORKFLOW_FAILED naming the unreadable key, not retryable, no entity
// committed, and no second try, the refusal being one another member would
// answer no better.
func TestCalloutNullSuccess_CriterionIsUnreadable(t *testing.T) {
	h := newCalloutHarness(t, nil)
	sfx := randSuffix(t) // repeated runs (go test -count=N) share this package's Postgres testcontainer
	model, wf, crit, tag := "nullsuccess-"+sfx, "nullsuccess-wf-"+sfx, "nullsuccess-crit-"+sfx, "nullsuccess-tag-"+sfx

	cn := h.AttachCnode(t, cnodeSpec{name: "nullsuccess", tags: []string{tag},
		script: scriptAlways(answerMatchesNullSuccess(true))})
	h.SetupModelWithWorkflow(t, model, criterionWorkflowJSON(wf, crit,
		map[string]any{"calculationNodesTags": tag, "responseTimeoutMs": 2000}))

	_, status, body := h.CreateEntity(t, model, 1, workflowSampleModel)
	pd := assertProblem(t, status, body, http.StatusBadRequest, "WORKFLOW_FAILED", false)
	if !strings.Contains(pd.Detail, "success was null") {
		t.Errorf("detail = %q; want it to name the key that could not be read", pd.Detail)
	}
	if n := h.countEntities(t, model); n != 0 {
		t.Errorf("%d entities committed; want none: the verdict was not read and the transition did not run", n)
	}
	if got := cn.Received(); len(got) != 1 {
		t.Errorf("the member received %d callouts; want 1: an unreadable answer is not tried again", len(got))
	}
}

// TestCalloutBadSuccess_AnswerThatDoesNotDecodeEndsTheCallout: a criterion
// answers `{"success":"yes","matches":true}` — present, not a boolean, so the
// answer does not decode into the shape its event type promises.
//
// What this pins is that it ends where the null does, and as promptly. The
// answer arrived; treating it as silence would spend the callout's whole
// answer limit and a try on a member that did in fact reply, and hand the
// client a retryable 503 about a server that was never unavailable. So:
// 400 WORKFLOW_FAILED, not retryable, nothing committed — and one callout on
// the member, which is the assertion the fix exists for.
func TestCalloutBadSuccess_AnswerThatDoesNotDecodeEndsTheCallout(t *testing.T) {
	h := newCalloutHarness(t, nil)
	sfx := randSuffix(t) // repeated runs (go test -count=N) share this package's Postgres testcontainer
	model, wf, crit, tag := "badsuccess-"+sfx, "badsuccess-wf-"+sfx, "badsuccess-crit-"+sfx, "badsuccess-tag-"+sfx

	cn := h.AttachCnode(t, cnodeSpec{name: "badsuccess", tags: []string{tag},
		script: scriptAlways(answerMatchesBadSuccess(true))})
	h.SetupModelWithWorkflow(t, model, criterionWorkflowJSON(wf, crit,
		map[string]any{"calculationNodesTags": tag, "responseTimeoutMs": 2000}))

	started := time.Now()
	_, status, body := h.CreateEntity(t, model, 1, workflowSampleModel)
	elapsed := time.Since(started)

	pd := assertProblem(t, status, body, http.StatusBadRequest, "WORKFLOW_FAILED", false)
	if !strings.Contains(pd.Detail, "did not decode") {
		t.Errorf("detail = %q; want it to say the answer did not decode", pd.Detail)
	}
	if n := h.countEntities(t, model); n != 0 {
		t.Errorf("%d entities committed; want none: the verdict was not read and the transition did not run", n)
	}
	if got := cn.Received(); len(got) != 1 {
		t.Errorf("the member received %d callouts; want 1: an unreadable answer is not tried again", len(got))
	}
	// The answer limit is 2s per try. Ending on the answer rather than waiting
	// it out is the whole point, so the operation must not have taken one.
	if elapsed >= 2*time.Second {
		t.Errorf("the operation took %v; want well under the 2s answer limit — the answer arrived and must end the callout at once", elapsed)
	}
}
