package e2e_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/app"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

// callback_scope_test.go — every joined request runs in a scope of its own,
// and the scope is all-or-nothing. A non-retryable failure of a joined write
// discards what it wrote and makes the transaction rollback-only: the owner's
// operation is refused 409 TRANSACTION_ROLLBACK_ONLY, and every later joined
// request of the transaction is refused the same way. A retryable failure
// discards the scope and leaves the transaction usable; a read that answered
// keeps its scope.
//
// The scenarios share one shape. Model p has a SYNC processor whose compute
// node (cnode p) makes the callbacks under test. Model x has a SYNC processor
// xproc on a tag of its own, whose cnode answers as the scenario directs: a
// callback that creates an x entity is a joined write whose own workflow can
// fail.

const scopeXPayload = `{"name":"x","amount":1,"status":"x"}`

// scopeModels imports p (one SYNC processor on tagP, in mode) and x (one SYNC
// processor xproc on tagX).
type scopeModels struct{ p, x, tagP, tagX string }

func newScopeModels(t *testing.T, h *callbackHarness, prefix, mode string) scopeModels {
	t.Helper()
	sfx := randSuffix(t) // repeated runs (go test -count=N) share this package's Postgres testcontainer
	m := scopeModels{p: prefix + "-p-" + sfx, x: prefix + "-x-" + sfx, tagP: prefix + "-tp-" + sfx, tagX: prefix + "-tx-" + sfx}
	h.SetupModelWithWorkflow(t, m.x, chainWorkflowJSON(m.x+"-wf", procSpec{"xproc", "SYNC", map[string]any{"calculationNodesTags": m.tagX}}))
	h.SetupModelWithWorkflow(t, m.p, chainWorkflowJSON(m.p+"-wf", procSpec{"pproc", mode, map[string]any{"calculationNodesTags": m.tagP}}))
	return m
}

// assertNoEntityTrace asserts that an entity whose create was discarded left
// nothing: it cannot be read, and the audit door does not know it.
func assertNoEntityTrace(t *testing.T, h *callbackHarness, entityID string) {
	t.Helper()
	if entityID == "" {
		t.Fatal("the discarded create named no entity id to look for")
	}
	if _, code := h.GetEntityState(t, entityID); code != http.StatusNotFound {
		t.Errorf("entity %s: GET answered %d; want 404 — a discarded write is not committed", entityID, code)
	}
	resp := h.DoAuth(t, http.MethodGet, "/api/audit/entity/"+entityID, "", "")
	if body := h.readBody(t, resp); resp.StatusCode != http.StatusNotFound {
		t.Errorf("entity %s: audit answered %d %s; want 404 — a discarded write leaves no audit event", entityID, resp.StatusCode, body)
	}
}

// assertRollbackOnlyEnvelope checks a gRPC refusal with TRANSACTION_ROLLBACK_ONLY:
// CLIENT_ERROR, the code as the message prefix, and no retryable flag at all.
func assertRollbackOnlyEnvelope(t *testing.T, door string, env txEnvelope, err error) {
	t.Helper()
	assertEnvelope(t, door, env, err, "TRANSACTION_ROLLBACK_ONLY", false)
	if env.Error != nil && env.Error.Retryable != nil {
		t.Errorf("%s: retryable = %v; want it absent", door, *env.Error.Retryable)
	}
}

func awaitCallback(t *testing.T, what string, ch <-chan callbackResult) callbackResult {
	t.Helper()
	select {
	case res := <-ch:
		return res
	case <-time.After(30 * time.Second):
		t.Fatalf("%s never returned", what)
		return callbackResult{}
	}
}

