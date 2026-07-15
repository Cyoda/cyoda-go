package parity

import (
	"encoding/json"
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// RunCriterionReasonInlineDefault verifies that an inline (non-FUNCTION)
// transition criterion which evaluates false records the default stoppage
// reason ("criterion did not match") on the TRANSITION_NOT_MATCH_CRITERION
// state-machine audit event, identically across all backends.
//
// The transition is AUTOMATED (not manual): automated cascade commits its
// transaction, so the audit event is durable. A manual-transition rejection
// rolls back and its audit is intentionally not durable (see
// TxBoundAuditFixture) — that shape is out of scope here. The criterion is
// inline ("simple") rather than a FUNCTION criterion so this scenario needs
// no compute node, and uses fixture.NewTenant rather than ComputeTenant.
func RunCriterionReasonInlineDefault(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "critreason-inline-default"
	const modelVersion = 1

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "critreason-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {"transitions": [{"name": "auto-promote", "next": "PREMIUM", "manual": false,
					"criterion": {"type":"simple","jsonPath":"$.amount","operatorType":"GREATER_THAN","value":100}}]},
				"PREMIUM": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, c, modelName, modelVersion, wf)

	entityID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"X","amount":10}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	auditResp, err := c.GetAuditEvents(t, entityID)
	if err != nil {
		t.Fatalf("GetAuditEvents: %v", err)
	}

	var found bool
	for i := range auditResp.Items {
		ev := &auditResp.Items[i]
		if ev.AuditEventType != "StateMachine" {
			continue
		}
		sm, err := ev.AsStateMachine()
		if err != nil || sm.EventType != "TRANSITION_NOT_MATCH_CRITERION" {
			continue
		}
		found = true

		var data map[string]interface{}
		if err := json.Unmarshal(sm.Data, &data); err != nil {
			t.Fatalf("unmarshal StateMachine audit event data: %v", err)
		}
		reason, _ := data["reason"].(string)
		if reason != "criterion did not match" {
			t.Errorf("reason = %q, want %q", reason, "criterion did not match")
		}
	}
	if !found {
		t.Fatal("no TRANSITION_NOT_MATCH_CRITERION audit event found")
	}
}
