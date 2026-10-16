package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"
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

	// The refusals are found by transition name: events of one instant are
	// not listed in recording order.
	refusals := map[any]map[string]any{}
	for _, ev := range smEventsOfType(events, "TRANSITION_NOT_MATCH_CRITERION") {
		d, _ := ev["data"].(map[string]any)
		refusals[d["transition"]] = d
	}
	if len(refusals) != 3 || refusals["a-refused"] == nil || refusals["b-first"] == nil || refusals["b-second"] == nil {
		t.Fatalf("TRANSITION_NOT_MATCH_CRITERION events = %v, want one each for a-refused, b-first, b-second", refusals)
	}
	// Only B's refusals, in declaration order.
	var wantRejected []any
	for _, name := range []string{"b-first", "b-second"} {
		d := refusals[name]
		wantRejected = append(wantRejected, map[string]any{
			"transitionName": name, "criterion": d["criterion"], "reason": d["reason"],
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

// processorRecordsByName returns the data of each STATE_PROCESS_RESULT event,
// keyed by processorName: events of one instant are not listed in recording
// order. It fails on a second record for one processor.
func processorRecordsByName(t *testing.T, events []map[string]any) map[any]map[string]any {
	t.Helper()
	out := map[any]map[string]any{}
	for _, ev := range smEventsOfType(events, "STATE_PROCESS_RESULT") {
		d, _ := ev["data"].(map[string]any)
		if _, dup := out[d["processorName"]]; dup {
			t.Errorf("a second STATE_PROCESS_RESULT for processor %v: %v", d["processorName"], events)
		}
		out[d["processorName"]] = d
	}
	return out
}

// TestAudit_ProcessorRecord: each processor run is one STATE_PROCESS_RESULT
// with its name and mode; failReason is the compute member's own failure text
// and nothing else. An ASYNC_NEW_TX processor's failure does not stop the
// run, so its record is durable.
func TestAudit_ProcessorRecord(t *testing.T) {
	h := newCalloutHarness(t, nil)
	sfx := randSuffix(t)

	asyncFail := func(t *testing.T, name, message string) map[string]any {
		t.Helper()
		model, tag := "proc-record-"+name+"-"+sfx, "proc-record-"+name+"-tag-"+sfx
		h.AttachCnode(t, cnodeSpec{name: "proc-record-" + name, tags: []string{tag}, script: scriptAlways(answerFail(message))})
		h.SetupModelWithWorkflow(t, model, procWorkflowJSON("proc-record-wf-"+name+"-"+sfx, "charge", "ASYNC_NEW_TX",
			map[string]any{"calculationNodesTags": tag}))
		id, status, body := h.CreateEntity(t, model, 1, workflowSampleModel)
		if status != http.StatusOK {
			t.Fatalf("create behind a failing ASYNC_NEW_TX processor: %d %s; want 200, the run continues", status, body)
		}
		requireState(t, h, id, "DONE")
		records := processorRecordsByName(t, h.GetSMAuditEvents(t, id))
		if len(records) != 1 || records["charge"] == nil {
			t.Fatalf("STATE_PROCESS_RESULT records = %v, want one for charge", records)
		}
		return records["charge"]
	}

	t.Run("member text is the failReason", func(t *testing.T) {
		got := asyncFail(t, "text", "card declined")
		want := map[string]any{"eventType": "STATE_PROCESS_RESULT", "processorName": "charge",
			"executionMode": "ASYNC_NEW_TX", "success": false, "failReason": "card declined"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("record = %v, want %v", got, want)
		}
	})

	t.Run("member sent no text", func(t *testing.T) {
		got := asyncFail(t, "notext", "")
		want := map[string]any{"eventType": "STATE_PROCESS_RESULT", "processorName": "charge",
			"executionMode": "ASYNC_NEW_TX", "success": false}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("record = %v, want %v (no failReason)", got, want)
		}
	})

	t.Run("names in order, empty mode is SYNC", func(t *testing.T) {
		model, tag := "proc-record-two-"+sfx, "proc-record-two-tag-"+sfx
		h.AttachCnode(t, cnodeSpec{name: "proc-record-two", tags: []string{tag}})
		unset := sProc("unset-mode", "", tag, true)
		delete(unset, "executionMode")
		h.SetupModelWithWorkflow(t, model, schedDoc("proc-record-two-wf-"+sfx, map[string]any{
			"Open": map[string]any{"transitions": []any{map[string]any{"name": "Go", "next": "Done", "manual": false,
				"processors": []any{sProc("first", "ASYNC_SAME_TX", tag, true), unset}}}},
			"Done": map[string]any{},
		}))
		id := createOpen(t, h, model, workflowSampleModel)
		requireState(t, h, id, "Done")

		events := h.GetSMAuditEvents(t, id)
		paused := onlySMData(t, events, "PAUSE_FOR_PROCESSING")
		if got, want := paused["processNames"], []any{"first", "unset-mode"}; !reflect.DeepEqual(got, want) {
			t.Errorf("PAUSE_FOR_PROCESSING processNames = %v, want %v", got, want)
		}
		records := processorRecordsByName(t, events)
		if len(records) != 2 {
			t.Fatalf("STATE_PROCESS_RESULT records = %v, want two", records)
		}
		if got := records["first"]["executionMode"]; got != "ASYNC_SAME_TX" {
			t.Errorf("first executionMode = %v, want ASYNC_SAME_TX", got)
		}
		want := map[string]any{"eventType": "STATE_PROCESS_RESULT", "processorName": "unset-mode",
			"executionMode": "SYNC", "success": true}
		if got := records["unset-mode"]; !reflect.DeepEqual(got, want) {
			t.Errorf("unset-mode record = %v, want %v", got, want)
		}
	})
}

// TestAudit_ProcessorRecord_CommitBeforeDispatchFailure: a
// COMMIT_BEFORE_DISPATCH processor whose compute member answers failure fails
// the operation, after its segment boundary committed the entity and the
// events before the callout. Its result is still recorded: the trail shows
// PAUSE_FOR_PROCESSING followed by one failed STATE_PROCESS_RESULT carrying
// the member's text, under the cascade's transaction id.
func TestAudit_ProcessorRecord_CommitBeforeDispatchFailure(t *testing.T) {
	h := newCalloutHarness(t, nil)
	sfx := randSuffix(t)
	for _, newTx := range []bool{false, true} {
		t.Run(fmt.Sprintf("startNewTxOnDispatch=%t", newTx), func(t *testing.T) {
			name := fmt.Sprintf("cbd-record-%t-%s", newTx, sfx)
			model, tag := name, name+"-tag"
			h.AttachCnode(t, cnodeSpec{name: name, tags: []string{tag}, script: scriptAlways(answerFail("card declined"))})
			h.SetupModelWithWorkflow(t, model, procWorkflowJSON(name+"-wf", "charge", "COMMIT_BEFORE_DISPATCH",
				map[string]any{"calculationNodesTags": tag, "startNewTxOnDispatch": newTx}))

			_, status, body := h.CreateEntity(t, model, 1, workflowSampleModel)
			if status == http.StatusOK {
				t.Fatalf("create: %d %s; want the processor's failure to fail the operation", status, body)
			}
			id := onlyEntityID(t, h, model)
			requireState(t, h, id, "NONE")

			events := h.GetSMAuditEvents(t, id)
			paused := smEventsOfType(events, "PAUSE_FOR_PROCESSING")
			results := smEventsOfType(events, "STATE_PROCESS_RESULT")
			if len(paused) != 1 || len(results) != 1 {
				t.Fatalf("PAUSE_FOR_PROCESSING = %d, STATE_PROCESS_RESULT = %d, want 1 each: %v", len(paused), len(results), events)
			}
			want := map[string]any{"eventType": "STATE_PROCESS_RESULT", "processorName": "charge",
				"executionMode": "COMMIT_BEFORE_DISPATCH", "success": false, "failReason": "card declined"}
			if got := results[0]["data"]; !reflect.DeepEqual(got, want) {
				t.Errorf("record = %v, want %v", got, want)
			}
			if results[0]["transactionId"] != paused[0]["transactionId"] {
				t.Errorf("record transactionId = %v, want the cascade's %v", results[0]["transactionId"], paused[0]["transactionId"])
			}
			pausedAt, resultAt := paused[0]["utcTime"].(string), results[0]["utcTime"].(string)
			if pt, rt := mustRFC3339(t, pausedAt), mustRFC3339(t, resultAt); rt.Before(pt) {
				t.Errorf("record at %s precedes the pause at %s", resultAt, pausedAt)
			}
		})
	}
}

// onlyEntityID returns the id of the single entity of model, version 1.
func onlyEntityID(t *testing.T, h *callbackHarness, model string) string {
	t.Helper()
	resp := h.DoAuth(t, http.MethodGet, fmt.Sprintf("/api/entity/%s/1", model), "", "")
	body := h.readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list %s: %d %s", model, resp.StatusCode, body)
	}
	var list []map[string]any
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("decode list of %s: %v (body: %s)", model, err, body)
	}
	if len(list) != 1 {
		t.Fatalf("%d entities of %s, want 1: %s", len(list), model, body)
	}
	meta, _ := list[0]["meta"].(map[string]any)
	id, _ := meta["id"].(string)
	if id == "" {
		t.Fatalf("no entity id in %s", body)
	}
	return id
}

func mustRFC3339(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("utcTime %q: %v", s, err)
	}
	return v
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
