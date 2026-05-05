package externalapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/externalapi/driver"
	"github.com/cyoda-platform/cyoda-go/e2e/parity"
)

func init() {
	// External API scenario suite — issue #228 audit-shape contract.
	parity.Register(
		parity.NamedTest{
			Name: "ExternalAPI_05_TransitionAbortedAuditEventPaired",
			Fn:   RunExternalAPI_05_TransitionAbortedAuditEventPaired,
		},
	)
}

// RunExternalAPI_05_TransitionAbortedAuditEventPaired pins the audit-
// trail shape from issue #228: when a single PUT against an entity
// fails its ifMatch precondition (stale txID), the entry-side
// STATE_MACHINE_START is paired with a TRANSITION_ABORTED event whose
// data payload references reason=ENTITY_MODIFIED, expectedTxId=<stale>,
// and actualTxId=<current>.
//
// Storage-binding asymmetry (parity.IsTxBoundAuditStore):
//
//   - On TX-bound audit stores (postgres; cassandra externally) the
//     rolled-back update transaction discards both the entry events and
//     the compensating ABORTED event — the audit log shows no entries
//     from the failed call. The scenario asserts none of the failed
//     call's audit events appear.
//   - On non-TX-bound audit stores (memory, sqlite) the in-process
//     audit bus already emitted the entry events before the rollback
//     ran, and the handler's emit-then-rollback discipline preserves
//     the compensating ABORTED event alongside them. The scenario
//     asserts the paired START + ABORTED shape with the expected data
//     payload.
//
// Per the parity-test pattern, fixtures opt-in to TX-bound semantics by
// implementing parity.TxBoundAuditFixture; fixtures that do not
// implement it default to non-TX-bound (the conservative, stricter
// assertion).
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
	// what makes the TX-bound vs non-TX-bound audit asymmetry
	// observable (the bulk endpoint isolates per-item failures and
	// commits the chunk regardless of TX-bound semantics, so the
	// pairing-after-commit shape is the same on every backend there).
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

	// 5. Branch on the fixture's TX-bound-audit capability.
	if parity.IsTxBoundAuditStore(fixture) {
		// TX-bound: the rolled-back update commits no audit events.
		// Both deltas must be zero.
		if deltaStart != 0 {
			t.Errorf("TX-bound audit store leaked %d STATE_MACHINE_START event(s) from failed call",
				deltaStart)
		}
		if deltaAbort != 0 {
			t.Errorf("TX-bound audit store leaked %d TRANSITION_ABORTED event(s) from failed call",
				deltaAbort)
		}
		return
	}

	// Non-TX-bound: the failed call must contribute exactly one paired
	// START + ABORTED to the audit log. Stricter than ">= 1" so a
	// future regression that double-emits or skips the abort is caught.
	if deltaStart != 1 {
		t.Errorf("non-TX-bound audit store: STATE_MACHINE_START delta = %d, want 1; events=%+v",
			deltaStart, postEvents)
	}
	if deltaAbort != 1 {
		t.Fatalf("non-TX-bound audit store: TRANSITION_ABORTED delta = %d, want 1; events=%+v",
			deltaAbort, postEvents)
	}

	// Validate the ABORTED event's data payload — there is now exactly
	// one in postEvents (preAbortCount was 0).
	abort := findFirstByType(postEvents, "TRANSITION_ABORTED")
	if abort == nil {
		t.Fatalf("internal: TRANSITION_ABORTED count says 1 but findFirstByType returned nil")
	}
	if abort.dataReason != "ENTITY_MODIFIED" {
		t.Errorf("TRANSITION_ABORTED.data.reason: got %q, want ENTITY_MODIFIED",
			abort.dataReason)
	}
	if abort.dataExpectedTxID != staleTxID {
		t.Errorf("TRANSITION_ABORTED.data.expectedTxId: got %q, want %q (stale ifMatch)",
			abort.dataExpectedTxID, staleTxID)
	}
	if abort.dataActualTxID != currentTxID {
		t.Errorf("TRANSITION_ABORTED.data.actualTxId: got %q, want %q (current row txid)",
			abort.dataActualTxID, currentTxID)
	}
	if abort.dataTransitionName == "" {
		t.Errorf("TRANSITION_ABORTED.data.transitionName is empty; want non-empty")
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

// findFirstByType returns a pointer to the first event with the given
// eventType, or nil if not found.
func findFirstByType(events []stateMachineEvent, eventType string) *stateMachineEvent {
	for i := range events {
		if events[i].eventType == eventType {
			return &events[i]
		}
	}
	return nil
}

// stateMachineEvent is the trimmed shape of a StateMachine audit event
// the parity TRANSITION_ABORTED scenario cares about: eventType plus the
// abort-specific fields nested under data{}. Decoded permissively so a
// future canonical-schema field addition doesn't trip the scenario.
type stateMachineEvent struct {
	eventType          string
	dataReason         string
	dataExpectedTxID   string
	dataActualTxID     string
	dataTransitionName string
}

// getStateMachineEvents fetches /api/audit/entity/{id} via the driver
// and returns the StateMachine subtype events flattened into the
// scenario-local view. Filters out non-StateMachine events because the
// scenario only cares about the SM event stream. Decodes the SM event
// data envelope permissively (no DisallowUnknownFields) so unrelated
// data fields on, e.g., FINISH events do not trip the decode.
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
		fe := stateMachineEvent{eventType: sm.EventType}
		if len(sm.Data) > 0 {
			var d struct {
				Reason         string `json:"reason"`
				ExpectedTxID   string `json:"expectedTxId"`
				ActualTxID     string `json:"actualTxId"`
				TransitionName string `json:"transitionName"`
			}
			// Permissive decode — TRANSITION_ABORTED's data carries the
			// four fields above; other SM event types carry different
			// data shapes. Ignoring decode errors here is safe because
			// we only assert on TRANSITION_ABORTED's fields below.
			_ = json.Unmarshal(sm.Data, &d)
			fe.dataReason = d.Reason
			fe.dataExpectedTxID = d.ExpectedTxID
			fe.dataActualTxID = d.ActualTxID
			fe.dataTransitionName = d.TransitionName
		}
		out = append(out, fe)
	}
	return out
}
