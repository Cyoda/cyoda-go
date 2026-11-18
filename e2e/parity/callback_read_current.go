package parity

import (
	"encoding/json"
	"testing"

	"github.com/cyoda/cyoda-go/e2e/parity/client"
)

// callback_read_current.go — a callout that carries the transaction token
// reads the entity its workflow runs for (the anchor) as the workflow has it
// when the callout is made, on every backend: with the data the request sent,
// and found on a create. The GET, a search and a grouped count by the
// anchor's current data, and the model's first page all answer so.
//
// Driven by the compute-test-client's cb-read-anchor processor and
// cb-criterion-reads-anchor criterion, which only READ the anchor: a callout
// never writes the entity it runs for. The processor reports what it saw
// through its returned data.

// crcSample declares every field cb-read-anchor returns, at zero value.
const crcSample = `{"name":"Test","amount":10,"status":"new","anchorGetStatus":0,"anchorGetState":"","anchorGetMarker":"","anchorSearchHits":0,"anchorCountByMarker":0,"anchorPageMarker":""}`

// crcContext is the processor context naming the anchor's model and, for the
// criterion, the marker it matches.
func crcContext(anchorModel, marker string) string {
	b, _ := json.Marshal(map[string]any{"anchorModel": anchorModel, "marker": marker})
	return string(b)
}

// crcRequireReads asserts what cb-read-anchor recorded: every joined read
// found the anchor with data.status wantMarker, in wantState.
func crcRequireReads(t *testing.T, data map[string]any, wantState, wantMarker string) {
	t.Helper()
	if got, _ := data["anchorGetStatus"].(float64); got != 200 {
		t.Fatalf("joined GET of the anchor answered %v; want 200 (the transaction holds the anchor before the callout): data=%+v", data["anchorGetStatus"], data)
	}
	if data["anchorGetState"] != wantState || data["anchorGetMarker"] != wantMarker {
		t.Errorf("joined GET saw state=%v status=%v; want state=%q status=%q", data["anchorGetState"], data["anchorGetMarker"], wantState, wantMarker)
	}
	if got, _ := data["anchorSearchHits"].(float64); got != 1 {
		t.Errorf("joined search by the anchor's current status found it %v time(s); want 1", data["anchorSearchHits"])
	}
	if got, _ := data["anchorCountByMarker"].(float64); got != 1 {
		t.Errorf("joined grouped count of the anchor's current status = %v; want 1", data["anchorCountByMarker"])
	}
	if data["anchorPageMarker"] != wantMarker {
		t.Errorf("joined page listed the anchor with status %v; want %q", data["anchorPageMarker"], wantMarker)
	}
}

// RunCallbackReadCurrent_UpdateSync: an update sends new data and names a
// manual transition whose SYNC processor reads the anchor under the token.
// Every read door sees the request's data, in the transition's source state.
func RunCallbackReadCurrent_UpdateSync(t *testing.T, fixture BackendFixture) {
	tenant := fixture.ComputeTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)
	const model = "crc-update-sync"
	cbSetupModel(t, c, model, crcSample, cbWorkflowDoc("crc-update-sync-wf", map[string]any{
		"NONE":  map[string]any{"transitions": []any{map[string]any{"name": "init", "next": "READY", "manual": false}}},
		"READY": map[string]any{"transitions": []any{map[string]any{"name": "go", "next": "DONE", "manual": true, "processors": []any{cbProc("cb-read-anchor", "SYNC", crcContext(model, ""), nil)}}}},
		"DONE":  map[string]any{},
	}))

	id, err := c.CreateEntity(t, model, 1, `{"name":"anchor","amount":1,"status":"old"}`)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := c.UpdateEntity(t, id, "go", `{"name":"anchor","amount":2,"status":"crc-new"}`); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err := c.GetEntity(t, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	crcRequireReads(t, got.Data, "READY", "crc-new")
}

