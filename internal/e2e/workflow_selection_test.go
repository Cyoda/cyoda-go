package e2e_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// Workflow-level selection is criterion-driven and applies on every engine
// door, not just creation (`cyoda help workflows`). These tests drive the
// full HTTP stack against a model carrying two active workflows that declare
// THE SAME STATE NAMES — the normal shape for a per-kind machine —
// distinguished only by their `criterion`.
//
// kind-a-wf is declared first, so a resolver that picks "the first active
// workflow declaring the entity's current state" returns it for every
// entity. Each workflow's transitions land in a differently-named target
// state, so the observed state alone identifies which definition ran. The
// criteria are inline predicates over the payload — no compute node needed.

// wfSelectionSample declares every field the selection criteria read. A
// locked model rejects undeclared fields, so `kind` and `go` must both be
// present with representative values.
const wfSelectionSample = `{"name": "Test", "kind": "a", "go": false}`

// wfSelectionWorkflows is the two-workflow import used by the tests below.
// Both declare NONE / VALIDATE; only the target states differ.
const wfSelectionWorkflows = `{
	"importMode": "REPLACE",
	"workflows": [
		{
			"version": "1.1", "name": "kind-a-wf", "initialState": "NONE", "active": true,
			"criterion": {"type": "simple", "jsonPath": "$.kind", "operatorType": "EQUALS", "value": "a"},
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "VALIDATE", "manual": false}]},
				"VALIDATE": {"transitions": [
					{"name": "check", "next": "A_CHECKED", "manual": true},
					{"name": "a-advance", "next": "A_ADVANCED", "manual": false,
						"criterion": {"type": "simple", "jsonPath": "$.go", "operatorType": "EQUALS", "value": true}}
				]},
				"A_CHECKED": {},
				"A_ADVANCED": {}
			}
		},
		{
			"version": "1.1", "name": "kind-b-wf", "initialState": "NONE", "active": true,
			"criterion": {"type": "simple", "jsonPath": "$.kind", "operatorType": "EQUALS", "value": "b"},
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "VALIDATE", "manual": false}]},
				"VALIDATE": {"transitions": [
					{"name": "check", "next": "B_CHECKED", "manual": true},
					{"name": "b-advance", "next": "B_ADVANCED", "manual": false,
						"criterion": {"type": "simple", "jsonPath": "$.go", "operatorType": "EQUALS", "value": true}}
				]},
				"B_CHECKED": {},
				"B_ADVANCED": {}
			}
		}
	]
}`

// TestWorkflowSelection_ManualTransitionUsesCriterionSelectedWorkflow is the
// end-to-end reproduction of the reported defect: a manual transition on a
// multi-workflow model must fire the definition the entity's criterion
// selects. A_CHECKED here would mean the first workflow declaring VALIDATE
// was used for a `kind=b` entity.
func TestWorkflowSelection_ManualTransitionUsesCriterionSelectedWorkflow(t *testing.T) {
	const model = "e2e-wfsel-manual"
	setupModelSampleWithWorkflow(t, model, wfSelectionSample, wfSelectionWorkflows)

	entityID := createEntityE2E(t, model, 1, `{"name":"Test","kind":"b","go":false}`)
	if got := getEntityState(t, entityID); got != "VALIDATE" {
		t.Fatalf("state after create = %q, want VALIDATE", got)
	}

	updateEntityE2E(t, entityID, "check", `{"name":"Test","kind":"b","go":false}`)

	if got := getEntityState(t, entityID); got != "B_CHECKED" {
		t.Errorf("state after manual transition = %q, want B_CHECKED (kind-b-wf)", got)
	}
}

// TestWorkflowSelection_ManualTransitionRecordsSelectionAudit asserts the
// manual door records the same WORKFLOW_SKIP / WORKFLOW_FOUND trail the
// creation door does, so the audit shows which definition ran.
func TestWorkflowSelection_ManualTransitionRecordsSelectionAudit(t *testing.T) {
	const model = "e2e-wfsel-audit"
	setupModelSampleWithWorkflow(t, model, wfSelectionSample, wfSelectionWorkflows)

	entityID := createEntityE2E(t, model, 1, `{"name":"Test","kind":"b","go":false}`)
	before := countEventTypes(t, entityID)

	updateEntityE2E(t, entityID, "check", `{"name":"Test","kind":"b","go":false}`)
	after := countEventTypes(t, entityID)

	if got := after["WORKFLOW_SKIP"] - before["WORKFLOW_SKIP"]; got != 1 {
		t.Errorf("WORKFLOW_SKIP events added by the manual transition = %d, want 1 (kind-a-wf)", got)
	}
	if got := after["WORKFLOW_FOUND"] - before["WORKFLOW_FOUND"]; got != 1 {
		t.Errorf("WORKFLOW_FOUND events added by the manual transition = %d, want 1 (kind-b-wf)", got)
	}
}

