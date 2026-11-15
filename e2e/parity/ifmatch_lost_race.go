package parity

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// ifmatch_lost_race.go — an update carries an If-Match that holds when the
// request starts, and another client commits a change to the entity E while
// the request's processor is called out. That is a lost race, not a failed
// precondition: the update answers the retryable 409 CONFLICT and E keeps the
// other client's write, on every backend.
//
// The processor (cb-rival-write) makes that write itself, without the
// transaction token, only to time it during the callout: it is another
// client's write, never one through the token. The request sends E's stored
// data unchanged. Before a callout that carries the token the engine saves E
// into the transaction when E differs from what the transaction holds, and on
// PostgreSQL that save keeps E's row locked until the transaction ends: a
// rival write made during the callout would wait for it. With E unchanged the
// engine saves nothing and the rival commits at once. So that an unchanged
// body still runs the processor, E is stored with status loop under a
// workflow without the transition (ifmRaceRestWorkflow); the workflow whose
// automated transition runs on status loop is imported after.

func init() {
	Register(
		NamedTest{Name: "IfMatch_ChangeAfterReadIs409", Fn: RunIfMatch_ChangeAfterReadIs409},
	)
}

// ifmRaceStored is E's data as stored and as the update sends it back:
// compact, keys in the order every backend returns them.
const ifmRaceStored = `{"k":1,"status":"loop"}`

// ifmRaceRestWorkflow rests a created entity in OPEN and has no other
// transition.
func ifmRaceRestWorkflow(name string) string {
	return cbWorkflowDoc(name, map[string]any{
		"NONE": map[string]any{"transitions": []any{map[string]any{"name": "store", "next": "OPEN", "manual": false}}},
		"OPEN": map[string]any{},
	})
}

// ifmRaceWorkflow moves E from OPEN to DONE through the automated transition
// "loop" when its status is loop, running procs on the way. The rival's write
// sets another status, so it leaves E in OPEN and runs no processor.
func ifmRaceWorkflow(name string, procs []any) string {
	return cbWorkflowDoc(name, map[string]any{
		"NONE": map[string]any{"transitions": []any{map[string]any{"name": "store", "next": "OPEN", "manual": false}}},
		"OPEN": map[string]any{"transitions": []any{map[string]any{
			"name": "loop", "next": "DONE", "manual": false,
			"criterion":  map[string]any{"type": "simple", "jsonPath": "$.status", "operatorType": "EQUALS", "value": "loop"},
			"processors": procs,
		}}},
		"DONE": map[string]any{},
	})
}

func RunIfMatch_ChangeAfterReadIs409(t *testing.T, fixture BackendFixture) {
	tenant := fixture.ComputeTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)
	rival := cbProc("cb-rival-write", "SYNC", `{"marker":"rival"}`, nil)

	for _, shape := range []struct {
		name  string
		procs []any
	}{
		// The rival commits before the request's final save.
		{name: "SYNC", procs: []any{rival}},
		// The rival commits before TX_pre's flush and commit.
		{name: "SYNC_ThenCommitBeforeDispatch", procs: []any{rival, cbProc("noop", "COMMIT_BEFORE_DISPATCH", "", nil)}},
	} {
		t.Run(shape.name, func(t *testing.T) {
			model := "ifm-race-" + uuid.NewString()[:8]
			setupModelWithWorkflow(t, c, model, 1, ifmRaceStored, ifmRaceRestWorkflow(model+"-rest-wf"))
			eID, err := c.CreateEntity(t, model, 1, ifmRaceStored)
			if err != nil {
				t.Fatalf("CreateEntity: %v", err)
			}
			before, err := c.GetEntity(t, eID)
			if err != nil {
				t.Fatalf("GetEntity: %v", err)
			}
			if before.Meta.State != "OPEN" || before.Meta.TransactionID == "" {
				t.Fatalf("E is %s at transaction %q; want OPEN with a transaction id", before.Meta.State, before.Meta.TransactionID)
			}
			if err := c.ImportWorkflow(t, model, 1, ifmRaceWorkflow(model+"-wf", shape.procs)); err != nil {
				t.Fatalf("import the racing workflow: %v", err)
			}

			status, raw, err := c.UpdateEntityDataWithIfMatchRaw(t, eID, ifmRaceStored, before.Meta.TransactionID)
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
			if e.Meta.State != "OPEN" || e.Data["status"] != "rival" {
				t.Fatalf("E is %s with status %v; the conflicted update must leave the rival's write (OPEN, status rival)", e.Meta.State, e.Data["status"])
			}
		})
	}
}
