package grpc

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
)

// callout_endpoints_rpc_test.go is the gRPC half of spec §8.2's coverage:
// the create envelope was the only one with a running-backend test for any
// callout code, and §8.2 names EntityManage and EntityManageCollection alike.
// This crosses the update, transition and both collection envelopes with every
// code a callout can answer with.
//
// The two collection envelopes are not the same path: EntityCreateCollection
// goes through the entity handler's collection loop, which wraps an item's
// error as "item N: " before classifying it, while EntityUpdateCollection
// loops over single UpdateEntity calls in THIS package and adds no prefix.
// Each cell states which, and the assertion is exact.

// rpcCalloutCase is one compute-member behaviour and the envelope it must
// produce on every door.
type rpcCalloutCase struct {
	name string
	// members is how many compute members serve the callout's tag.
	members int
	// answer completes a tracked request; nil means the member takes the work
	// and never answers.
	answer func() *ProcessingResponse
	// policy is the callout's retryPolicy; "" leaves it unset (FIXED).
	policy     string
	idempotent bool
	wantCode   string
	// wantRetryable is the envelope's Error.Retryable, which is set only when
	// true.
	wantRetryable bool
	// wantMessage builds the message the client must get, character for
	// character. prefix is "item 0: " on a door that runs its items through the
	// entity handler's collection loop and this failure keeps the prefix, and
	// "" otherwise; tag is the callout's tag. Every case fixes its message:
	// §8.2 fixes the CALLOUT_FAILED shape and the MemberFailed wrap, and the
	// rest are the codes' own authored text.
	wantMessage func(prefix, tag string) string
	// wantItemPrefix is whether "item 0: " reaches the client on a door that
	// runs its items through the entity handler's collection loop.
	wantItemPrefix bool
	// wantAsked is how many requests the members received in total.
	wantAsked int
}

func rpcCalloutCases() []rpcCalloutCase {
	yes, no := true, false
	// The engine's wrap names the processor for every failure that reaches the
	// classifier's catch-all or its MemberFailed branch; a failure carrying an
	// *AppError is returned as it is, wrap and item prefix alike dropped.
	wrapped := func(inner string) func(string, string) string {
		return func(prefix, _ string) string { return prefix + "processor proc failed: " + inner }
	}
	return []rpcCalloutCase{
		{
			name: "no-member", members: 0,
			wantCode: "NO_COMPUTE_MEMBER_FOR_TAG", wantRetryable: true, wantItemPrefix: true, wantAsked: 0,
			wantMessage: func(prefix, tag string) string {
				return prefix + `processor proc failed: no matching calculation member: tags "` + tag + `"`
			},
		},
		{
			name: "one-try-no-answer", members: 1, policy: "NONE",
			wantCode: "DISPATCH_TIMEOUT", wantRetryable: true, wantAsked: 1,
			wantMessage: func(string, string) string {
				return "processor dispatch timed out after 50ms: no response"
			},
		},
		{
			name: "every-try-used", members: 2, idempotent: true,
			wantCode: "CALLOUT_FAILED", wantRetryable: true, wantAsked: 2,
			// §8.2's shape, character for character: the angle brackets are
			// literal, the count precedes collapsing, the entries keep the order
			// the members were tried in (Candidates orders by ConnectedAt then
			// id), and each carries the try's own "CODE: text".
			wantMessage: func(string, string) string {
				const cause = "DISPATCH_TIMEOUT: processor dispatch timed out after 50ms: no response"
				return "the callout could not be completed, got 2 failures: " +
					"[member<m-0>: " + cause + "], [member<m-1>: " + cause + "]"
			},
		},
		{
			name: "member-failed-retryable", members: 1,
			answer: func() *ProcessingResponse {
				return &ProcessingResponse{Success: false, Error: "boom", Retryable: &yes}
			},
			wantCode: "WORKFLOW_FAILED", wantRetryable: true,
			wantItemPrefix: true, wantAsked: 1, wantMessage: wrapped("boom"),
		},
		{
			name: "member-failed", members: 1,
			answer: func() *ProcessingResponse {
				return &ProcessingResponse{Success: false, Error: "boom", Retryable: &no}
			},
			wantCode: "WORKFLOW_FAILED", wantItemPrefix: true, wantAsked: 1,
			wantMessage: wrapped("boom"),
		},
		{
			name: "terminal", members: 1,
			answer: func() *ProcessingResponse {
				// Success, with a payload that is a JSON string rather than an
				// object: an answer no other member would read any better.
				return &ProcessingResponse{Success: true, Payload: json.RawMessage(`"not-an-object"`)}
			},
			wantCode: "WORKFLOW_FAILED", wantItemPrefix: true, wantAsked: 1,
			wantMessage: wrapped("the compute member's response could not be read: the payload did not decode"),
		},
	}
}

