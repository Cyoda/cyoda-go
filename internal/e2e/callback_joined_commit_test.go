package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// callback_joined_commit_test.go — a callback never commits the transaction it
// joined.
//
// A compute node's callback runs in the transaction of the operation that
// called it out. When the write it makes runs a workflow that reaches a
// COMMIT_BEFORE_DISPATCH processor — which commits the transaction it runs in —
// the write is refused with 409 COMMIT_IN_JOINED_TRANSACTION before anything is
// written, and the outer transaction stays open and uncommitted for its owner.
//
// Every scenario has the same shape. The outer operation's SYNC processor makes
// two callbacks in its transaction T: a plain create of a marker entity, then
// the write under test. It then fails on purpose, so the owner rolls T back.
// The marker is the witness: had the second callback committed T, the marker
// would have been committed with it and would still be readable after the
// rollback. The COMMIT_BEFORE_DISPATCH processor itself must never be
// dispatched.
//
// Covered: every entity write door a compute node can call back through — the
// four HTTP entity flows that run the workflow engine (create, create
// collection, update with a transition, update collection) and the two gRPC
// write doors (EntityManage, EntityManageCollection) — each with
// startNewTxOnDispatch false and true.

// joinedCommitWrite is one door's write, made from inside a callback under the
// callout's pass. It reports what the door answered, in a door-neutral form.
type joinedCommitWrite func(h *callbackHarness, rc *reqCtx, model, entityID string) joinedCommitOutcome

type joinedCommitOutcome struct {
	status    int    // HTTP status; 0 for a gRPC door
	errorCode string // problem-detail errorCode, or the gRPC envelope's domain-code prefix
	retryable bool
	detail    string
	err       error // transport failure
}

func httpJoinedOutcome(res callbackResult, err error) joinedCommitOutcome {
	if err != nil {
		return joinedCommitOutcome{err: err}
	}
	var pd problemDoc
	_ = json.Unmarshal([]byte(res.Body), &pd)
	code, _ := pd.Properties["errorCode"].(string)
	retryable, _ := pd.Properties["retryable"].(bool)
	return joinedCommitOutcome{status: res.StatusCode, errorCode: code, retryable: retryable, detail: res.Body}
}

func grpcJoinedOutcome(env txEnvelope, err error) joinedCommitOutcome {
	if err != nil {
		return joinedCommitOutcome{err: err}
	}
	if env.Success || env.Error == nil {
		return joinedCommitOutcome{errorCode: "", detail: "success"}
	}
	code, _, _ := strings.Cut(env.Error.Message, ":")
	return joinedCommitOutcome{
		errorCode: code,
		retryable: env.Error.Retryable != nil && *env.Error.Retryable,
		detail:    env.Error.Code + " " + env.Error.Message,
	}
}

