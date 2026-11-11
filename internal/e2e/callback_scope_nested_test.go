package e2e_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// callback_scope_nested_test.go — a mark climbs two levels. Callback A's write
// runs a SYNC processor, whose callout Q is made from A's scope. Q's callback
// fails a write, not retryably: that marks A and cancels Q. A's processor site
// then fails with 409 TRANSACTION_ROLLBACK_ONLY, A ends marked and, as a
// write, passes the mark up to the owner's root: the owner is refused 409
// TRANSACTION_ROLLBACK_ONLY naming the first failure's code, and nothing of
// the operation is committed.

// TestCallbackScope_MarkedByDescendant_ClimbsToTheOwner: the owner creates p,
// whose SYNC processor's cnode makes callback A — a create of a. a's SYNC
// processor's cnode makes callback D — a create of x, whose processor fails, not
// retryably. D is answered 400 WORKFLOW_FAILED; A is answered 409
// TRANSACTION_ROLLBACK_ONLY although its cnode answered success; the owner is
// refused 409 TRANSACTION_ROLLBACK_ONLY naming WORKFLOW_FAILED; no p, a or x
// entity is committed, and no StateMachine audit row of any of them.
func TestCallbackScope_MarkedByDescendant_ClimbsToTheOwner(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(0, 100*time.Millisecond))
	x, xnode := scopeFailingX(t, h, "cs-nest")
	sfx := randSuffix(t)
	p, a, tagP, tagA := "cs-nest-p-"+sfx, "cs-nest-a-"+sfx, "cs-nest-tp-"+sfx, "cs-nest-ta-"+sfx
	h.SetupModelWithWorkflow(t, a, chainWorkflowJSON(a+"-wf", procSpec{"cs-nest-aproc", "SYNC", map[string]any{"calculationNodesTags": tagA}}))
	h.SetupModelWithWorkflow(t, p, chainWorkflowJSON(p+"-wf", procSpec{"cs-nest-pproc", "SYNC", map[string]any{"calculationNodesTags": tagP}}))

	dRes := make(chan callbackResult, 1)
	anode := h.AttachCnode(t, cnodeSpec{name: "a", tags: []string{tagA}, script: failWriteThen(x, dRes, answerOK())})
	aRes := make(chan callbackResult, 1)
	pnode := h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{tagP}, script: func(_ context.Context, _ receivedCallout, rc *reqCtx) cnodeReply {
		res, err := rc.CreateEntity(a, 1, `{"name":"a","amount":1,"status":"a"}`)
		if err != nil {
			res = callbackResult{StatusCode: -1, Body: err.Error()}
		}
		aRes <- res
		return answerOK()
	}})

	owner := h.CreateEntityRaw(p, 1, workflowSampleModel)

	d := awaitCallback(t, "callback D", dRes)
	assertRefusal(t, d.StatusCode, d.Body, http.StatusBadRequest, "WORKFLOW_FAILED")
	ar := awaitCallback(t, "callback A", aRes)
	assertRefusal(t, ar.StatusCode, ar.Body, http.StatusConflict, "TRANSACTION_ROLLBACK_ONLY")
	pd := assertRefusal(t, owner.status, owner.body, http.StatusConflict, "TRANSACTION_ROLLBACK_ONLY")
	if !strings.Contains(pd.Detail, "WORKFLOW_FAILED") {
		t.Errorf("owner detail = %q; want it to name D's WORKFLOW_FAILED", pd.Detail)
	}
	for _, model := range []string{p, a, x} {
		if n := h.countEntities(t, model); n != 0 {
			t.Errorf("%d %s entities committed; want none", n, model)
		}
	}
	for name, cnode := range map[string]*scriptedCnode{"p": pnode, "a": anode, "x": xnode} {
		if n := smAuditRows(t, firstEntity(t, cnode)); n != 0 {
			t.Errorf("the %s entity has %d StateMachine audit rows committed; want none", name, n)
		}
	}
}
