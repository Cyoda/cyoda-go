package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cyoda/cyoda-go/app"
	internalgrpc "github.com/cyoda/cyoda-go/internal/grpc"
)

// callback_scope_test.go — every joined request runs in a scope of its own,
// and the scope is all-or-nothing. A failure of a joined write, retryable or
// not, discards what it wrote and leaves the transaction usable: later joined
// requests on the pass are served, and the processor that made the callback
// decides the outcome — the owner commits when it answers success, and fails
// as for any failed processor when it answers failure. A read that answered
// keeps its scope. A callback that lost a write race, or whose PostgreSQL
// session has ended, cannot succeed in that transaction, and neither can its
// owner.
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
// nothing: it cannot be read, the audit door has no version of it, and no
// StateMachine audit row names it. The audit door answers 404 before it merges
// StateMachine events when the entity has no version, so those rows are
// counted in PostgreSQL itself.
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
		t.Errorf("entity %s: audit answered %d %s; want 404 — a discarded write leaves no version", entityID, resp.StatusCode, body)
	}
	if n := smAuditRows(t, entityID); n != 0 {
		t.Errorf("entity %s: %d StateMachine audit rows committed; want none — a discarded write leaves no audit event", entityID, n)
	}
}

// smAuditRows counts the committed StateMachine audit rows of an entity.
func smAuditRows(t *testing.T, entityID string) int {
	t.Helper()
	var n int
	if err := dbPool.QueryRow(context.Background(),
		`SELECT count(*) FROM sm_audit_events WHERE entity_id = $1`, entityID).Scan(&n); err != nil {
		t.Fatalf("count sm_audit_events for %s: %v", entityID, err)
	}
	return n
}

// firstEntity is the entity id of the first callout cnode received, failing
// the test when it received none.
func firstEntity(t *testing.T, cnode *scriptedCnode) string {
	t.Helper()
	got := cnode.Received()
	if len(got) == 0 {
		t.Fatalf("cnode %s received no callout", cnode.name)
	}
	return got[0].EntityID
}

// assertRefusal asserts a problem response with wantStatus and wantCode that
// does not carry the retryable flag at all.
func assertRefusal(t *testing.T, status int, body string, wantStatus int, wantCode string) problemDoc {
	t.Helper()
	pd := assertProblem(t, status, body, wantStatus, wantCode, false)
	if _, present := pd.Properties["retryable"]; present {
		t.Errorf("retryable is set; want it absent (body: %s)", body)
	}
	return pd
}

// callbackOutcome folds a transport error into the callbackResult, so a cnode
// goroutine can hand back one value.
func callbackOutcome(res callbackResult, err error) callbackResult {
	if err != nil {
		return callbackResult{StatusCode: -1, Body: err.Error()}
	}
	return res
}

// onlyEntityTxID asserts model holds exactly one committed entity and returns
// the transaction that committed it (its meta.transactionId).
func onlyEntityTxID(t *testing.T, h *callbackHarness, model string) string {
	t.Helper()
	resp := h.DoAuth(t, http.MethodGet, fmt.Sprintf("/api/entity/%s/1", model), "", "")
	body := h.readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list %s: %d %s", model, resp.StatusCode, body)
	}
	var list []struct {
		Meta struct {
			ID            string `json:"id"`
			TransactionID string `json:"transactionId"`
		} `json:"meta"`
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("decode list of %s: %v (body: %s)", model, err, body)
	}
	if len(list) != 1 {
		t.Fatalf("%d %s entities committed; want exactly 1 (body: %s)", len(list), model, body)
	}
	if list[0].Meta.TransactionID == "" {
		t.Fatalf("the %s entity %s carries no transactionId (body: %s)", model, list[0].Meta.ID, body)
	}
	return list[0].Meta.TransactionID
}

