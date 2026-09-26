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

// joined_callback_conflict_test.go — a SYNC processor's joined callback
// updates a second entity F that a concurrent client committed a change to
// after the request's transaction began. The callback's write loses first-
// committer-wins, which is a transaction conflict: every door that ran the
// processor answers a retryable 409 CONFLICT. 412 ENTITY_MODIFIED belongs to
// the request's own ifMatch precondition only, and none is sent here.

// conflictDoorWorkflow runs the SYNC processors procs, in order, on three
// paths: at create when status is "create", on a loopback update when status
// is "loop", and on the manual transition "go". Any other status rests in OPEN.
func conflictDoorWorkflow(name, tag string, procs ...string) string {
	list := make([]string, len(procs))
	for i, proc := range procs {
		list[i] = fmt.Sprintf(`{"type": "calculator", "name": %q, "executionMode": "SYNC",
			"config": {"attachEntity": true, "calculationNodesTags": %q, "responseTimeoutMs": 60000}}`, proc, tag)
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

func TestJoinedCallbackConflict_EveryDoorAnswersRetryable409(t *testing.T) {
	doors := []struct {
		name string
		// request makes the client call that runs the processor.
		request func(h *callbackHarness, model, eID, eTxID string) (callbackResult, error)
		// preCreate reports whether the door needs E to exist first.
		preCreate bool
		// engineOnly limits the door to the InEngine shape.
		engineOnly bool
	}{
		{name: "Create", request: func(h *callbackHarness, model, _, _ string) (callbackResult, error) {
			return h.callback(http.MethodPost, "/api/entity/JSON/"+model+"/1", `{"name":"e","amount":1,"status":"create"}`, "")
		}},
		{name: "Update", preCreate: true, request: func(h *callbackHarness, _, eID, _ string) (callbackResult, error) {
			return h.callback(http.MethodPut, "/api/entity/JSON/"+eID, `{"name":"e","amount":1,"status":"loop"}`, "")
		}},
		// The request's own precondition holds: nobody wrote E. The conflict
		// is the callback's, so it is not ENTITY_MODIFIED either. Only the
		// engine shape: at the handler's save the If-Match compare is the
		// statement that meets the aborted transaction, and a store conflict
		// there does not say whether the precondition failed.
		{name: "UpdateWithIfMatch", preCreate: true, engineOnly: true, request: func(h *callbackHarness, _, eID, eTxID string) (callbackResult, error) {
			return h.putIfMatch("/api/entity/JSON/"+eID, eTxID, `{"name":"e","amount":1,"status":"loop"}`)
		}},
		{name: "ManualTransition", preCreate: true, request: func(h *callbackHarness, _, eID, _ string) (callbackResult, error) {
			return h.callback(http.MethodPut, "/api/entity/JSON/"+eID+"/go", `{"name":"e","amount":1,"status":"draft"}`, "")
		}},
	}
	// Where the aborted transaction is first noticed: with one processor, at
	// the handler's save; with a second, when the engine reads the entity
	// before dispatching it.
	shapes := []struct {
		name  string
		procs int
	}{{"AtSave", 1}, {"InEngine", 2}}
	for _, door := range doors {
		for _, shape := range shapes {
			if door.engineOnly && shape.procs == 1 {
				continue
			}
			t.Run(door.name+"/"+shape.name, func(t *testing.T) {
				h := newCalloutHarness(t, nil)
				fModel, model, tag := uniq("jcc-f"), uniq("jcc"), uniq("jcc-tag")
				procs := make([]string, shape.procs)
				for i := range procs {
					procs[i] = uniq("jcc-p")
				}
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
					return answerOK()
				}})
				h.SetupModelWithWorkflow(t, model, conflictDoorWorkflow(model, tag, procs...))

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
			})
		}
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
