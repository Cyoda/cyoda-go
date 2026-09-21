package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// callout_endpoints_test.go crosses every client-facing door that runs a
// workflow with every code a callout can answer with. The plain create is
// covered by callout_errors_test.go and callout_failover_test.go; this file
// covers the other five doors spec §8.2 names — create-collection, update with
// a transition, a loopback update, update-collection, and the transitions
// query.
//
// The transitions query's only callout is the WORKFLOW-level function
// criterion: GetAvailableTransitionsForEntity names a state's transitions and
// evaluates none of their criteria, while workflow selection evaluates the
// workflow's own. So that door is driven through a workflow criterion, which
// is also why its model is set up twice — the entity is created under a
// workflow with no criterion (nothing to call out to), and the criterion
// arrives with a second import.
//
// The two collection doors are not repetition of the single-entity ones: both
// classify through classifyWorkflowError(fmt.Errorf("item %d: %w", …)), so
// whether that prefix survives depends on which branch of the classifier the
// failure takes. Each case states which, and the assertion is exact.

// calloutCase is one compute-member behaviour and the answer the client must
// get for it, on every door.
type calloutCase struct {
	name string
	// members is how many scripted compute members serve the callout's tag.
	members int
	reply   cnodeReply
	// policy is the callout's retryPolicy; "" leaves it unset (FIXED).
	policy string
	// idempotent declares the processor repeat-safe. A criterion is
	// repeat-safe by rule and the field is not part of its config, so the
	// criterion door ignores it.
	idempotent    bool
	wantStatus    int
	wantCode      string
	wantRetryable bool
	// wantMemberText is the compute member's own words the message must
	// carry; empty when the message is wholly the platform's.
	wantMemberText string
	// wantItemPrefix: on a collection door, whether the "item 0: " the
	// collection loop adds survives into the client's message. It does not
	// when the failure carries an *AppError, which the classifier returns
	// as it is.
	wantItemPrefix bool
	// wantPerMember is how many callouts EACH attached member must have
	// received. A member that received a second one would mean the callout
	// was repeated where it must not be.
	wantPerMember int
}

// calloutCases is the §8.2 column set: every code a callout can answer with on
// any of these doors.
func calloutCases() []calloutCase {
	return []calloutCase{
		{
			name: "no-member", members: 0, reply: answerOK(),
			wantStatus: http.StatusServiceUnavailable, wantCode: "NO_COMPUTE_MEMBER_FOR_TAG", wantRetryable: true,
			wantItemPrefix: true, wantPerMember: 0,
		},
		{
			name: "one-try-no-answer", members: 1, reply: neverAnswer(), policy: "NONE",
			wantStatus: http.StatusServiceUnavailable, wantCode: "DISPATCH_TIMEOUT", wantRetryable: true, wantPerMember: 1,
		},
		{
			name: "one-try-dropped", members: 1, reply: closeStream(), policy: "NONE",
			wantStatus: http.StatusServiceUnavailable, wantCode: "COMPUTE_MEMBER_DISCONNECTED", wantRetryable: true, wantPerMember: 1,
		},
		{
			name: "every-try-used", members: 2, reply: closeStream(), idempotent: true,
			wantStatus: http.StatusServiceUnavailable, wantCode: "CALLOUT_FAILED", wantRetryable: true, wantPerMember: 1,
		},
		{
			name: "member-failed-retryable", members: 1, reply: answerFailVerdict("boom", true),
			wantStatus: http.StatusBadRequest, wantCode: "WORKFLOW_FAILED", wantRetryable: true,
			wantMemberText: "boom", wantItemPrefix: true, wantPerMember: 1,
		},
		{
			name: "member-failed", members: 1, reply: answerFailVerdict("boom", false),
			wantStatus: http.StatusBadRequest, wantCode: "WORKFLOW_FAILED", wantRetryable: false,
			wantMemberText: "boom", wantItemPrefix: true, wantPerMember: 1,
		},
		{
			// On a processor this is a response payload that is not an
			// object; on the criterion door it is a response that answers
			// success with no `matches` — the invented verdict the platform
			// refuses. Read as "does not match" it would select some OTHER
			// workflow and answer 200 with its transitions.
			name: "terminal", members: 1, reply: answerMalformedPayload(),
			wantStatus: http.StatusBadRequest, wantCode: "WORKFLOW_FAILED", wantRetryable: false,
			wantItemPrefix: true, wantPerMember: 1,
		},
	}
}

