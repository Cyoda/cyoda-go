package e2e_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

// ifmatch_callback_self_write_test.go — every door that takes If-Match, with
// If-Match naming E's current version, runs a processor whose joined callback
// writes E itself. If-Match states the version the request starts from, so the
// callback's write does not fail it: the request succeeds and E keeps the
// callback's write.

// ifMatchDoor is one door that takes If-Match, run on E with the If-Match
// eTxID. Its request moves E through the workflow's processors.
type ifMatchDoor struct {
	name    string
	request func(h *callbackHarness, eID, eTxID string) joinedCommitOutcome
}

const ifMatchLoop = `{"name":"e","amount":1,"status":"loop"}`

// ifMatchDoors is every door that takes If-Match. A collection item's
// precondition failure, reported inside a 200 as failed[], reads as 412.
func ifMatchDoors() []ifMatchDoor {
	const loop = ifMatchLoop
	return []ifMatchDoor{
		{name: "Update", request: func(h *callbackHarness, eID, eTxID string) joinedCommitOutcome {
			return httpJoinedOutcome(h.putIfMatch("/api/entity/JSON/"+eID, eTxID, loop))
		}},
		{name: "ManualTransition", request: func(h *callbackHarness, eID, eTxID string) joinedCommitOutcome {
			return httpJoinedOutcome(h.putIfMatch("/api/entity/JSON/"+eID+"/go", eTxID, `{"name":"e","amount":1,"status":"draft"}`))
		}},
		{name: "Patch", request: func(h *callbackHarness, eID, eTxID string) joinedCommitOutcome {
			return httpJoinedOutcome(h.joinedPatch(eID, eTxID, `{"status":"loop"}`, ""))
		}},
		{name: "Collection", request: func(h *callbackHarness, eID, eTxID string) joinedCommitOutcome {
			out := httpJoinedOutcome(h.callback(http.MethodPut, "/api/entity/JSON", collectionUpdateBody(eID, eTxID, loop), ""))
			// A per-item precondition failure is reported inside a 200.
			var chunks []struct {
				Failed []any `json:"failed"`
			}
			if out.status == http.StatusOK && json.Unmarshal([]byte(out.detail), &chunks) == nil {
				for _, c := range chunks {
					if len(c.Failed) > 0 {
						out.status, out.errorCode = http.StatusPreconditionFailed, "failed[]"
					}
				}
			}
			return out
		}},
		{name: "GRPCPatch", request: func(h *callbackHarness, eID, eTxID string) joinedCommitOutcome {
			return grpcJoinedOutcome(h.joinedManageGRPC(internalgrpc.EntityPatchRequest, map[string]any{
				"id": "ifm-self-patch", "patchFormat": "MERGE_PATCH",
				"payload": map[string]any{"entityId": eID, "ifMatch": eTxID, "patch": map[string]any{"status": "loop"}},
			}, ""))
		}},
	}
}

