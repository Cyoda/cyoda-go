package e2e_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	cyodapb "github.com/cyoda-platform/cyoda-go/api/grpc/cyoda"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

// callback_joined_commit_test.go — a callback never commits the transaction it
// joined.
//
// A compute node's callback runs in the transaction of the operation that
// called it out. When the write it makes runs a workflow that reaches a
// COMMIT_BEFORE_DISPATCH processor — which commits the transaction it runs in —
// the write is refused with 409 COMMIT_IN_JOINED_TRANSACTION at that processor,
// before the joined transaction is flushed or committed and before the
// processor is dispatched. The joined transaction stays open for its owner.
//
// The refusal scenario: the outer operation's SYNC processor makes two
// callbacks in its transaction T — a plain create of a marker entity, then the
// write under test — and then fails on purpose, so the owner rolls T back. The
// marker is the witness: had the second callback committed T, the marker would
// have been committed with it and would still be readable after the rollback.
//
// The owner-commits scenario: the outer processor ignores the refusal and
// answers success, so the owner commits T. The marker is committed with it, and
// the refused write left nothing behind.
//
// Covered doors — every entity write a compute node can call back through that
// runs the workflow engine: HTTP create, create collection, update with a
// transition, loopback update, PATCH, update collection; gRPC EntityManage
// create, update (loopback), update with a transition, transition, and
// EntityManageCollection create. Each with startNewTxOnDispatch false and true.

// joinedCommitShape says where the target workflow's COMMIT_BEFORE_DISPATCH
// processor sits, which decides how a write reaches it.
type joinedCommitShape int

const (
	// shapeOnCreate: on the create's automated transition.
	shapeOnCreate joinedCommitShape = iota
	// shapeOnManual: on a manual transition "go" out of READY, which a create
	// reaches with no processor.
	shapeOnManual
	// shapeOnLoopback: on an automated transition out of READY guarded by
	// status == "go", so a create with another status rests in READY and a
	// loopback update setting it reaches the processor.
	shapeOnLoopback
)

// target is the entity a door acts on, when it acts on an existing one.
type joinedCommitTarget struct {
	model string // the target model, for every shape
	id    string // the existing entity, for shapes other than shapeOnCreate
	txID  string // the transaction id of its latest version: PATCH's If-Match
}

// joinedCommitWrite is one door's write, made from inside a callback under the
// callout's pass. It reports what the door answered, in a door-neutral form.
type joinedCommitWrite func(h *callbackHarness, rc *reqCtx, model string, target joinedCommitTarget) joinedCommitOutcome

type joinedCommitOutcome struct {
	status       int    // HTTP status; 0 for a gRPC door
	envelopeCode string // gRPC envelope class; "" for an HTTP door
	errorCode    string // problem-detail errorCode, or the gRPC envelope's domain-code prefix
	retryable    bool
	detail       string
	err          error // transport failure
}

const (
	joinedCommitPayload   = `{"name":"joined","amount":1,"status":"new"}`
	joinedCommitGoPayload = `{"name":"joined","amount":1,"status":"go"}`
)

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
		return joinedCommitOutcome{detail: "success"}
	}
	code, _, _ := strings.Cut(env.Error.Message, ":")
	return joinedCommitOutcome{
		envelopeCode: env.Error.Code,
		errorCode:    code,
		retryable:    env.Error.Retryable != nil && *env.Error.Retryable,
		detail:       env.Error.Code + " " + env.Error.Message,
	}
}

// joinedPatch is a joined PATCH: the callback door's merge-patch form, which
// h.callback cannot send (it always sets application/json).
func (h *callbackHarness) joinedPatch(entityID, ifMatch, patch, pass string) (callbackResult, error) {
	req, err := http.NewRequest(http.MethodPatch, h.baseURL+"/api/entity/JSON/"+entityID, strings.NewReader(patch))
	if err != nil {
		return callbackResult{}, err
	}
	tok, _ := h.bearerVal.Load().(string)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/merge-patch+json")
	req.Header.Set("If-Match", ifMatch)
	req.Header.Set("X-Tx-Token", pass)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return callbackResult{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return callbackResult{StatusCode: resp.StatusCode, Body: string(raw)}, nil
}

// joinedManageGRPC sends one EntityManage request of eventType under pass.
func (h *callbackHarness) joinedManageGRPC(eventType string, body map[string]any, pass string) (txEnvelope, error) {
	reqCE, err := internalgrpc.NewCloudEvent(eventType, body)
	if err != nil {
		return txEnvelope{}, err
	}
	respCE, err := cyodapb.NewCloudEventsServiceClient(h.apiConn).EntityManage(h.grpcCtx(pass), reqCE)
	if err != nil {
		return txEnvelope{}, err
	}
	return parseTxEnvelope(respCE)
}

func mustJSONMap(s string) map[string]any {
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		panic(err)
	}
	return m
}