// TestCallbackScope_FailedJoinedWrite_OwnerRollbackOnly_HTTP: the callback's
// create of x runs xproc, which fails, not retryably: the callback is answered
// its own 400 WORKFLOW_FAILED, nothing of x is kept, and the owner — whose
// cnode then answers success — is refused 409 TRANSACTION_ROLLBACK_ONLY naming
// the callback's code. Nothing of p is committed.
func TestCallbackScope_FailedJoinedWrite_OwnerRollbackOnly_HTTP(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(0, 100*time.Millisecond))
	m := newScopeModels(t, h, "cs-fw", "SYNC")
	xnode := h.AttachCnode(t, cnodeSpec{name: "x", tags: []string{m.tagX}, script: scriptAlways(answerFailVerdict("x refused", false))})
	cb := make(chan callbackResult, 1)
	h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{m.tagP}, script: func(_ context.Context, _ receivedCallout, rc *reqCtx) cnodeReply {
		res, err := rc.CreateEntity(m.x, 1, scopeXPayload)
		if err != nil {
			res = callbackResult{StatusCode: -1, Body: err.Error()}
		}
		cb <- res
		return answerOK()
	}})

	owner := h.CreateEntityRaw(m.p, 1, workflowSampleModel)

	res := awaitCallback(t, "the callback", cb)
	assertProblem(t, res.StatusCode, res.Body, http.StatusBadRequest, "WORKFLOW_FAILED", false)
	pd := assertProblem(t, owner.status, owner.body, http.StatusConflict, "TRANSACTION_ROLLBACK_ONLY", false)
	if !strings.Contains(pd.Detail, "WORKFLOW_FAILED") {
		t.Errorf("owner detail = %q; want it to name the callback's WORKFLOW_FAILED", pd.Detail)
	}
	if n := h.countEntities(t, m.x); n != 0 {
		t.Errorf("%d x entities committed; want none", n)
	}
	assertNoEntityTrace(t, h, xnode.Received()[0].EntityID)
	if n := h.countEntities(t, m.p); n != 0 {
		t.Errorf("%d p entities committed; want none", n)
	}
}

// TestCallbackScope_FailedJoinedWrite_OwnerRollbackOnly_GRPC is the same
// through gRPC on both sides: the owner's create and the callback.
func TestCallbackScope_FailedJoinedWrite_OwnerRollbackOnly_GRPC(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(0, 100*time.Millisecond))
	m := newScopeModels(t, h, "cs-fwg", "SYNC")
	xnode := h.AttachCnode(t, cnodeSpec{name: "x", tags: []string{m.tagX}, script: scriptAlways(answerFailVerdict("x refused", false))})
	type envResult struct {
		env txEnvelope
		err error
	}
	cb := make(chan envResult, 1)
	h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{m.tagP}, script: func(_ context.Context, call receivedCallout, _ *reqCtx) cnodeReply {
		env, err := h.ReplayCreateGRPC(call.Pass(), m.x, 1, scopeXPayload)
		cb <- envResult{env, err}
		return answerOK()
	}})

	owner, ownerErr := h.createEntityGRPC(m.p, 1, workflowSampleModel)

	var res envResult
	select {
	case res = <-cb:
	case <-time.After(30 * time.Second):
		t.Fatal("the callback never returned")
	}
	assertEnvelope(t, "callback", res.env, res.err, "WORKFLOW_FAILED", false)
	assertRollbackOnlyEnvelope(t, "owner", owner, ownerErr)
	if n := h.countEntities(t, m.x); n != 0 {
		t.Errorf("%d x entities committed; want none", n)
	}
	assertNoEntityTrace(t, h, xnode.Received()[0].EntityID)
	if n := h.countEntities(t, m.p); n != 0 {
		t.Errorf("%d p entities committed; want none", n)
	}
}