func TestIfMatch_OwnEntityCallbackWriteIsKept(t *testing.T) {
	doors := ifMatchDoors()
	shapes := []struct {
		name  string
		procs []conflictProc
	}{
		{name: "SYNC", procs: []conflictProc{{name: "ifm-self-p", mode: "SYNC"}}},
		{name: "ASYNC_SAME_TX", procs: []conflictProc{{name: "ifm-self-p", mode: "ASYNC_SAME_TX"}}},
		// The callback's write lands before a COMMIT_BEFORE_DISPATCH segment
		// commits.
		{name: "SYNC_ThenCommitBeforeDispatch", procs: []conflictProc{
			{name: "ifm-self-p", mode: "SYNC"}, {name: "ifm-self-cbd", mode: "COMMIT_BEFORE_DISPATCH"},
		}},
	}
	for _, door := range doors {
		for _, shape := range shapes {
			t.Run(door.name+"/"+shape.name, func(t *testing.T) {
				h := newCalloutHarness(t, nil)
				model, tag := uniq("ifm-self"), uniq("ifm-self-tag")
				var calls atomic.Int32
				callbackStatus := make(chan int, 1)
				h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{tag}, script: func(_ context.Context, _ receivedCallout, rc *reqCtx) cnodeReply {
					if calls.Add(1) > 1 {
						return answerOK()
					}
					// The joined write of E itself. Its status matches no
					// transition, so its own loopback leaves E in OPEN.
					res, err := rc.UpdateEntity(rc.entityID, `{"name":"e","amount":7,"status":"self"}`)
					if err != nil || res.StatusCode != http.StatusOK {
						callbackStatus <- res.StatusCode
						return answerFail("the joined write of E was refused")
					}
					return answerOK()
				}})
				h.SetupModelWithWorkflow(t, model, conflictDoorWorkflow(model, tag, shape.procs...))
				eID, eTxID := createWithTx(t, h, model, workflowSampleModel)

				out := door.request(h, eID, eTxID)
				if out.err != nil {
					t.Fatalf("request: %v", out.err)
				}
				select {
				case st := <-callbackStatus:
					t.Fatalf("the joined write of E answered %d", st)
				default:
				}
				if (out.status != 0 && out.status != http.StatusOK) || out.errorCode != "" {
					t.Fatalf("answered %d %s; want success: %s", out.status, out.errorCode, out.detail)
				}
				if st, _ := h.GetEntityState(t, eID); st != "DONE" {
					t.Fatalf("E is %s; want DONE", st)
				}
				if amount := h.GetEntityData(t, eID)["amount"]; amount != float64(7) {
					t.Fatalf("E has amount %v; want the callback's write (7)", amount)
				}
			})
		}
	}
}

// TestIfMatch_ChangeAfterReadIs409 — another client commits a change to E
// after the request read it (the processor makes that write itself, without
// the transaction token). The request's If-Match held when it started, so
// this is a lost race, not a failed precondition: a retryable 409 CONFLICT,
// and E keeps the other client's write. A collection item is not isolated:
// the transaction the chunk runs in cannot commit.
func TestIfMatch_ChangeAfterReadIs409(t *testing.T) {
	shapes := []struct {
		name  string
		procs []conflictProc
	}{
		// The rival commits before the request's final save.
		{name: "SYNC", procs: []conflictProc{{name: "ifm-rival-p", mode: "SYNC"}}},
		// The rival commits before TX_pre's flush and commit.
		{name: "SYNC_ThenCommitBeforeDispatch", procs: []conflictProc{
			{name: "ifm-rival-p", mode: "SYNC"}, {name: "ifm-rival-cbd", mode: "COMMIT_BEFORE_DISPATCH"},
		}},
	}
	for _, door := range ifMatchDoors() {
		for _, shape := range shapes {
			t.Run(door.name+"/"+shape.name, func(t *testing.T) {
				h := newCalloutHarness(t, nil)
				model, tag := uniq("ifm-rival"), uniq("ifm-rival-tag")
				var calls atomic.Int32
				rivalStatus := make(chan int, 1)
				h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{tag}, script: func(_ context.Context, _ receivedCallout, rc *reqCtx) cnodeReply {
					if calls.Add(1) > 1 {
						return answerOK()
					}
					// The other client's write: no transaction token, so it
					// commits at once. Its status matches no transition.
					res, err := h.callback(http.MethodPut, "/api/entity/JSON/"+rc.entityID, `{"name":"e","amount":5,"status":"rival"}`, "")
					if err != nil || res.StatusCode != http.StatusOK {
						rivalStatus <- res.StatusCode
					}
					return answerOK()
				}})
				h.SetupModelWithWorkflow(t, model, conflictDoorWorkflow(model, tag, shape.procs...))
				eID, eTxID := createWithTx(t, h, model, workflowSampleModel)

				out := door.request(h, eID, eTxID)
				if out.err != nil {
					t.Fatalf("request: %v", out.err)
				}
				select {
				case st := <-rivalStatus:
					t.Fatalf("the other client's write answered %d", st)
				default:
				}
				if (out.status != 0 && out.status != http.StatusConflict) || out.errorCode != "CONFLICT" || !out.retryable {
					t.Fatalf("answered %d %s retryable=%v; want a retryable 409 CONFLICT: %s", out.status, out.errorCode, out.retryable, out.detail)
				}
				if n := calls.Load(); n != 1 {
					t.Fatalf("%d callouts fired; want only the SYNC processor's (the conflicted segment must not dispatch)", n)
				}
				if st, _ := h.GetEntityState(t, eID); st != "OPEN" {
					t.Fatalf("E is %s; the conflicted request must leave it OPEN", st)
				}
				if amount := h.GetEntityData(t, eID)["amount"]; amount != float64(5) {
					t.Fatalf("E has amount %v; want the other client's write (5)", amount)
				}
			})
		}
	}
}

