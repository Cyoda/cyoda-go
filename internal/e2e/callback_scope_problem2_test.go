package e2e_test

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// callback_scope_problem2_test.go — a sibling's acknowledged write survives the
// rollback of an ASYNC_NEW_TX processor it did not run under.
//
// Callback A of the transaction creates an entity whose ASYNC_NEW_TX processor
// is out at its compute node. While it is out, sibling callback B (sent by the
// same compute node) creates y. Then A's processor fails, and its writes are
// undone. Before callback scopes, B ran inside the window of A's processor and
// was answered 200 — and the undo of A's processor took y with it. Under the
// stack rule B, which is not a descendant of the paused scope, is refused 503
// TRANSACTION_BUSY while A is paused and admitted after A's answer, so every
// callback answered 2xx has its entity committed.
//
// This file is self-contained on purpose: it uses only the callout harness, so
// it can be run against the commit before the coordination existed to show it
// RED there (B answered 200 and y missing).

func TestCallbackScope_SiblingAckedWriteSurvivesAsyncNewTxRollback(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(0, 100*time.Millisecond))
	sfx := randSuffix(t) // repeated runs (go test -count=N) share this package's Postgres testcontainer
	pModel, aModel, yModel := "cs-p2-p-"+sfx, "cs-p2-a-"+sfx, "cs-p2-y-"+sfx
	tagP, tagA := "cs-p2-tp-"+sfx, "cs-p2-ta-"+sfx
	h.SetupModelWithWorkflow(t, yModel, secondaryWorkflow)
	h.SetupModelWithWorkflow(t, aModel, chainWorkflowJSON(aModel+"-wf", procSpec{"cs-p2-aproc", "ASYNC_NEW_TX", map[string]any{"calculationNodesTags": tagA}}))
	h.SetupModelWithWorkflow(t, pModel, chainWorkflowJSON(pModel+"-wf", procSpec{"cs-p2-pproc", "SYNC", map[string]any{"calculationNodesTags": tagP}}))

	aOut := make(chan struct{})
	aOutNow := closeOnce(aOut)
	bFirst := make(chan struct{})
	bFirstDone := closeOnce(bFirst)
	t.Cleanup(aOutNow)
	t.Cleanup(bFirstDone)
	// A's ASYNC_NEW_TX processor: out until B has made its first attempt, then
	// it fails, and its scope is undone.
	h.AttachCnode(t, cnodeSpec{name: "a", tags: []string{tagA}, script: func(ctx context.Context, _ receivedCallout, _ *reqCtx) cnodeReply {
		aOutNow()
		select {
		case <-bFirst:
		case <-ctx.Done():
			return neverAnswer()
		}
		return answerFailVerdict("a's processor fails", false)
	}})

	type outcome struct {
		a, bFirst, bLast callbackResult
		bAttempts        int
	}
	got := make(chan outcome, 1)
	h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{tagP}, script: func(ctx context.Context, _ receivedCallout, rc *reqCtx) cnodeReply {
		var o outcome
		aRes := make(chan callbackResult, 1)
		go func() {
			res, err := rc.CreateEntity(aModel, 1, `{"name":"a","amount":1,"status":"a"}`)
			if err != nil {
				res = callbackResult{StatusCode: -1, Body: err.Error()}
			}
			aRes <- res
		}()
		select {
		case <-aOut:
		case <-ctx.Done():
			return neverAnswer()
		}
		deadline := time.Now().Add(20 * time.Second)
		for {
			res, err := rc.CreateEntity(yModel, 1, `{"name":"y","amount":1,"status":"y"}`)
			if err != nil {
				res = callbackResult{StatusCode: -1, Body: err.Error()}
			}
			o.bAttempts++
			if o.bAttempts == 1 {
				o.bFirst = res
				bFirstDone()
			}
			o.bLast = res
			if res.StatusCode != http.StatusServiceUnavailable || time.Now().After(deadline) {
				break
			}
			time.Sleep(50 * time.Millisecond) // a retryable refusal: send it again
		}
		o.a = <-aRes
		got <- o
		return answerOK()
	}})

	owner := h.CreateEntityRaw(pModel, 1, workflowSampleModel)

	var o outcome
	select {
	case o = <-got:
	case <-time.After(60 * time.Second):
		t.Fatal("the callbacks never returned")
	}
	if owner.err != nil || owner.status != http.StatusOK {
		t.Fatalf("owner: %d %s %v; want 200", owner.status, owner.body, owner.err)
	}
	if o.a.StatusCode != http.StatusOK {
		t.Fatalf("callback A: %d %s; want 200 — an ASYNC_NEW_TX failure does not fail A's write", o.a.StatusCode, o.a.Body)
	}
	if o.bFirst.StatusCode != http.StatusOK {
		assertProblem(t, o.bFirst.StatusCode, o.bFirst.Body, http.StatusServiceUnavailable, "TRANSACTION_BUSY", true)
	}
	if o.bLast.StatusCode != http.StatusOK {
		t.Fatalf("callback B after %d attempts: %d %s; want 200", o.bAttempts, o.bLast.StatusCode, o.bLast.Body)
	}
	// Every callback answered 2xx has its entity committed.
	if n := h.countEntities(t, aModel); n != 1 {
		t.Errorf("%d a entities committed; want callback A's one", n)
	}
	if n := h.countEntities(t, yModel); n != 1 {
		t.Errorf("%d y entities committed; callback B was answered 200, so its y must be committed (attempts: %d, first answer %d)",
			n, o.bAttempts, o.bFirst.StatusCode)
	}
}
