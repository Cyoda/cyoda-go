package e2e_test

import (
	"net/http"
	"testing"
)

// processor_no_payload_test.go: a processor that answers with no data — no
// payload, or a payload whose data is null — leaves the entity as it is.

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
