package parity

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// callback_txjoin_conflict.go — a SYNC processor's joined callback loses a
// write race, and the update that ran the processor answers a retryable 409
// CONFLICT on every backend, with or without an If-Match. The race needs no
// second client: cb-race-target writes its target outside the transaction
// first, then again inside it. Where each backend notices the loss differs —
// PostgreSQL at the joined write, memory and sqlite at the commit — and the
// answer must not.

func init() {
	Register(
		NamedTest{Name: "CallbackTxJoin_LostWriteRaceIs409", Fn: RunCallbackTxJoin_LostWriteRaceIs409},
	)
}

const (
	cbRaceTargetSample  = `{"k":1,"flavor":"one"}`
	cbRacePrimarySample = `{"k":1,"flavor":"one","targetId":"00000000-0000-0000-0000-000000000000"}`
)

func RunCallbackTxJoin_LostWriteRaceIs409(t *testing.T, fixture BackendFixture) {
	tenant := fixture.ComputeTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)
	suffix := uuid.NewString()[:8]
	target, primary := "cbtj-race-f-"+suffix, "cbtj-race-e-"+suffix

	setupModelWithWorkflow(t, c, target, 1, cbRaceTargetSample, cbWorkflowDoc(target+"-wf", map[string]any{
		"NONE":   map[string]any{"transitions": []any{map[string]any{"name": "store", "next": "STORED", "manual": false}}},
		"STORED": map[string]any{},
	}))
	setupModelWithWorkflow(t, c, primary, 1, cbRacePrimarySample, cbWorkflowDoc(primary+"-wf", map[string]any{
		"NONE": map[string]any{"transitions": []any{map[string]any{"name": "store", "next": "OPEN", "manual": false}}},
		"OPEN": map[string]any{"transitions": []any{map[string]any{
			"name": "go", "next": "DONE", "manual": false,
			"criterion":  map[string]any{"type": "simple", "jsonPath": "$.flavor", "operatorType": "EQUALS", "value": "go"},
			"processors": []any{cbProc("cb-race-target-strict", "SYNC", "", nil)},
		}}},
		"DONE": map[string]any{},
	}))

	for _, withIfMatch := range []bool{false, true} {
		t.Run(fmt.Sprintf("IfMatch=%v", withIfMatch), func(t *testing.T) {
			fID, err := c.CreateEntity(t, target, 1, cbRaceTargetSample)
			if err != nil {
				t.Fatalf("CreateEntity target: %v", err)
			}
			eID, err := c.CreateEntity(t, primary, 1, fmt.Sprintf(`{"k":1,"flavor":"one","targetId":%q}`, fID))
			if err != nil {
				t.Fatalf("CreateEntity primary: %v", err)
			}
			ifMatch := ""
			if withIfMatch {
				e, err := c.GetEntity(t, eID)
				if err != nil {
					t.Fatalf("GetEntity: %v", err)
				}
				ifMatch = e.Meta.TransactionID
			}

			body := fmt.Sprintf(`{"k":1,"flavor":"go","targetId":%q}`, fID)
			var status int
			var raw []byte
			if withIfMatch {
				status, raw, err = c.UpdateEntityDataWithIfMatchRaw(t, eID, body, ifMatch)
			} else {
				status, raw, err = c.UpdateEntityDataWithIfMatchRaw(t, eID, body, "")
			}
			if err != nil {
				t.Fatalf("update: %v", err)
			}
			var pd struct {
				Properties map[string]any `json:"properties"`
			}
			_ = json.Unmarshal(raw, &pd)
			code, _ := pd.Properties["errorCode"].(string)
			retryable, _ := pd.Properties["retryable"].(bool)
			if status != http.StatusConflict || code != "CONFLICT" || !retryable {
				t.Fatalf("update answered %d %s retryable=%v; want a retryable 409 CONFLICT: %s", status, code, retryable, raw)
			}
			e, err := c.GetEntity(t, eID)
			if err != nil {
				t.Fatalf("GetEntity: %v", err)
			}
			if e.Meta.State != "OPEN" {
				t.Fatalf("the primary is in %q; the conflicted update must leave it OPEN", e.Meta.State)
			}
		})
	}
}