// RunCallbackReadCurrent_CreateSync: a create's SYNC processor reads the
// anchor under the token. The anchor is not committed, and every read door
// finds it with the data the request sent.
func RunCallbackReadCurrent_CreateSync(t *testing.T, fixture BackendFixture) {
	tenant := fixture.ComputeTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)
	const model = "crc-create-sync"
	cbSetupModel(t, c, model, crcSample,
		cbPrimaryProcWorkflow("crc-create-sync-wf", "cb-read-anchor", "SYNC", crcContext(model, "")))

	id, err := c.CreateEntity(t, model, 1, `{"name":"anchor","amount":1,"status":"crc-created"}`)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := c.GetEntity(t, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	crcRequireReads(t, got.Data, "NONE", "crc-created")
}

// RunCallbackReadCurrent_FunctionCriterion: an update sends new data through a
// manual transition into MID, whose automated transition is guarded by a
// function criterion that matches only when its joined read of the anchor
// shows the request's data. The entity reaches DONE.
func RunCallbackReadCurrent_FunctionCriterion(t *testing.T, fixture BackendFixture) {
	tenant := fixture.ComputeTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)
	const model = "crc-function-criterion"
	const marker = "crc-crit-new"
	cbSetupModel(t, c, model, cbSampleNoWriteback, cbWorkflowDoc("crc-function-criterion-wf", map[string]any{
		"NONE":  map[string]any{"transitions": []any{map[string]any{"name": "init", "next": "READY", "manual": false}}},
		"READY": map[string]any{"transitions": []any{map[string]any{"name": "go", "next": "MID", "manual": true}}},
		"MID": map[string]any{"transitions": []any{map[string]any{
			"name": "check", "next": "DONE", "manual": false,
			"criterion": map[string]any{"type": "function", "function": map[string]any{
				"name":   "cb-criterion-reads-anchor",
				"config": map[string]any{"attachEntity": true, "calculationNodesTags": "", "context": crcContext(model, marker)},
			}},
		}}},
		"DONE": map[string]any{},
	}))

	id, err := c.CreateEntity(t, model, 1, `{"name":"anchor","amount":1,"status":"old"}`)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := c.UpdateEntity(t, id, "go", `{"name":"anchor","amount":2,"status":"`+marker+`"}`); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err := c.GetEntity(t, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Meta.State != "DONE" {
		t.Fatalf("state = %q; want DONE — the criterion's joined read did not see the request's data", got.Meta.State)
	}
}

// RunCallbackPayloadTxID_IdenticalUpdate: an update whose data equals the
// stored data names a manual transition whose SYNC processor records the
// callout's transactionId and its payload's meta.transactionId. They are
// equal on every backend, whether or not the transaction wrote the entity
// before the callout.
func RunCallbackPayloadTxID_IdenticalUpdate(t *testing.T, fixture BackendFixture) {
	tenant := fixture.ComputeTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)
	const model = "crc-payload-txid"
	const body = `{"name":"anchor","amount":1,"status":"same"}`
	cbSetupModel(t, c, model, `{"name":"Test","amount":10,"status":"new","requestTxId":"","payloadTxId":""}`,
		cbWorkflowDoc("crc-payload-txid-wf", map[string]any{
			"NONE":  map[string]any{"transitions": []any{map[string]any{"name": "init", "next": "READY", "manual": false}}},
			"READY": map[string]any{"transitions": []any{map[string]any{"name": "go", "next": "DONE", "manual": true, "processors": []any{cbProc("record-txids", "SYNC", "", nil)}}}},
			"DONE":  map[string]any{},
		}))

	id, err := c.CreateEntity(t, model, 1, body)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := c.UpdateEntity(t, id, "go", body); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err := c.GetEntity(t, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	requestTxID, _ := got.Data["requestTxId"].(string)
	payloadTxID, _ := got.Data["payloadTxId"].(string)
	if requestTxID == "" {
		t.Fatalf("the processor saw no request transactionId: data=%+v", got.Data)
	}
	if payloadTxID != requestTxID {
		t.Errorf("payload meta.transactionId = %q, want the callout's transactionId %q", payloadTxID, requestTxID)
	}
}