type joinedCommitDoor struct {
	name  string
	shape joinedCommitShape
	write joinedCommitWrite
}

func joinedCommitDoors() []joinedCommitDoor {
	return []joinedCommitDoor{
		{"http-create", shapeOnCreate, func(h *callbackHarness, rc *reqCtx, model string, _ joinedCommitTarget) joinedCommitOutcome {
			return httpJoinedOutcome(rc.CreateEntity(model, 1, joinedCommitPayload))
		}},
		{"http-create-collection", shapeOnCreate, func(h *callbackHarness, rc *reqCtx, model string, _ joinedCommitTarget) joinedCommitOutcome {
			return httpJoinedOutcome(h.callback(http.MethodPost, "/api/entity/JSON", collectionCreateItems(model, joinedCommitPayload), rc.token))
		}},
		{"http-update-transition", shapeOnManual, func(h *callbackHarness, rc *reqCtx, _ string, tg joinedCommitTarget) joinedCommitOutcome {
			return httpJoinedOutcome(h.callback(http.MethodPut, "/api/entity/JSON/"+tg.id+"/go", joinedCommitPayload, rc.token))
		}},
		{"http-update-loopback", shapeOnLoopback, func(h *callbackHarness, rc *reqCtx, _ string, tg joinedCommitTarget) joinedCommitOutcome {
			return httpJoinedOutcome(h.callback(http.MethodPut, "/api/entity/JSON/"+tg.id, joinedCommitGoPayload, rc.token))
		}},
		{"http-patch", shapeOnLoopback, func(h *callbackHarness, rc *reqCtx, _ string, tg joinedCommitTarget) joinedCommitOutcome {
			return httpJoinedOutcome(h.joinedPatch(tg.id, tg.txID, `{"status":"go"}`, rc.token))
		}},
		{"http-update-collection", shapeOnManual, func(h *callbackHarness, rc *reqCtx, _ string, tg joinedCommitTarget) joinedCommitOutcome {
			return httpJoinedOutcome(h.callback(http.MethodPut, "/api/entity/JSON", collectionUpdateItems(tg.id, "go", joinedCommitPayload), rc.token))
		}},
		{"grpc-create", shapeOnCreate, func(h *callbackHarness, rc *reqCtx, model string, _ joinedCommitTarget) joinedCommitOutcome {
			return grpcJoinedOutcome(h.ReplayCreateGRPC(rc.token, model, 1, joinedCommitPayload))
		}},
		{"grpc-update-loopback", shapeOnLoopback, func(h *callbackHarness, rc *reqCtx, _ string, tg joinedCommitTarget) joinedCommitOutcome {
			return grpcJoinedOutcome(h.joinedManageGRPC(internalgrpc.EntityUpdateRequest, map[string]any{
				"id": "jc-update", "dataFormat": "JSON",
				"payload": map[string]any{"entityId": tg.id, "data": mustJSONMap(joinedCommitGoPayload)},
			}, rc.token))
		}},
		{"grpc-update-transition", shapeOnManual, func(h *callbackHarness, rc *reqCtx, _ string, tg joinedCommitTarget) joinedCommitOutcome {
			return grpcJoinedOutcome(h.joinedManageGRPC(internalgrpc.EntityUpdateRequest, map[string]any{
				"id": "jc-update-transition", "dataFormat": "JSON",
				"payload": map[string]any{"entityId": tg.id, "transition": "go", "data": mustJSONMap(joinedCommitPayload)},
			}, rc.token))
		}},
		{"grpc-transition", shapeOnManual, func(h *callbackHarness, rc *reqCtx, _ string, tg joinedCommitTarget) joinedCommitOutcome {
			return grpcJoinedOutcome(h.joinedManageGRPC(internalgrpc.EntityTransitionRequest, map[string]any{
				"id": "jc-transition", "entityId": tg.id, "transition": "go",
			}, rc.token))
		}},
		{"grpc-create-collection", shapeOnCreate, func(h *callbackHarness, rc *reqCtx, model string, _ joinedCommitTarget) joinedCommitOutcome {
			return grpcJoinedOutcome(h.ReplayCreateCollectionGRPC(rc.token, model, 1, joinedCommitPayload))
		}},
	}
}