// assertCommittedWithOwner asserts the owner's model and the later write's
// model each hold one committed entity, and that the later joined write was
// committed by the owner's transaction.
func assertCommittedWithOwner(t *testing.T, h *callbackHarness, ownerModel, laterModel string) {
	t.Helper()
	ownerTx := onlyEntityTxID(t, h, ownerModel)
	if got := onlyEntityTxID(t, h, laterModel); got != ownerTx {
		t.Errorf("the later joined write was committed by transaction %q; want the owner's %q", got, ownerTx)
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

// newScopeLaterModel imports a plain model for the later joined write a
// scenario's processor makes after its failed one.
func newScopeLaterModel(t *testing.T, h *callbackHarness, prefix string) string {
	t.Helper()
	y := prefix + "-y-" + randSuffix(t)
	h.SetupModelWithWorkflow(t, y, secondaryWorkflow)
	return y
}

// TestCallbackScope_FailedJoinedWrite_ProcessorHandlesIt_HTTP: the callback's
// create of x runs xproc, which fails, not retryably: the callback is answered
// its own 400 WORKFLOW_FAILED and nothing of x is kept. The transaction goes
// on: cnode p then creates y in it and answers success. The owner commits its
// own p and y, in one transaction; x is absent.
func TestCallbackScope_FailedJoinedWrite_ProcessorHandlesIt_HTTP(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(0, 100*time.Millisecond))
	m := newScopeModels(t, h, "cs-fw", "SYNC")
	y := newScopeLaterModel(t, h, "cs-fw")
	xnode := h.AttachCnode(t, cnodeSpec{name: "x", tags: []string{m.tagX}, script: scriptAlways(answerFailVerdict("x refused", false))})
	type observed struct{ failed, later callbackResult }
	cb := make(chan observed, 1)
	h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{m.tagP}, script: func(_ context.Context, _ receivedCallout, rc *reqCtx) cnodeReply {
		var o observed
		o.failed = callbackOutcome(rc.CreateEntity(m.x, 1, scopeXPayload))
		o.later = callbackOutcome(rc.CreateEntity(y, 1, scopeXPayload))
		cb <- o
		return answerOK()
	}})

	owner := h.CreateEntityRaw(m.p, 1, workflowSampleModel)

	o := awaitAttempt(t, "the callbacks", cb)
	assertRefusal(t, o.failed.StatusCode, o.failed.Body, http.StatusBadRequest, "WORKFLOW_FAILED")
	if o.later.StatusCode != http.StatusOK {
		t.Errorf("the later joined write: %d %s; want 200 — a failed write leaves the transaction usable", o.later.StatusCode, o.later.Body)
	}
	if owner.err != nil || owner.status != http.StatusOK {
		t.Fatalf("owner: %d %s %v; want 200 — the processor answered success", owner.status, owner.body, owner.err)
	}
	if n := h.countEntities(t, m.x); n != 0 {
		t.Errorf("%d x entities committed; want none", n)
	}
	assertNoEntityTrace(t, h, firstEntity(t, xnode))
	assertCommittedWithOwner(t, h, m.p, y)
}

// TestCallbackScope_FailedJoinedWrite_ProcessorHandlesIt_GRPC is the same
// through gRPC on both sides: the owner's create and the callbacks.
func TestCallbackScope_FailedJoinedWrite_ProcessorHandlesIt_GRPC(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(0, 100*time.Millisecond))
	m := newScopeModels(t, h, "cs-fwg", "SYNC")
	y := newScopeLaterModel(t, h, "cs-fwg")
	xnode := h.AttachCnode(t, cnodeSpec{name: "x", tags: []string{m.tagX}, script: scriptAlways(answerFailVerdict("x refused", false))})
	type envResult struct {
		env txEnvelope
		err error
	}
	type observed struct{ failed, later envResult }
	cb := make(chan observed, 1)
	h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{m.tagP}, script: func(_ context.Context, call receivedCallout, _ *reqCtx) cnodeReply {
		var o observed
		o.failed.env, o.failed.err = h.ReplayCreateGRPC(call.Pass(), m.x, 1, scopeXPayload)
		o.later.env, o.later.err = h.ReplayCreateGRPC(call.Pass(), y, 1, scopeXPayload)
		cb <- o
		return answerOK()
	}})

	owner, ownerErr := h.createEntityGRPC(m.p, 1, workflowSampleModel)

	o := awaitAttempt(t, "the callbacks", cb)
	assertEnvelope(t, "callback", o.failed.env, o.failed.err, "WORKFLOW_FAILED", false)
	if o.later.err != nil || !o.later.env.Success {
		t.Errorf("the later joined write: success=%t error=%v err=%v; want success — a failed write leaves the transaction usable", o.later.env.Success, o.later.env.Error, o.later.err)
	}
	if ownerErr != nil || !owner.Success {
		t.Fatalf("owner: success=%t error=%v err=%v; want success — the processor answered success", owner.Success, owner.Error, ownerErr)
	}
	if n := h.countEntities(t, m.x); n != 0 {
		t.Errorf("%d x entities committed; want none", n)
	}
	assertNoEntityTrace(t, h, firstEntity(t, xnode))
	assertCommittedWithOwner(t, h, m.p, y)
}

