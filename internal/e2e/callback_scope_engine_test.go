package e2e_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// callback_scope_engine_test.go — callouts and ASYNC_NEW_TX processors run in
// coordination scopes. A callback's failed write marks the scope its callout
// was made from: an ASYNC_NEW_TX processor's scope absorbs the mark (the
// processor fails, the operation goes on), every other callout site fails its
// operation with TRANSACTION_ROLLBACK_ONLY. While a callback waits in a callout
// of its own, a sibling is refused 503 TRANSACTION_BUSY. A callback still in
// its handler when its callout ends is discarded and answered 410.

// scopeFailingX imports model x, whose SYNC processor's cnode fails, not
// retryably, and attaches that cnode. A joined create of x is a failed write.
func scopeFailingX(t *testing.T, h *callbackHarness, prefix string) (model string, xnode *scriptedCnode) {
	t.Helper()
	sfx := randSuffix(t)
	model, tag := prefix+"-x-"+sfx, prefix+"-tx-"+sfx
	h.SetupModelWithWorkflow(t, model, chainWorkflowJSON(model+"-wf", procSpec{"xproc", "SYNC", map[string]any{"calculationNodesTags": tag}}))
	return model, h.AttachCnode(t, cnodeSpec{name: "x", tags: []string{tag}, script: scriptAlways(answerFailVerdict("x refused", false))})
}

// failWriteThen is a cnode script whose callback creates x — a write that
// fails — and that then gives reply.
func failWriteThen(x string, cb chan<- callbackResult, reply cnodeReply) cnodeScript {
	return func(_ context.Context, _ receivedCallout, rc *reqCtx) cnodeReply {
		res, err := rc.CreateEntity(x, 1, scopeXPayload)
		if err != nil {
			res = callbackResult{StatusCode: -1, Body: err.Error()}
		}
		cb <- res
		return reply
	}
}

// processResult returns the STATE_PROCESS_RESULT event the entity's audit
// trail holds for processor proc, nil when there is none.
func processResult(events []map[string]any, proc string) map[string]any {
	for _, ev := range events {
		if et, _ := ev["eventType"].(string); et != "STATE_PROCESS_RESULT" {
			continue
		}
		data, _ := ev["data"].(map[string]any)
		if name, _ := data["processorName"].(string); name == proc {
			return data
		}
	}
	return nil
}

// TestCallbackScope_AsyncNewTxAbsorbsFailedCallbackWrite: the owner's
// ASYNC_NEW_TX processor's callback fails a write. The processor's scope is
// marked and discarded with it: the processor counts as failed, the operation
// commits, and nothing of the callback exists.
func TestCallbackScope_AsyncNewTxAbsorbsFailedCallbackWrite(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(0, 100*time.Millisecond))
	x, xnode := scopeFailingX(t, h, "cs-ant")
	sfx := randSuffix(t)
	p, tag := "cs-ant-p-"+sfx, "cs-ant-tp-"+sfx
	h.SetupModelWithWorkflow(t, p, chainWorkflowJSON(p+"-wf", procSpec{"cs-ant-proc", "ASYNC_NEW_TX", map[string]any{"calculationNodesTags": tag}}))
	cb := make(chan callbackResult, 1)
	h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{tag}, script: failWriteThen(x, cb, answerOK())})

	id, status, body := h.CreateEntity(t, p, 1, workflowSampleModel)

	res := awaitCallback(t, "the callback", cb)
	assertRefusal(t, res.StatusCode, res.Body, http.StatusBadRequest, "WORKFLOW_FAILED")
	if status != http.StatusOK {
		t.Fatalf("owner: %d %s; want 200 — an ASYNC_NEW_TX processor absorbs its callbacks' failures", status, body)
	}
	result := processResult(h.GetSMAuditEvents(t, id), "cs-ant-proc")
	if result == nil {
		t.Fatal("no STATE_PROCESS_RESULT for the ASYNC_NEW_TX processor")
	}
	if success, ok := result["success"].(bool); !ok || success {
		t.Errorf("STATE_PROCESS_RESULT = %v; want the processor failed", result)
	}
	if n := h.countEntities(t, x); n != 0 {
		t.Errorf("%d x entities committed; want none", n)
	}
	assertNoEntityTrace(t, h, firstEntity(t, xnode))
}

