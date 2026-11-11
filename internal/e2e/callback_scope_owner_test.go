package e2e_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

// callback_scope_owner_test.go — the owner's operations on a rollback-only
// transaction. A callback of the operation's SYNC processor fails a write, not
// retryably: the transaction's root is marked, and every owner operation that
// runs the processor is refused 409 TRANSACTION_ROLLBACK_ONLY (CLIENT_ERROR,
// with the code as the message prefix, on gRPC), naming the callback's code.
// Nothing of the operation is committed: no entity, no new version, and no
// StateMachine audit row. A scheduled fire whose processor's callback fails a
// write fails the same way, and its task records the refusal.

// ownerOpTarget is the entity an owner operation acts on, for the operations
// that act on an existing one.
type ownerOpTarget struct {
	id, txID string
}

// ownerOp is one owner operation. It reports what the door answered in a
// door-neutral form (joinedCommitOutcome).
type ownerOp struct {
	name  string
	shape joinedCommitShape
	run   func(h *callbackHarness, model string, tg ownerOpTarget) joinedCommitOutcome
}

func ownerOps() []ownerOp {
	return []ownerOp{
		{"http-create", shapeOnCreate, func(h *callbackHarness, model string, _ ownerOpTarget) joinedCommitOutcome {
			return httpJoinedOutcome(h.callback(http.MethodPost, "/api/entity/JSON/"+model+"/1", joinedCommitPayload, ""))
		}},
		{"http-create-collection", shapeOnCreate, func(h *callbackHarness, model string, _ ownerOpTarget) joinedCommitOutcome {
			return httpJoinedOutcome(h.callback(http.MethodPost, "/api/entity/JSON", collectionCreateItems(model, joinedCommitPayload), ""))
		}},
		{"http-update-loopback", shapeOnLoopback, func(h *callbackHarness, _ string, tg ownerOpTarget) joinedCommitOutcome {
			return httpJoinedOutcome(h.callback(http.MethodPut, "/api/entity/JSON/"+tg.id, joinedCommitGoPayload, ""))
		}},
		{"http-update-collection", shapeOnManual, func(h *callbackHarness, _ string, tg ownerOpTarget) joinedCommitOutcome {
			return httpJoinedOutcome(h.callback(http.MethodPut, "/api/entity/JSON", collectionUpdateItems(tg.id, "go", joinedCommitPayload), ""))
		}},
		{"http-patch", shapeOnLoopback, func(h *callbackHarness, _ string, tg ownerOpTarget) joinedCommitOutcome {
			return httpJoinedOutcome(h.ownerPatch(tg.id, tg.txID, `{"status":"go"}`))
		}},
		{"http-transition", shapeOnManual, func(h *callbackHarness, _ string, tg ownerOpTarget) joinedCommitOutcome {
			return httpJoinedOutcome(h.callback(http.MethodPut, "/api/entity/JSON/"+tg.id+"/go", joinedCommitPayload, ""))
		}},
		{"grpc-create", shapeOnCreate, func(h *callbackHarness, model string, _ ownerOpTarget) joinedCommitOutcome {
			return grpcJoinedOutcome(h.createEntityGRPC(model, 1, joinedCommitPayload))
		}},
		{"grpc-update", shapeOnLoopback, func(h *callbackHarness, _ string, tg ownerOpTarget) joinedCommitOutcome {
			return grpcJoinedOutcome(h.joinedManageGRPC(internalgrpc.EntityUpdateRequest, map[string]any{
				"id": "owner-update", "dataFormat": "JSON",
				"payload": map[string]any{"entityId": tg.id, "data": mustJSONMap(joinedCommitGoPayload)},
			}, ""))
		}},
		{"grpc-transition", shapeOnManual, func(h *callbackHarness, _ string, tg ownerOpTarget) joinedCommitOutcome {
			return grpcJoinedOutcome(h.joinedManageGRPC(internalgrpc.EntityTransitionRequest, map[string]any{
				"id": "owner-transition", "entityId": tg.id, "transition": "go",
			}, ""))
		}},
		{"grpc-create-collection", shapeOnCreate, func(h *callbackHarness, model string, _ ownerOpTarget) joinedCommitOutcome {
			tok, _ := h.bearerVal.Load().(string)
			return grpcJoinedOutcome(h.replayCreateCollectionGRPCAs(tok, "", model, 1, joinedCommitPayload))
		}},
		{"grpc-update-collection", shapeOnManual, func(h *callbackHarness, _ string, tg ownerOpTarget) joinedCommitOutcome {
			return grpcJoinedOutcome(h.joinedManageCollectionGRPC(internalgrpc.EntityUpdateCollectionRequest, map[string]any{
				"id": "owner-update-collection", "dataFormat": "JSON",
				"payloads": []any{map[string]any{"entityId": tg.id, "transition": "go", "data": mustJSONMap(joinedCommitPayload)}},
			}, ""))
		}},
	}
}

