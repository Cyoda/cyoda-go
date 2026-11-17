package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

// ifmatch_doors_test.go — every door that takes If-Match. If-Match states the
// version the request starts from: a stale one is 412 before any processor
// runs, and a change another client commits after the request read E is a
// lost race, the retryable 409.

// ifMatchDoor is one door that takes If-Match, run on E with the If-Match
// eTxID. Its request moves E through the workflow's processors.
type ifMatchDoor struct {
	name    string
	request func(h *callbackHarness, eID, eTxID string) joinedCommitOutcome
}

const ifMatchLoop = `{"name":"e","amount":1,"status":"loop"}`

// ifMatchDoors is every door that takes If-Match. loop is the body of the
// updates without a transition, which sets status loop and so runs the
// automated "loop" transition; goBody is the body of the manual transition
// "go". The patches set status loop. A collection item's precondition
// failure, reported inside a 200 as failed[], reads as 412.
func ifMatchDoors(loop, goBody string) []ifMatchDoor {
	return []ifMatchDoor{
		{name: "Update", request: func(h *callbackHarness, eID, eTxID string) joinedCommitOutcome {
			return httpJoinedOutcome(h.putIfMatch("/api/entity/JSON/"+eID, eTxID, loop))
		}},
		{name: "ManualTransition", request: func(h *callbackHarness, eID, eTxID string) joinedCommitOutcome {
			return httpJoinedOutcome(h.putIfMatch("/api/entity/JSON/"+eID+"/go", eTxID, goBody))
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

// ifMatchStored is E's data as the 409 test stores it and as every door
// sends it back: compact, keys in the order PostgreSQL returns them.
const ifMatchStored = `{"amount":1,"name":"e","status":"loop"}`

// ifMatchRestWorkflow rests a created entity in OPEN and has no other
// transition, so E can be stored there with status loop.
func ifMatchRestWorkflow(name string) string {
	return fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": %q, "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "rest", "next": "OPEN", "manual": false}]},
				"OPEN": {}
			}
		}]
	}`, name+"-rest-wf")
}

// TestIfMatch_ChangeAfterReadIs409 — another client commits a change to E
// after the request read it. The processor's callout is held while the test
// makes that write, then released; the processor itself writes nothing. The
// request's If-Match held when it started, so this is a lost race, not a
// failed precondition: a retryable 409 CONFLICT, and E keeps the other
// client's write. A collection item is not isolated: the transaction the
// chunk runs in cannot commit.
//
// Every door sends E's stored data unchanged (ifMatchStored; the patches set
// the status E already has). Before a callout that carries the transaction
// token the engine saves E into the transaction when E differs from what the
// transaction holds, and on PostgreSQL that save holds E's row lock until the
// transaction ends: a rival write made during the callout would wait for it
// (anchor_row_lock_test.go). With E unchanged the engine saves nothing before
// the callout and the rival commits at once. So that an unchanged body still
// runs the automated "loop" transition, E is stored with status loop under a
// workflow without it (ifMatchRestWorkflow), and the door workflow is
// imported after.
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
	for _, door := range ifMatchDoors(ifMatchStored, ifMatchStored) {
		for _, shape := range shapes {
			t.Run(door.name+"/"+shape.name, func(t *testing.T) {
				h := newCalloutHarness(t, nil)
				model, tag := uniq("ifm-rival"), uniq("ifm-rival-tag")
				var calls atomic.Int32
				running, release := make(chan struct{}), make(chan struct{})
				var releaseOnce sync.Once
				releaseCallout := func() { releaseOnce.Do(func() { close(release) }) }
				t.Cleanup(releaseCallout)
				h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{tag}, script: func(context.Context, receivedCallout, *reqCtx) cnodeReply {
					if calls.Add(1) > 1 {
						return answerOK()
					}
					close(running)
					select {
					case <-release:
					case <-time.After(20 * time.Second):
					}
					return answerOK()
				}})
				h.SetupModelWithWorkflow(t, model, ifMatchRestWorkflow(model))
				eID, eTxID := createWithTx(t, h, model, ifMatchStored)
				resp := h.DoAuth(t, http.MethodPost, fmt.Sprintf("/api/model/%s/1/workflow/import", model), conflictDoorWorkflow(model, tag, shape.procs...), "")
				if body := h.readBody(t, resp); resp.StatusCode != http.StatusOK {
					t.Fatalf("import the door workflow: %d %s", resp.StatusCode, body)
				}

				done := make(chan joinedCommitOutcome, 1)
				go func() { done <- door.request(h, eID, eTxID) }()
				select {
				case <-running:
				case out := <-done:
					t.Fatalf("the request answered %d %s before its processor was called out: %s", out.status, out.errorCode, out.detail)
				case <-time.After(30 * time.Second):
					t.Fatal("the processor was never called out")
				}
				// The other client's write, while the callout is held: no
				// transaction token, so it commits at once. Its status matches
				// no transition.
				rival, err := h.callback(http.MethodPut, "/api/entity/JSON/"+eID, `{"name":"e","amount":5,"status":"rival"}`, "")
				releaseCallout()
				if err != nil || rival.StatusCode != http.StatusOK {
					t.Fatalf("the other client's write: %v %d %s; want 200", err, rival.StatusCode, rival.Body)
				}

				var out joinedCommitOutcome
				select {
				case out = <-done:
				case <-time.After(30 * time.Second):
					t.Fatal("the request did not finish")
				}
				if out.err != nil {
					t.Fatalf("request: %v", out.err)
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
	for _, door := range ifMatchDoors(ifMatchLoop, `{"name":"e","amount":1,"status":"draft"}`) {
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
		req.Header.Set("Authorization", "Bearer "+suiteToken(t))
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