// TestCallbackScope_SiblingOfPausedCallbackBusy: callback A's write runs a
// workflow whose SYNC processor is out at its compute node: A is paused in a
// callout of its own. A sibling sent meanwhile is not A's descendant, so it is
// refused at once with 503 TRANSACTION_BUSY, retryable; sent again after A's
// answer, it succeeds.
func TestCallbackScope_SiblingOfPausedCallbackBusy(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(0, 100*time.Millisecond))
	sfx := randSuffix(t)
	pModel, aModel, yModel := "cs-busy-p-"+sfx, "cs-busy-a-"+sfx, "cs-busy-y-"+sfx
	tagP, tagA := "cs-busy-tp-"+sfx, "cs-busy-ta-"+sfx
	h.SetupModelWithWorkflow(t, yModel, secondaryWorkflow)
	h.SetupModelWithWorkflow(t, aModel, chainWorkflowJSON(aModel+"-wf", procSpec{"cs-busy-aproc", "SYNC", map[string]any{"calculationNodesTags": tagA}}))
	h.SetupModelWithWorkflow(t, pModel, chainWorkflowJSON(pModel+"-wf", procSpec{"cs-busy-pproc", "SYNC", map[string]any{"calculationNodesTags": tagP}}))

	aOut, bFirst := make(chan struct{}), make(chan struct{})
	aOutNow, bFirstDone := closeOnce(aOut), closeOnce(bFirst)
	t.Cleanup(aOutNow)
	t.Cleanup(bFirstDone)
	h.AttachCnode(t, cnodeSpec{name: "a", tags: []string{tagA}, script: func(ctx context.Context, _ receivedCallout, _ *reqCtx) cnodeReply {
		aOutNow()
		select {
		case <-bFirst:
		case <-ctx.Done():
			return neverAnswer()
		}
		return answerOK()
	}})
	type outcome struct{ a, bFirst, bAfter callbackResult }
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
		var err error
		if o.bFirst, err = rc.CreateEntity(yModel, 1, `{"name":"y","amount":1,"status":"y"}`); err != nil {
			o.bFirst = callbackResult{StatusCode: -1, Body: err.Error()}
		}
		bFirstDone()
		o.a = <-aRes
		if o.bAfter, err = rc.CreateEntity(yModel, 1, `{"name":"y","amount":1,"status":"y"}`); err != nil {
			o.bAfter = callbackResult{StatusCode: -1, Body: err.Error()}
		}
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
	assertProblem(t, o.bFirst.StatusCode, o.bFirst.Body, http.StatusServiceUnavailable, "TRANSACTION_BUSY", true)
	if o.a.StatusCode != http.StatusOK {
		t.Errorf("callback A: %d %s; want 200", o.a.StatusCode, o.a.Body)
	}
	if o.bAfter.StatusCode != http.StatusOK {
		t.Errorf("the sibling sent again after A's answer: %d %s; want 200", o.bAfter.StatusCode, o.bAfter.Body)
	}
	if owner.err != nil || owner.status != http.StatusOK {
		t.Fatalf("owner: %d %s %v; want 200 — a refused sibling marks nothing", owner.status, owner.body, owner.err)
	}
	if n := h.countEntities(t, yModel); n != 1 {
		t.Errorf("%d y entities committed; want the one the retried sibling created", n)
	}
}