// TestWorkflowSelection_LoopbackUsesCriterionSelectedWorkflow is the
// automated-cascade counterpart, driven through a transition-less PUT.
func TestWorkflowSelection_LoopbackUsesCriterionSelectedWorkflow(t *testing.T) {
	const model = "e2e-wfsel-loopback"
	setupModelSampleWithWorkflow(t, model, wfSelectionSample, wfSelectionWorkflows)

	entityID := createEntityE2E(t, model, 1, `{"name":"Test","kind":"b","go":false}`)
	if got := getEntityState(t, entityID); got != "VALIDATE" {
		t.Fatalf("state after create = %q, want VALIDATE", got)
	}

	// Transition-less PUT: the engine re-evaluates automated transitions
	// from the current state. go=true now satisfies the `b-advance` criterion.
	path := fmt.Sprintf("/api/entity/JSON/%s", entityID)
	resp := doAuth(t, http.MethodPut, path, `{"name":"Test","kind":"b","go":true}`)
	if body := readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("loopback update: expected 200, got %d: %s", resp.StatusCode, body)
	}

	if got := getEntityState(t, entityID); got != "B_ADVANCED" {
		t.Errorf("state after loopback = %q, want B_ADVANCED (kind-b-wf)", got)
	}
}

// TestWorkflowSelection_TransitionsQueryUsesCriterionSelectedWorkflow pins
// that GET /entity/{id}/transitions answers for the same definition a
// subsequent transition will run — the automated transition is named per
// workflow (a-advance / b-advance), so the returned names identify it — and
// that the query records no audit events of its own.
func TestWorkflowSelection_TransitionsQueryUsesCriterionSelectedWorkflow(t *testing.T) {
	const model = "e2e-wfsel-query"
	setupModelSampleWithWorkflow(t, model, wfSelectionSample, wfSelectionWorkflows)

	entityID := createEntityE2E(t, model, 1, `{"name":"Test","kind":"b","go":false}`)

	resp := doAuth(t, http.MethodGet, fmt.Sprintf("/api/entity/%s/transitions", entityID), "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET transitions: expected 200, got %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "b-advance") || strings.Contains(body, "a-advance") {
		t.Errorf("transitions = %s, want kind-b-wf's VALIDATE transitions (check, b-advance)", body)
	}

	// A read must not write: the query records no state-machine audit
	// events of its own.
	before := countEventTypes(t, entityID)
	resp = doAuth(t, http.MethodGet, fmt.Sprintf("/api/entity/%s/transitions", entityID), "")
	readBody(t, resp)
	after := countEventTypes(t, entityID)
	for et, n := range after {
		if n != before[et] {
			t.Errorf("transitions query added %d %s audit event(s); a read must record none", n-before[et], et)
		}
	}
}

// TestWorkflowSelection_StateAbsentFromSelectedWorkflowIsRejected asserts
// the engine does not hop to a definition that merely declares the state.
// The `kind=a` entity selects kind-a-only-wf, whose ONLY state is NONE;
// kind-b-wf declares the ORPHAN state the entity is parked in. Firing must
// be rejected, not silently served by kind-b-wf.
func TestWorkflowSelection_StateAbsentFromSelectedWorkflowIsRejected(t *testing.T) {
	const model = "e2e-wfsel-orphan"

	// kind-b-wf is declared FIRST so a state-based resolver picks it for
	// every entity, including the kind=a one created below.
	const wf = `{
		"importMode": "REPLACE",
		"workflows": [
			{
				"version": "1.1", "name": "kind-b-wf", "initialState": "NONE", "active": true,
				"criterion": {"type": "simple", "jsonPath": "$.kind", "operatorType": "EQUALS", "value": "b"},
				"states": {
					"NONE": {"transitions": [{"name": "init", "next": "ORPHAN", "manual": false}]},
					"ORPHAN": {"transitions": [{"name": "check", "next": "B_CHECKED", "manual": true}]},
					"B_CHECKED": {}
				}
			},
			{
				"version": "1.1", "name": "kind-a-wf", "initialState": "NONE", "active": true,
				"criterion": {"type": "simple", "jsonPath": "$.kind", "operatorType": "EQUALS", "value": "a"},
				"states": {"NONE": {}}
			}
		]
	}`
	setupModelSampleWithWorkflow(t, model, wfSelectionSample, wf)

	// kind=b reaches ORPHAN through its own workflow; then flip the payload
	// to kind=a so the entity re-binds to kind-a-wf, which has no ORPHAN.
	entityID := createEntityE2E(t, model, 1, `{"name":"Test","kind":"b","go":false}`)
	if got := getEntityState(t, entityID); got != "ORPHAN" {
		t.Fatalf("state after create = %q, want ORPHAN", got)
	}

	path := fmt.Sprintf("/api/entity/JSON/%s/check", entityID)
	resp := doAuth(t, http.MethodPut, path, `{"name":"Test","kind":"a","go":false}`)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("manual transition on a state absent from the selected workflow: "+
			"expected 400, got %d: %s", resp.StatusCode, body)
	}
	if got := getEntityState(t, entityID); got != "ORPHAN" {
		t.Errorf("state = %q, want ORPHAN (rejected transitions must not advance the entity)", got)
	}
}

// countEventTypes returns a histogram of an entity's state-machine audit
// event types. The limit is raised well above the endpoint default so a
// multi-write entity's whole history is counted.
func countEventTypes(t *testing.T, entityID string) map[string]int {
	t.Helper()
	counts := make(map[string]int)
	for _, ev := range getSMAuditEventsWithLimit(t, entityID, 200) {
		if et, ok := ev["eventType"].(string); ok {
			counts[et]++
		}
	}
	return counts
}