// TestCallbackScope_RetryableFailure_RetriedInTransaction: xproc first fails
// retryably — the callback is answered 400 retryable and its write discarded,
// the transaction stays usable — and the cnode sends the same callback again,
// which succeeds. The owner commits exactly one x, and the audit trail holds
// the second run only.
func TestCallbackScope_RetryableFailure_RetriedInTransaction(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(0, 100*time.Millisecond))
	m := newScopeModels(t, h, "cs-rt", "SYNC")
	xnode := h.AttachCnode(t, cnodeSpec{name: "x", tags: []string{m.tagX},
		script: scriptSequence(answerFailVerdict("x busy", true), answerOK())})
	cb := make(chan callbackResult, 2)
	h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{m.tagP}, script: func(_ context.Context, _ receivedCallout, rc *reqCtx) cnodeReply {
		for range 2 {
			res, err := rc.CreateEntity(m.x, 1, scopeXPayload)
			if err != nil {
				res = callbackResult{StatusCode: -1, Body: err.Error()}
			}
			cb <- res
			if res.StatusCode == http.StatusOK {
				break
			}
		}
		return answerOK()
	}})

	owner := h.CreateEntityRaw(m.p, 1, workflowSampleModel)

	first := awaitCallback(t, "the first callback", cb)
	assertProblem(t, first.StatusCode, first.Body, http.StatusBadRequest, "WORKFLOW_FAILED", true)
	second := awaitCallback(t, "the retried callback", cb)
	if second.StatusCode != http.StatusOK {
		t.Fatalf("the retried callback: %d %s; want 200 — a retryable failure leaves the transaction usable", second.StatusCode, second.Body)
	}
	if owner.err != nil || owner.status != http.StatusOK {
		t.Fatalf("owner: %d %s %v; want 200", owner.status, owner.body, owner.err)
	}
	if n := h.countEntities(t, m.x); n != 1 {
		t.Errorf("%d x entities committed; want exactly the retried one", n)
	}
	runs := xnode.Received()
	if len(runs) != 2 || runs[0].EntityID == runs[1].EntityID {
		t.Fatalf("xproc ran %d times (%v); want two runs on two entities", len(runs), runs)
	}
	assertNoEntityTrace(t, h, runs[0].EntityID)
	if n := len(h.GetAllAuditEvents(t, runs[1].EntityID)); n == 0 {
		t.Error("the retried x has no audit events")
	}
}

// TestCallbackScope_FailedRead404_TransactionGoesOn: a joined read of an
// unknown entity is answered 404 and keeps its scope; the owner commits.
func TestCallbackScope_FailedRead404_TransactionGoesOn(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(0, 100*time.Millisecond))
	m := newScopeModels(t, h, "cs-rd", "SYNC")
	cb := make(chan callbackResult, 1)
	h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{m.tagP}, script: func(_ context.Context, _ receivedCallout, rc *reqCtx) cnodeReply {
		res, err := rc.GetEntity(uuid.NewString())
		if err != nil {
			res = callbackResult{StatusCode: -1, Body: err.Error()}
		}
		cb <- res
		return answerOK()
	}})

	owner := h.CreateEntityRaw(m.p, 1, workflowSampleModel)

	res := awaitCallback(t, "the read", cb)
	assertProblem(t, res.StatusCode, res.Body, http.StatusNotFound, "ENTITY_NOT_FOUND", false)
	if owner.err != nil || owner.status != http.StatusOK {
		t.Fatalf("owner: %d %s %v; want 200 — a read's 404 does not touch the transaction", owner.status, owner.body, owner.err)
	}
	if n := h.countEntities(t, m.p); n != 1 {
		t.Errorf("%d p entities committed; want 1", n)
	}
}