// calloutDoor is one client-facing door: the workflow the callout lives in,
// how an entity reaches the state the door acts on, and the request that fires
// the callout.
type calloutDoor struct {
	name string
	// collection says the door carries its items through the collection loop,
	// which prefixes an item's failure with "item N: " before classifying it.
	collection bool
	// setup imports the model and workflow and returns the id of the entity
	// the door acts on ("" when the door creates its own).
	setup func(t *testing.T, h *callbackHarness, model, wfName, tag string, tc calloutCase) string
	// drive makes the request that fires the callout.
	drive func(t *testing.T, h *callbackHarness, model, entityID string) (int, string)
}

// TestCalloutEndpoints_EveryDoorEveryCode is the §8.2 coverage matrix: one
// request per door × code, on a real backend, asserting status, errorCode,
// retryable, the compute member's own words where the message carries them,
// and — on the collection doors — whether the "item 0: " prefix reaches the
// client.
func TestCalloutEndpoints_EveryDoorEveryCode(t *testing.T) {
	// One try plus one retry, and no patience: a callout with no member fails
	// at once, and a repeat-safe one that lost an answer gets a second member.
	h := newCalloutHarness(t, calloutTuning(1, 0))
	sfx := randSuffix(t) // repeated runs (go test -count=N) share this package's Postgres testcontainer

	for _, door := range calloutDoors() {
		t.Run(door.name, func(t *testing.T) {
			for _, tc := range calloutCases() {
				t.Run(tc.name, func(t *testing.T) {
					// Model, workflow and tag are unique per cell: the stack is
					// shared and a repeated run shares the database.
					base := fmt.Sprintf("%s-%s-%s", doorKey(door.name), tc.name, sfx)
					tag, model, wfName := "tag-"+base, "m-"+base, "wf-"+base

					members := make([]*scriptedCnode, 0, tc.members)
					for i := 0; i < tc.members; i++ {
						members = append(members, h.AttachCnode(t, cnodeSpec{
							name: fmt.Sprintf("m%d", i), tags: []string{tag},
							script: scriptAlways(tc.reply),
						}))
					}
					entityID := door.setup(t, h, model, wfName, tag, tc)

					status, body := door.drive(t, h, model, entityID)
					pd := assertProblem(t, status, body, tc.wantStatus, tc.wantCode, tc.wantRetryable)
					if tc.wantMemberText != "" && !strings.Contains(pd.Detail, tc.wantMemberText) {
						t.Errorf("detail = %q; want it to carry the compute member's own %q", pd.Detail, tc.wantMemberText)
					}
					assertNoDoorLeak(t, pd.Detail)
					if door.collection {
						if got := strings.Contains(pd.Detail, "item 0: "); got != tc.wantItemPrefix {
							t.Errorf("detail = %q; \"item 0: \" present = %t, want %t", pd.Detail, got, tc.wantItemPrefix)
						}
					}
					for i, m := range members {
						if got := m.Received(); len(got) != tc.wantPerMember {
							t.Errorf("compute member %d received %d callouts; want %d: %v", i, len(got), tc.wantPerMember, got)
						}
					}
				})
			}
		})
	}
}

// assertNoDoorLeak fails t if a door's message names the node rather than the
// tenant's own compute member: node ids and addresses are never client text.
func assertNoDoorLeak(t *testing.T, detail string) {
	t.Helper()
	for _, leak := range []string{"127.0.0.1", "localhost", "goroutine"} {
		if strings.Contains(detail, leak) {
			t.Errorf("detail = %q; it leaks %q", detail, leak)
		}
	}
}

// doorKey is a door's name reduced to what a model name may carry.
func doorKey(name string) string { return strings.ReplaceAll(name, "-", "") }