// rpcEnvelope is the part of a response envelope every door's reply reduces to.
type rpcEnvelope struct {
	success   bool
	class     string
	message   string
	retryable bool
}

// rpcCalloutDoor is one gRPC door: the workflow its callout lives in, how an
// entity reaches the state it acts on, and the call that fires the callout.
type rpcCalloutDoor struct {
	name string
	// itemLooped says the door's items go through the entity handler's
	// collection loop, which prefixes an item's failure with "item N: ".
	itemLooped bool
	workflow   func(wfName, tag string, tc rpcCalloutCase) string
	// seed creates the entity the door acts on; "" for a door that creates.
	seed  func(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context, model string) string
	drive func(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context, model, entityID string) rpcEnvelope
}

// TestRPC_CalloutEndpoints_EveryDoorEveryCode crosses the gRPC doors §8.2
// names, other than the already-covered create, with every code a callout can
// answer with.
func TestRPC_CalloutEndpoints_EveryDoorEveryCode(t *testing.T) {
	for _, door := range rpcCalloutDoors() {
		t.Run(door.name, func(t *testing.T) {
			for _, tc := range rpcCalloutCases() {
				t.Run(tc.name, func(t *testing.T) {
					// One try plus one retry, and no patience: a callout with
					// no member fails at once and a repeat-safe one that lost
					// an answer gets the second member.
					svc, wfHandler, ctx := newTestEnvWithOwner(t, 30*time.Second, 60*time.Second,
						OwnerTestConfig{FixedNumRetries: 1})
					model := "grpc-door-" + door.name + "-" + tc.name
					tag := "tag-" + door.name + "-" + tc.name
					setupScheduledWorkflowRPCEnv(t, svc, wfHandler, ctx, model,
						door.workflow("wf-"+door.name+"-"+tc.name, tag, tc))

					asked := registerAnswering(t, svc, tag, tc)

					entityID := ""
					if door.seed != nil {
						entityID = door.seed(t, svc, ctx, model)
					}
					env := door.drive(t, svc, ctx, model, entityID)

					// The "item 0: " prefix is part of the expected message, so
					// the exact comparison below carries the prefix assertion
					// too: a door with no collection loop expects none, and a
					// door with one expects it exactly where the classifier
					// keeps it.
					prefix := ""
					if door.itemLooped && tc.wantItemPrefix {
						prefix = "item 0: "
					}
					assertCalloutEnvelope(t, env, tc, prefix, tag)
					if got := asked(); got != tc.wantAsked {
						t.Errorf("%d requests reached compute members; want %d", got, tc.wantAsked)
					}
				})
			}
		})
	}
}

// assertCalloutEnvelope checks the envelope class, the code, the retryable flag
// and the whole message.
//
// The code is asserted as the message's PREFIX, not as a substring: the envelope
// contract is "the code, a colon, then the detail", and CALLOUT_FAILED's detail
// lists each attempt's own code — so a substring test would let a one-try cell
// pass on a CALLOUT_FAILED envelope that named DISPATCH_TIMEOUT only inside its
// attempt list.
func assertCalloutEnvelope(t *testing.T, env rpcEnvelope, tc rpcCalloutCase, prefix, tag string) {
	t.Helper()
	if env.success {
		t.Fatalf("the call succeeded; want a refusal with %s", tc.wantCode)
	}
	if env.class != "CLIENT_ERROR" {
		t.Errorf("Error.Code = %q; want CLIENT_ERROR", env.class)
	}
	if !strings.HasPrefix(env.message, tc.wantCode+": ") {
		t.Errorf("Error.Message = %q; want it to BEGIN with %q", env.message, tc.wantCode+": ")
	}
	if env.retryable != tc.wantRetryable {
		t.Errorf("Error.Retryable = %t; want %t (message: %q)", env.retryable, tc.wantRetryable, env.message)
	}
	want := tc.wantCode + ": " + tc.wantMessage(prefix, tag)
	if env.message != want {
		t.Errorf("Error.Message = %q;\n want the literal %q", env.message, want)
	}
}

