package e2e_test

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// callback_scope_nested_test.go — a failed write two levels down stays where
// it failed. Callback A's write runs a SYNC processor, whose callout Q is made
// from A's scope. Q's callback D fails a write, not retryably: D's write is
// discarded, and nothing else is. Q's member answers success, so A's write
// succeeds; the owner's member answers success, so the owner commits p and a
// in one transaction, without x.

// TestCallbackScope_FailedWriteOfDescendant_StaysWithIt: the owner creates p,
// whose SYNC processor's cnode makes callback A — a create of a. a's SYNC
// processor's cnode makes callback D — a create of x, whose processor fails,
// not retryably. D is answered 400 WORKFLOW_FAILED; A is answered 200, since
// its cnode answered success; the owner commits p and a in its transaction,
// and nothing of x exists.
func TestCallbackScope_FailedWriteOfDescendant_StaysWithIt(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(0, 100*time.Millisecond))
	x, xnode := scopeFailingX(t, h, "cs-nest")
	sfx := randSuffix(t)
	p, a, tagP, tagA := "cs-nest-p-"+sfx, "cs-nest-a-"+sfx, "cs-nest-tp-"+sfx, "cs-nest-ta-"+sfx
	h.SetupModelWithWorkflow(t, a, chainWorkflowJSON(a+"-wf", procSpec{"cs-nest-aproc", "SYNC", map[string]any{"calculationNodesTags": tagA}}))
	h.SetupModelWithWorkflow(t, p, chainWorkflowJSON(p+"-wf", procSpec{"cs-nest-pproc", "SYNC", map[string]any{"calculationNodesTags": tagP}}))

	dRes := make(chan callbackResult, 1)
	h.AttachCnode(t, cnodeSpec{name: "a", tags: []string{tagA}, script: failWriteThen(x, dRes, answerOK())})
	aRes := make(chan callbackResult, 1)
	h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{tagP}, script: func(_ context.Context, _ receivedCallout, rc *reqCtx) cnodeReply {
		aRes <- callbackOutcome(rc.CreateEntity(a, 1, `{"name":"a","amount":1,"status":"a"}`))
		return answerOK()
	}})

	owner := h.CreateEntityRaw(p, 1, workflowSampleModel)

	d := awaitCallback(t, "callback D", dRes)
	assertRefusal(t, d.StatusCode, d.Body, http.StatusBadRequest, "WORKFLOW_FAILED")
	if ar := awaitCallback(t, "callback A", aRes); ar.StatusCode != http.StatusOK {
		t.Errorf("callback A: %d %s; want 200 — its processor's member answered success", ar.StatusCode, ar.Body)
	}
	if owner.err != nil || owner.status != http.StatusOK {
		t.Fatalf("owner: %d %s %v; want 200 — its processor's member answered success", owner.status, owner.body, owner.err)
	}
	assertCommittedWithOwner(t, h, p, a)
	if n := h.countEntities(t, x); n != 0 {
		t.Errorf("%d x entities committed; want none", n)
	}
	assertNoEntityTrace(t, h, firstEntity(t, xnode))
}