// TestCallbackScope_FailedJoinedWrite_ProcessorFails_HTTP: as above, but
// after its later write cnode p answers failure, not retryably. The owner
// fails as for any failed SYNC processor: 400 WORKFLOW_FAILED, not retryable,
// carrying the member's message. Nothing of the transaction is committed: no
// p, no y, no x.
func TestCallbackScope_FailedJoinedWrite_ProcessorFails_HTTP(t *testing.T) {
	const pMessage = "p gives up after its failed callback"
	h := newCalloutHarness(t, calloutTuning(0, 100*time.Millisecond))
	m := newScopeModels(t, h, "cs-fwf", "SYNC")
	y := newScopeLaterModel(t, h, "cs-fwf")
	xnode := h.AttachCnode(t, cnodeSpec{name: "x", tags: []string{m.tagX}, script: scriptAlways(answerFailVerdict("x refused", false))})
	type observed struct{ failed, later callbackResult }
	cb := make(chan observed, 1)
	pnode := h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{m.tagP}, script: func(_ context.Context, _ receivedCallout, rc *reqCtx) cnodeReply {
		var o observed
		o.failed = callbackOutcome(rc.CreateEntity(m.x, 1, scopeXPayload))
		o.later = callbackOutcome(rc.CreateEntity(y, 1, scopeXPayload))
		cb <- o
		return answerFailVerdict(pMessage, false)
	}})

	owner := h.CreateEntityRaw(m.p, 1, workflowSampleModel)

	o := awaitAttempt(t, "the callbacks", cb)
	assertRefusal(t, o.failed.StatusCode, o.failed.Body, http.StatusBadRequest, "WORKFLOW_FAILED")
	if o.later.StatusCode != http.StatusOK {
		t.Errorf("the later joined write: %d %s; want 200 — a failed write leaves the transaction usable", o.later.StatusCode, o.later.Body)
	}
	assertProblem(t, owner.status, owner.body, http.StatusBadRequest, "WORKFLOW_FAILED", false)
	if !strings.Contains(owner.body, pMessage) {
		t.Errorf("owner body does not carry the member's message %q: %s", pMessage, owner.body)
	}
	for _, model := range []string{m.p, m.x, y} {
		if n := h.countEntities(t, model); n != 0 {
			t.Errorf("%d %s entities committed; want none", n, model)
		}
	}
	assertNoEntityTrace(t, h, firstEntity(t, xnode))
	assertNoEntityTrace(t, h, firstEntity(t, pnode))
}

// TestCallbackScope_RetryableFailure_RetriedInTransaction: xproc first fails
// retryably — the callback is refused retryable (HTTP 400 WORKFLOW_FAILED with
// retryable, gRPC the CLIENT_ERROR envelope with Retryable=true) and its write
// discarded, the transaction stays usable — and the cnode sends the same
// callback again, which succeeds. The owner commits exactly one x, and the
// audit trail holds the second run only: the first run's StateMachine events
// were discarded with its scope. Both doors.
func TestCallbackScope_RetryableFailure_RetriedInTransaction(t *testing.T) {
	for _, door := range []string{"http", "grpc"} {
		t.Run(door, func(t *testing.T) {
			h := newCalloutHarness(t, calloutTuning(0, 100*time.Millisecond))
			m := newScopeModels(t, h, "cs-rt-"+door, "SYNC")
			xnode := h.AttachCnode(t, cnodeSpec{name: "x", tags: []string{m.tagX},
				script: scriptSequence(answerFailVerdict("x busy", true), answerOK())})
			type attempt struct {
				res callbackResult
				env txEnvelope
				err error
			}
			cb := make(chan attempt, 2)
			h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{m.tagP}, script: func(_ context.Context, call receivedCallout, rc *reqCtx) cnodeReply {
				for range 2 {
					var a attempt
					if door == "http" {
						a.res, a.err = rc.CreateEntity(m.x, 1, scopeXPayload)
					} else {
						a.env, a.err = h.ReplayCreateGRPC(call.Pass(), m.x, 1, scopeXPayload)
					}
					cb <- a
					if a.err == nil && (a.res.StatusCode == http.StatusOK || a.env.Success) {
						break
					}
				}
				return answerOK()
			}})

			owner := h.CreateEntityRaw(m.p, 1, workflowSampleModel)

			first, second := awaitAttempt(t, "the first callback", cb), awaitAttempt(t, "the retried callback", cb)
			if door == "http" {
				if first.err != nil || second.err != nil {
					t.Fatalf("callbacks: %v, %v", first.err, second.err)
				}
				assertProblem(t, first.res.StatusCode, first.res.Body, http.StatusBadRequest, "WORKFLOW_FAILED", true)
				if second.res.StatusCode != http.StatusOK {
					t.Fatalf("the retried callback: %d %s; want 200 — a retryable failure leaves the transaction usable", second.res.StatusCode, second.res.Body)
				}
			} else {
				assertEnvelope(t, "the first callback", first.env, first.err, "WORKFLOW_FAILED", true)
				if second.err != nil || !second.env.Success {
					t.Fatalf("the retried callback: success=%t error=%v err=%v; want success — a retryable failure leaves the transaction usable", second.env.Success, second.env.Error, second.err)
				}
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
			if n := smAuditRows(t, runs[1].EntityID); n == 0 {
				t.Error("the retried x has no StateMachine audit rows")
			}
		})
	}
}

