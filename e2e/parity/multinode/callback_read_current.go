package multinode

import (
	"encoding/json"
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// callback_read_current.go — a callout on another node reads the entity its
// workflow runs for (the anchor) as the workflow has it when the callout is
// made: with the data the request sent, and found on a create.
//
// The request goes to node 1, the owner, which has no compute member. Its SYNC
// processor requires computeMemberTag, which only node 0 advertises, so the
// callout is handed to node 0; the member's callbacks carry the transaction
// token, land on node 0 and are proxied to node 1, where they join the
// transaction. The processor is the compute-test-client's cb-read-anchor: it
// only reads the anchor (GET, a search and a grouped count by its current
// data.status, the model's first page) and reports what it saw through its
// returned data. A callout never writes the entity it runs for.

func init() {
	Register(
		NamedTest{Name: "CallbackReadCurrent_OtherNode_UpdateSync", Fn: RunCallbackReadCurrent_OtherNode_UpdateSync},
		NamedTest{Name: "CallbackReadCurrent_OtherNode_CreateSync", Fn: RunCallbackReadCurrent_OtherNode_CreateSync},
	)
}

// mnCRCSample declares every field cb-read-anchor returns, at zero value.
const mnCRCSample = `{"name":"Test","amount":10,"status":"new","anchorGetStatus":0,"anchorGetState":"","anchorGetMarker":"","anchorSearchHits":0,"anchorCountByMarker":0,"anchorPageMarker":""}`

// mnCRCContext is the processor context naming the anchor's model.
func mnCRCContext(anchorModel string) string {
	b, _ := json.Marshal(map[string]any{"anchorModel": anchorModel})
	return string(b)
}

// mnCRCRequireReads asserts what cb-read-anchor recorded: every joined read
// found the anchor with data.status wantMarker, in wantState.
func mnCRCRequireReads(t *testing.T, data map[string]any, wantState, wantMarker string) {
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

// mnCRCOwner returns a client of node 1, the owner, after checking the cluster
// has the two nodes the scenario needs.
func mnCRCOwner(t *testing.T, fixture MultiNodeFixture, token string) (setup, owner *client.Client) {
	t.Helper()
	urls := fixture.BaseURLs()
	if len(urls) < 2 {
		t.Fatalf("a callout on another node needs ≥2 nodes, got %d", len(urls))
	}
	return client.NewClient(urls[0], token), client.NewClient(urls[1], token)
}

// RunCallbackReadCurrent_OtherNode_UpdateSync: an update sent to node 1 carries
// new data and names a manual transition whose SYNC processor runs on node 0's
// compute member and reads the anchor under the token. Every read door sees
// the request's data, in the transition's source state — not the last
// committed copy.
func RunCallbackReadCurrent_OtherNode_UpdateSync(t *testing.T, fixture MultiNodeFixture) {
	tenant := fixture.ComputeTenant(t)
	cSetup, cOwner := mnCRCOwner(t, fixture, tenant.Token)
	const model = "mn-crc-update-sync"
	wf, _ := json.Marshal(map[string]any{
		"importMode": "REPLACE",
		"workflows": []any{map[string]any{
			"version": "1.1", "name": "mn-crc-update-sync-wf", "initialState": "NONE", "active": true,
			"states": map[string]any{
				"NONE": map[string]any{"transitions": []any{map[string]any{"name": "init", "next": "READY", "manual": false}}},
				"READY": map[string]any{"transitions": []any{map[string]any{
					"name": "go", "next": "DONE", "manual": true,
					"processors": []any{map[string]any{
						"type": "externalized", "name": "cb-read-anchor", "executionMode": "SYNC",
						"config": map[string]any{"attachEntity": true, "calculationNodesTags": computeMemberTag, "context": mnCRCContext(model)},
					}},
				}}},
				"DONE": map[string]any{},
			},
		}},
	})
	cbRouteSetupModel(t, cSetup, model, mnCRCSample, string(wf))

	id, err := cOwner.CreateEntity(t, model, 1, `{"name":"anchor","amount":1,"status":"old"}`)
	if err != nil {
		t.Fatalf("create via node 1: %v", err)
	}
	if err := cOwner.UpdateEntity(t, id, "go", `{"name":"anchor","amount":2,"status":"mn-crc-new"}`); err != nil {
		t.Fatalf("update via node 1 (forwarded dispatch + cross-node callback): %v", err)
	}
	got, err := cOwner.GetEntity(t, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Meta.State != "DONE" {
		t.Fatalf("state = %q; want DONE", got.Meta.State)
	}
	mnCRCRequireReads(t, got.Data, "READY", "mn-crc-new")
}

// RunCallbackReadCurrent_OtherNode_CreateSync: a create sent to node 1 runs a
// SYNC processor on node 0's compute member that reads the anchor under the
// token. The anchor is not committed, and every read door finds it with the
// data the request sent.
func RunCallbackReadCurrent_OtherNode_CreateSync(t *testing.T, fixture MultiNodeFixture) {
	tenant := fixture.ComputeTenant(t)
	cSetup, cOwner := mnCRCOwner(t, fixture, tenant.Token)
	const model = "mn-crc-create-sync"
	cbRouteSetupModel(t, cSetup, model, mnCRCSample,
		cbRoutePrimaryWorkflow("mn-crc-create-sync-wf", "cb-read-anchor", mnCRCContext(model)))

	id, err := cOwner.CreateEntity(t, model, 1, `{"name":"anchor","amount":1,"status":"mn-crc-created"}`)
	if err != nil {
		t.Fatalf("create via node 1 (forwarded dispatch + cross-node callback): %v", err)
	}
	got, err := cOwner.GetEntity(t, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Meta.State != "ACTIVE" {
		t.Fatalf("state = %q; want ACTIVE", got.Meta.State)
	}
	mnCRCRequireReads(t, got.Data, "NONE", "mn-crc-created")
}
