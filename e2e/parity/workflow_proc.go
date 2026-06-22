package parity

import (
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// setupModelWithWorkflow imports a model, locks it, and imports the given
// workflow JSON. This is the parity equivalent of the internal/e2e helper
// with the same name.
func setupModelWithWorkflow(t *testing.T, c *client.Client, modelName string, modelVersion int, workflowJSON string) {
	t.Helper()

	if err := c.ImportModel(t, modelName, modelVersion, `{"name":"Test","amount":10,"status":"new"}`); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if err := c.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	if err := c.ImportWorkflow(t, modelName, modelVersion, workflowJSON); err != nil {
		t.Fatalf("ImportWorkflow: %v", err)
	}
}

// RunWorkflowProcessorChainOnCreation verifies that a processor runs
// during an auto-transition on entity creation. The workflow transitions
// NONE -> CREATED with a "tag-with-foo" processor that adds tag="foo".
func RunWorkflowProcessorChainOnCreation(t *testing.T, fixture BackendFixture) {
	tenant := fixture.ComputeTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "wfproc-chain-on-create"
	const modelVersion = 1

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "proc-chain-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false,
					"processors": [{"type": "calculator", "name": "tag-with-foo", "executionMode": "SYNC",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]
				}]},
				"CREATED": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, c, modelName, modelVersion, wf)

	entityID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Test","amount":10,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	got, err := c.GetEntity(t, entityID)
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}

	// Verify processor ran: tag-with-foo sets tag="foo".
	if got.Data["tag"] != "foo" {
		t.Errorf("expected data.tag=\"foo\", got %v", got.Data["tag"])
	}

	// Verify entity reached CREATED state.
	if got.Meta.State != "CREATED" {
		t.Errorf("expected state CREATED, got %s", got.Meta.State)
	}
}

// RunWorkflowCriteriaMatch verifies that an auto-transition with a
// criteria function fires when the criterion returns true.
// Workflow: NONE -> CREATED (auto) -> APPROVED (auto, criterion: always-true).
func RunWorkflowCriteriaMatch(t *testing.T, fixture BackendFixture) {
	tenant := fixture.ComputeTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "wfproc-criteria-match"
	const modelVersion = 1

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "criteria-match-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {"transitions": [{"name": "auto-approve", "next": "APPROVED", "manual": false,
					"criterion": {"type": "function", "function": {"name": "always-true"}}
				}]},
				"APPROVED": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, c, modelName, modelVersion, wf)

	entityID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Test","amount":10,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	got, err := c.GetEntity(t, entityID)
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}

	if got.Meta.State != "APPROVED" {
		t.Errorf("expected state APPROVED (criteria matched), got %s", got.Meta.State)
	}
}

// RunWorkflowCriteriaNoMatch verifies that an auto-transition with a
// criteria function does NOT fire when the criterion returns false.
// Workflow: NONE -> CREATED (auto) -> APPROVED (auto, criterion: always-false).
func RunWorkflowCriteriaNoMatch(t *testing.T, fixture BackendFixture) {
	tenant := fixture.ComputeTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "wfproc-criteria-nomatch"
	const modelVersion = 1

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "criteria-nomatch-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {"transitions": [{"name": "auto-approve", "next": "APPROVED", "manual": false,
					"criterion": {"type": "function", "function": {"name": "always-false"}}
				}]},
				"APPROVED": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, c, modelName, modelVersion, wf)

	entityID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Test","amount":10,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	got, err := c.GetEntity(t, entityID)
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}

	if got.Meta.State != "CREATED" {
		t.Errorf("expected state CREATED (criteria did not match), got %s", got.Meta.State)
	}
}

// RunWorkflowMultiStateCascade verifies a multi-state auto-transition
// cascade with processors on each transition.
// Workflow: NONE -> CREATED (auto) -> ENRICHED (auto, tag-with-foo) -> APPROVED (auto, bump-amount).
// Asserts: state == APPROVED, data.tag == "foo", data.amount == 11 (original 10 + 1).
func RunWorkflowMultiStateCascade(t *testing.T, fixture BackendFixture) {
	tenant := fixture.ComputeTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "wfproc-cascade"
	const modelVersion = 1

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "cascade-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {"transitions": [{"name": "enrich", "next": "ENRICHED", "manual": false,
					"processors": [{"type": "calculator", "name": "tag-with-foo", "executionMode": "SYNC",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]
				}]},
				"ENRICHED": {"transitions": [{"name": "approve", "next": "APPROVED", "manual": false,
					"processors": [{"type": "calculator", "name": "bump-amount", "executionMode": "SYNC",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]
				}]},
				"APPROVED": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, c, modelName, modelVersion, wf)

	entityID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Test","amount":10,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	got, err := c.GetEntity(t, entityID)
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}

	if got.Meta.State != "APPROVED" {
		t.Errorf("expected state APPROVED after cascade, got %s", got.Meta.State)
	}

	// First processor: tag-with-foo adds tag="foo".
	if got.Data["tag"] != "foo" {
		t.Errorf("expected data.tag=\"foo\" (tag-with-foo processor), got %v", got.Data["tag"])
	}

	// Second processor: bump-amount increments amount by 1.
	if got.Data["amount"] != float64(11) {
		t.Errorf("expected data.amount=11 (bump-amount processor), got %v", got.Data["amount"])
	}
}

// RunWorkflowManualTransition verifies that a manual transition does not
// auto-fire, and that firing it via UpdateEntity applies the processor.
// Workflow: NONE -> CREATED (auto) -> APPROVED (manual "approve", tag-with-foo).
func RunWorkflowManualTransition(t *testing.T, fixture BackendFixture) {
	tenant := fixture.ComputeTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "wfproc-manual"
	const modelVersion = 1

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "manual-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {"transitions": [{"name": "approve", "next": "APPROVED", "manual": true,
					"processors": [{"type": "calculator", "name": "tag-with-foo", "executionMode": "SYNC",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]
				}]},
				"APPROVED": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, c, modelName, modelVersion, wf)

	entityID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Test","amount":10,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	// Entity should be in CREATED (manual transition not auto-fired).
	got, err := c.GetEntity(t, entityID)
	if err != nil {
		t.Fatalf("GetEntity (before manual): %v", err)
	}
	if got.Meta.State != "CREATED" {
		t.Fatalf("expected state CREATED before manual transition, got %s", got.Meta.State)
	}

	// Fire manual transition via UpdateEntity.
	if err := c.UpdateEntity(t, entityID, "approve", `{"name":"Test","amount":10,"status":"new"}`); err != nil {
		t.Fatalf("UpdateEntity (approve): %v", err)
	}

	// After manual transition, entity should be APPROVED with processor applied.
	got, err = c.GetEntity(t, entityID)
	if err != nil {
		t.Fatalf("GetEntity (after manual): %v", err)
	}
	if got.Meta.State != "APPROVED" {
		t.Errorf("expected state APPROVED after manual transition, got %s", got.Meta.State)
	}

	// tag-with-foo adds tag="foo".
	if got.Data["tag"] != "foo" {
		t.Errorf("expected data.tag=\"foo\" (tag-with-foo processor), got %v", got.Data["tag"])
	}
}
