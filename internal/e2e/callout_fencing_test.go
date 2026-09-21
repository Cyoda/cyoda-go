package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
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

// TestCalloutFence_FirstCnodeRefusedOnceReplaced (shape F-replaced): the owner
// gave the work to a second cnode of its own; while that callout is still in
// progress the first cnode's pass is refused at once on every door, and the
// second's is admitted.
func TestCalloutFence_FirstCnodeRefusedOnceReplaced(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(3, 100*time.Millisecond))
	const model, refused, control, tag = "s7-replaced", "s7-replaced-refused", "s7-replaced-control", "s7-replaced"
	h.SetupModelWithWorkflow(t, refused, secondaryWorkflow)
	h.SetupModelWithWorkflow(t, control, secondaryWorkflow)
	targetID, auditBefore := committedTarget(t, h, "s7-replaced-target")
	h.SetupModelWithWorkflow(t, model, chainWorkflowJSON("s7-replaced-wf", procSpec{"s7-proc", "SYNC",
		map[string]any{"calculationNodesTags": tag, "responseTimeoutMs": 400, "idempotent": true}}))

	release := make(chan struct{})
	releaseNow := closeOnce(release)
	t.Cleanup(releaseNow)
	first := h.AttachCnode(t, cnodeSpec{name: "first", tags: []string{tag}, script: scriptAlways(neverAnswer())})
	second := h.AttachCnode(t, cnodeSpec{name: "second", tags: []string{tag}, script: scriptHold(release, answerOK())})

	done := make(chan createEntityResult, 1)
	go func() { done <- h.CreateEntityRaw(model, 1, workflowSampleModel) }()

	current := awaitCnodeReceived(t, second, 1, 15*time.Second)[0]
	replaced := first.Received()[0]

	assertRefusedOnAllDoors(t, h, replaced.Pass(), refused, targetID,
		http.StatusGone, "CALLOUT_SUPERSEDED", supersededDetail)
	assertUpdateRefused(t, h, replaced.Pass(), targetID, http.StatusGone, "CALLOUT_SUPERSEDED")

	// Control: the cnode that holds the work now is admitted, read and write.
	if res, err := h.ReplayGetHTTP(current.Pass(), targetID); err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("the current cnode's joined read: status=%d err=%v body=%s; want 200", res.StatusCode, err, res.Body)
	}
	if res, err := h.ReplayCreateHTTP(current.Pass(), control, 1, `{"name":"current-child","amount":1,"status":"ok"}`); err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("the current cnode's joined write: status=%d err=%v body=%s; want 200", res.StatusCode, err, res.Body)
	}

	releaseNow()
	if res := awaitCreate(t, done, 15*time.Second); res.status != http.StatusOK {
		t.Fatalf("create: %d %s; want 200", res.status, res.body)
	}
	if n := h.countEntities(t, control); n != 1 {
		t.Errorf("%d entities committed in %s; want exactly the current cnode's one", n, control)
	}
	assertNothingLanded(t, h, targetID, auditBefore, refused)
	if n := len(first.Received()); n != 1 {
		t.Errorf("the first cnode received %d callouts; want 1", n)
	}
	if n := len(second.Received()); n != 1 {
		t.Errorf("the second cnode received %d callouts; want 1", n)
	}
}

