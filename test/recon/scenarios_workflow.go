//go:build cyoda_recon

package recon

// WorkflowExclusions are fields excluded from workflow export comparisons.
var WorkflowExclusions = []string{"/entityName", "/modelVersion"}

// workflowScenarios returns the workflow reconciliation flows.
func workflowScenarios() []Scenario {
	return []Scenario{
		workflowLifecycleFlow(),
	}
}

// workflowLifecycleFlow exercises workflow import, export, and entity creation
// that traverses the workflow's state machine.
func workflowLifecycleFlow() Scenario {
	return Scenario{
		Name: "Workflow Lifecycle",
		Setup: func() map[string]string {
			return map[string]string{"model": uniqueName("WorkflowLifecycle")}
		},
		Steps: []Step{
			// 1. Import model
			{
				Name:         "Import model",
				Method:       "POST",
				PathTemplate: "/model/import/JSON/SAMPLE_DATA/{model}/1",
				Body:         `{"name":"Alice","age":30}`,
				ExpectStatus: 200,
			},
			// 2. Lock model
			{
				Name:         "Lock model",
				Method:       "PUT",
				PathTemplate: "/model/{model}/1/lock",
				ExpectStatus: 200,
				Exclusions:   ActionResultExclusions,
			},
			// 3. Import workflow
			{
				Name:         "Import workflow",
				Method:       "POST",
				PathTemplate: "/model/{model}/1/workflow/import",
				Body:         `{"importMode":"MERGE","workflows":[{"version":"1.1","name":"TestWorkflow","initialState":"NEW","active":true,"states":{"NEW":{"transitions":[{"name":"VALIDATE","next":"VALIDATED","manual":false}]},"VALIDATED":{"transitions":[]}}}]}`,
				ExpectStatus: 200,
			},
			// 4. Export workflow
			{
				Name:         "Export workflow",
				Method:       "GET",
				PathTemplate: "/model/{model}/1/workflow/export",
				ExpectStatus: 200,
				Exclusions:   WorkflowExclusions,
			},
			// 5. Create entity — goes through workflow
			{
				Name:         "Create entity through workflow",
				Method:       "POST",
				PathTemplate: "/entity/JSON/{model}/1?waitForConsistencyAfter=true",
				Body:         `{"name":"Alice","age":30}`,
				ExpectStatus: 200,
				Exclusions:   EntityExclusions,
				Capture: map[string]string{
					"entityId": "0.entityIds.0",
				},
			},
			// 6. Get entity by ID — verify state after workflow
			{
				Name:         "Get entity by ID after workflow",
				Method:       "GET",
				PathTemplate: "/entity/{entityId}",
				ExpectStatus: 200,
				Exclusions:   EntityEnvelopeExclusions,
			},
			// 7. Delete entity
			{
				Name:         "Delete entity",
				Method:       "DELETE",
				PathTemplate: "/entity/{entityId}",
				ExpectStatus: 200,
				Exclusions:   []string{"/id", "/transactionId"},
			},
		},
	}
}