// ownerPatch is an owner's PATCH: merge-patch, with If-Match, no pass.
func (h *callbackHarness) ownerPatch(entityID, ifMatch, patch string) (callbackResult, error) {
	req, err := http.NewRequestWithContext(h.ownerCtx(), http.MethodPatch, h.baseURL+"/api/entity/JSON/"+entityID, strings.NewReader(patch))
	if err != nil {
		return callbackResult{}, err
	}
	tok, _ := h.bearerVal.Load().(string)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/merge-patch+json")
	req.Header.Set("If-Match", ifMatch)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return callbackResult{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return callbackResult{StatusCode: resp.StatusCode, Body: string(raw)}, nil
}

// ownerOpWorkflow places one SYNC processor on tag as shape says.
func ownerOpWorkflow(name, tag string, shape joinedCommitShape) string {
	proc := []any{map[string]any{"type": "externalized", "name": "cs-own-proc", "executionMode": "SYNC",
		"config": map[string]any{"attachEntity": true, "calculationNodesTags": tag}}}
	var states map[string]any
	switch shape {
	case shapeOnCreate:
		states = map[string]any{
			"NONE": map[string]any{"transitions": []any{map[string]any{"name": "init", "next": "DONE", "manual": false, "processors": proc}}},
			"DONE": map[string]any{},
		}
	case shapeOnManual:
		states = map[string]any{
			"NONE":  map[string]any{"transitions": []any{map[string]any{"name": "store", "next": "READY", "manual": false}}},
			"READY": map[string]any{"transitions": []any{map[string]any{"name": "go", "next": "DONE", "manual": true, "processors": proc}}},
			"DONE":  map[string]any{},
		}
	case shapeOnLoopback:
		states = map[string]any{
			"NONE": map[string]any{"transitions": []any{map[string]any{"name": "store", "next": "READY", "manual": false}}},
			"READY": map[string]any{"transitions": []any{map[string]any{"name": "go", "next": "DONE", "manual": false,
				"criterion":  map[string]any{"type": "simple", "jsonPath": "$.status", "operatorType": "EQUALS", "value": "go"},
				"processors": proc}}},
			"DONE": map[string]any{},
		}
	}
	return workflowDocJSON(name+"-wf", states)
}

// failWriteNoWait is failWriteThen that never blocks on cb: a scheduled fire
// may send the callout again while the test is not receiving.
func failWriteNoWait(x string, cb chan<- callbackResult, reply cnodeReply) cnodeScript {
	return func(_ context.Context, _ receivedCallout, rc *reqCtx) cnodeReply {
		res, err := rc.CreateEntity(x, 1, scopeXPayload)
		if err != nil {
			res = callbackResult{StatusCode: -1, Body: err.Error()}
		}
		select {
		case cb <- res:
		default:
		}
		return reply
	}
}

// assertOwnerRollbackOnly asserts the owner's refusal: 409 (CLIENT_ERROR on
// gRPC) TRANSACTION_ROLLBACK_ONLY, not retryable, naming the callback's
// WORKFLOW_FAILED.
func assertOwnerRollbackOnly(t *testing.T, o joinedCommitOutcome) {
	t.Helper()
	if o.err != nil {
		t.Fatalf("transport error: %v", o.err)
	}
	if o.status != 0 && o.status != http.StatusConflict {
		t.Errorf("status = %d; want 409 (%s)", o.status, o.detail)
	}
	if o.status == 0 && o.envelopeCode != "CLIENT_ERROR" {
		t.Errorf("envelope code = %q; want CLIENT_ERROR (%s)", o.envelopeCode, o.detail)
	}
	if o.errorCode != "TRANSACTION_ROLLBACK_ONLY" {
		t.Errorf("error code = %q; want TRANSACTION_ROLLBACK_ONLY (%s)", o.errorCode, o.detail)
	}
	if o.retryable {
		t.Errorf("refusal marked retryable (%s)", o.detail)
	}
	if !strings.Contains(o.detail, "WORKFLOW_FAILED") {
		t.Errorf("refusal does not name the callback's WORKFLOW_FAILED: %s", o.detail)
	}
}

// TestCallbackScope_OwnerOps_RootMarked: every owner operation that runs the
// workflow, on both doors, is refused 409 TRANSACTION_ROLLBACK_ONLY when a
// callback of its processor failed a write, and commits nothing.
func TestCallbackScope_OwnerOps_RootMarked(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(0, 100*time.Millisecond))
	x, _ := scopeFailingX(t, h, "cs-own")
	for _, op := range ownerOps() {
		t.Run(op.name, func(t *testing.T) {
			sfx := randSuffix(t)
			p, tag := "cs-own-p-"+op.name+"-"+sfx, "cs-own-tp-"+op.name+"-"+sfx
			h.SetupModelWithWorkflow(t, p, ownerOpWorkflow(p, tag, op.shape))
			var tg ownerOpTarget
			auditBefore := 0
			if op.shape != shapeOnCreate {
				id, status, body := h.CreateEntity(t, p, 1, joinedCommitPayload)
				if status != http.StatusOK {
					t.Fatalf("create the target entity: %d %s", status, body)
				}
				var arr []map[string]any
				_ = json.Unmarshal([]byte(body), &arr)
				tg.id = id
				tg.txID, _ = arr[0]["transactionId"].(string)
				auditBefore = smAuditRows(t, tg.id)
				if auditBefore == 0 {
					t.Fatalf("the target's create committed no StateMachine audit row; the count cannot witness anything")
				}
			}
			cb := make(chan callbackResult, 1)
			pnode := h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{tag}, script: failWriteNoWait(x, cb, answerOK())})

			o := op.run(h, p, tg)
			if o.err != nil {
				t.Fatalf("transport error: %v", o.err)
			}

			res := awaitCallback(t, "the callback", cb)
			assertRefusal(t, res.StatusCode, res.Body, http.StatusBadRequest, "WORKFLOW_FAILED")
			assertOwnerRollbackOnly(t, o)
			if n := h.countEntities(t, x); n != 0 {
				t.Errorf("%d x entities committed; want none", n)
			}
			if op.shape == shapeOnCreate {
				if n := h.countEntities(t, p); n != 0 {
					t.Errorf("%d p entities committed; want none", n)
				}
				assertNoEntityTrace(t, h, firstEntity(t, pnode))
				return
			}
			if st, code := h.GetEntityState(t, tg.id); code != http.StatusOK || st != "READY" {
				t.Errorf("target state = %q (http %d); want READY, untouched", st, code)
			}
			if got := h.GetEntityData(t, tg.id)["status"]; got != "new" {
				t.Errorf("target status = %v; want \"new\", untouched", got)
			}
			if n := smAuditRows(t, tg.id); n != auditBefore {
				t.Errorf("target has %d StateMachine audit rows; want %d — the refused operation commits no audit event", n, auditBefore)
			}
		})
	}
}