// TestCallbackScope_LaterRequestOnRollbackOnly_409: after a failed joined
// write, every later joined request of the transaction — a read as much as a
// write, on either door — is refused at once with 409 TRANSACTION_ROLLBACK_ONLY,
// not retryable.
func TestCallbackScope_LaterRequestOnRollbackOnly_409(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(0, 100*time.Millisecond))
	m := newScopeModels(t, h, "cs-later", "SYNC")
	targetID, _ := committedTarget(t, h, "cs-later-target-"+randSuffix(t))
	h.AttachCnode(t, cnodeSpec{name: "x", tags: []string{m.tagX}, script: scriptAlways(answerFailVerdict("x refused", false))})

	type later struct {
		httpGet, httpPost callbackResult
		grpcGet, grpcPost txEnvelope
		errs              []error
	}
	got := make(chan later, 1)
	h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{m.tagP}, script: func(_ context.Context, call receivedCallout, rc *reqCtx) cnodeReply {
		var l later
		if res, err := rc.CreateEntity(m.x, 1, scopeXPayload); err != nil || res.StatusCode != http.StatusBadRequest {
			l.errs = append(l.errs, fmt.Errorf("the failing write: %d %s %v", res.StatusCode, res.Body, err))
		}
		var err error
		if l.httpGet, err = rc.GetEntity(targetID); err != nil {
			l.errs = append(l.errs, err)
		}
		if l.httpPost, err = rc.CreateEntity(m.x, 1, scopeXPayload); err != nil {
			l.errs = append(l.errs, err)
		}
		if l.grpcGet, err = h.ReplayGetGRPC(call.Pass(), targetID); err != nil {
			l.errs = append(l.errs, err)
		}
		if l.grpcPost, err = h.ReplayCreateGRPC(call.Pass(), m.x, 1, scopeXPayload); err != nil {
			l.errs = append(l.errs, err)
		}
		got <- l
		return answerOK()
	}})

	owner := h.CreateEntityRaw(m.p, 1, workflowSampleModel)

	var l later
	select {
	case l = <-got:
	case <-time.After(30 * time.Second):
		t.Fatal("the callbacks never returned")
	}
	for _, err := range l.errs {
		t.Error(err)
	}
	t.Run("http-read", func(t *testing.T) {
		assertProblem(t, l.httpGet.StatusCode, l.httpGet.Body, http.StatusConflict, "TRANSACTION_ROLLBACK_ONLY", false)
	})
	t.Run("http-write", func(t *testing.T) {
		assertProblem(t, l.httpPost.StatusCode, l.httpPost.Body, http.StatusConflict, "TRANSACTION_ROLLBACK_ONLY", false)
	})
	t.Run("grpc-read", func(t *testing.T) { assertRollbackOnlyEnvelope(t, "grpc-read", l.grpcGet, nil) })
	t.Run("grpc-write", func(t *testing.T) { assertRollbackOnlyEnvelope(t, "grpc-write", l.grpcPost, nil) })
	assertProblem(t, owner.status, owner.body, http.StatusConflict, "TRANSACTION_ROLLBACK_ONLY", false)
	if n := h.countEntities(t, m.x); n != 0 {
		t.Errorf("%d x entities committed; want none", n)
	}
}