// registerAnswering registers the case's compute members and returns how many
// requests they have received.
func registerAnswering(t *testing.T, svc *CloudEventsServiceImpl, tag string, tc rpcCalloutCase) func() int {
	t.Helper()
	counted := make(chan struct{}, 16)
	for i := 0; i < tc.members; i++ {
		id := fmt.Sprintf("m-%d", i)
		svc.registry.Register(id, testTenant, []string{tag}, func(ce *cepb.CloudEvent) error {
			counted <- struct{}{}
			if tc.answer == nil {
				return nil // takes the work, never answers
			}
			reqID, err := extractRequestID(ce)
			if err != nil {
				t.Errorf("extractRequestID: %v", err)
				return nil
			}
			svc.registry.Get(id).CompleteRequest(reqID, tc.answer())
			return nil
		}, nil)
	}
	return func() int { return len(counted) }
}

// rpcCalloutDoors is the gRPC doors of §8.2 the create envelope does not cover.
func rpcCalloutDoors() []rpcCalloutDoor {
	return []rpcCalloutDoor{
		{
			name:     "update-transition",
			workflow: manualProcRPCWorkflowJSON,
			seed:     seedQuietEntity,
			drive: func(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context, _, entityID string) rpcEnvelope {
				return manageEnvelope(t, svc, ctx, makeCE(EntityUpdateRequest, map[string]any{
					"id": "update-1", "dataFormat": "JSON",
					"payload": map[string]any{
						"entityId":   entityID,
						"transition": "go",
						"data":       map[string]any{"name": "Test Order", "amount": 100, "status": "draft"},
					},
				}))
			},
		},
		{
			name:     "update-loopback",
			workflow: readyProcRPCWorkflowJSON,
			seed:     seedQuietEntity,
			drive: func(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context, _, entityID string) rpcEnvelope {
				return manageEnvelope(t, svc, ctx, makeCE(EntityUpdateRequest, map[string]any{
					"id": "loopback-1", "dataFormat": "JSON",
					"payload": map[string]any{
						"entityId": entityID,
						"data":     map[string]any{"name": "Test Order", "amount": 100, "status": "ready"},
					},
				}))
			},
		},
		{
			name:     "transition",
			workflow: manualProcRPCWorkflowJSON,
			seed:     seedQuietEntity,
			drive: func(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context, _, entityID string) rpcEnvelope {
				return transitionEnvelope(t, svc, ctx, makeCE(EntityTransitionRequest, map[string]any{
					"id": "transition-1", "entityId": entityID, "transition": "go",
				}))
			},
		},
		{
			name:       "create-collection",
			itemLooped: true,
			workflow:   autoProcRPCWorkflowJSON,
			drive: func(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context, model, _ string) rpcEnvelope {
				return collectionEnvelope(t, svc, ctx, makeCE(EntityCreateCollectionRequest, map[string]any{
					"id": "create-collection-1", "dataFormat": "JSON",
					"payloads": []any{map[string]any{
						"model": map[string]any{"name": model, "version": 1},
						"data":  map[string]any{"name": "Test Order", "amount": 100, "status": "draft"},
					}},
				}))
			},
		},
		{
			name:     "update-collection",
			workflow: manualProcRPCWorkflowJSON,
			seed:     seedQuietEntity,
			drive: func(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context, _, entityID string) rpcEnvelope {
				return collectionEnvelope(t, svc, ctx, makeCE(EntityUpdateCollectionRequest, map[string]any{
					"id": "update-collection-1", "dataFormat": "JSON",
					"payloads": []any{map[string]any{
						"entityId":   entityID,
						"transition": "go",
						"data":       map[string]any{"name": "Test Order", "amount": 100, "status": "draft"},
					}},
				}))
			},
		},
	}
}

// manageEnvelope calls EntityManage and reduces its transaction reply.
func manageEnvelope(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context, ce *cepb.CloudEvent) rpcEnvelope {
	t.Helper()
	var typed events.EntityTransactionResponseJson
	validateResponse(t, manageReply(t, svc, ctx, ce), &typed)
	return reduceEnvelope(typed.Success, typed.Error)
}

// transitionEnvelope is manageEnvelope for the transition door, whose reply is
// an EntityTransitionResponse — the same success/error shape in a message of
// its own, with no requestId.
func transitionEnvelope(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context, ce *cepb.CloudEvent) rpcEnvelope {
	t.Helper()
	var typed events.EntityTransitionResponseJson
	validateResponse(t, manageReply(t, svc, ctx, ce), &typed)
	env := rpcEnvelope{success: typed.Success}
	if typed.Error != nil {
		env.class = typed.Error.Code
		env.message = typed.Error.Message
		env.retryable = typed.Error.Retryable != nil && *typed.Error.Retryable
	}
	return env
}