// TestCallbackScope_AnsweredBeforeCallbackReturned_410 (Review Focus 1): the
// compute node answers its callout while its own callback is still in the
// handler, held inside PostgreSQL by a row lock. The callout ends under the
// callback: it is answered 410 CALLOUT_SUPERSEDED, and nothing of it commits.
func TestCallbackScope_AnsweredBeforeCallbackReturned_410(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(0, 100*time.Millisecond))
	sfx := randSuffix(t)
	model, tag := "cs-early-"+sfx, "cs-early-"+sfx
	victim := h.seedVictim(t, "cs-early-victim-"+sfx)
	h.SetupModelWithWorkflow(t, model, chainWorkflowJSON(model+"-wf", procSpec{"cs-early-proc", "SYNC", map[string]any{"calculationNodesTags": tag}}))

	unlock := holdRowLock(t, victim)
	defer unlock()
	answerNow := make(chan struct{})
	answerNext := closeOnce(answerNow)
	t.Cleanup(answerNext)
	type started struct {
		res  <-chan callbackResult
		pass string
	}
	inProgress := make(chan started, 1)
	h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{tag}, script: func(ctx context.Context, call receivedCallout, _ *reqCtx) cnodeReply {
		inProgress <- started{goJoined(h, http.MethodPut, "/api/entity/JSON/"+victim, victimUpdate, call.Pass()), call.Pass()}
		select {
		case <-answerNow:
			return answerOK() // before its own callback has returned
		case <-ctx.Done():
			return neverAnswer()
		}
	}})

	done := make(chan createEntityResult, 1)
	go func() { done <- h.CreateEntityRaw(model, 1, workflowSampleModel) }()

	var st started
	select {
	case st = <-inProgress:
	case <-time.After(30 * time.Second):
		t.Fatal("the processor callout never arrived")
	}
	awaitBlockedStatement(t, "entities", stmtCeilingLockerAppName)
	answerNext()
	// The owner has taken the answer once its callout has ended: the pass is
	// refused on entry from then on, while the callback is still held.
	awaitCalloutEnded(t, h, st.pass, victim)
	unlock()
	writeRes := st.res

	res := awaitResult(t, "the callback in progress", writeRes)
	assertRefusal(t, res.StatusCode, res.Body, http.StatusGone, "CALLOUT_SUPERSEDED")
	if cr := awaitCreate(t, done, 15*time.Second); cr.status != http.StatusOK {
		t.Fatalf("owner: %d %s; want 200 — a discarded callback marks nothing", cr.status, cr.body)
	}
	if got := h.GetEntityData(t, victim)["status"]; got == "held" {
		t.Errorf("victim.status = %v; the discarded callback's write was committed", got)
	}
}

// awaitCalloutEnded polls with a joined read under pass until it is refused
// 410 on entry: the pass's callout has ended. A read admitted before that
// queues behind the callback in progress, and its short wait ends without an
// answer; it is sent again.
func awaitCalloutEnded(t *testing.T, h *callbackHarness, pass, entityID string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(e2eCtx(t), 300*time.Millisecond)
		res, err := h.joinedRequest(ctx, http.MethodGet, "/api/entity/"+entityID, "", pass)
		cancel()
		if err == nil && res.StatusCode == http.StatusGone {
			return
		}
	}
	t.Fatal("the callout never ended")
}

// ownerModeWorkflows are the callout sites other than ASYNC_NEW_TX: a callback
// of each that fails a write fails the owner's operation.
func ownerModeWorkflows(name, tag string) map[string]string {
	cbd, _ := json.Marshal(map[string]any{
		"type": "externalized", "name": "cs-om-cbd", "executionMode": "COMMIT_BEFORE_DISPATCH", "startNewTxOnDispatch": true,
		"config": map[string]any{"attachEntity": true, "calculationNodesTags": tag},
	})
	var cbdProc map[string]any
	_ = json.Unmarshal(cbd, &cbdProc)
	fn, _ := json.Marshal(map[string]any{"name": "cs-om-fn", "resultKind": "Schedule", "calculationNodesTags": tag})
	return map[string]string{
		"SyncProcessor":       chainWorkflowJSON(name+"-sync-wf", procSpec{"cs-om-sync", "SYNC", map[string]any{"calculationNodesTags": tag}}),
		"SyncProcessorFailed": chainWorkflowJSON(name+"-syncf-wf", procSpec{"cs-om-syncf", "SYNC", map[string]any{"calculationNodesTags": tag}}),
		"AsyncSameTx":         chainWorkflowJSON(name+"-ast-wf", procSpec{"cs-om-ast", "ASYNC_SAME_TX", map[string]any{"calculationNodesTags": tag}}),
		"FunctionCriterion":   criterionWorkflowJSON(name+"-crit-wf", "cs-om-crit", map[string]any{"calculationNodesTags": tag}),
		"ScheduleFunction":    scheduleFunctionWorkflowJSON(name+"-fn-wf", string(fn)),
		"CBDTxPost":           chainProcsWorkflowJSON(name+"-cbd-wf", cbdProc),
	}
}

// ownerModeReply is what the callout site's cnode answers after its callback
// failed: a success of the site's own kind.
var ownerModeReply = map[string]cnodeReply{
	"SyncProcessor": answerOK(),
	// The processor's own failure, answered after its callback's failed
	// write: the scope's refusal decides the callout, whichever came first.
	"SyncProcessorFailed": answerFailVerdict("p failed after its callback", false),
	"AsyncSameTx":         answerOK(),
	"FunctionCriterion":   answerMatches(true),
	"ScheduleFunction":    answerResult("Schedule", map[string]any{"fireAfterMs": int64(3600000)}),
	"CBDTxPost":           answerOK(),
}

