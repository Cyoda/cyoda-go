package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"testing"
)

// audit_event_data_test.go — the typed `data` of state-machine audit events,
// through the full HTTP stack on PostgreSQL. The enforce-mode OpenAPI
// validator checks each response's shape; these tests assert the values.

// onlySMData returns the data of the single event of eventType.
func onlySMData(t *testing.T, events []map[string]any, eventType string) map[string]any {
	t.Helper()
	found := smEventsOfType(events, eventType)
	if len(found) != 1 {
		t.Fatalf("%d %s events, want 1: %v", len(found), eventType, events)
	}
	data, ok := found[0]["data"].(map[string]any)
	if !ok {
		t.Fatalf("%s data = %T (%v), want an object", eventType, found[0]["data"], found[0]["data"])
	}
	return data
}

func TestAudit_CreateEventData(t *testing.T) {
	const model = "e2e-audit-create-data"
	setupModelWithWorkflow(t, model, `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "create-data-wf", "initialState": "NEW", "active": true,
			"states": {
				"NEW":  {"transitions": [{"name": "go", "next": "DONE", "manual": false}]},
				"DONE": {"transitions": [{"name": "back", "next": "NEW", "manual": true}]}
			}
		}]
	}`)
	id := createEntityE2E(t, model, 1, `{"name":"A","amount":1,"status":"new"}`)
	events := getSMAuditEvents(t, id)

	if d := onlySMData(t, events, "STATE_MACHINE_START"); d["state"] != nil {
		t.Errorf("START data = %v, want no state on create", d)
	}
	if d := onlySMData(t, events, "WORKFLOW_FOUND"); d["workflowName"] != "create-data-wf" {
		t.Errorf("WORKFLOW_FOUND data = %v, want workflowName create-data-wf", d)
	}
	if d := onlySMData(t, events, "TRANSITION_MAKE"); d["newState"] != "DONE" || d["transition"] != "go" || d["workflowName"] != "create-data-wf" {
		t.Errorf("TRANSITION_MAKE data = %v, want go to DONE in create-data-wf", d)
	}
	finish := onlySMData(t, events, "STATE_MACHINE_FINISH")
	want := map[string]any{
		"eventType": "STATE_MACHINE_FINISH",
		"state":     "DONE",
		"success":   true,
		"stopReason": map[string]any{
			"type":        "COMPLETED_NO_AUTO_TRANSITIONS",
			"description": `No automated transition from state "DONE"`,
		},
	}
	if !reflect.DeepEqual(finish, want) {
		t.Errorf("FINISH data = %v, want %v", finish, want)
	}
}

func TestAudit_FinishAllCriteriaRejected(t *testing.T) {
	const model = "e2e-audit-all-rejected"
	setupModelWithWorkflow(t, model, `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "rejected-wf", "initialState": "A", "active": true,
			"states": {
				"A": {"transitions": [
					{"name": "a-refused", "next": "C", "manual": false,
						"criterion": {"type":"simple","jsonPath":"$.amount","operatorType":"GREATER_THAN","value":1000}},
					{"name": "a-go", "next": "B", "manual": false}
				]},
				"B": {"transitions": [
					{"name": "b-first", "next": "C", "manual": false,
						"criterion": {"type":"simple","jsonPath":"$.amount","operatorType":"GREATER_THAN","value":1000}},
					{"name": "b-manual", "next": "C", "manual": true},
					{"name": "b-second", "next": "C", "manual": false,
						"criterion": {"type":"simple","jsonPath":"$.amount","operatorType":"GREATER_THAN","value":5000}}
				]},
				"C": {}
			}
		}]
	}`)
	id := createEntityE2E(t, model, 1, `{"name":"A","amount":1,"status":"new"}`)
	events := getSMAuditEvents(t, id)

	// Oldest first: A's refusal, then B's two.
	refusals := smEventsOfType(events, "TRANSITION_NOT_MATCH_CRITERION")
	if len(refusals) != 3 {
		t.Fatalf("%d TRANSITION_NOT_MATCH_CRITERION events, want 3: %v", len(refusals), events)
	}
	var wantRejected []any
	for _, ev := range refusals[1:] {
		d, _ := ev["data"].(map[string]any)
		wantRejected = append(wantRejected, map[string]any{
			"transitionName": d["transition"], "criterion": d["criterion"], "reason": d["reason"],
		})
	}
	stop, _ := onlySMData(t, events, "STATE_MACHINE_FINISH")["stopReason"].(map[string]any)
	want := map[string]any{
		"type":                "COMPLETED_ALL_CRITERIA_REJECTED",
		"description":         `Every automated transition from state "B" was refused by its criterion`,
		"rejectedTransitions": wantRejected,
	}
	if !reflect.DeepEqual(stop, want) {
		t.Errorf("stopReason = %v, want %v", stop, want)
	}
	if names := []any{wantRejected[0].(map[string]any)["transitionName"], wantRejected[1].(map[string]any)["transitionName"]}; !reflect.DeepEqual(names, []any{"b-first", "b-second"}) {
		t.Errorf("rejectedTransitions names = %v, want b-first then b-second (A's refusal not listed)", names)
	}
}