// TestCallbackScope_LostRace_CallbackNotRetryable_OwnerRetryable: the
// callback's update of F loses to a concurrent client's commit. The callback
// cannot succeed in that transaction, so it is answered 409 CONFLICT without
// retryable; the owner's operation is the one to retry, and it gets the
// retryable 409 CONFLICT. On both doors.
func TestCallbackScope_LostRace_CallbackNotRetryable_OwnerRetryable(t *testing.T) {
	for _, door := range []string{"http", "grpc"} {
		t.Run(door, func(t *testing.T) {
			h := newCalloutHarness(t, nil)
			fModel, model, tag := uniq("cs-race-f"), uniq("cs-race"), uniq("cs-race-tag")
			h.SetupModelWithWorkflow(t, fModel, conflictDoorWorkflow(fModel, uniq("cs-race-unused")))
			fID := createOpen(t, h, fModel, workflowSampleModel)
			h.SetupModelWithWorkflow(t, model, chainWorkflowJSON(model+"-wf", procSpec{"cs-race-p", "SYNC", map[string]any{"calculationNodesTags": tag}}))

			gotWork, release := make(chan struct{}, 1), make(chan struct{})
			rel := closeOnce(release)
			t.Cleanup(rel)
			type cbOutcome struct {
				res callbackResult
				env txEnvelope
				err error
			}
			cb := make(chan cbOutcome, 1)
			const update = `{"name":"Test Order","amount":2,"status":"draft"}`
			h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{tag}, script: func(ctx context.Context, call receivedCallout, rc *reqCtx) cnodeReply {
				gotWork <- struct{}{}
				select {
				case <-release:
				case <-ctx.Done():
					return neverAnswer()
				}
				var o cbOutcome
				if door == "http" {
					o.res, o.err = rc.UpdateEntity(fID, update)
				} else {
					o.env, o.err = h.joinedManageGRPC(internalgrpc.EntityUpdateRequest, map[string]any{
						"id": "cs-race-update", "dataFormat": "JSON",
						"payload": map[string]any{"entityId": fID, "data": mustJSONMap(update)},
					}, call.Pass())
				}
				cb <- o
				return answerOK()
			}})

			type ownerOutcome struct {
				res createEntityResult
				env txEnvelope
				err error
			}
			done := make(chan ownerOutcome, 1)
			go func() {
				if door == "http" {
					done <- ownerOutcome{res: h.CreateEntityRaw(model, 1, workflowSampleModel)}
					return
				}
				env, err := h.createEntityGRPC(model, 1, workflowSampleModel)
				done <- ownerOutcome{env: env, err: err}
			}()

			select {
			case <-gotWork:
			case <-time.After(30 * time.Second):
				t.Fatal("the processor callout never arrived")
			}
			resp := h.DoAuth(t, http.MethodPut, "/api/entity/JSON/"+fID, `{"name":"Test Order","amount":1,"status":"draft"}`, "")
			if body := h.readBody(t, resp); resp.StatusCode != http.StatusOK {
				t.Fatalf("client update of F: %d %s", resp.StatusCode, body)
			}
			rel()

			var c cbOutcome
			select {
			case c = <-cb:
			case <-time.After(30 * time.Second):
				t.Fatal("the callback never returned")
			}
			var o ownerOutcome
			select {
			case o = <-done:
			case <-time.After(60 * time.Second):
				t.Fatal("the owner never answered")
			}
			if door == "http" {
				if c.err != nil {
					t.Fatalf("callback: %v", c.err)
				}
				assertProblem(t, c.res.StatusCode, c.res.Body, http.StatusConflict, "CONFLICT", false)
				assertProblem(t, o.res.status, o.res.body, http.StatusConflict, "CONFLICT", true)
			} else {
				assertEnvelope(t, "callback", c.env, c.err, "CONFLICT", false)
				assertEnvelope(t, "owner", o.env, o.err, "CONFLICT", true)
			}
			if amount := h.GetEntityData(t, fID)["amount"]; amount != float64(1) {
				t.Errorf("F has amount %v; the callback's write must not commit over the client's", amount)
			}
		})
	}
}