// joinedCommitCase stands up one scenario's models and processors, runs the
// outer create, and returns what the outer processor's callbacks observed.
type joinedCommitObserved struct {
	markerID  string
	markerOK  bool
	markerRes string
	outcome   joinedCommitOutcome
}

func runJoinedCommitCase(t *testing.T, h *callbackHarness, door joinedCommitDoor, startNewTx, ownerCommits bool) (observed joinedCommitObserved, outerStatus int, outerBody string, target joinedCommitTarget, cbdDispatched int32) {
	t.Helper()
	suffix := fmt.Sprintf("%s-%t-%t", door.name, startNewTx, ownerCommits)
	outer := "jc-outer-" + suffix
	marker := "jc-marker-" + suffix
	targetModel := "jc-target-" + suffix
	outerProc := "jc-outer-proc-" + suffix
	cbdProc := "jc-cbd-proc-" + suffix

	h.SetupModelWithWorkflow(t, marker, secondaryWorkflow)
	h.SetupModelWithWorkflow(t, targetModel, joinedCommitTargetWorkflow(targetModel, cbdProc, startNewTx, door.shape))
	target.model = targetModel
	if door.shape != shapeOnCreate {
		id, status, body := h.CreateEntity(t, targetModel, 1, joinedCommitPayload)
		if status != http.StatusOK {
			t.Fatalf("create the target entity: %d %s", status, body)
		}
		var arr []map[string]any
		_ = json.Unmarshal([]byte(body), &arr)
		txID, _ := arr[0]["transactionId"].(string)
		target.id, target.txID = id, txID
	}

	var dispatched atomic.Int32
	h.RegisterProc(cbdProc, func(*reqCtx) (map[string]any, error) {
		dispatched.Add(1)
		return nil, nil
	})

	seen := make(chan joinedCommitObserved, 1)
	h.RegisterProc(outerProc, func(rc *reqCtx) (map[string]any, error) {
		var o joinedCommitObserved
		res, err := rc.CreateEntity(marker, 1, joinedCommitPayload)
		o.markerOK = err == nil && res.StatusCode == http.StatusOK
		o.markerID, o.markerRes = res.EntityID, fmt.Sprintf("%d %s %v", res.StatusCode, res.Body, err)
		o.outcome = door.write(h, rc, targetModel, target)
		seen <- o
		if ownerCommits {
			return nil, nil // ignore the refusal: the owner commits T
		}
		return nil, fmt.Errorf("outer processor fails on purpose so the owner rolls its transaction back")
	})
	h.SetupModelWithWorkflow(t, outer, joinedCommitOuterWorkflow(outer, outerProc))

	_, outerStatus, outerBody = h.CreateEntity(t, outer, 1, joinedCommitPayload)

	select {
	case observed = <-seen:
	case <-time.After(15 * time.Second):
		t.Fatal("timeout: the outer processor's callbacks never ran")
	}
	if !observed.markerOK {
		t.Fatalf("marker create in the joined transaction failed: %s", observed.markerRes)
	}
	return observed, outerStatus, outerBody, target, dispatched.Load()
}

// assertJoinedCommitRefused checks the write under test was refused with the
// domain error, on either kind of door, and that the processor never ran.
func assertJoinedCommitRefused(t *testing.T, o joinedCommitOutcome, cbdProc string, cbdDispatched int32) {
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
	if o.errorCode != "COMMIT_IN_JOINED_TRANSACTION" {
		t.Errorf("error code = %q; want COMMIT_IN_JOINED_TRANSACTION (%s)", o.errorCode, o.detail)
	}
	if o.retryable {
		t.Errorf("refusal marked retryable (%s)", o.detail)
	}
	if !strings.Contains(o.detail, cbdProc) {
		t.Errorf("refusal does not name the processor %q: %s", cbdProc, o.detail)
	}
	if cbdDispatched != 0 {
		t.Errorf("the COMMIT_BEFORE_DISPATCH processor was dispatched %d time(s)", cbdDispatched)
	}
}

