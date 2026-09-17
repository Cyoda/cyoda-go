package parity

import (
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// RunAuditEntityHistory verifies that creating an entity with a workflow
// produces audit events (both EntityChange and StateMachine) that are
// retrievable via the audit REST API.
//
// Port of internal/e2e TestAudit_EntityCreationGeneratesEvents, using the
// discriminated-union audit types from Task 1.2b:
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