// TestCallbackScope_WriteOverResponseCeiling_413AndMarked: a joined collection
// create whose answer passes the response ceiling is answered 413
// JOINED_RESPONSE_TOO_LARGE. The compute node cannot know what its write did,
// so the write is discarded and the transaction made rollback-only: the owner
// is refused 409 TRANSACTION_ROLLBACK_ONLY. On both doors.
func TestCallbackScope_WriteOverResponseCeiling_413AndMarked(t *testing.T) {
	const ceiling = 64 // bytes; smaller than any collection-create answer
	for _, door := range []string{"http", "grpc"} {
		t.Run(door, func(t *testing.T) {
			h := newCalloutHarness(t, func(cfg *app.Config) {
				calloutTuning(0, 100*time.Millisecond)(cfg)
				cfg.Callout.JoinedResponseMaxBytes = ceiling
			})
			sfx := randSuffix(t)
			model, secondary, tag := "cs-ceil-"+sfx, "cs-ceil-y-"+sfx, "cs-ceil-"+sfx
			h.SetupModelWithWorkflow(t, secondary, secondaryWorkflow)
			h.SetupModelWithWorkflow(t, model, chainWorkflowJSON(model+"-wf", procSpec{"cs-ceil-p", "SYNC", map[string]any{"calculationNodesTags": tag}}))
			type cbOutcome struct {
				res callbackResult
				env txEnvelope
				err error
			}
			cb := make(chan cbOutcome, 1)
			h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{tag}, script: func(_ context.Context, call receivedCallout, _ *reqCtx) cnodeReply {
				var o cbOutcome
				if door == "http" {
					o.res, o.err = h.callback(http.MethodPost, "/api/entity/JSON", collectionCreateItems(secondary, scopeXPayload), call.Pass())
				} else {
					o.env, o.err = h.ReplayCreateCollectionGRPC(call.Pass(), secondary, 1, scopeXPayload)
				}
				cb <- o
				return answerOK()
			}})

			owner := h.CreateEntityRaw(model, 1, workflowSampleModel)

			var c cbOutcome
			select {
			case c = <-cb:
			case <-time.After(30 * time.Second):
				t.Fatal("the callback never returned")
			}
			if door == "http" {
				if c.err != nil {
					t.Fatalf("callback: %v", c.err)
				}
				assertProblem(t, c.res.StatusCode, c.res.Body, http.StatusRequestEntityTooLarge, "JOINED_RESPONSE_TOO_LARGE", false)
			} else {
				assertEnvelope(t, "callback", c.env, c.err, "JOINED_RESPONSE_TOO_LARGE", false)
			}
			pd := assertProblem(t, owner.status, owner.body, http.StatusConflict, "TRANSACTION_ROLLBACK_ONLY", false)
			if !strings.Contains(pd.Detail, "JOINED_RESPONSE_TOO_LARGE") {
				t.Errorf("owner detail = %q; want it to name JOINED_RESPONSE_TOO_LARGE", pd.Detail)
			}
			if n := h.countEntities(t, secondary); n != 0 {
				t.Errorf("%d secondary entities committed; the refused write must be discarded", n)
			}
		})
	}
}

// TestCallbackScope_PostgresSessionReclaimed: PostgreSQL ends the transaction's
// session while the owner waits in its callout (idle_in_transaction_session_
// timeout). A callback made after that is answered 404 TRANSACTION_NOT_FOUND,
// not retryable — it cannot succeed in that transaction — and the owner gets
// the retryable 503 STORAGE_UNAVAILABLE: its operation can, on a fresh one.
func TestCallbackScope_PostgresSessionReclaimed(t *testing.T) {
	const idle = 1500 * time.Millisecond
	h := newCalloutHarness(t, func(cfg *app.Config) {
		calloutTuning(0, 100*time.Millisecond)(cfg)
		// Read by the postgres plugin's own getenv at factory-open time, which
		// happens inside app.New — after this mutator runs.
		t.Setenv("CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT", idle.String())
	})
	sfx := randSuffix(t)
	model, secondary, tag := "cs-idle-"+sfx, "cs-idle-y-"+sfx, "cs-idle-"+sfx
	h.SetupModelWithWorkflow(t, secondary, secondaryWorkflow)
	h.SetupModelWithWorkflow(t, model, chainWorkflowJSON(model+"-wf", procSpec{"cs-idle-p", "SYNC",
		map[string]any{"calculationNodesTags": tag, "responseTimeoutMs": 30000}}))
	cb := make(chan callbackResult, 1)
	h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{tag}, script: func(ctx context.Context, _ receivedCallout, rc *reqCtx) cnodeReply {
		select {
		case <-time.After(idle + 2*time.Second): // the session is reclaimed meanwhile
		case <-ctx.Done():
			return neverAnswer()
		}
		res, err := rc.CreateEntity(secondary, 1, scopeXPayload)
		if err != nil {
			res = callbackResult{StatusCode: -1, Body: err.Error()}
		}
		cb <- res
		return answerOK()
	}})

	owner := h.CreateEntityRaw(model, 1, workflowSampleModel)

	res := awaitCallback(t, "the callback", cb)
	assertProblem(t, res.StatusCode, res.Body, http.StatusNotFound, "TRANSACTION_NOT_FOUND", false)
	assertProblem(t, owner.status, owner.body, http.StatusServiceUnavailable, "STORAGE_UNAVAILABLE", true)
	if n := h.countEntities(t, secondary); n != 0 {
		t.Errorf("%d secondary entities committed; want none", n)
	}
}