// awaitAttempt receives one value from ch, failing the test when none arrives
// in time.
func awaitAttempt[T any](t *testing.T, what string, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(30 * time.Second):
		t.Fatalf("%s never returned", what)
		var zero T
		return zero
	}
}

// TestCallbackScope_FailedRead404_TransactionGoesOn: a joined read of an
// unknown entity is answered 404, and the owner commits.
// Whether the read's scope is kept (row 7) or discarded (row 8) does not change
// that outcome, so this test does not tell them apart; TestEnd_Row7_Read4xxKeeps
// in internal/txcoord pins row 7.
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
	assertRefusal(t, res.StatusCode, res.Body, http.StatusNotFound, "ENTITY_NOT_FOUND")
	if owner.err != nil || owner.status != http.StatusOK {
		t.Fatalf("owner: %d %s %v; want 200 — a read's 404 does not touch the transaction", owner.status, owner.body, owner.err)
	}
	if n := h.countEntities(t, m.p); n != 1 {
		t.Errorf("%d p entities committed; want 1", n)
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
				assertRefusal(t, c.res.StatusCode, c.res.Body, http.StatusConflict, "CONFLICT")
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

// TestCallbackScope_WriteOverResponseCeiling_413Discarded: a joined collection
// create whose answer passes the response ceiling is answered 413
// JOINED_RESPONSE_TOO_LARGE. The compute node cannot know what its write did,
// so the write is discarded. The transaction goes on: the member answers
// success, and the owner commits without the write. On both doors.
func TestCallbackScope_WriteOverResponseCeiling_413Discarded(t *testing.T) {
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
				assertRefusal(t, c.res.StatusCode, c.res.Body, http.StatusRequestEntityTooLarge, "JOINED_RESPONSE_TOO_LARGE")
			} else {
				assertEnvelope(t, "callback", c.env, c.err, "JOINED_RESPONSE_TOO_LARGE", false)
			}
			if owner.err != nil || owner.status != http.StatusOK {
				t.Fatalf("owner: %d %s %v; want 200 — the member answered success", owner.status, owner.body, owner.err)
			}
			if n := h.countEntities(t, secondary); n != 0 {
				t.Errorf("%d secondary entities committed; the refused write must be discarded", n)
			}
			if n := h.countEntities(t, model); n != 1 {
				t.Errorf("%d owner entities committed; want 1", n)
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
		if err := awaitSessionReclaimed(ctx, idle+15*time.Second); err != nil {
			cb <- callbackResult{StatusCode: -1, Body: err.Error()}
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
	assertRefusal(t, res.StatusCode, res.Body, http.StatusNotFound, "TRANSACTION_NOT_FOUND")
	assertProblem(t, owner.status, owner.body, http.StatusServiceUnavailable, "STORAGE_UNAVAILABLE", true)
	if n := h.countEntities(t, secondary); n != 0 {
		t.Errorf("%d secondary entities committed; want none", n)
	}
}

// awaitSessionReclaimed waits, within bound, until PostgreSQL has ended the
// owner's session: it finds the one settled idle-in-transaction backend of the
// database (the owner waits in its callout; e2e tests run one at a time), then
// polls until that backend is gone. It returns an error instead of failing the
// test: it runs on a cnode's goroutine.
func awaitSessionReclaimed(ctx context.Context, bound time.Duration) error {
	const idleInTx = `SELECT pid FROM pg_stat_activity
		 WHERE datname = current_database() AND state = 'idle in transaction'
		   AND pid <> pg_backend_pid() AND state_change < now() - interval '200 milliseconds'`
	deadline := time.Now().Add(bound)
	var pid int32
	for {
		rows, err := dbPool.Query(ctx, idleInTx)
		if err != nil {
			return fmt.Errorf("pg_stat_activity: %w", err)
		}
		pids, err := pgx.CollectRows(rows, pgx.RowTo[int32])
		if err != nil {
			return fmt.Errorf("pg_stat_activity: %w", err)
		}
		if len(pids) == 1 {
			pid = pids[0]
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("found %d settled idle-in-transaction backends; want exactly the owner's", len(pids))
		}
		time.Sleep(20 * time.Millisecond)
	}
	for {
		var alive bool
		if err := dbPool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE pid = $1)`, pid).Scan(&alive); err != nil {
			return fmt.Errorf("pg_stat_activity: %w", err)
		}
		if !alive {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the owner's session (pid %d) was not reclaimed within %v", pid, bound)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// laterRequest is what a joined request made after the failed write answered,
// in a door-neutral form.
type laterRequest struct {
	door string
	ok   bool   // HTTP 2xx, or a gRPC envelope with success
	code string // the error code of a refusal
	raw  string
	err  error
}

func laterHTTP(res callbackResult, err error) laterRequest {
	o := httpJoinedOutcome(res, err)
	return laterRequest{ok: err == nil && res.StatusCode/100 == 2, code: o.errorCode, raw: fmt.Sprintf("%d %s", res.StatusCode, res.Body), err: err}
}

func laterGRPC(env txEnvelope, err error) laterRequest {
	o := grpcJoinedOutcome(env, err)
	return laterRequest{ok: err == nil && env.Success, code: o.errorCode, raw: o.detail, err: err}
}

// assertLaterServed asserts a later request on the pass was served.
func assertLaterServed(t *testing.T, r laterRequest) {
	t.Helper()
	if r.err != nil {
		t.Fatalf("%s: transport error: %v", r.door, r.err)
	}
	if !r.ok {
		t.Errorf("%s: refused (%s); want it served — a failed write leaves the transaction usable", r.door, r.raw)
	}
}

// TestCallbackScope_FailedWrite_LaterRequestsOnThePassServed: after a
// callback's write failed, not retryably, a later joined read and a later
// joined write on the same pass are served. The member answers success, and
// the owner commits p and the later write, in one transaction, without the
// failed write. Both doors.
func TestCallbackScope_FailedWrite_LaterRequestsOnThePassServed(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(0, 100*time.Millisecond))
	for _, door := range []string{"http", "grpc"} {
		t.Run(door, func(t *testing.T) {
			m := newScopeModels(t, h, "cs-later-"+door, "SYNC")
			y := newScopeLaterModel(t, h, "cs-later-"+door)
			h.AttachCnode(t, cnodeSpec{name: "x", tags: []string{m.tagX}, script: scriptAlways(answerFailVerdict("x refused", false))})
			type observed struct {
				failed       laterRequest
				read, create laterRequest
			}
			got := make(chan observed, 1)
			h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{m.tagP}, script: func(_ context.Context, call receivedCallout, rc *reqCtx) cnodeReply {
				var o observed
				if door == "http" {
					o.failed = laterHTTP(rc.CreateEntity(m.x, 1, scopeXPayload))
					o.read = laterHTTP(rc.GetEntity(call.EntityID))
					o.create = laterHTTP(rc.CreateEntity(y, 1, scopeXPayload))
				} else {
					o.failed = laterGRPC(h.ReplayCreateGRPC(call.Pass(), m.x, 1, scopeXPayload))
					o.read = laterGRPC(h.ReplayGetGRPC(call.Pass(), call.EntityID))
					o.create = laterGRPC(h.ReplayCreateGRPC(call.Pass(), y, 1, scopeXPayload))
				}
				o.failed.door, o.read.door, o.create.door = "failed write", "later read", "later write"
				select {
				case got <- o:
				default:
				}
				return answerOK()
			}})

			owner := h.CreateEntityRaw(m.p, 1, workflowSampleModel)

			var o observed
			select {
			case o = <-got:
			case <-time.After(30 * time.Second):
				t.Fatal("the callbacks never ran")
			}
			if o.failed.code != "WORKFLOW_FAILED" {
				t.Fatalf("the first write: code %q; want WORKFLOW_FAILED (%s)", o.failed.code, o.failed.raw)
			}
			assertLaterServed(t, o.read)
			assertLaterServed(t, o.create)
			if owner.err != nil || owner.status != http.StatusOK {
				t.Fatalf("owner: %d %s %v; want 200 — the member answered success", owner.status, owner.body, owner.err)
			}
			if n := h.countEntities(t, m.x); n != 0 {
				t.Errorf("%d x entities committed; want none", n)
			}
			assertCommittedWithOwner(t, h, m.p, y)
		})
	}
}
