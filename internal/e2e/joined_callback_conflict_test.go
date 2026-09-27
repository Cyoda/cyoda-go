package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// joined_callback_conflict_test.go — a processor's joined callback (SYNC,
// ASYNC_NEW_TX, or COMMIT_BEFORE_DISPATCH on its new transaction) updates a
// second entity F that a concurrent client committed a change to
// after the request's transaction began. The callback's write loses first-
// committer-wins, which is a transaction conflict: every door that ran the
// processor answers a retryable 409 CONFLICT. 412 ENTITY_MODIFIED belongs to
// the request's own If-Match precondition only, and that precondition holds
// here: nobody else wrote E.

// conflictProc is one processor of the doors' workflow. startNewTx sets
// startNewTxOnDispatch, which gives a COMMIT_BEFORE_DISPATCH dispatch the new
// transaction's token.
type conflictProc struct {
	name, mode string
	startNewTx bool
}

// conflictDoorWorkflow runs procs, in order, on three paths: at create when
// status is "create", on a loopback update when status is "loop", and on the
// manual transition "go". Any other status rests in OPEN.
func conflictDoorWorkflow(name, tag string, procs ...conflictProc) string {
	list := make([]string, len(procs))
	for i, proc := range procs {
		list[i] = fmt.Sprintf(`{"type": "calculator", "name": %q, "executionMode": %q,
			"config": {"attachEntity": true, "calculationNodesTags": %q, "responseTimeoutMs": 60000, "startNewTxOnDispatch": %t}}`,
			proc.name, proc.mode, tag, proc.startNewTx)
	}
	p := `"processors": [` + strings.Join(list, ",") + `]`
	when := func(status string) string {
		return fmt.Sprintf(`"criterion": {"type": "simple", "jsonPath": "$.status", "operatorType": "EQUALS", "value": %q}`, status)
	}
	return fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": %q, "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "OPEN", "manual": false, %s, %s},
				                         {"name": "rest", "next": "OPEN", "manual": false}]},
				"OPEN": {"transitions": [{"name": "loop", "next": "DONE", "manual": false, %s, %s},
				                         {"name": "go", "next": "DONE", "manual": true, %s}]},
				"DONE": {}
			}
		}]
	}`, name+"-wf", when("create"), p, when("loop"), p, p)
}

// collectionUpdateBody is a PUT /entity/JSON body updating id with payload,
// carrying ifMatch when it is non-empty.
func collectionUpdateBody(id, ifMatch, payload string) string {
	item := map[string]any{"id": id, "payload": payload}
	if ifMatch != "" {
		item["ifMatch"] = ifMatch
	}
	b, _ := json.Marshal([]any{item})
	return string(b)
}

func TestJoinedCallbackConflict_EveryDoorAnswersRetryable409(t *testing.T) {
	const loop = `{"name":"e","amount":1,"status":"loop"}`
	doors := []struct {
		name string
		// request makes the client call that runs the processor.
		request func(h *callbackHarness, model, eID, eTxID string) (callbackResult, error)
		// preCreate reports whether the door needs E to exist first.
		preCreate bool
	}{
		{name: "Create", request: func(h *callbackHarness, model, _, _ string) (callbackResult, error) {
			return h.callback(http.MethodPost, "/api/entity/JSON/"+model+"/1", `{"name":"e","amount":1,"status":"create"}`, "")
		}},
		{name: "Update", preCreate: true, request: func(h *callbackHarness, _, eID, _ string) (callbackResult, error) {
			return h.callback(http.MethodPut, "/api/entity/JSON/"+eID, loop, "")
		}},
		{name: "UpdateWithIfMatch", preCreate: true, request: func(h *callbackHarness, _, eID, eTxID string) (callbackResult, error) {
			return h.putIfMatch("/api/entity/JSON/"+eID, eTxID, loop)
		}},
		{name: "ManualTransition", preCreate: true, request: func(h *callbackHarness, _, eID, _ string) (callbackResult, error) {
			return h.callback(http.MethodPut, "/api/entity/JSON/"+eID+"/go", `{"name":"e","amount":1,"status":"draft"}`, "")
		}},
		// The whole request answers 409: its transaction is gone, so no item
		// is isolated.
		{name: "Collection", preCreate: true, request: func(h *callbackHarness, _, eID, _ string) (callbackResult, error) {
			return h.callback(http.MethodPut, "/api/entity/JSON", collectionUpdateBody(eID, "", loop), "")
		}},
		{name: "CollectionWithIfMatch", preCreate: true, request: func(h *callbackHarness, _, eID, eTxID string) (callbackResult, error) {
			return h.callback(http.MethodPut, "/api/entity/JSON", collectionUpdateBody(eID, eTxID, loop), "")
		}},
	}
	sync := func(name string) conflictProc { return conflictProc{name: name, mode: "SYNC"} }
	// Where the aborted transaction is first noticed.
	shapes := []struct {
		name string
		// procs builds the pipeline; the first processor makes the joined write.
		procs func() []conflictProc
		// strict: the processor fails when its joined write is not answered 200.
		strict bool
		// thenFails: the processor fails after its joined write, whatever the
		// write was answered.
		thenFails bool
		// atomic: the request runs in one transaction, so the conflict leaves
		// nothing committed.
		atomic bool
	}{
		// The handler's save.
		{name: "AtSave", atomic: true, procs: func() []conflictProc { return []conflictProc{sync(uniq("jcc-p"))} }},
		// The engine's read of the entity before dispatching a second processor.
		{name: "InEngine", atomic: true, procs: func() []conflictProc { return []conflictProc{sync(uniq("jcc-p")), sync(uniq("jcc-p"))} }},
		// The processor checks its callback's answer and fails: the engine
		// asks the transaction manager whether the transaction lost a race.
		{name: "StrictProcessor", atomic: true, strict: true, procs: func() []conflictProc { return []conflictProc{sync(uniq("jcc-p"))} }},
		// The same, dispatched with the token of the transaction that
		// COMMIT_BEFORE_DISPATCH opens after its commit: the engine's question
		// about that transaction.
		{name: "StrictCommitBeforeDispatch", strict: true, procs: func() []conflictProc {
			return []conflictProc{{name: uniq("jcc-cbd"), mode: "COMMIT_BEFORE_DISPATCH", startNewTx: true}}
		}},
		// The same, inside ASYNC_NEW_TX's savepoint: the processor's failure
		// is not fatal and the savepoint is rolled back, and the conflict
		// still refuses the commit.
		{name: "StrictAsyncNewTx", atomic: true, strict: true, procs: func() []conflictProc {
			return []conflictProc{{name: uniq("jcc-ant"), mode: "ASYNC_NEW_TX"}}
		}},
		// The processor fails after its joined write whatever the answer: the
		// failure is a consequence of the lost race, and the engine's question
		// to the transaction manager answers the conflict.
		{name: "ThenFailsProcessor", atomic: true, thenFails: true, procs: func() []conflictProc { return []conflictProc{sync(uniq("jcc-p"))} }},
		// The same in the transaction COMMIT_BEFORE_DISPATCH opens after its
		// commit.
		{name: "ThenFailsCommitBeforeDispatch", thenFails: true, procs: func() []conflictProc {
			return []conflictProc{{name: uniq("jcc-cbd"), mode: "COMMIT_BEFORE_DISPATCH", startNewTx: true}}
		}},
		// The first COMMIT_BEFORE_DISPATCH segment's flush and commit, after
		// a SYNC processor's joined write lost the race.
		{name: "Segmented", procs: func() []conflictProc {
			return []conflictProc{sync(uniq("jcc-p")), {name: uniq("jcc-cbd"), mode: "COMMIT_BEFORE_DISPATCH"}}
		}},
	}
	for _, door := range doors {
		for _, shape := range shapes {
			t.Run(door.name+"/"+shape.name, func(t *testing.T) {
				h := newCalloutHarness(t, nil)
				fModel, model, tag := uniq("jcc-f"), uniq("jcc"), uniq("jcc-tag")
				h.SetupModelWithWorkflow(t, fModel, conflictDoorWorkflow(fModel, uniq("jcc-unused-tag")))
				fID := createOpen(t, h, fModel, workflowSampleModel)

				gotWork, release := make(chan struct{}, 1), make(chan struct{})
				rel := closeOnce(release)
				t.Cleanup(rel)
				callbackStatus := make(chan int, 1)
				var calls atomic.Int32
				h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{tag}, script: func(ctx context.Context, _ receivedCallout, rc *reqCtx) cnodeReply {
					if calls.Add(1) > 1 {
						return answerOK()
					}
					gotWork <- struct{}{}
					select {
					case <-release:
					case <-ctx.Done():
						return neverAnswer()
					}
					res, err := rc.UpdateEntity(fID, `{"name":"Test Order","amount":2,"status":"draft"}`)
					if err == nil {
						callbackStatus <- res.StatusCode
					}
					if shape.strict && (err != nil || res.StatusCode != http.StatusOK) {
						return answerFail("the joined write of F was refused")
					}
					if shape.thenFails {
						return answerFail("the processor fails after its joined write")
					}
					return answerOK()
				}})
				h.SetupModelWithWorkflow(t, model, conflictDoorWorkflow(model, tag, shape.procs()...))

				var eID, eTxID string
				if door.preCreate {
					eID, eTxID = createWithTx(t, h, model, workflowSampleModel)
				}

				done := make(chan joinedCommitOutcome, 1)
				go func() { done <- httpJoinedOutcome(door.request(h, model, eID, eTxID)) }()

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

				var out joinedCommitOutcome
				select {
				case out = <-done:
				case <-time.After(60 * time.Second):
					t.Fatal("the request never answered")
				}
				if out.err != nil {
					t.Fatalf("request: %v", out.err)
				}
				select {
				case st := <-callbackStatus:
					t.Logf("joined callback answered %d", st)
				default:
				}
				if out.status != http.StatusConflict || out.errorCode != "CONFLICT" || !out.retryable {
					t.Fatalf("answered %d %s retryable=%v; want a retryable 409 CONFLICT: %s",
						out.status, out.errorCode, out.retryable, out.detail)
				}
				if shape.atomic {
					requireNothingCommitted(t, h, model, eID, eTxID, fID)
				}
			})
		}
	}
}

// requireNothingCommitted asserts that a conflicted request committed none of
// its work: E is still OPEN at the version eTxID names (or, when the request
// created E, the model has no entity), and F holds the concurrent client's
// write, not the callback's.
func requireNothingCommitted(t *testing.T, h *callbackHarness, model, eID, eTxID, fID string) {
	t.Helper()
	if eID == "" {
		resp := h.DoAuth(t, http.MethodGet, "/api/entity/"+model+"/1", "", "")
		body := h.readBody(t, resp)
		var list []any
		if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &list) != nil || len(list) != 0 {
			t.Fatalf("the conflicted create committed an entity: %d %s", resp.StatusCode, body)
		}
	} else {
		resp := h.DoAuth(t, http.MethodGet, "/api/entity/"+eID, "", "")
		body := h.readBody(t, resp)
		var env struct {
			Meta struct {
				State         string `json:"state"`
				TransactionID string `json:"transactionId"`
			} `json:"meta"`
		}
		if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &env) != nil {
			t.Fatalf("read E: %d %s", resp.StatusCode, body)
		}
		if env.Meta.State != "OPEN" || env.Meta.TransactionID != eTxID {
			t.Fatalf("E is %s at %s; the conflicted request must leave it OPEN at %s", env.Meta.State, env.Meta.TransactionID, eTxID)
		}
	}
	if amount := h.GetEntityData(t, fID)["amount"]; amount != float64(1) {
		t.Fatalf("F has amount %v; the callback's write (2) must not commit over the client's (1)", amount)
	}
}

// createWithTx creates an entity and returns its id and the transaction id of
// the version created, which is what an If-Match names.
func createWithTx(t *testing.T, h *callbackHarness, model, payload string) (id, txID string) {
	t.Helper()
	h.token(t)
	res, err := h.callback(http.MethodPost, "/api/entity/JSON/"+model+"/1", payload, "")
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("create: %v %d %s", err, res.StatusCode, res.Body)
	}
	var arr []map[string]any
	if err := json.Unmarshal([]byte(res.Body), &arr); err != nil || len(arr) == 0 {
		t.Fatalf("create: unparseable response %s", res.Body)
	}
	ids, _ := arr[0]["entityIds"].([]any)
	txID, _ = arr[0]["transactionId"].(string)
	if len(ids) != 1 || txID == "" {
		t.Fatalf("create: no id or transaction id in %s", res.Body)
	}
	id, _ = ids[0].(string)
	return id, txID
}

// putIfMatch is a client PUT carrying an If-Match precondition.
func (h *callbackHarness) putIfMatch(path, ifMatch, body string) (callbackResult, error) {
	req, err := http.NewRequest(http.MethodPut, h.baseURL+path, strings.NewReader(body))
	if err != nil {
		return callbackResult{}, err
	}
	tok, _ := h.bearerVal.Load().(string)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", ifMatch)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return callbackResult{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return callbackResult{StatusCode: resp.StatusCode, Body: string(raw)}, nil
}
