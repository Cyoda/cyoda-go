package e2e_test

import (
	"context"
	"net/http"
	"testing"
)

// processor_no_payload_test.go: a processor that answers with no data — no
// payload, or a payload whose data is null — leaves the entity as it is in the
// transaction. That includes a write the processor made to the entity through
// a joined callback: the engine keeps that write and does not apply the
// payload it dispatched with over it.

// TestProcessorNullData_EntityLeftAsItWas: `"payload": {"data": null}` is not
// data to apply. The transition runs and the entity keeps the data it had.
func TestProcessorNullData_EntityLeftAsItWas(t *testing.T) {
	h := newCalloutHarness(t, nil)
	sfx := randSuffix(t) // repeated runs (go test -count=N) share this package's Postgres testcontainer
	model, wf, proc, tag := "nulldata-"+sfx, "nulldata-wf-"+sfx, "nulldata-proc-"+sfx, "nulldata-tag-"+sfx

	h.AttachCnode(t, cnodeSpec{name: "nulldata", tags: []string{tag}, script: scriptAlways(answerNullData())})
	h.SetupModelWithWorkflow(t, model, procWorkflowJSON(wf, proc, "SYNC",
		map[string]any{"calculationNodesTags": tag}))

	id, status, body := h.CreateEntity(t, model, 1, workflowSampleModel)
	if status != http.StatusOK {
		t.Fatalf("create behind a processor that answered null data: %d %s", status, body)
	}
	data := h.GetEntityData(t, id)
	if amount, _ := data["amount"].(float64); amount != 100 || data["status"] != "draft" {
		t.Errorf("data = %v; want the entity as it was created", data)
	}
	requireState(t, h, id, "DONE")
}

// TestProcessorNullData_KeepsJoinedCallbackWrite: a processor writes its
// entity through the joined callback and answers with null data. The write is
// kept, as it is when the processor answers with no payload at all.
func TestProcessorNullData_KeepsJoinedCallbackWrite(t *testing.T) {
	h := newCalloutHarness(t, nil)
	sfx := randSuffix(t)
	model, tag := "nulldata-jw-"+sfx, "nulldata-jw-tag-"+sfx
	cb := make(chan int, 1)
	h.AttachCnode(t, cnodeSpec{name: "nulldata-jw", tags: []string{tag},
		script: func(ctx context.Context, rcv receivedCallout, rc *reqCtx) cnodeReply {
			joinedWriteScript(cb)(ctx, rcv, rc)
			return answerNullData()
		}})
	h.SetupModelWithWorkflow(t, model, schedDoc("nulldata-jw-wf-"+sfx, map[string]any{
		"Open": map[string]any{"transitions": []any{map[string]any{"name": "Start", "next": "Mid", "manual": true}}},
		"Mid": map[string]any{"transitions": []any{map[string]any{"name": "Go", "next": "Done", "manual": false,
			"processors": []any{sProc("p1", "SYNC", tag, true)}}}},
		"Done": map[string]any{},
	}))
	id := createOpen(t, h, model, workflowSampleModel)
	resp := h.DoAuth(t, http.MethodPut, "/api/entity/JSON/"+id+"/Start", workflowSampleModel, "")
	if body := h.readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("the transition: %d %s; want 200", resp.StatusCode, body)
	}
	if st := awaitStatus(t, cb, "joined write"); st != http.StatusOK {
		t.Fatalf("the joined write answered %d", st)
	}
	requireState(t, h, id, "Done")
	if amount, _ := h.GetEntityData(t, id)["amount"].(float64); amount != 7 {
		t.Errorf("amount = %v; want 7, the callback's write", amount)
	}
}
