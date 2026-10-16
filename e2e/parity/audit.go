package parity

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// RunAuditEntityHistory verifies that creating an entity with a workflow
// produces audit events (both EntityChange and StateMachine) that are
// retrievable via the audit REST API.
//
// Port of internal/e2e TestAudit_EntityCreationGeneratesEvents, using the
// discriminated-union audit response types:
//   - GetAuditEvents -> EntityAuditEventsResponse -> []AuditEvent
//   - AsStateMachine() / AsEntityChange() for typed subtype assertions
//
// Replaces the queryDB(sm_audit_events) check with API-based assertions.
func RunAuditEntityHistory(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "audit-entity-history"
	const modelVersion = 1

	// Setup: import model, lock, import workflow with NONE->CREATED auto-transition.
	setupSimpleWorkflow(t, c, modelName, modelVersion)

	// Create entity (triggers workflow: NONE -> CREATED).
	entityID, err := c.CreateEntity(t, modelName, modelVersion,
		`{"name":"AuditTest","amount":100,"status":"draft"}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	// Get audit events via REST API.
	auditResp, err := c.GetAuditEvents(t, entityID)
	if err != nil {
		t.Fatalf("GetAuditEvents: %v", err)
	}

	// Classify events by type.
	entityChangeCount := 0
	stateMachineCount := 0
	hasTransactionID := false
	hasStart := false
	hasFinish := false

	for i := range auditResp.Items {
		ev := &auditResp.Items[i]

		if ev.TransactionID != "" {
			hasTransactionID = true
		}

		switch ev.AuditEventType {
		case "EntityChange":
			entityChangeCount++
			ec, err := ev.AsEntityChange()
			if err != nil {
				t.Errorf("AsEntityChange on event %d: %v", i, err)
				continue
			}
			if ec.ChangeType == "" {
				t.Errorf("EntityChange event %d: changeType is empty", i)
			}

		case "StateMachine":
			stateMachineCount++
			sm, err := ev.AsStateMachine()
			if err != nil {
				t.Errorf("AsStateMachine on event %d: %v", i, err)
				continue
			}
			// SPI canonical values match the openapi spec eventType enum:
			// STATE_MACHINE_START and STATE_MACHINE_FINISH (spi.SMEventStarted/SMEventFinished).
			switch sm.EventType {
			case "STATE_MACHINE_START":
				hasStart = true
			case "STATE_MACHINE_FINISH":
				hasFinish = true
			}
		}
	}

	if entityChangeCount < 1 {
		t.Errorf("expected >= 1 EntityChange events, got %d", entityChangeCount)
	}
	if stateMachineCount < 2 {
		t.Errorf("expected >= 2 StateMachine events (START + FINISH), got %d", stateMachineCount)
	}
	if !hasStart {
		t.Error("missing STATE_MACHINE_START event")
	}
	if !hasFinish {
		t.Error("missing STATE_MACHINE_FINISH event")
	}
	if !hasTransactionID {
		t.Error("expected at least one audit event with a non-empty transactionId")
	}
}

// RunAuditWorkflowEvents verifies that audit events created during
// entity creation carry the transaction ID, enabling cross-referencing
// between entity versions and workflow events.
//
// Port of internal/e2e TestAudit_EventsHaveTransactionID.
func RunAuditWorkflowEvents(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "audit-workflow-events"
	const modelVersion = 1

	// Setup: import model, lock, import workflow.
	setupSimpleWorkflow(t, c, modelName, modelVersion)

	// Create entity.
	entityID, err := c.CreateEntity(t, modelName, modelVersion,
		`{"name":"TxAudit","amount":200,"status":"active"}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	// Get audit events.
	auditResp, err := c.GetAuditEvents(t, entityID)
	if err != nil {
		t.Fatalf("GetAuditEvents: %v", err)
	}

	// At least one event should have a transactionId.
	hasTransactionID := false
	for _, ev := range auditResp.Items {
		if ev.TransactionID != "" {
			hasTransactionID = true
			break
		}
	}
	if !hasTransactionID {
		t.Error("expected at least one audit event with a transactionId for cross-referencing")
	}
}

// RunAuditPostTxIdMatchesWorkflowFinished verifies that the transactionId
// returned by POST /entity can be used directly with
// /audit/entity/{id}/workflow/{txId}/finished to look up the workflow result.
// This confirms the behaviour holds across all storage backends.
func RunAuditPostTxIdMatchesWorkflowFinished(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "audit-txid-parity"
	const modelVersion = 1

	setupSimpleWorkflow(t, c, modelName, modelVersion)

	entityID, txID, err := c.CreateEntityWithTxID(t, modelName, modelVersion,
		`{"name":"TxParity","amount":50,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntityWithTxID: %v", err)
	}
	if txID == "" {
		t.Fatal("POST /entity returned empty transactionId")
	}

	status, result, err := c.GetWorkflowFinished(t, entityID, txID)
	if err != nil {
		t.Fatalf("GetWorkflowFinished with POST txId %q: status %d, err: %v", txID, status, err)
	}
	if status != 200 {
		t.Fatalf("expected 200 from workflow finished endpoint using POST txId, got %d", status)
	}
	if result["state"] != "CREATED" {
		t.Errorf("expected state=CREATED in workflow finished response, got %v", result["state"])
	}
	if result["eventType"] != "STATE_MACHINE_FINISH" {
		t.Errorf("finished eventType = %v, want STATE_MACHINE_FINISH", result["eventType"])
	}
	data, _ := result["data"].(map[string]any)
	stop, _ := data["stopReason"].(map[string]any)
	if stop["type"] != "COMPLETED_NO_AUTO_TRANSITIONS" {
		t.Errorf("finished data.stopReason = %v, want type COMPLETED_NO_AUTO_TRANSITIONS (data: %v)", stop, data)
	}
}

// stateMachineEvents returns the entity's state-machine audit events, oldest
// first, with each event's data decoded (numbers as json.Number).
func stateMachineEvents(t *testing.T, c *client.Client, id uuid.UUID) []decodedSMEvent {
	t.Helper()
	resp, err := c.GetAuditEvents(t, id)
	if err != nil {
		t.Fatalf("GetAuditEvents: %v", err)
	}
	var out []decodedSMEvent
	for i := len(resp.Items) - 1; i >= 0; i-- {
		ev := &resp.Items[i]
		if ev.AuditEventType != "StateMachine" {
			continue
		}
		sm, err := ev.AsStateMachine()
		if err != nil {
			t.Fatalf("decode state-machine audit event: %v", err)
		}
		d := decodedSMEvent{eventType: sm.EventType}
		if len(sm.Data) > 0 && string(sm.Data) != "null" {
			dec := json.NewDecoder(bytes.NewReader(sm.Data))
			dec.UseNumber()
			if err := dec.Decode(&d.data); err != nil {
				t.Fatalf("decode %s data: %v (data: %s)", sm.EventType, err, sm.Data)
			}
		}
		out = append(out, d)
	}
	return out
}

// decodedSMEvent is a state-machine audit event's type and decoded data; data
// is nil when the event carried none.
type decodedSMEvent struct {
	eventType string
	data      map[string]any
}

// onlySMEventData returns the data of the single event of eventType.
func onlySMEventData(t *testing.T, events []decodedSMEvent, eventType string) map[string]any {
	t.Helper()
	var found []decodedSMEvent
	for _, ev := range events {
		if ev.eventType == eventType {
			found = append(found, ev)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d %s events, want 1: %v", len(found), eventType, events)
	}
	return found[0].data
}

// RunAuditCreateEventData: the data the create path records — START without
// a state, WORKFLOW_FOUND naming the workflow, TRANSITION_MAKE with its new
// state, and FINISH with the stop reason COMPLETED_NO_AUTO_TRANSITIONS.
func RunAuditCreateEventData(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "audit-create-event-data"
	const modelVersion = 1
	setupSimpleWorkflow(t, c, modelName, modelVersion)

	id, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Data","amount":1,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	events := stateMachineEvents(t, c, id)

	if d := onlySMEventData(t, events, "STATE_MACHINE_START"); d == nil || d["state"] != nil {
		t.Errorf("START data = %v, want an object without state", d)
	}
	if d := onlySMEventData(t, events, "WORKFLOW_FOUND"); d["workflowName"] != "simple-wf" {
		t.Errorf("WORKFLOW_FOUND data = %v, want workflowName simple-wf", d)
	}
	if d := onlySMEventData(t, events, "TRANSITION_MAKE"); d["newState"] != "CREATED" || d["transition"] != "create" || d["workflowName"] != "simple-wf" {
		t.Errorf("TRANSITION_MAKE data = %v, want create to CREATED in simple-wf", d)
	}
	want := map[string]any{
		"eventType": "STATE_MACHINE_FINISH",
		"state":     "CREATED",
		"success":   true,
		"stopReason": map[string]any{
			"type":        "COMPLETED_NO_AUTO_TRANSITIONS",
			"description": `No automated transition from state "CREATED"`,
		},
	}
	if d := onlySMEventData(t, events, "STATE_MACHINE_FINISH"); !reflect.DeepEqual(d, want) {
		t.Errorf("FINISH data = %v, want %v", d, want)
	}
}

// rejectingWorkflowJSON refuses A's first automated transition, moves on to
// B, and refuses both of B's automated transitions.
const rejectingWorkflowJSON = `{
	"importMode": "REPLACE",
	"workflows": [{
		"version": "1.1", "name": "rejecting-wf", "initialState": "A", "active": true,
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
}`

// RunAuditFinishAllCriteriaRejected: FINISH carries the stop reason
// COMPLETED_ALL_CRITERIA_REJECTED with the refusals of the last pass only —
// B's two, in declaration order, with the criterion and reason of their
// TRANSITION_NOT_MATCH_CRITERION events; A's earlier refusal is not listed.
func RunAuditFinishAllCriteriaRejected(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "audit-finish-all-rejected"
	const modelVersion = 1
	if err := c.ImportModel(t, modelName, modelVersion, `{"name":"Test","amount":1,"status":"new"}`); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if err := c.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	if err := c.ImportWorkflow(t, modelName, modelVersion, rejectingWorkflowJSON); err != nil {
		t.Fatalf("ImportWorkflow: %v", err)
	}

	id, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Refused","amount":1,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	events := stateMachineEvents(t, c, id)

	var refusals []map[string]any
	for _, ev := range events {
		if ev.eventType == "TRANSITION_NOT_MATCH_CRITERION" {
			refusals = append(refusals, ev.data)
		}
	}
	if len(refusals) != 3 {
		t.Fatalf("%d TRANSITION_NOT_MATCH_CRITERION events, want 3 (A once, B twice): %v", len(refusals), events)
	}
	var rejected []any
	for _, d := range refusals[1:] {
		rejected = append(rejected, map[string]any{
			"transitionName": d["transition"], "criterion": d["criterion"], "reason": d["reason"],
		})
	}
	if refusals[1]["transition"] != "b-first" || refusals[2]["transition"] != "b-second" {
		t.Fatalf("B's refusals = %v, %v; want b-first then b-second", refusals[1]["transition"], refusals[2]["transition"])
	}
	want := map[string]any{
		"eventType": "STATE_MACHINE_FINISH",
		"state":     "B",
		"success":   true,
		"stopReason": map[string]any{
			"type":                "COMPLETED_ALL_CRITERIA_REJECTED",
			"description":         `Every automated transition from state "B" was refused by its criterion`,
			"rejectedTransitions": rejected,
		},
	}
	if d := onlySMEventData(t, events, "STATE_MACHINE_FINISH"); !reflect.DeepEqual(d, want) {
		t.Errorf("FINISH data = %v, want %v", d, want)
	}
}

// RunAuditDataEventTypeMatchesOuter: every state-machine event has data, on
// the search and on …/finished, and data.eventType is the event's own
// eventType.
func RunAuditDataEventTypeMatchesOuter(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "audit-data-event-type"
	const modelVersion = 1
	if err := c.ImportModel(t, modelName, modelVersion, `{"name":"Test","amount":1,"status":"new"}`); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if err := c.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	if err := c.ImportWorkflow(t, modelName, modelVersion, rejectingWorkflowJSON); err != nil {
		t.Fatalf("ImportWorkflow: %v", err)
	}

	id, txID, err := c.CreateEntityWithTxID(t, modelName, modelVersion, `{"name":"Types","amount":1,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntityWithTxID: %v", err)
	}
	events := stateMachineEvents(t, c, id)
	if len(events) == 0 {
		t.Fatalf("entity %s has no state-machine audit events", id)
	}
	for _, ev := range events {
		if ev.data == nil {
			t.Errorf("%s has no data", ev.eventType)
			continue
		}
		if ev.data["eventType"] != ev.eventType {
			t.Errorf("%s: data.eventType = %v, want the outer eventType", ev.eventType, ev.data["eventType"])
		}
	}

	status, finished, err := c.GetWorkflowFinished(t, id, txID)
	if err != nil || status != 200 {
		t.Fatalf("GetWorkflowFinished: status %d, err %v", status, err)
	}
	data, ok := finished["data"].(map[string]any)
	if !ok {
		t.Fatalf("finished data = %T (%v), want an object", finished["data"], finished["data"])
	}
	if finished["eventType"] != "STATE_MACHINE_FINISH" || data["eventType"] != finished["eventType"] {
		t.Errorf("finished eventType = %v, data.eventType = %v; want both STATE_MACHINE_FINISH", finished["eventType"], data["eventType"])
	}
}

// RunAuditCommitInstantSharedWithVersionHistory asserts that every audit event
// belonging to one transaction reports that transaction's commit instant — so
// the audit trail and the version history are on the same clock and cannot
// drift apart or invert.
//
// Both halves of the comparison come from the same endpoint, which is what
// makes the property expressible as a black-box parity scenario: the audit
// response builds an EntityChange event's utcTime from the entity VERSION's
// timestamp and a StateMachine event's utcTime from the SM EVENT's timestamp
// (internal/domain/audit). Those are two different storage values, written at
// two different moments — the version at commit, the event when the engine
// recorded it, mid-transaction, from its own clock. Requiring them to be equal
// requires the backend to have moved the event onto the commit instant.
//
// This is a cross-backend contract, not a postgres detail: the timestamp an
// audit event reports is part of what a caller sees, so a backend reporting
// the recording clock while another reports the commit instant is a
// divergence to fix, not a difference to accept.
func RunAuditCommitInstantSharedWithVersionHistory(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "audit-commit-instant"
	const modelVersion = 1

	setupSimpleWorkflow(t, c, modelName, modelVersion)

	entityID, txID, err := c.CreateEntityWithTxID(t, modelName, modelVersion,
		`{"name":"CommitInstant","amount":7,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntityWithTxID: %v", err)
	}
	if txID == "" {
		t.Fatal("POST /entity returned empty transactionId")
	}

	auditResp, err := c.GetAuditEvents(t, entityID)
	if err != nil {
		t.Fatalf("GetAuditEvents: %v", err)
	}

	var (
		entityChangeCount int
		stateMachineCount int
		reference         *client.AuditEvent
	)
	for i := range auditResp.Items {
		ev := &auditResp.Items[i]
		if ev.TransactionID != txID {
			continue
		}
		switch ev.AuditEventType {
		case "EntityChange":
			entityChangeCount++
		case "StateMachine":
			stateMachineCount++
		default:
			continue
		}
		if reference == nil {
			reference = ev
			continue
		}
		if !ev.UtcTime.Equal(reference.UtcTime) {
			t.Errorf("audit events of transaction %s report different instants: %s event at %s, %s event at %s — "+
				"every event of one transaction must carry that transaction's commit instant",
				txID, reference.AuditEventType, reference.UtcTime.Format(time.RFC3339Nano),
				ev.AuditEventType, ev.UtcTime.Format(time.RFC3339Nano))
		}
	}

	// Anti-vacuity: the comparison above is trivially satisfied by zero or one
	// matching event, and is only meaningful when BOTH kinds are present —
	// they are the two independently-written values the scenario exists to
	// tie together.
	if entityChangeCount < 1 {
		t.Errorf("expected >= 1 EntityChange audit event for transaction %s, got %d — "+
			"without one the instant comparison is vacuous", txID, entityChangeCount)
	}
	if stateMachineCount < 1 {
		t.Errorf("expected >= 1 StateMachine audit event for transaction %s, got %d — "+
			"without one the instant comparison is vacuous", txID, stateMachineCount)
	}
}

// StateMachineEventData returns the decoded data of the entity's first
// state-machine audit event of type eventType, failing when there is none.
// Numbers decode as json.Number, so they keep every digit.
func StateMachineEventData(t *testing.T, c *client.Client, id uuid.UUID, eventType string) map[string]any {
	t.Helper()
	resp, err := c.GetAuditEvents(t, id)
	if err != nil {
		t.Fatalf("GetAuditEvents: %v", err)
	}
	for i := range resp.Items {
		ev := &resp.Items[i]
		if ev.AuditEventType != "StateMachine" {
			continue
		}
		sm, err := ev.AsStateMachine()
		if err != nil {
			t.Fatalf("decode state-machine audit event: %v", err)
		}
		if sm.EventType != eventType {
			continue
		}
		data := map[string]any{}
		if len(sm.Data) > 0 {
			dec := json.NewDecoder(bytes.NewReader(sm.Data))
			dec.UseNumber()
			if err := dec.Decode(&data); err != nil {
				t.Fatalf("decode %s data: %v (data: %s)", eventType, err, sm.Data)
			}
		}
		return data
	}
	t.Fatalf("entity %s has no %s audit event", id, eventType)
	return nil
}

// FunctionRecords returns the `functions` list of an audit event's data,
// failing when there is none.
func FunctionRecords(t *testing.T, data map[string]any) []map[string]any {
	t.Helper()
	raw, ok := data["functions"].([]any)
	if !ok {
		t.Fatalf("audit data has no functions list: %v", data)
	}
	out := make([]map[string]any, len(raw))
	for i, r := range raw {
		m, ok := r.(map[string]any)
		if !ok {
			t.Fatalf("functions[%d] is not an object: %v", i, r)
		}
		out[i] = m
	}
	return out
}
