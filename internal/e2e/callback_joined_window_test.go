package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

// callback_joined_window_test.go — a request that joined an open transaction
// is never split into transactionWindow chunks.
//
// A window is a commit after every so many items, and a joined request commits
// nothing: its transaction's owner does. So on the three HTTP doors that chunk
// a collection (POST /entity/{format}/{entityName}/{modelVersion} with an array
// body, POST /entity/{format}, PUT /entity/{format}):
//   - an explicit transactionWindow on a joined request is refused with 400
//     BAD_REQUEST, as transactionSize and transactionTimeoutMillis are;
//   - without one, the joined collection runs as one unit, so a failure in any
//     item is the request's failure — here the COMMIT_IN_JOINED_TRANSACTION
//     refusal of item 101, which chunked would have come back as a 200.

// joinedWindowItemCount is one more than the server's default window.
const joinedWindowItemCount = 101

// joinedWindowTargetWorkflow reaches a COMMIT_BEFORE_DISPATCH processor only
// for an entity whose status is "go" — on create or on a later update — and
// otherwise rests in READY.
func joinedWindowTargetWorkflow(name, proc string) string {
	cbd := fmt.Sprintf(`"criterion": {"type": "simple", "jsonPath": "$.status", "operatorType": "EQUALS", "value": "go"},
		"processors": [{"type": "calculator", "name": %q, "executionMode": "COMMIT_BEFORE_DISPATCH",
			"config": {"attachEntity": true, "calculationNodesTags": ""}}]`, proc)
	return fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": %q, "initialState": "NONE", "active": true,
			"states": {
				"NONE":  {"transitions": [{"name": "go", "next": "DONE", "manual": false, %s},
				                          {"name": "store", "next": "READY", "manual": false}]},
				"READY": {"transitions": [{"name": "go", "next": "DONE", "manual": false, %s}]},
				"DONE":  {}
			}
		}]
	}`, name+"-wf", cbd, cbd)
}

// joinedWindowDoor is one chunking door. body builds the request body from the
// per-item payloads (for an update, paired with existing entity ids).
type joinedWindowDoor struct {
	name   string
	method string
	path   func(model string) string
	update bool
	body   func(model string, ids, payloads []string) string
}

func joinedWindowDoors() []joinedWindowDoor {
	return []joinedWindowDoor{
		{"http-create-array", http.MethodPost, func(m string) string { return "/api/entity/JSON/" + m + "/1" }, false,
			func(_ string, _, payloads []string) string { return "[" + strings.Join(payloads, ",") + "]" }},
		{"http-create-collection", http.MethodPost, func(string) string { return "/api/entity/JSON" }, false,
			func(model string, _, payloads []string) string {
				items := make([]any, len(payloads))
				for i, p := range payloads {
					items[i] = map[string]any{"model": map[string]any{"name": model, "version": 1}, "payload": p}
				}
				b, _ := json.Marshal(items)
				return string(b)
			}},
		{"http-update-collection", http.MethodPut, func(string) string { return "/api/entity/JSON" }, true,
			func(_ string, ids, payloads []string) string {
				items := make([]any, len(payloads))
				for i, p := range payloads {
					items[i] = map[string]any{"id": ids[i], "payload": p}
				}
				b, _ := json.Marshal(items)
				return string(b)
			}},
	}
}

// joinedWindowPayloads is joinedWindowItemCount payloads, the last with status
// "go".
func joinedWindowPayloads() []string {
	payloads := make([]string, joinedWindowItemCount)
	for i := range payloads {
		status := "new"
		if i == len(payloads)-1 {
			status = "go"
		}
		payloads[i] = fmt.Sprintf(`{"name":"item-%d","amount":1,"status":%q}`, i, status)
	}
	return payloads
}

// runJoinedWindowCase makes one joined request on door from inside an outer
// SYNC processor, under the callout's pass, and returns its answer. The outer
// processor then fails on purpose, so the owner rolls its transaction back.
func runJoinedWindowCase(t *testing.T, h *callbackHarness, door joinedWindowDoor, suffix, query string) callbackResult {
	t.Helper()
	target := "jw-target-" + suffix
	outer := "jw-outer-" + suffix
	outerProc := "jw-outer-proc-" + suffix
	cbdProc := "jw-cbd-proc-" + suffix

	h.SetupModelWithWorkflow(t, target, joinedWindowTargetWorkflow(target, cbdProc))
	h.RegisterProc(cbdProc, func(*reqCtx) (map[string]any, error) { return nil, nil })

	var ids []string
	if door.update {
		ids = make([]string, joinedWindowItemCount)
		for i := range ids {
			id, status, body := h.CreateEntity(t, target, 1, joinedCommitPayload)
			if status != http.StatusOK {
				t.Fatalf("create target %d: %d %s", i, status, body)
			}
			ids[i] = id
		}
	}
	body := door.body(target, ids, joinedWindowPayloads())

	answered := make(chan callbackResult, 1)
	h.RegisterProc(outerProc, func(rc *reqCtx) (map[string]any, error) {
		res, err := h.callback(door.method, door.path(target)+query, body, rc.token)
		if err != nil {
			res = callbackResult{Body: err.Error()}
		}
		answered <- res
		return nil, fmt.Errorf("outer processor fails on purpose so the owner rolls its transaction back")
	})
	h.SetupModelWithWorkflow(t, outer, joinedCommitOuterWorkflow(outer, outerProc))
	_, _, _ = h.CreateEntity(t, outer, 1, joinedCommitPayload)

	select {
	case res := <-answered:
		return res
	case <-time.After(30 * time.Second):
		t.Fatal("timeout: the outer processor's callback never ran")
		return callbackResult{}
	}
}

func TestCallback_JoinedCollection_TransactionWindowRefused(t *testing.T) {
	h := newCallbackHarness(t)
	for _, door := range joinedWindowDoors() {
		t.Run(door.name, func(t *testing.T) {
			res := runJoinedWindowCase(t, h, door, "refused-"+door.name, "?transactionWindow=10")
			pd := assertProblem(t, res.StatusCode, res.Body, http.StatusBadRequest, "BAD_REQUEST", false)
			if !strings.Contains(pd.Detail, "transactionWindow") || !strings.Contains(pd.Detail, "joins an open transaction") {
				t.Errorf("detail must name the param and the joined transaction: %q", pd.Detail)
			}
		})
	}
}

func TestCallback_JoinedCollection_NotChunked(t *testing.T) {
	h := newCallbackHarness(t)
	for _, door := range joinedWindowDoors() {
		t.Run(door.name, func(t *testing.T) {
			res := runJoinedWindowCase(t, h, door, "unchunked-"+door.name, "")
			assertProblem(t, res.StatusCode, res.Body, http.StatusConflict, "COMMIT_IN_JOINED_TRANSACTION", false)
		})
	}
}

// TestCallback_JoinedCollection_GRPCTransactionWindowRefused: the gRPC
// collection events carry transactionWindow too; on a joined request it is
// refused like the HTTP parameter, not accepted and ignored.
func TestCallback_JoinedCollection_GRPCTransactionWindowRefused(t *testing.T) {
	h := newCallbackHarness(t)
	target := "jw-grpc-target"
	h.SetupModelWithWorkflow(t, target, secondaryWorkflow)
	existingID := createQuietEntity(t, h, target, joinedCommitPayload)

	for _, tc := range []struct {
		name      string
		eventType string
		body      map[string]any
	}{
		{"grpc-create-collection", internalgrpc.EntityCreateCollectionRequest, map[string]any{
			"id": "jw-create", "dataFormat": "JSON", "transactionWindow": 10,
			"payloads": []any{map[string]any{"model": map[string]any{"name": target, "version": 1}, "data": mustJSONMap(joinedCommitPayload)}},
		}},
		{"grpc-update-collection", internalgrpc.EntityUpdateCollectionRequest, map[string]any{
			"id": "jw-update", "dataFormat": "JSON", "transactionWindow": 10,
			"payloads": []any{map[string]any{"entityId": existingID, "data": mustJSONMap(joinedCommitPayload)}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outer := "jw-grpc-outer-" + tc.name
			outerProc := "jw-grpc-outer-proc-" + tc.name
			answered := make(chan joinedCommitOutcome, 1)
			h.RegisterProc(outerProc, func(rc *reqCtx) (map[string]any, error) {
				answered <- grpcJoinedOutcome(h.joinedManageCollectionGRPC(tc.eventType, tc.body, rc.token))
				return nil, fmt.Errorf("outer processor fails on purpose so the owner rolls its transaction back")
			})
			h.SetupModelWithWorkflow(t, outer, joinedCommitOuterWorkflow(outer, outerProc))
			_, _, _ = h.CreateEntity(t, outer, 1, joinedCommitPayload)

			var o joinedCommitOutcome
			select {
			case o = <-answered:
			case <-time.After(15 * time.Second):
				t.Fatal("timeout: the outer processor's callback never ran")
			}
			if o.err != nil {
				t.Fatalf("transport error: %v", o.err)
			}
			if o.envelopeCode != "CLIENT_ERROR" || o.errorCode != "BAD_REQUEST" {
				t.Fatalf("envelope %q code %q; want CLIENT_ERROR BAD_REQUEST (%s)", o.envelopeCode, o.errorCode, o.detail)
			}
			if !strings.Contains(o.detail, "transactionWindow") || !strings.Contains(o.detail, "joins an open transaction") {
				t.Errorf("message must name transactionWindow and the joined transaction: %s", o.detail)
			}
		})
	}
}