// manageReply calls EntityManage and fails the test on a transport error.
func manageReply(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context, ce *cepb.CloudEvent) *cepb.CloudEvent {
	t.Helper()
	resp, err := svc.EntityManage(ctx, ce)
	if err != nil {
		t.Fatalf("unexpected gRPC transport error: %v", err)
	}
	return resp
}

// collectionEnvelope calls EntityManageCollection and reduces its one frame.
func collectionEnvelope(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context, ce *cepb.CloudEvent) rpcEnvelope {
	t.Helper()
	stream := &mockManageStream{ctx: ctx}
	if err := svc.EntityManageCollection(ce, stream); err != nil {
		t.Fatalf("unexpected gRPC transport error: %v", err)
	}
	if len(stream.sent) != 1 {
		t.Fatalf("the stream sent %d frames; a refusal is exactly one", len(stream.sent))
	}
	var typed events.EntityTransactionResponseJson
	validateResponse(t, stream.sent[0], &typed)
	return reduceEnvelope(typed.Success, typed.Error)
}

// reduceEnvelope folds a response's success/error pair into rpcEnvelope.
func reduceEnvelope(success bool, e *events.EntityTransactionResponseJsonError) rpcEnvelope {
	env := rpcEnvelope{success: success}
	if e != nil {
		env.class = e.Code
		env.message = e.Message
		env.retryable = e.Retryable != nil && *e.Retryable
	}
	return env
}

// seedQuietEntity creates the entity a door acts on, under a workflow whose
// callout the create does not reach.
func seedQuietEntity(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context, model string) string {
	t.Helper()
	typed := createScheduledEntity(t, svc, ctx, model)
	if !typed.Success {
		t.Fatalf("seeding the entity the door acts on failed: %v", typed.Error)
	}
	if len(typed.TransactionInfo.EntityIds) == 0 {
		t.Fatal("the seeding create returned no entity id")
	}
	return typed.TransactionInfo.EntityIds[0]
}

// manualProcRPCWorkflowJSON is NONE -[go]-> DONE with one MANUAL transition
// carrying one SYNC processor: creating an entity fires nothing.
func manualProcRPCWorkflowJSON(wfName, tag string, tc rpcCalloutCase) string {
	return rpcWorkflowJSON(wfName, fmt.Sprintf(`"NONE": {"transitions": [{"name": "go", "next": "DONE", "manual": true,
		"processors": [%s]}]}, "DONE": {}`, rpcProcJSON(tag, tc)))
}

// autoProcRPCWorkflowJSON is NONE -[init]-> DONE with one AUTOMATED transition
// carrying the processor: creating an entity fires it.
func autoProcRPCWorkflowJSON(wfName, tag string, tc rpcCalloutCase) string {
	return rpcWorkflowJSON(wfName, fmt.Sprintf(`"NONE": {"transitions": [{"name": "init", "next": "DONE", "manual": false,
		"processors": [%s]}]}, "DONE": {}`, rpcProcJSON(tag, tc)))
}

// readyProcRPCWorkflowJSON is autoProcRPCWorkflowJSON guarded by an inline
// criterion on the payload: an entity created with any other status stays in
// NONE, and the loopback that writes "ready" is what fires the callout.
func readyProcRPCWorkflowJSON(wfName, tag string, tc rpcCalloutCase) string {
	return rpcWorkflowJSON(wfName, fmt.Sprintf(`"NONE": {"transitions": [{"name": "auto", "next": "DONE", "manual": false,
		"criterion": {"type": "simple", "jsonPath": "$.status", "operatorType": "EQUALS", "value": "ready"},
		"processors": [%s]}]}, "DONE": {}`, rpcProcJSON(tag, tc)))
}

// rpcWorkflowJSON wraps states in a schema 1.5 REPLACE import document — the
// minor that accepts a processor's idempotent.
func rpcWorkflowJSON(wfName, states string) string {
	return fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.5", "name": %q, "initialState": "NONE", "active": true,
			"states": {%s}
		}]
	}`, wfName, states)
}

// rpcProcJSON is the processor literal for one case: its tag, a short answer
// limit, and the case's retryPolicy and idempotent declaration.
func rpcProcJSON(tag string, tc rpcCalloutCase) string {
	cfg := fmt.Sprintf(`"attachEntity": true, "calculationNodesTags": %q, "responseTimeoutMs": 50`, tag)
	if tc.policy != "" {
		cfg += fmt.Sprintf(`, "retryPolicy": %q`, tc.policy)
	}
	if tc.idempotent {
		cfg += `, "idempotent": true`
	}
	return fmt.Sprintf(`{"type": "calculator", "name": "proc", "executionMode": "SYNC", "config": {%s}}`, cfg)
}