// calloutDoors is the five doors of §8.2 that the plain create does not cover.
func calloutDoors() []calloutDoor {
	// sampleReady is workflowSampleModel with the status the loopback door's
	// automated transition waits for.
	const sampleReady = `{"name": "Test Order", "amount": 100, "status": "ready"}`

	return []calloutDoor{
		{
			name: "create-collection", collection: true,
			setup: func(t *testing.T, h *callbackHarness, model, wfName, tag string, tc calloutCase) string {
				h.SetupModelWithWorkflow(t, model, chainWorkflowJSON(wfName,
					procSpec{"proc", "SYNC", calloutProcConfig(tag, tc)}))
				return ""
			},
			drive: func(t *testing.T, h *callbackHarness, model, _ string) (int, string) {
				return doorRequest(t, h, http.MethodPost, "/api/entity/JSON",
					collectionCreateItems(model, workflowSampleModel))
			},
		},
		{
			name: "update-transition",
			setup: func(t *testing.T, h *callbackHarness, model, wfName, tag string, tc calloutCase) string {
				h.SetupModelWithWorkflow(t, model, manualProcWorkflowJSON(wfName, calloutProcConfig(tag, tc)))
				return createQuietEntity(t, h, model, workflowSampleModel)
			},
			drive: func(t *testing.T, h *callbackHarness, _, entityID string) (int, string) {
				return doorRequest(t, h, http.MethodPut, "/api/entity/JSON/"+entityID+"/go", workflowSampleModel)
			},
		},
		{
			name: "loopback",
			setup: func(t *testing.T, h *callbackHarness, model, wfName, tag string, tc calloutCase) string {
				h.SetupModelWithWorkflow(t, model, readyProcWorkflowJSON(wfName, calloutProcConfig(tag, tc)))
				return createQuietEntity(t, h, model, workflowSampleModel)
			},
			drive: func(t *testing.T, h *callbackHarness, _, entityID string) (int, string) {
				return doorRequest(t, h, http.MethodPut, "/api/entity/JSON/"+entityID, sampleReady)
			},
		},
		{
			name: "update-collection", collection: true,
			setup: func(t *testing.T, h *callbackHarness, model, wfName, tag string, tc calloutCase) string {
				h.SetupModelWithWorkflow(t, model, manualProcWorkflowJSON(wfName, calloutProcConfig(tag, tc)))
				return createQuietEntity(t, h, model, workflowSampleModel)
			},
			drive: func(t *testing.T, h *callbackHarness, _, entityID string) (int, string) {
				return doorRequest(t, h, http.MethodPut, "/api/entity/JSON",
					collectionUpdateItems(entityID, "go", workflowSampleModel))
			},
		},
		{
			name: "transitions",
			setup: func(t *testing.T, h *callbackHarness, model, wfName, tag string, tc calloutCase) string {
				// The entity is created under a workflow with no criterion —
				// nothing to call out to — and the criterion arrives after it
				// exists, so only the query below fires the callout.
				h.SetupModelWithWorkflow(t, model, manualNoProcWorkflowJSON(wfName))
				id := createQuietEntity(t, h, model, workflowSampleModel)
				resp := h.DoAuth(t, http.MethodPost, fmt.Sprintf("/api/model/%s/1/workflow/import", model),
					workflowCriterionWorkflowJSON(wfName, "crit", calloutCriterionConfig(tag, tc)), "")
				if body := h.readBody(t, resp); resp.StatusCode != http.StatusOK {
					t.Fatalf("re-import workflow %s: %d %s", model, resp.StatusCode, body)
				}
				return id
			},
			drive: func(t *testing.T, h *callbackHarness, _, entityID string) (int, string) {
				return doorRequest(t, h, http.MethodGet, "/api/entity/"+entityID+"/transitions", "")
			},
		},
	}
}

// doorRequest makes one client-facing request and returns its status and body.
func doorRequest(t *testing.T, h *callbackHarness, method, path, body string) (int, string) {
	t.Helper()
	resp := h.DoAuth(t, method, path, body, "")
	return resp.StatusCode, h.readBody(t, resp)
}

// createQuietEntity creates an entity whose workflow makes no callout and
// returns its id.
func createQuietEntity(t *testing.T, h *callbackHarness, model, payload string) string {
	t.Helper()
	id, status, body := h.CreateEntity(t, model, 1, payload)
	if status != http.StatusOK {
		t.Fatalf("create the entity the door acts on: %d %s", status, body)
	}
	return id
}