// TestCalloutFence_WriteUnderTheEarlierPassIsKept (shape F-kept) is the other
// half of "replaced": a callback the first cnode had already made is neither
// interrupted nor undone. It is answered 200, what it wrote is in the committed
// result, and it is the pass alone that is shut out, from the moment the work
// moved. The first cnode drops its stream once its callback has returned, so the
// hand-over to the second cnode is driven by something the test causes and never
// by a race between the callback and the answer limit.
func TestCalloutFence_WriteUnderTheEarlierPassIsKept(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(3, 100*time.Millisecond))
	const model, written, refused, tag = "s7-kept", "s7-kept-written", "s7-kept-refused", "s7-kept"
	h.SetupModelWithWorkflow(t, written, secondaryWorkflow)
	h.SetupModelWithWorkflow(t, refused, secondaryWorkflow)
	targetID, auditBefore := committedTarget(t, h, "s7-kept-target")
	h.SetupModelWithWorkflow(t, model, chainWorkflowJSON("s7-kept-wf", procSpec{"s7-kept-proc", "SYNC",
		map[string]any{"calculationNodesTags": tag, "responseTimeoutMs": 2000, "idempotent": true}}))

	release := make(chan struct{})
	releaseNow := closeOnce(release)
	t.Cleanup(releaseNow)
	firstWrote := make(chan callbackResult, 1)
	first := h.AttachCnode(t, cnodeSpec{name: "first", tags: []string{tag},
		script: func(_ context.Context, _ receivedCallout, rc *reqCtx) cnodeReply {
			res, err := rc.CreateEntity(written, 1, `{"name":"under-the-earlier-pass","amount":1,"status":"kept"}`)
			if err != nil {
				res = callbackResult{StatusCode: -1, Body: err.Error()}
			}
			firstWrote <- res
			return closeStream()
		}})
	second := h.AttachCnode(t, cnodeSpec{name: "second", tags: []string{tag}, script: scriptHold(release, answerOK())})

	done := make(chan createEntityResult, 1)
	go func() { done <- h.CreateEntityRaw(model, 1, workflowSampleModel) }()

	var wrote callbackResult
	select {
	case wrote = <-firstWrote:
	case <-time.After(15 * time.Second):
		t.Fatal("the first cnode's callback never finished")
	}
	if wrote.StatusCode != http.StatusOK {
		t.Fatalf("the first cnode's joined write: %d %s; want 200 — a callback made under the pass that is current is not refused",
			wrote.StatusCode, wrote.Body)
	}

	// The work has moved: from here the first cnode's pass is refused, while what
	// it wrote before the move stands.
	awaitCnodeReceived(t, second, 1, 15*time.Second)
	replaced := first.Received()[0]
	assertRefusedOnAllDoors(t, h, replaced.Pass(), refused, targetID,
		http.StatusGone, "CALLOUT_SUPERSEDED", supersededDetail)
	assertUpdateRefused(t, h, replaced.Pass(), targetID, http.StatusGone, "CALLOUT_SUPERSEDED")

	releaseNow()
	if res := awaitCreate(t, done, 15*time.Second); res.status != http.StatusOK {
		t.Fatalf("create: %d %s; want 200", res.status, res.body)
	}
	if n := h.countEntities(t, written); n != 1 {
		t.Errorf("%d entities committed in %s; want the one the replaced cnode wrote before the work moved", n, written)
	}
	assertNothingLanded(t, h, targetID, auditBefore, refused)
	if n := len(first.Received()); n != 1 {
		t.Errorf("the first cnode received %d callouts; want 1", n)
	}
	if n := len(second.Received()); n != 1 {
		t.Errorf("the second cnode received %d callouts; want 1", n)
	}
}

