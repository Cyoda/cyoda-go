package parity

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

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

// AssertNULAnswerRefused checks the outcome of a create whose criterion member
// answered with a reason holding U+0000: the answer is unreadable, so the create
// fails with 400 WORKFLOW_FAILED, not retryable, naming the cause in this
// node's own words, and no entity of the model is committed.
func AssertNULAnswerRefused(t *testing.T, c *client.Client, model string, status int, body []byte) {
	t.Helper()
	detail := calloutProblem(t, status, body, http.StatusBadRequest, "WORKFLOW_FAILED", false)
	if !strings.Contains(detail, "a reason or error text held U+0000") {
		t.Errorf("detail = %q; want it to name the U+0000", detail)
	}
	entities, err := c.ListEntitiesByModel(t, model, 1)
	if err != nil {
		t.Fatalf("ListEntitiesByModel: %v", err)
	}
	if len(entities) != 0 {
		t.Errorf("%d entities committed; want none: the answer was not read and the create did not commit", len(entities))
	}
}

// RunAuditNULAnswerRefused verifies that a compute member's text holding U+0000
// cannot take records out of the audit trail. The audit trail records no event
// holding U+0000, and a criterion's reason is copied into later events (FINISH,
// TRANSITION_MAKE), so the answer is refused as unreadable instead: the create
// fails and commits nothing, identically on memory, SQLite and PostgreSQL.
func RunAuditNULAnswerRefused(t *testing.T, fixture BackendFixture) {
	requireComputeClients(t, fixture)
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const model, tag = "audit-nul", "audit-nul"
	setupModelWithWorkflow(t, c, model, 1, wfBaseSample, AuditNULWorkflow("audit-nul-wf", tag))
	StartComputeClientOrSkip(t, fixture, ComputeClientSpec{
		TenantID: tenant.ID, Tags: []string{tag}, Behaviour: ComputeBehaviourCriterionReasonNUL,
	})

	status, body, err := c.CreateEntityRaw(t, model, 1, wfBaseSample)
	if err != nil {
		t.Fatalf("CreateEntityRaw: %v", err)
	}
	AssertNULAnswerRefused(t, c, model, status, body)
}
