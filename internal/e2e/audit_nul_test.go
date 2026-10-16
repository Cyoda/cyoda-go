package e2e_test

import (
	"context"
	"encoding/json"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// TestAudit_NULReasonNotRecorded: an automated transition refused by a
// criterion whose member answers with a reason holding U+0000. PostgreSQL
// refuses U+0000 in jsonb and the refused INSERT aborts the transaction, so
// the engine refuses to record such an event on every backend: the entity is
// saved, and the TRANSITION_NOT_MATCH_CRITERION event is not there.
func TestAudit_NULReasonNotRecorded(t *testing.T) {
	const model = "e2e-audit-nul-reason"
	procSvc.RegisterCriteriaReason("nul-reason",
		func(ctx context.Context, e *spi.Entity, c json.RawMessage) (bool, string, error) {
			return false, "bad\x00reason", nil
		})
	defer procSvc.Reset()

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "nul-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {"transitions": [{"name": "auto-promote", "next": "PREMIUM", "manual": false,
					"criterion": {"type": "function", "function": {"name": "nul-reason"}}}]},
				"PREMIUM": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, model, wf)
	entityID := createEntityE2E(t, model, 1, `{"name":"X","amount":10}`)

	events, _ := auditPage(t, entityID, "eventType=StateMachine")
	if len(events) == 0 {
		t.Fatal("no state-machine audit events at all: the entity's other events must still be recorded")
	}
	for _, ev := range events {
		if ev["eventType"] == "TRANSITION_NOT_MATCH_CRITERION" {
			t.Errorf("an event holding U+0000 was recorded: %v", ev)
		}
	}
}