// TestIfMatch_StaleIs412 — another client changed E before the request read
// it: the request's own precondition fails, 412 ENTITY_MODIFIED, and E keeps
// the other client's write. No processor runs.
func TestIfMatch_StaleIs412(t *testing.T) {
	for _, door := range ifMatchDoors() {
		t.Run(door.name, func(t *testing.T) {
			h := newCalloutHarness(t, nil)
			model, tag := uniq("ifm-stale"), uniq("ifm-stale-tag")
			var calls atomic.Int32
			h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{tag}, script: func(context.Context, receivedCallout, *reqCtx) cnodeReply {
				calls.Add(1)
				return answerOK()
			}})
			h.SetupModelWithWorkflow(t, model, conflictDoorWorkflow(model, tag, conflictProc{name: "ifm-stale-p", mode: "SYNC"}))
			eID, eTxID := createWithTx(t, h, model, workflowSampleModel)
			if res, err := h.callback(http.MethodPut, "/api/entity/JSON/"+eID, `{"name":"e","amount":5,"status":"other"}`, ""); err != nil || res.StatusCode != http.StatusOK {
				t.Fatalf("the other client's write: %v %d", err, res.StatusCode)
			}

			out := door.request(h, eID, eTxID)
			if out.err != nil {
				t.Fatalf("request: %v", out.err)
			}
			if (out.status != 0 && out.status != http.StatusPreconditionFailed) || (out.errorCode != "ENTITY_MODIFIED" && out.errorCode != "failed[]") {
				t.Fatalf("answered %d %s; want 412 ENTITY_MODIFIED: %s", out.status, out.errorCode, out.detail)
			}
			if n := calls.Load(); n != 0 {
				t.Fatalf("%d callouts fired despite the stale If-Match", n)
			}
			if st, _ := h.GetEntityState(t, eID); st != "OPEN" {
				t.Fatalf("E is %s; the rejected request must leave it OPEN", st)
			}
			if amount := h.GetEntityData(t, eID)["amount"]; amount != float64(5) {
				t.Fatalf("E has amount %v; want the other client's write (5)", amount)
			}
		})
	}
}

// TestIfMatch_StaleLoopbackPut412 is the loopback PUT's 412 cell on the shared
// server, which the OpenAPI conformance validator records: another client
// updated E after the caller read it, and the caller's PUT with the old
// If-Match answers 412 ENTITY_MODIFIED.
func TestIfMatch_StaleLoopbackPut412(t *testing.T) {
	const model = "e2e-ifmatch-stale-loopback"
	setupModelWithWorkflow(t, model, `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "ifmatch-stale-loopback-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE":    {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {}
			}
		}]
	}`)
	entityID, staleTxID := createEntityE2EWithTxID(t, model, 1, `{"name":"Stale","amount":1,"status":"draft"}`)
	put := func(ifMatch, body string) (int, string) {
		t.Helper()
		req, err := e2eNewRequest(t, http.MethodPut, serverURL+"/api/entity/JSON/"+entityID, strings.NewReader(body))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+getToken(t, "testclient", "testsecret"))
		req.Header.Set("Content-Type", "application/json")
		if ifMatch != "" {
			req.Header.Set("If-Match", ifMatch)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("PUT: %v", err)
		}
		return resp.StatusCode, readBody(t, resp)
	}
	if status, body := put("", `{"name":"Stale","amount":2,"status":"draft"}`); status != http.StatusOK {
		t.Fatalf("the other client's update: %d %s", status, body)
	}
	status, body := put(staleTxID, `{"name":"Stale","amount":3,"status":"draft"}`)
	var pd problemDoc
	_ = json.Unmarshal([]byte(body), &pd)
	if code, _ := pd.Properties["errorCode"].(string); status != http.StatusPreconditionFailed || code != "ENTITY_MODIFIED" {
		t.Fatalf("answered %d %s; want 412 ENTITY_MODIFIED: %s", status, code, body)
	}
}