func TestCallback_CommitBeforeDispatchInJoinedTransaction_Refused(t *testing.T) {
	h := newCallbackHarness(t)

	const payload = `{"name":"joined","amount":1,"status":"new"}`

	doors := []struct {
		name string
		// onTransition: the COMMIT_BEFORE_DISPATCH processor sits on a manual
		// transition of an entity that already exists, rather than on the
		// automated transition a create runs.
		onTransition bool
		write        joinedCommitWrite
	}{
		{"http-create", false, func(h *callbackHarness, rc *reqCtx, model, _ string) joinedCommitOutcome {
			return httpJoinedOutcome(rc.CreateEntity(model, 1, payload))
		}},
		{"http-create-collection", false, func(h *callbackHarness, rc *reqCtx, model, _ string) joinedCommitOutcome {
			return httpJoinedOutcome(h.callback(http.MethodPost, "/api/entity/JSON", collectionCreateItems(model, payload), rc.token))
		}},
		{"http-update-transition", true, func(h *callbackHarness, rc *reqCtx, _, id string) joinedCommitOutcome {
			return httpJoinedOutcome(h.callback(http.MethodPut, "/api/entity/JSON/"+id+"/go", payload, rc.token))
		}},
		{"http-update-collection", true, func(h *callbackHarness, rc *reqCtx, _, id string) joinedCommitOutcome {
			return httpJoinedOutcome(h.callback(http.MethodPut, "/api/entity/JSON", collectionUpdateItems(id, "go", payload), rc.token))
		}},
		{"grpc-create", false, func(h *callbackHarness, rc *reqCtx, model, _ string) joinedCommitOutcome {
			return grpcJoinedOutcome(h.ReplayCreateGRPC(rc.token, model, 1, payload))
		}},
		{"grpc-create-collection", false, func(h *callbackHarness, rc *reqCtx, model, _ string) joinedCommitOutcome {
			return grpcJoinedOutcome(h.ReplayCreateCollectionGRPC(rc.token, model, 1, payload))
		}},
	}

	for _, startNewTx := range []bool{false, true} {
		for _, door := range doors {
			t.Run(fmt.Sprintf("%s/startNewTxOnDispatch=%t", door.name, startNewTx), func(t *testing.T) {
				suffix := strings.ReplaceAll(fmt.Sprintf("%s-%t", door.name, startNewTx), "_", "-")
				outer := "jc-outer-" + suffix
				marker := "jc-marker-" + suffix
				target := "jc-target-" + suffix
				outerProc := "jc-outer-proc-" + suffix
				cbdProc := "jc-cbd-proc-" + suffix

				h.SetupModelWithWorkflow(t, marker, secondaryWorkflow)
				h.SetupModelWithWorkflow(t, target, joinedCommitTargetWorkflow(target, cbdProc, startNewTx, door.onTransition))
				targetID := ""
				if door.onTransition {
					targetID = createQuietEntity(t, h, target, payload)
				}

				var cbdDispatched atomic.Int32
				h.RegisterProc(cbdProc, func(*reqCtx) (map[string]any, error) {
					cbdDispatched.Add(1)
					return nil, nil
				})

				type observed struct {
					markerID  string
					markerOK  bool
					markerRes string
					outcome   joinedCommitOutcome
				}
				seen := make(chan observed, 1)
				h.RegisterProc(outerProc, func(rc *reqCtx) (map[string]any, error) {
					var o observed
					res, err := rc.CreateEntity(marker, 1, payload)
					o.markerOK = err == nil && res.StatusCode == http.StatusOK
					o.markerID, o.markerRes = res.EntityID, fmt.Sprintf("%d %s %v", res.StatusCode, res.Body, err)
					o.outcome = door.write(h, rc, target, targetID)
					seen <- o
					return nil, fmt.Errorf("outer processor fails on purpose so the owner rolls its transaction back")
				})
				h.SetupModelWithWorkflow(t, outer, joinedCommitOuterWorkflow(outer, outerProc))

				_, status, body := h.CreateEntity(t, outer, 1, payload)
				if status == http.StatusOK {
					t.Fatalf("outer create succeeded; its processor fails on purpose: %s", body)
				}

				var o observed
				select {
				case o = <-seen:
				case <-time.After(15 * time.Second):
					t.Fatal("timeout: the outer processor's callbacks never ran")
				}
				if !o.markerOK {
					t.Fatalf("marker create in the joined transaction failed: %s", o.markerRes)
				}

				// The write under test was refused with the domain error.
				if o.outcome.err != nil {
					t.Fatalf("transport error: %v", o.outcome.err)
				}
				if o.outcome.status != 0 && o.outcome.status != http.StatusConflict {
					t.Errorf("status = %d; want 409 (%s)", o.outcome.status, o.outcome.detail)
				}
				if o.outcome.errorCode != "COMMIT_IN_JOINED_TRANSACTION" {
					t.Errorf("error code = %q; want COMMIT_IN_JOINED_TRANSACTION (%s)", o.outcome.errorCode, o.outcome.detail)
				}
				if o.outcome.retryable {
					t.Errorf("refusal marked retryable (%s)", o.outcome.detail)
				}
				if !strings.Contains(o.outcome.detail, cbdProc) {
					t.Errorf("refusal does not name the processor %q: %s", cbdProc, o.outcome.detail)
				}
				if n := cbdDispatched.Load(); n != 0 {
					t.Errorf("the COMMIT_BEFORE_DISPATCH processor was dispatched %d time(s)", n)
				}

				// The witness: the callback did not commit T. The owner rolled
				// T back, and the marker went with it.
				if st, code := h.GetEntityState(t, o.markerID); code != http.StatusNotFound {
					t.Errorf("marker %s readable after the owner's rollback (http %d, state %q): the joined transaction was committed by the callback",
						o.markerID, code, st)
				}
				// And the target was not touched: an updated entity is still in
				// its pre-transition state.
				if door.onTransition {
					if st, code := h.GetEntityState(t, targetID); code != http.StatusOK || st != "READY" {
						t.Errorf("target state = %q (http %d); want READY, untouched", st, code)
					}
				}
			})
		}
	}
}

// joinedCommitOuterWorkflow runs one SYNC processor on the create's automated
// transition: the callout whose callbacks the scenario makes.
func joinedCommitOuterWorkflow(name, proc string) string {
	return fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": %q, "initialState": "NONE", "active": true,
			"states": {
				"NONE":   {"transitions": [{"name": "init", "next": "ACTIVE", "manual": false,
					"processors": [{"type": "calculator", "name": %q, "executionMode": "SYNC",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]}]},
				"ACTIVE": {}
			}
		}]
	}`, name+"-wf", proc)
}

// joinedCommitTargetWorkflow puts a COMMIT_BEFORE_DISPATCH processor either on
// the create's automated transition or on a manual transition "go" out of
// READY, which a create reaches with no processor.
func joinedCommitTargetWorkflow(name, proc string, startNewTx, onTransition bool) string {
	cbd := fmt.Sprintf(`"processors": [{"type": "calculator", "name": %q, "executionMode": "COMMIT_BEFORE_DISPATCH",
		"config": {"attachEntity": true, "calculationNodesTags": "", "startNewTxOnDispatch": %t}}]`, proc, startNewTx)
	states := fmt.Sprintf(`
		"NONE": {"transitions": [{"name": "init", "next": "DONE", "manual": false, %s}]},
		"DONE": {}`, cbd)
	if onTransition {
		states = fmt.Sprintf(`
		"NONE":  {"transitions": [{"name": "store", "next": "READY", "manual": false}]},
		"READY": {"transitions": [{"name": "go", "next": "DONE", "manual": true, %s}]},
		"DONE":  {}`, cbd)
	}
	return fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": %q, "initialState": "NONE", "active": true,
			"states": {%s}
		}]
	}`, name+"-wf", states)
}