// runOwnerMode runs one callout site whose callback fails a write, and asserts
// the owner is refused 409 TRANSACTION_ROLLBACK_ONLY naming the callback's
// code, with nothing of the callback committed.
//
// For CBDTxPost the operation's first segment (TX_pre) has committed before
// the dispatch, so p itself exists in the state TX_pre left it in; only the
// callback's x is asserted absent.
func runOwnerMode(t *testing.T, mode string) {
	h := newCalloutHarness(t, calloutTuning(0, 100*time.Millisecond))
	x, _ := scopeFailingX(t, h, "cs-om")
	sfx := randSuffix(t)
	p, tag := "cs-om-p-"+sfx, "cs-om-tp-"+sfx
	h.SetupModelWithWorkflow(t, p, ownerModeWorkflows(p, tag)[mode])
	cb := make(chan callbackResult, 1)
	h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{tag}, script: failWriteThen(x, cb, ownerModeReply[mode])})

	owner := h.CreateEntityRaw(p, 1, workflowSampleModel)

	res := awaitCallback(t, "the callback", cb)
	assertRefusal(t, res.StatusCode, res.Body, http.StatusBadRequest, "WORKFLOW_FAILED")
	pd := assertRefusal(t, owner.status, owner.body, http.StatusConflict, "TRANSACTION_ROLLBACK_ONLY")
	if !strings.Contains(pd.Detail, "WORKFLOW_FAILED") {
		t.Errorf("owner detail = %q; want it to name the callback's WORKFLOW_FAILED", pd.Detail)
	}
	if n := h.countEntities(t, x); n != 0 {
		t.Errorf("%d x entities committed; want none", n)
	}
}

func TestCallbackScope_OwnerModes_SyncProcessor(t *testing.T) { runOwnerMode(t, "SyncProcessor") }
func TestCallbackScope_OwnerModes_SyncProcessorFailed(t *testing.T) {
	runOwnerMode(t, "SyncProcessorFailed")
}
func TestCallbackScope_OwnerModes_AsyncSameTx(t *testing.T) { runOwnerMode(t, "AsyncSameTx") }
func TestCallbackScope_OwnerModes_FunctionCriterion(t *testing.T) {
	runOwnerMode(t, "FunctionCriterion")
}
func TestCallbackScope_OwnerModes_ScheduleFunction(t *testing.T) { runOwnerMode(t, "ScheduleFunction") }
func TestCallbackScope_OwnerModes_CBDTxPost(t *testing.T)        { runOwnerMode(t, "CBDTxPost") }

// TestCallbackScope_MarkStopsFurtherTries: a processor with FIXED retries
// whose compute members' callbacks fail a write, and which then never answer —
// so once the answer limit passed, the owner's loop would give the work to the
// other member. The failed write marks the transaction and the mark cancels
// the callout: no further try is made, and the owner is refused 409
// TRANSACTION_ROLLBACK_ONLY.
func TestCallbackScope_MarkStopsFurtherTries(t *testing.T) {
	const answerLimit = 1500 * time.Millisecond
	h := newCalloutHarness(t, calloutTuning(1, 100*time.Millisecond))
	x, _ := scopeFailingX(t, h, "cs-stop")
	sfx := randSuffix(t)
	p, tag := "cs-stop-p-"+sfx, "cs-stop-tp-"+sfx
	h.SetupModelWithWorkflow(t, p, chainWorkflowJSON(p+"-wf", procSpec{"cs-stop-proc", "SYNC",
		map[string]any{"calculationNodesTags": tag, "retryPolicy": "FIXED", "idempotent": true, "responseTimeoutMs": answerLimit.Milliseconds()}}))
	cb := make(chan callbackResult, 2)
	script := failWriteThen(x, cb, neverAnswer())
	first := h.AttachCnode(t, cnodeSpec{name: "first", tags: []string{tag}, script: script})
	second := h.AttachCnode(t, cnodeSpec{name: "second", tags: []string{tag}, script: script})

	owner := h.CreateEntityRaw(p, 1, workflowSampleModel)

	assertRefusal(t, owner.status, owner.body, http.StatusConflict, "TRANSACTION_ROLLBACK_ONLY")
	// Without the mark the other member would be tried once the answer limit
	// passed: wait past it before counting.
	time.Sleep(2 * answerLimit)
	if n := len(first.Received()) + len(second.Received()); n != 1 {
		t.Errorf("the members received %d callouts in all; want 1 — the mark stops every further try", n)
	}
}
