package externalapi

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/externalapi/driver"
	"github.com/cyoda-platform/cyoda-go/e2e/parity"
)

func init() {
	// External API scenario suite — audit-shape contract.
	parity.Register(
		parity.NamedTest{
			Name: "ExternalAPI_05_TransitionAbortedAuditEventPaired",
			Fn:   RunExternalAPI_05_TransitionAbortedAuditEventPaired,
		},
	)
}

// RunExternalAPI_05_TransitionAbortedAuditEventPaired pins the audit trail
// of a rolled-back update: when a single PUT against an entity fails its
// ifMatch precondition (stale txID), the update's transaction rolls back,
// and so do the audit events it recorded — the entry-side
// STATE_MACHINE_START and the compensating TRANSITION_ABORTED. Audit events
// are bound to the transaction on every backend, so the audit log shows no
// event of the failed call.
func RunExternalAPI_05_TransitionAbortedAuditEventPaired(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)

	const (
		modelName    = "audit-aborted-paired"
		modelVersion = 1
	)
	if err := d.CreateModelFromSample(modelName, modelVersion,
		`{"name":"x","amount":1,"status":"new"}`); err != nil {
		t.Fatalf("CreateModelFromSample: %v", err)
	}
	if err := d.LockModel(modelName, modelVersion); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	if err := d.ImportWorkflow(modelName, modelVersion, trivialWorkflowJSON); err != nil {
		t.Fatalf("ImportWorkflow: %v", err)
	}

	// 1. Seed one entity and capture its initial transactionId — this
	// is the value that becomes "stale" after the intervening update.
	id, err := d.CreateEntity(modelName, modelVersion,
		`{"name":"orig","amount":1,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	got, err := d.GetEntity(id)
	if err != nil {
		t.Fatalf("GetEntity (initial): %v", err)
	}
	staleTxID := got.Meta.TransactionID
	if staleTxID == "" {
		t.Fatalf("initial GetEntity returned empty meta.transactionId")
	}

	// 2. Modify the entity independently so staleTxID is no longer the
	// row's current transactionId.
	if err := d.UpdateEntityData(id,
		`{"name":"intervening","amount":42,"status":"upd"}`); err != nil {
		t.Fatalf("UpdateEntityData (intervening): %v", err)
	}
	got2, err := d.GetEntity(id)
	if err != nil {
		t.Fatalf("GetEntity (post-intervening): %v", err)
	}
	currentTxID := got2.Meta.TransactionID
	if currentTxID == "" || currentTxID == staleTxID {
		t.Fatalf("post-intervening txid: got %q (stale was %q); intervening update did not advance txid",
			currentTxID, staleTxID)
	}

	// Snapshot the audit log AFTER the legitimate intervening update so
	// we can quantify the delta introduced by the failed call below.
	// The audit endpoint orders most-recent-first; we count by event
	// type rather than slicing by index so the assertion is independent
	// of ordering choice.
	preFailEvents := getStateMachineEvents(t, d, id)
	preStartCount := countByType(preFailEvents, "STATE_MACHINE_START")
	preAbortCount := countByType(preFailEvents, "TRANSITION_ABORTED")
	if preAbortCount != 0 {
		t.Fatalf("test invariant: pre-fail TRANSITION_ABORTED count = %d, want 0", preAbortCount)
	}

	// 3. Issue a stale-ifMatch single PUT — the single-PUT endpoint
	// rolls the entire transaction back on ifMatch failure, which is
	// what makes the audit rollback observable (the bulk endpoint
	// isolates per-item failures and commits the chunk).
	status, rawBody, err := d.UpdateEntityDataWithIfMatchRaw(id,
		`{"name":"stale-attempt","amount":99,"status":"upd"}`, staleTxID)
	if err != nil {
		t.Fatalf("UpdateEntityDataWithIfMatchRaw (stale ifMatch): %v", err)
	}
	// Sanity: the response must be 412 Precondition Failed with an
	// ENTITY_MODIFIED error envelope — if it succeeded, our staleness
	// premise is broken and the audit assertions below would be
	// meaningless.
	if status != http.StatusPreconditionFailed {
		t.Fatalf("expected 412 Precondition Failed on stale ifMatch single PUT; got %d, body: %s",
			status, rawBody)
	}
	if !strings.Contains(string(rawBody), "ENTITY_MODIFIED") {
		t.Fatalf("expected ENTITY_MODIFIED in response body; got: %s", rawBody)
	}

	// Confirm the entity state on disk did NOT change — the failed
	// item's payload must not have leaked.
	postFail, err := d.GetEntity(id)
	if err != nil {
		t.Fatalf("GetEntity (post-failure): %v", err)
	}
	if postFail.Data["name"] == "stale-attempt" {
		t.Fatalf("stale-ifMatch update leaked: entity reflects the rejected payload; data=%v",
			postFail.Data)
	}

	// 4. Read the audit log post-failure and quantify the delta from
	// the pre-fail snapshot.
	postEvents := getStateMachineEvents(t, d, id)
	postStartCount := countByType(postEvents, "STATE_MACHINE_START")
	postAbortCount := countByType(postEvents, "TRANSITION_ABORTED")
	deltaStart := postStartCount - preStartCount
	deltaAbort := postAbortCount - preAbortCount

	// 5. The rolled-back update keeps none of its audit events.
	if deltaStart != 0 {
		t.Errorf("rolled-back update left %d STATE_MACHINE_START event(s); events=%+v",
			deltaStart, postEvents)
	}
	if deltaAbort != 0 {
		t.Errorf("rolled-back update left %d TRANSITION_ABORTED event(s); events=%+v",
			deltaAbort, postEvents)
	}
}

// countByType returns the number of events with the given eventType.
func countByType(events []stateMachineEvent, eventType string) int {
	n := 0
	for _, ev := range events {
		if ev.eventType == eventType {
			n++
		}
	}
	return n
}

// stateMachineEvent is the trimmed shape of a StateMachine audit event the
// scenario counts.
type stateMachineEvent struct {
	eventType string
}

// getStateMachineEvents fetches /api/audit/entity/{id} via the driver
// and returns the StateMachine subtype events flattened into the
// scenario-local view. Filters out non-StateMachine events because the
// scenario only cares about the SM event stream.
func getStateMachineEvents(t *testing.T, d *driver.Driver, id uuid.UUID) []stateMachineEvent {
	t.Helper()
	resp, err := d.GetAuditEvents(id)
	if err != nil {
		t.Fatalf("GetAuditEvents: %v", err)
	}
	out := make([]stateMachineEvent, 0, len(resp.Items))
	for i := range resp.Items {
		ev := &resp.Items[i]
		if ev.AuditEventType != "StateMachine" {
			continue
		}
		sm, err := ev.AsStateMachine()
		if err != nil {
			t.Errorf("AsStateMachine: %v", err)
			continue
		}
		out = append(out, stateMachineEvent{eventType: sm.EventType})
	}
	return out
}