// TestAudit_LoopbackAbortHasNoTransition: a stale ifMatch on a batch item with
// no transition is a loopback, which has no transition, so TRANSITION_ABORTED
// carries none. The isolated item's events survive the chunk's commit.
func TestAudit_LoopbackAbortHasNoTransition(t *testing.T) {
	const model = "e2e-audit-loopback-abort"
	setupModelWithWorkflow(t, model, trivialModelWF())
	id := createEntityE2E(t, model, 1, `{"name":"A","amount":1,"status":"new"}`)
	actual := getEntityTxID(t, id)
	const stale = "00000000-0000-0000-0000-000000000000"

	status, body := putUpdateCollection(t, []string{
		updateCollectionItem(id, `{"name":"A_STALE","amount":2,"status":"upd"}`, stale),
	}, "")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", status, body)
	}
	assertTransitionAbortedPaired(t, id, stale, actual, "")
}

// TestAudit_DataEventTypeEqualsOuter: every state-machine event, on the
// search and on …/finished, has `data`, and `data.eventType` is the event's
// own `eventType`.
func TestAudit_DataEventTypeEqualsOuter(t *testing.T) {
	const model = "e2e-audit-data-event-type"
	setupModelWithWorkflow(t, model, `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "event-type-wf", "initialState": "NEW", "active": true,
			"states": {
				"NEW":  {"transitions": [
					{"name": "never", "next": "DONE", "manual": false,
						"criterion": {"type":"simple","jsonPath":"$.amount","operatorType":"GREATER_THAN","value":1000}},
					{"name": "go", "next": "DONE", "manual": false}
				]},
				"DONE": {}
			}
		}]
	}`)
	id, txID := createEntityE2EWithTxID(t, model, 1, `{"name":"A","amount":1,"status":"new"}`)

	items, _ := auditPage(t, id, "limit=100")
	var sm int
	for _, item := range items {
		if item["auditEventType"] != "StateMachine" {
			continue
		}
		sm++
		assertDataEventType(t, item)
	}
	if sm == 0 {
		t.Fatalf("no state-machine events for %s: %v", id, items)
	}

	resp := doAuth(t, http.MethodGet, fmt.Sprintf("/api/audit/entity/%s/workflow/%s/finished", id, txID), "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("finished: status %d: %s", resp.StatusCode, body)
	}
	var finished map[string]any
	if err := json.Unmarshal([]byte(body), &finished); err != nil {
		t.Fatalf("finished: decode: %v; body %s", err, body)
	}
	if finished["eventType"] != "STATE_MACHINE_FINISH" {
		t.Errorf("finished eventType = %v, want STATE_MACHINE_FINISH", finished["eventType"])
	}
	assertDataEventType(t, finished)
}

func assertDataEventType(t *testing.T, event map[string]any) {
	t.Helper()
	data, ok := event["data"].(map[string]any)
	if !ok {
		t.Errorf("event %v: data = %T, want an object", event["eventType"], event["data"])
		return
	}
	if data["eventType"] != event["eventType"] {
		t.Errorf("data.eventType = %v, want the outer eventType %v", data["eventType"], event["eventType"])
	}
}