// assertTargetUntouched checks the refused write left nothing behind: a create
// stored no target entity, and an existing target is still in its
// pre-transition state with its original data.
func assertTargetUntouched(t *testing.T, h *callbackHarness, door joinedCommitDoor, target joinedCommitTarget) {
	t.Helper()
	if door.shape == shapeOnCreate {
		if n := h.countEntities(t, target.model); n != 0 {
			t.Errorf("%d target entities stored by a refused create; want none", n)
		}
		return
	}
	if st, code := h.GetEntityState(t, target.id); code != http.StatusOK || st != "READY" {
		t.Errorf("target state = %q (http %d); want READY, untouched", st, code)
	}
	if got := h.GetEntityData(t, target.id)["status"]; got != "new" {
		t.Errorf("target status = %v; want \"new\", untouched", got)
	}
}

func TestCallback_CommitBeforeDispatchInJoinedTransaction_Refused(t *testing.T) {
	h := newCallbackHarness(t)
	for _, startNewTx := range []bool{false, true} {
		for _, door := range joinedCommitDoors() {
			t.Run(fmt.Sprintf("%s/startNewTxOnDispatch=%t", door.name, startNewTx), func(t *testing.T) {
				o, outerStatus, outerBody, target, dispatched := runJoinedCommitCase(t, h, door, startNewTx, false)
				if outerStatus == http.StatusOK {
					t.Fatalf("outer create succeeded; its processor fails on purpose: %s", outerBody)
				}
				assertJoinedCommitRefused(t, o.outcome, fmt.Sprintf("jc-cbd-proc-%s-%t-false", door.name, startNewTx), dispatched)

				// The witness: the callback did not commit T. The owner rolled
				// T back, and the marker went with it.
				if st, code := h.GetEntityState(t, o.markerID); code != http.StatusNotFound {
					t.Errorf("marker %s readable after the owner's rollback (http %d, state %q): the joined transaction was committed by the callback",
						o.markerID, code, st)
				}
				assertTargetUntouched(t, h, door, target)
			})
		}
	}
}

// TestCallback_CommitBeforeDispatchInJoinedTransaction_OwnerStillCommits: the
// refusal leaves the joined transaction to its owner. An outer processor that
// ignores the 409 and answers success lets the owner commit T: the outer entity
// completes its transition, the marker written in T is committed with it, and
// the refused write left nothing behind.
func TestCallback_CommitBeforeDispatchInJoinedTransaction_OwnerStillCommits(t *testing.T) {
	h := newCallbackHarness(t)
	for _, startNewTx := range []bool{false, true} {
		for _, door := range joinedCommitDoors() {
			t.Run(fmt.Sprintf("%s/startNewTxOnDispatch=%t", door.name, startNewTx), func(t *testing.T) {
				o, outerStatus, outerBody, target, dispatched := runJoinedCommitCase(t, h, door, startNewTx, true)
				if outerStatus != http.StatusOK {
					t.Fatalf("outer create: %d %s; want 200, the owner commits T", outerStatus, outerBody)
				}
				assertJoinedCommitRefused(t, o.outcome, fmt.Sprintf("jc-cbd-proc-%s-%t-true", door.name, startNewTx), dispatched)

				if st, code := h.GetEntityState(t, o.markerID); code != http.StatusOK || st != "STORED" {
					t.Errorf("marker state = %q (http %d); want STORED, committed with T by its owner", st, code)
				}
				assertTargetUntouched(t, h, door, target)
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

// joinedCommitTargetWorkflow places a COMMIT_BEFORE_DISPATCH processor as shape
// says.
func joinedCommitTargetWorkflow(name, proc string, startNewTx bool, shape joinedCommitShape) string {
	cbd := fmt.Sprintf(`"processors": [{"type": "calculator", "name": %q, "executionMode": "COMMIT_BEFORE_DISPATCH",
		"config": {"attachEntity": true, "calculationNodesTags": "", "startNewTxOnDispatch": %t}}]`, proc, startNewTx)
	var states string
	switch shape {
	case shapeOnCreate:
		states = fmt.Sprintf(`
		"NONE": {"transitions": [{"name": "init", "next": "DONE", "manual": false, %s}]},
		"DONE": {}`, cbd)
	case shapeOnManual:
		states = fmt.Sprintf(`
		"NONE":  {"transitions": [{"name": "store", "next": "READY", "manual": false}]},
		"READY": {"transitions": [{"name": "go", "next": "DONE", "manual": true, %s}]},
		"DONE":  {}`, cbd)
	case shapeOnLoopback:
		states = fmt.Sprintf(`
		"NONE":  {"transitions": [{"name": "store", "next": "READY", "manual": false}]},
		"READY": {"transitions": [{"name": "go", "next": "DONE", "manual": false,
			"criterion": {"type": "simple", "jsonPath": "$.status", "operatorType": "EQUALS", "value": "go"}, %s}]},
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
