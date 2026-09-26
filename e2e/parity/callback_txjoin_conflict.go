package parity

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// callback_txjoin_conflict.go — a processor's joined callback loses a write
// race, and the update that ran the processor answers a retryable 409 CONFLICT
// on every backend, with or without an If-Match, in every execution mode whose
// dispatch carries a transaction. The race needs no second client:
// cb-race-target-strict writes its target outside the transaction first, then
// again inside it, and fails when the joined write is refused. Where each
// backend notices the loss differs — PostgreSQL at the joined write, memory and
// sqlite at the commit — and the answer must not. cb-race-target-thenfail makes
// the same lost write and then fails regardless, so under ASYNC_NEW_TX the
// savepoint rollback discards the write: the transaction still lost the race,
// and the update still answers 409 with nothing committed.

func init() {
	Register(
		NamedTest{Name: "CallbackTxJoin_LostWriteRaceIs409", Fn: RunCallbackTxJoin_LostWriteRaceIs409},
	)
}

const (
	cbRaceTargetSample  = `{"k":1,"flavor":"one"}`
	cbRacePrimarySample = `{"k":1,"flavor":"one","targetId":"00000000-0000-0000-0000-000000000000"}`
)

// cbRaceModes are the execution modes whose dispatch carries a transaction a
// callback can join, each with the processor entry that runs cb-race-target-strict,
// and ASYNC_NEW_TX once more with cb-race-target-thenfail.
var cbRaceModes = []struct {
	name string
	proc map[string]any
	// segmented: the mode commits the request's work before the dispatch, so
	// the conflicted update leaves the primary at the version that commit wrote.
	segmented bool
}{
	{name: "SYNC", proc: cbProc("cb-race-target-strict", "SYNC", "", nil)},
	{name: "ASYNC_NEW_TX", proc: cbProc("cb-race-target-strict", "ASYNC_NEW_TX", "", nil)},
	{name: "ASYNC_NEW_TX_ThenFails", proc: cbProc("cb-race-target-thenfail", "ASYNC_NEW_TX", "", nil)},
	{name: "COMMIT_BEFORE_DISPATCH", segmented: true,
		proc: cbProc("cb-race-target-strict", "COMMIT_BEFORE_DISPATCH", "", map[string]any{"startNewTxOnDispatch": true})},
}

func RunCallbackTxJoin_LostWriteRaceIs409(t *testing.T, fixture BackendFixture) {
	tenant := fixture.ComputeTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)
	for _, mode := range cbRaceModes {
		t.Run(mode.name, func(t *testing.T) {
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
					"processors": []any{mode.proc},
				}}},
				"DONE": map[string]any{},
			}))
			for _, withIfMatch := range []bool{false, true} {
				t.Run(fmt.Sprintf("IfMatch=%v", withIfMatch), func(t *testing.T) {
					runLostWriteRace(t, c, target, primary, withIfMatch, mode.segmented)
				})
			}
		})
	}
}

func runLostWriteRace(t *testing.T, c *client.Client, target, primary string, withIfMatch, segmented bool) {
	fID, err := c.CreateEntity(t, target, 1, cbRaceTargetSample)
	if err != nil {
		t.Fatalf("CreateEntity target: %v", err)
	}
	eID, err := c.CreateEntity(t, primary, 1, fmt.Sprintf(`{"k":1,"flavor":"one","targetId":%q}`, fID))
	if err != nil {
		t.Fatalf("CreateEntity primary: %v", err)
	}
	before, err := c.GetEntity(t, eID)
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}
	ifMatch := ""
	if withIfMatch {
		ifMatch = before.Meta.TransactionID
	}

	body := fmt.Sprintf(`{"k":1,"flavor":"go","targetId":%q}`, fID)
	status, raw, err := c.UpdateEntityDataWithIfMatchRaw(t, eID, body, ifMatch)
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
	if e.Meta.State == "DONE" {
		t.Fatal("the primary reached DONE; the conflicted transition must not complete")
	}
	if segmented {
		return
	}
	if e.Meta.State != "OPEN" || e.Meta.TransactionID != before.Meta.TransactionID || e.Data["flavor"] != "one" {
		t.Fatalf("the primary is %q at transaction %s with flavor %v; the conflicted update must leave it unchanged (OPEN at %s, flavor one)",
			e.Meta.State, e.Meta.TransactionID, e.Data["flavor"], before.Meta.TransactionID)
	}
}