// TestCalloutFence_NestedCalloutReleased (shape F-nested): out1's callback is
// waiting on a callout of its own when out1 is replaced. The inner callout
// ends, the callback is answered 410 — not 200 — in every processor mode of the
// inner processor, ASYNC_NEW_TX as the last processor included; the inner
// cnode's pass, which names the replaced callout as enclosing, is refused; and
// nothing of the chain is committed.
func TestCalloutFence_NestedCalloutReleased(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(3, 100*time.Millisecond))

	for _, innerMode := range []string{"SYNC", "ASYNC_SAME_TX", "ASYNC_NEW_TX"} {
		t.Run(innerMode, func(t *testing.T) {
			sfx := strings.ToLower(strings.ReplaceAll(innerMode, "_", "-"))
			outer, inner, third := "s7-outer-"+sfx, "s7-inner-"+sfx, "s7-third-"+sfx
			tagOut, tagIn := "s7-out-"+sfx, "s7-in-"+sfx
			h.SetupModelWithWorkflow(t, third, secondaryWorkflow)
			targetID, auditBefore := committedTarget(t, h, "s7-target-"+sfx)
			h.SetupModelWithWorkflow(t, inner, chainWorkflowJSON("s7-inner-wf-"+sfx,
				procSpec{"s7-in", innerMode, map[string]any{"calculationNodesTags": tagIn}}))
			h.SetupModelWithWorkflow(t, outer, chainWorkflowJSON("s7-outer-wf-"+sfx,
				procSpec{"s7-out", "SYNC", map[string]any{"calculationNodesTags": tagOut, "responseTimeoutMs": 700, "idempotent": true}}))

			holdIn, holdOut2 := make(chan struct{}), make(chan struct{})
			releaseIn, releaseOut2 := closeOnce(holdIn), closeOnce(holdOut2)
			t.Cleanup(releaseIn)
			t.Cleanup(releaseOut2)

			out1Res := make(chan callbackResult, 1)
			out1 := h.AttachCnode(t, cnodeSpec{name: "out1", tags: []string{tagOut},
				script: func(_ context.Context, _ receivedCallout, rc *reqCtx) cnodeReply {
					res, err := rc.CreateEntity(inner, 1, workflowSampleModel)
					if err != nil {
						res = callbackResult{StatusCode: -1, Body: err.Error()}
					}
					out1Res <- res
					return neverAnswer()
				}})
			in1 := h.AttachCnode(t, cnodeSpec{name: "in1", tags: []string{tagIn}, script: scriptHold(holdIn, answerOK())})
			out2 := h.AttachCnode(t, cnodeSpec{name: "out2", tags: []string{tagOut}, script: scriptHold(holdOut2, answerOK())})
			defer out2.Detach(t)
			defer in1.Detach(t)
			defer out1.Detach(t)

			done := make(chan createEntityResult, 1)
			go func() { done <- h.CreateEntityRaw(outer, 1, workflowSampleModel) }()

			innerCall := awaitCnodeReceived(t, in1, 1, 15*time.Second)[0] // out1's callback is now waiting on IN
			awaitCnodeReceived(t, out2, 1, 15*time.Second)                // OUT was given to out2: the wait is over

			select {
			case res := <-out1Res:
				// A subtest, so an answer of the wrong shape does not stop the
				// scenario: what the transaction then committed is the other half
				// of the claim.
				t.Run("released-callback", func(t *testing.T) {
					pd := assertProblem(t, res.StatusCode, res.Body, http.StatusGone, "CALLOUT_SUPERSEDED", false)
					if !strings.Contains(pd.Detail, supersededDetail) {
						t.Errorf("detail = %q; want %q", pd.Detail, supersededDetail)
					}
				})
			case <-time.After(10 * time.Second):
				t.Fatal("out1's callback was not released when out1 was replaced")
			}

			// in1 still holds the inner work; its pass names the replaced callout
			// as enclosing, and its own callout has been ended.
			assertRefusedOnAllDoors(t, h, innerCall.Pass(), third, targetID,
				http.StatusGone, "CALLOUT_SUPERSEDED", supersededDetail)
			assertUpdateRefused(t, h, innerCall.Pass(), targetID, http.StatusGone, "CALLOUT_SUPERSEDED")

			releaseIn()
			releaseOut2()
			if res := awaitCreate(t, done, 15*time.Second); res.status != http.StatusOK {
				t.Fatalf("outer create: %d %s; want 200", res.status, res.body)
			}
			if n := h.countEntities(t, inner); n != 0 {
				t.Errorf("%d inner entities committed; the superseded chain must write nothing further", n)
			}
			assertNothingLanded(t, h, targetID, auditBefore, third)
			if n := len(out1.Received()); n != 1 {
				t.Errorf("out1 received %d callouts; want 1", n)
			}
			if n := len(in1.Received()); n != 1 {
				t.Errorf("in1 received %d callouts; want 1", n)
			}
			if n := len(out2.Received()); n != 1 {
				t.Errorf("out2 received %d callouts; want 1", n)
			}
		})
	}
}
