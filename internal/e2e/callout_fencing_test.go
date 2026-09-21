package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"
)

// callout_fencing_test.go drives the fence through the client-facing doors: a
// pass whose callout has ended, or whose compute node the owner replaced, is
// refused on every door that compute node could call back through — and the
// refused request touches no store, which is asserted on what the transaction
// committed and on the audit trail, not on the response alone.

// awaitCnodeReceived waits until c has received at least n callouts.
func awaitCnodeReceived(t *testing.T, c *scriptedCnode, n int, within time.Duration) []receivedCallout {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if got := c.Received(); len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("cnode %s received %d callouts within %s; want %d (all: %v)", c.name, len(c.Received()), within, n, c.h.ReceivedCallouts())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// closeOnce returns a func that closes ch the first time it is called, so a
// test can release a held script early and still register it as a cleanup.
func closeOnce(ch chan struct{}) func() {
	var once sync.Once
	return func() { once.Do(func() { close(ch) }) }
}

// awaitCreate waits for a create driven from a goroutine.
func awaitCreate(t *testing.T, done <-chan createEntityResult, within time.Duration) createEntityResult {
	t.Helper()
	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("create: %v", res.err)
		}
		return res
	case <-time.After(within):
		t.Fatalf("the create did not complete within %s", within)
		return createEntityResult{}
	}
}

// refusedUpdateName is the name an admitted late update would leave in the
// entity it updated. No admitted door in these scenarios ever writes it, so
// finding it afterwards means a refused callback reached the store.
const refusedUpdateName = "refused-late-update"

// committedTarget imports a model with the trivial workflow and puts one
// committed entity in it, returning the entity's id and how many audit events it
// has. That entity is what the refused read and the refused update are aimed at:
// both need one that exists outside the transaction under test — a joined read of
// the entity a create is still building sees nothing, and a refused create never
// gives back an id to look up afterwards.
func committedTarget(t *testing.T, h *callbackHarness, model string) (entityID string, auditBefore int) {
	t.Helper()
	h.SetupModelWithWorkflow(t, model, secondaryWorkflow)
	id, status, body := h.CreateEntity(t, model, 1, `{"name":"target","amount":1,"status":"ok"}`)
	if status != http.StatusOK {
		t.Fatalf("create the refusal target in %s: %d %s", model, status, body)
	}
	return id, h.auditEventCount(t, id)
}

// assertUpdateRefused presents pass as a joined loopback update of the committed
// target. It is the one refused door whose store-side effect can be looked for by
// id afterwards, which is what turns "touched no store" into an assertion about
// the entity and its audit trail (assertNothingLanded) rather than about the
// response.
func assertUpdateRefused(t *testing.T, h *callbackHarness, pass, entityID string, wantStatus int, wantCode string) {
	t.Run("http-update", func(t *testing.T) {
		res, err := h.callback(http.MethodPut, "/api/entity/JSON/"+entityID,
			fmt.Sprintf(`{"name":%q,"amount":1,"status":"late"}`, refusedUpdateName), pass)
		if err != nil {
			t.Fatalf("%v", err)
		}
		assertProblem(t, res.StatusCode, res.Body, wantStatus, wantCode, false)
	})
}

// auditEventCount is how many audit events an entity has, of every type the door
// reports by default (StateMachine and EntityChange).
func (h *callbackHarness) auditEventCount(t *testing.T, entityID string) int {
	t.Helper()
	resp := h.DoAuth(t, http.MethodGet, "/api/audit/entity/"+entityID, "", "")
	body := h.readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("audit of %s: %d %s", entityID, resp.StatusCode, body)
	}
	var page struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatalf("decode the audit page: %v (body: %s)", err, body)
	}
	return len(page.Items)
}

