package parity

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// AuditNULWorkflow is a workflow whose automated transition CREATED ->
// PREMIUM is guarded by a function criterion routed to tag.
func AuditNULWorkflow(wfName, tag string) string {
	wf, _ := json.Marshal(map[string]any{
		"importMode": "REPLACE",
		"workflows": []any{map[string]any{
			"version": "1.1", "name": wfName, "initialState": "NONE", "active": true,
			"states": map[string]any{
				"NONE": map[string]any{"transitions": []any{map[string]any{"name": "init", "next": "CREATED", "manual": false}}},
				"CREATED": map[string]any{"transitions": []any{map[string]any{
					"name": "auto-promote", "next": "PREMIUM", "manual": false,
					"criterion": map[string]any{"type": "function", "function": map[string]any{
						"name":   "always-true",
						"config": map[string]any{"calculationNodesTags": tag},
					}},
				}}},
				"PREMIUM": map[string]any{},
			},
		}},
	})
	return string(wf)
}

// AssertNoNotMatchEvent fails the test when the entity's audit trail holds a
// TRANSITION_NOT_MATCH_CRITERION event, and when it holds no state-machine
// event at all (the entity's other events are recorded).
func AssertNoNotMatchEvent(t *testing.T, c *client.Client, entityID uuid.UUID) {
	t.Helper()
	auditResp, err := c.GetAuditEvents(t, entityID)
	if err != nil {
		t.Fatalf("GetAuditEvents: %v", err)
	}
	var stateMachine int
	for i := range auditResp.Items {
		ev := &auditResp.Items[i]
		if ev.AuditEventType != "StateMachine" {
			continue
		}
		sm, err := ev.AsStateMachine()
		if err != nil {
			t.Fatalf("decode a state-machine audit event: %v", err)
		}
		stateMachine++
		if sm.EventType == "TRANSITION_NOT_MATCH_CRITERION" {
			t.Errorf("an event holding U+0000 was recorded: %s", sm.Data)
		}
	}
	if stateMachine == 0 {
		t.Error("no state-machine audit events at all: the entity's other events must still be recorded")
	}
}

// RunAuditNULEventNotRecorded verifies that an audit event holding U+0000 is
// written on no backend. An automated transition is refused by a criterion
// whose member answers with a reason holding U+0000: the entity is saved and
// the TRANSITION_NOT_MATCH_CRITERION event is not recorded, identically on
// memory, SQLite and PostgreSQL (PostgreSQL's jsonb refuses U+0000, and the
// refused INSERT used to abort the whole save).
func RunAuditNULEventNotRecorded(t *testing.T, fixture BackendFixture) {
	requireComputeClients(t, fixture)
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const model, tag = "audit-nul", "audit-nul"
	setupModelWithWorkflow(t, c, model, 1, wfBaseSample, AuditNULWorkflow("audit-nul-wf", tag))
	StartComputeClientOrSkip(t, fixture, ComputeClientSpec{
		TenantID: tenant.ID, Tags: []string{tag}, Behaviour: ComputeBehaviourCriterionReasonNUL,
	})

	entityID, err := c.CreateEntity(t, model, 1, wfBaseSample)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	got, err := c.GetEntity(t, entityID)
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}
	if got.Meta.State != "CREATED" {
		t.Errorf("state = %q; want CREATED (the criterion refused the transition)", got.Meta.State)
	}
	AssertNoNotMatchEvent(t, c, entityID)
}
