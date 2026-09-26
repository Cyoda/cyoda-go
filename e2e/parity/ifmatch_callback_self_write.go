package parity

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// ifmatch_callback_self_write.go — an update carries If-Match, and a
// processor writes the request's own entity E through a callback that joined
// the request's transaction. If-Match states the version the request starts
// from, so the callback's write does not break it: the update answers 200 and
// keeps the callback's write, on every backend. A stale If-Match (another
// client changed E before the request began) answers 412. When the callback's
// write loses a race to a write another client committed after the request
// began, the answer is a retryable 409 on every backend.

func init() {
	Register(
		NamedTest{Name: "IfMatch_OwnEntityCallbackWrite", Fn: RunIfMatch_OwnEntityCallbackWrite},
	)
}

const ifmSelfSample = `{"k":1,"status":"one","targetId":"00000000-0000-0000-0000-000000000000"}`

// ifmSelfWorkflow moves E from OPEN to DONE through "go" when a write sets
// status to "go", running procs on the way. The callback's own write sets
// another status, so its loopback leaves E in OPEN and runs no processor.
func ifmSelfWorkflow(name string, procs []any) string {
	return cbWorkflowDoc(name, map[string]any{
		"NONE": map[string]any{"transitions": []any{map[string]any{"name": "store", "next": "OPEN", "manual": false}}},
		"OPEN": map[string]any{"transitions": []any{map[string]any{
			"name": "go", "next": "DONE", "manual": false,
			"criterion":  map[string]any{"type": "simple", "jsonPath": "$.status", "operatorType": "EQUALS", "value": "go"},
			"processors": procs,
		}}},
		"DONE": map[string]any{},
	})
}

func RunIfMatch_OwnEntityCallbackWrite(t *testing.T, fixture BackendFixture) {
	tenant := fixture.ComputeTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)
	selfWrite := func(mode string) map[string]any {
		return cbProc("cb-write-self", mode, `{"marker":"self"}`, nil)
	}

	// The callback writes E and succeeds: 200, and E carries its write.
	for _, mode := range []struct {
		name  string
		procs []any
	}{
		{name: "SYNC", procs: []any{selfWrite("SYNC")}},
		{name: "ASYNC_SAME_TX", procs: []any{selfWrite("ASYNC_SAME_TX")}},
		// The write lands before a COMMIT_BEFORE_DISPATCH segment commits.
		{name: "SYNC_ThenCommitBeforeDispatch", procs: []any{selfWrite("SYNC"), cbProc("noop", "COMMIT_BEFORE_DISPATCH", "", nil)}},
	} {
		t.Run("Kept/"+mode.name, func(t *testing.T) {
			model := "ifm-self-" + uuid.NewString()[:8]
			setupModelWithWorkflow(t, c, model, 1, ifmSelfSample, ifmSelfWorkflow(model+"-wf", mode.procs))
			eID, before := ifmSelfCreate(t, c, model)

			status, raw, err := c.UpdateEntityDataWithIfMatchRaw(t, eID, ifmSelfBody(eID, "go"), before.Meta.TransactionID)
			if err != nil {
				t.Fatalf("update: %v", err)
			}
			if status != http.StatusOK {
				t.Fatalf("update answered %d; want 200: %s", status, raw)
			}
			e, err := c.GetEntity(t, eID)
			if err != nil {
				t.Fatalf("GetEntity: %v", err)
			}
			if e.Meta.State != "DONE" || e.Data["status"] != "self" {
				t.Fatalf("E is %s with status %v; want DONE with the callback's write (status self)", e.Meta.State, e.Data["status"])
			}
		})
	}

	// Control: another client changed E before the request began.
	t.Run("StaleIfMatch", func(t *testing.T) {
		model := "ifm-self-" + uuid.NewString()[:8]
		setupModelWithWorkflow(t, c, model, 1, ifmSelfSample, ifmSelfWorkflow(model+"-wf", []any{selfWrite("SYNC")}))
		eID, before := ifmSelfCreate(t, c, model)
		if err := c.UpdateEntityData(t, eID, ifmSelfBody(eID, "two")); err != nil {
			t.Fatalf("the other client's update: %v", err)
		}
		status, raw, err := c.UpdateEntityDataWithIfMatchRaw(t, eID, ifmSelfBody(eID, "go"), before.Meta.TransactionID)
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		if code := ifmSelfErrorCode(raw); status != http.StatusPreconditionFailed || code != "ENTITY_MODIFIED" {
			t.Fatalf("update answered %d %s; want 412 ENTITY_MODIFIED: %s", status, code, raw)
		}
		e, err := c.GetEntity(t, eID)
		if err != nil {
			t.Fatalf("GetEntity: %v", err)
		}
		if e.Meta.State != "OPEN" || e.Data["status"] != "two" {
			t.Fatalf("E is %s with status %v; the rejected update must leave the other client's write (OPEN, status two)", e.Meta.State, e.Data["status"])
		}
	})

	// The callback's write of E loses to a write that another client
	// committed after the request began. cb-race-target-strict makes that
	// rival write itself, outside the transaction, and then writes E again
	// through the joined callback.
	for _, mode := range []string{"SYNC", "ASYNC_SAME_TX"} {
		t.Run("LostRace/"+mode, func(t *testing.T) {
			model := "ifm-self-" + uuid.NewString()[:8]
			setupModelWithWorkflow(t, c, model, 1, ifmSelfSample,
				ifmSelfWorkflow(model+"-wf", []any{cbProc("cb-race-target-strict", mode, "", nil)}))
			eID, before := ifmSelfCreate(t, c, model)

			status, raw, err := c.UpdateEntityDataWithIfMatchRaw(t, eID, ifmSelfBody(eID, "go"), before.Meta.TransactionID)
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
			if e.Meta.State != "OPEN" || e.Data["status"] != "one" {
				t.Fatalf("E is %s with status %v; the conflicted update must leave the rival's write (OPEN, status one)", e.Meta.State, e.Data["status"])
			}
		})
	}
}

// ifmSelfCreate creates E, points its targetId at itself, and returns E as
// read after that write: the version an If-Match names.
func ifmSelfCreate(t *testing.T, c *client.Client, model string) (uuid.UUID, client.EntityResult) {
	t.Helper()
	eID, err := c.CreateEntity(t, model, 1, ifmSelfSample)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	if err := c.UpdateEntityData(t, eID, ifmSelfBody(eID, "one")); err != nil {
		t.Fatalf("set targetId: %v", err)
	}
	e, err := c.GetEntity(t, eID)
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}
	if e.Meta.State != "OPEN" || e.Meta.TransactionID == "" {
		t.Fatalf("E is %s at transaction %q; want OPEN with a transaction id", e.Meta.State, e.Meta.TransactionID)
	}
	return eID, e
}

func ifmSelfBody(eID uuid.UUID, status string) string {
	return fmt.Sprintf(`{"k":1,"status":%q,"targetId":%q}`, status, eID)
}

func ifmSelfErrorCode(raw []byte) string {
	var pd struct {
		Properties map[string]any `json:"properties"`
	}
	_ = json.Unmarshal(raw, &pd)
	code, _ := pd.Properties["errorCode"].(string)
	return code
}
