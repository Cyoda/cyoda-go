package parity

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"github.com/google/uuid"
)

// RunAuditCBDFailedProcessorRecorded: a COMMIT_BEFORE_DISPATCH processor whose
// compute member answers failure fails the operation after its segment
// boundary committed the entity and the events before the callout. Its result
// is recorded all the same, identically on every backend: one failed
// STATE_PROCESS_RESULT with the member's text, after the PAUSE_FOR_PROCESSING,
// under the cascade's transaction id. Both startNewTxOnDispatch values.
func RunAuditCBDFailedProcessorRecorded(t *testing.T, fixture BackendFixture) {
	requireComputeClients(t, fixture)
	for _, newTx := range []bool{false, true} {
		t.Run(fmt.Sprintf("startNewTxOnDispatch=%t", newTx), func(t *testing.T) {
			name := fmt.Sprintf("cbd-record-%t", newTx)
			cc := newCalloutCase(t, fixture, name, calloutSample, func(tag string) string {
				cfg := map[string]any{"calculationNodesTags": tag}
				return calloutDoc("co-"+name+"-wf", "NONE", map[string]any{
					"NONE": map[string]any{"transitions": []any{map[string]any{
						"name": "init", "next": "ACTIVE", "manual": false,
						"processors": []any{withStartNewTx(cbProc("charge", "COMMIT_BEFORE_DISPATCH", "", cfg), newTx)},
					}}},
					"ACTIVE": map[string]any{},
				})
			}, ComputeBehaviourFail)

			status, body, err := cc.c.CreateEntityRaw(t, cc.model, 1, calloutSample)
			if err != nil {
				t.Fatalf("CreateEntityRaw: %v", err)
			}
			calloutProblem(t, status, body, http.StatusBadRequest, "WORKFLOW_FAILED", false)

			list, err := cc.c.ListEntitiesByModel(t, cc.model, 1)
			if err != nil || len(list) != 1 {
				t.Fatalf("entities after the failed create: %d err=%v; want 1, committed by the segment boundary", len(list), err)
			}
			if list[0].Meta.State != "NONE" {
				t.Errorf("state = %q; want NONE", list[0].Meta.State)
			}
			entityID, err := uuid.Parse(list[0].Meta.ID)
			if err != nil {
				t.Fatalf("entity id %q: %v", list[0].Meta.ID, err)
			}

			auditResp, err := cc.c.GetAuditEvents(t, entityID)
			if err != nil {
				t.Fatalf("GetAuditEvents: %v", err)
			}
			type smEvent struct {
				txID string
				data map[string]any
			}
			byType := map[string][]smEvent{}
			for i := range auditResp.Items {
				ev := &auditResp.Items[i]
				if ev.AuditEventType != "StateMachine" {
					continue
				}
				sm, err := ev.AsStateMachine()
				if err != nil {
					t.Fatalf("decode a state-machine audit event: %v", err)
				}
				var data map[string]any
				if err := json.Unmarshal(sm.Data, &data); err != nil {
					t.Fatalf("decode %s data: %v", sm.EventType, err)
				}
				byType[sm.EventType] = append(byType[sm.EventType], smEvent{txID: ev.TransactionID, data: data})
			}
			paused, results := byType["PAUSE_FOR_PROCESSING"], byType["STATE_PROCESS_RESULT"]
			if len(paused) != 1 || len(results) != 1 {
				t.Fatalf("PAUSE_FOR_PROCESSING = %d, STATE_PROCESS_RESULT = %d; want 1 each", len(paused), len(results))
			}
			want := map[string]any{"eventType": "STATE_PROCESS_RESULT", "processorName": "charge",
				"executionMode": "COMMIT_BEFORE_DISPATCH", "success": false, "failReason": "scripted failure: fail"}
			if !reflect.DeepEqual(results[0].data, want) {
				t.Errorf("record = %v; want %v", results[0].data, want)
			}
			if results[0].txID != paused[0].txID {
				t.Errorf("record transactionId = %q; want the cascade's %q", results[0].txID, paused[0].txID)
			}
			// The two timestamps are not ordered here: on PostgreSQL the pause
			// carries TX_pre's commit instant, while the record, labelled with
			// the cascade's transaction id but written in a transaction of its
			// own, keeps the engine's clock (plugins/postgres/transaction_manager.go,
			// the audit-event stamp), so their order depends on the two clocks.
		})
	}
}
