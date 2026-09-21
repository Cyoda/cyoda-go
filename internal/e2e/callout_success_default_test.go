package e2e_test

import (
	"net/http"
	"testing"
)

// callout_success_default_test.go: a compute member's `success` is optional
// with the default `true` (docs/cyoda/schema/common/BaseEvent.json). A member
// that omits it has answered success, and the operation behind the callout
// completes — the published schema and the running server agree.

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