// assertNothingLanded is the store-side half of a refusal: the target a refused
// update named carries none of that update's data and its audit trail has not
// grown, and the model a refused create named holds nothing.
func assertNothingLanded(t *testing.T, h *callbackHarness, targetID string, auditBefore int, createModel string) {
	t.Helper()
	if got, _ := h.GetEntityData(t, targetID)["name"].(string); got == refusedUpdateName {
		t.Error("the refused update's name is in the committed entity: a refused callback wrote to the store")
	}
	if got := h.auditEventCount(t, targetID); got != auditBefore {
		t.Errorf("the target's audit trail grew from %d to %d events; a refused callback wrote an audit row", auditBefore, got)
	}
	if n := h.countEntities(t, createModel); n != 0 {
		t.Errorf("%d entities committed in %s; a refused callback's create must not be in the result", n, createModel)
	}
}

// TestCalloutFence_LateCallback (shape F-ended): processor A's callout has
// ended — answered, or failed under ASYNC_NEW_TX — while processor B keeps the
// transaction open. A's pass is refused 410 on every door, write and read, and
// what it tried to write is not in the committed result. Once the transaction
// has ended the same pass is 404, as it always was.
func TestCalloutFence_LateCallback(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(3, 100*time.Millisecond))

	cases := []struct {
		name   string
		mode   string
		aReply cnodeReply
	}{
		{"answered-sync", "SYNC", answerOK()},
		{"failed-async-new-tx", "ASYNC_NEW_TX", answerFail("s6 failed")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			model, secondary := "s6-model-"+tc.name, "s6-secondary-"+tc.name
			tagA, tagB := "s6-a-"+tc.name, "s6-b-"+tc.name
			h.SetupModelWithWorkflow(t, secondary, secondaryWorkflow)
			targetID, auditBefore := committedTarget(t, h, "s6-target-"+tc.name)
			h.SetupModelWithWorkflow(t, model, chainWorkflowJSON("s6-wf-"+tc.name,
				procSpec{"s6-proc-a", tc.mode, map[string]any{"calculationNodesTags": tagA}},
				procSpec{"s6-proc-b", "SYNC", map[string]any{"calculationNodesTags": tagB}}))

			release := make(chan struct{})
			releaseNow := closeOnce(release)
			t.Cleanup(releaseNow)
			a := h.AttachCnode(t, cnodeSpec{name: "a", tags: []string{tagA}, script: scriptAlways(tc.aReply)})
			b := h.AttachCnode(t, cnodeSpec{name: "b", tags: []string{tagB}, script: scriptHold(release, answerOK())})
			defer b.Detach(t)
			defer a.Detach(t)

			done := make(chan createEntityResult, 1)
			go func() { done <- h.CreateEntityRaw(model, 1, workflowSampleModel) }()

			awaitCnodeReceived(t, b, 1, 15*time.Second) // A's callout has ended; B's is in progress
			ended := a.Received()[0]

			assertRefusedOnAllDoors(t, h, ended.Pass(), secondary, targetID,
				http.StatusGone, "CALLOUT_SUPERSEDED", supersededDetail)
			assertUpdateRefused(t, h, ended.Pass(), targetID, http.StatusGone, "CALLOUT_SUPERSEDED")

			releaseNow()
			if res := awaitCreate(t, done, 15*time.Second); res.status != http.StatusOK {
				t.Fatalf("create: %d %s; want 200 — a refused late callback does not touch the operation", res.status, res.body)
			}
			if n := h.countEntities(t, secondary); n != 0 {
				t.Errorf("%d secondary entities committed; the late callback's write must not be in the result", n)
			}

			// The transaction has ended: looked up first, so 404 as today.
			assertRefusedOnAllDoors(t, h, ended.Pass(), secondary, targetID,
				http.StatusNotFound, "TRANSACTION_NOT_FOUND", "")
			assertUpdateRefused(t, h, ended.Pass(), targetID, http.StatusNotFound, "TRANSACTION_NOT_FOUND")

			assertNothingLanded(t, h, targetID, auditBefore, secondary)
			if n := len(a.Received()); n != 1 {
				t.Errorf("cnode a received %d callouts; want 1", n)
			}
			if n := len(b.Received()); n != 1 {
				t.Errorf("cnode b received %d callouts; want 1", n)
			}
		})
	}
}