// TestCallbackScope_ScheduledFire_RootMarked: a scheduled transition's SYNC
// processor's callback fails a write. The fire fails: its task records a
// failed attempt naming TRANSACTION_ROLLBACK_ONLY, the entity stays in its
// state, and no StateMachine audit row of the fire is committed. The processor
// is idempotent, so the failure is a safe one and the task waits for its next
// attempt rather than failing (which would commit a FAIL event of its own).
func TestCallbackScope_ScheduledFire_RootMarked(t *testing.T) {
	h, s := newSchedulerHarness(t, nil)
	x, _ := scopeFailingX(t, h, "cs-sf")
	model, tag := uniq("cs-sf-p"), uniq("cs-sf-tp")
	cb := make(chan callbackResult, 1)
	h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{tag}, script: failWriteNoWait(x, cb, answerOK())})
	h.SetupModelWithWorkflow(t, model, fireOpenToDone("cs-sf-wf", 100, 0, sProc("cs-sf-proc", "SYNC", tag, true)))
	id := createOpen(t, h, model, workflowSampleModel)
	auditBefore := s.count(t, `SELECT count(*) FROM sm_audit_events WHERE entity_id = $1`, id)
	if auditBefore == 0 {
		t.Fatal("the create committed no StateMachine audit row; the count cannot witness anything")
	}

	r := firstAttempt(t, s, id, "Fire")

	res := awaitReceive(t, cb, scheduledFireTimeout, "the callback")
	assertRefusal(t, res.StatusCode, res.Body, http.StatusBadRequest, "WORKFLOW_FAILED")
	if r.Status != "WAITING" || !strings.Contains(r.LastError, "TRANSACTION_ROLLBACK_ONLY") {
		t.Errorf("after the fire: %+v; want WAITING with a lastError naming TRANSACTION_ROLLBACK_ONLY", r)
	}
	requireState(t, h, id, "Open")
	if n := s.count(t, `SELECT count(*) FROM sm_audit_events WHERE entity_id = $1`, id); n != auditBefore {
		t.Errorf("entity has %d StateMachine audit rows; want %d — the failed fire commits no audit event", n, auditBefore)
	}
	if n := h.countEntities(t, x); n != 0 {
		t.Errorf("%d x entities committed; want none", n)
	}
}