// calloutProcConfig is a processor config for one case: the case's tag, a
// short answer limit, its retryPolicy and its idempotent declaration.
func calloutProcConfig(tag string, tc calloutCase) map[string]any {
	cfg := map[string]any{"calculationNodesTags": tag, "responseTimeoutMs": 300}
	if tc.policy != "" {
		cfg["retryPolicy"] = tc.policy
	}
	if tc.idempotent {
		cfg["idempotent"] = true
	}
	return cfg
}

// calloutCriterionConfig is calloutProcConfig for a criterion: a criterion is
// repeat-safe by rule and its config has no idempotent.
func calloutCriterionConfig(tag string, tc calloutCase) map[string]any {
	cfg := map[string]any{"calculationNodesTags": tag, "responseTimeoutMs": 300}
	if tc.policy != "" {
		cfg["retryPolicy"] = tc.policy
	}
	return cfg
}

// manualProcWorkflowJSON builds NONE -[go]-> DONE with one MANUAL transition
// carrying one SYNC processor: creating an entity fires nothing, and the
// transition is what the update doors ask for.
func manualProcWorkflowJSON(wfName string, cfg map[string]any) string {
	return workflowDocJSON(wfName, map[string]any{
		"NONE": map[string]any{"transitions": []any{map[string]any{
			"name": "go", "next": "DONE", "manual": true,
			"processors": []any{map[string]any{
				"type": "calculator", "name": "proc", "executionMode": "SYNC",
				"config": mergeConfig(cfg),
			}},
		}}},
		"DONE": map[string]any{},
	})
}

// manualNoProcWorkflowJSON is manualProcWorkflowJSON with no processor: a
// workflow that makes no callout at all.
func manualNoProcWorkflowJSON(wfName string) string {
	return workflowDocJSON(wfName, map[string]any{
		"NONE": map[string]any{"transitions": []any{map[string]any{
			"name": "go", "next": "DONE", "manual": true,
		}}},
		"DONE": map[string]any{},
	})
}

// readyProcWorkflowJSON builds NONE -[auto]-> DONE with one AUTOMATED
// transition guarded by an inline criterion on the payload: an entity created
// with any other status stays in NONE without a callout, and the loopback that
// writes "ready" is what fires it.
func readyProcWorkflowJSON(wfName string, cfg map[string]any) string {
	return workflowDocJSON(wfName, map[string]any{
		"NONE": map[string]any{"transitions": []any{map[string]any{
			"name": "auto", "next": "DONE", "manual": false,
			"criterion": map[string]any{
				"type": "simple", "jsonPath": "$.status", "operatorType": "EQUALS", "value": "ready",
			},
			"processors": []any{map[string]any{
				"type": "calculator", "name": "proc", "executionMode": "SYNC",
				"config": mergeConfig(cfg),
			}},
		}}},
		"DONE": map[string]any{},
	})
}

// workflowCriterionWorkflowJSON builds a workflow whose own criterion is a
// FUNCTION criterion — the one callout the transitions query makes — over the
// same NONE -[go]-> DONE states.
func workflowCriterionWorkflowJSON(wfName, critName string, config map[string]any) string {
	return workflowDocWithCriterionJSON(wfName,
		map[string]any{"type": "function", "function": map[string]any{"name": critName, "config": config}},
		map[string]any{
			"NONE": map[string]any{"transitions": []any{map[string]any{
				"name": "go", "next": "DONE", "manual": true,
			}}},
			"DONE": map[string]any{},
		})
}

// mergeConfig is the processor-config default every door's processor carries,
// with the case's own config over it.
func mergeConfig(cfg map[string]any) map[string]any {
	out := map[string]any{"attachEntity": true}
	for k, v := range cfg {
		out[k] = v
	}
	return out
}

// collectionCreateItems is a one-item POST /api/entity/JSON body. The item's
// payload is a JSON-encoded STRING, as the collection doors' wire contract
// requires.
func collectionCreateItems(model, payload string) string {
	b, _ := json.Marshal([]any{map[string]any{
		"model":   map[string]any{"name": model, "version": 1},
		"payload": payload,
	}})
	return string(b)
}

// collectionUpdateItems is a one-item PUT /api/entity/JSON body.
func collectionUpdateItems(entityID, transition, payload string) string {
	b, _ := json.Marshal([]any{map[string]any{
		"id": entityID, "transition": transition, "payload": payload,
	}})
	return string(b)
}
