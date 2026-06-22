//go:build cyoda_recon

package recon

import (
	"fmt"
	"time"
)

// CommonExclusions are fields excluded from all model comparisons.
var CommonExclusions = []string{"/modelId", "/modelUpdateDate"}

// ActionResultExclusions are fields excluded from action result comparisons.
var ActionResultExclusions = []string{"/modelId"}

// uniqueName generates a unique name with the given prefix for idempotent test runs.
func uniqueName(prefix string) string {
	return fmt.Sprintf("Recon_%d_%s", time.Now().UnixNano(), prefix)
}

// modelScenarios returns the model lifecycle reconciliation flow.
func modelScenarios() []Scenario {
	return []Scenario{
		modelLifecycleFlow(),
	}
}

// modelLifecycleFlow exercises the full model lifecycle: import, export,
// changeLevel, lock, entity-driven extension, conflict paths, unlock, delete.
func modelLifecycleFlow() Scenario {
	return Scenario{
		Name: "Model Lifecycle",
		Setup: func() map[string]string {
			return map[string]string{"model": uniqueName("ModelLifecycle")}
		},
		Steps: []Step{
			// 1. Import model from sample data
			{
				Name:         "Import model from sample data",
				Method:       "POST",
				PathTemplate: "/model/import/JSON/SAMPLE_DATA/{model}/1",
				Body:         `{"name":"Alice","age":30,"active":true}`,
				ExpectStatus: 200,
			},
			// 2. Export as JSON_SCHEMA
			{
				Name:         "Export as JSON_SCHEMA",
				Method:       "GET",
				PathTemplate: "/model/export/JSON_SCHEMA/{model}/1",
				ExpectStatus: 200,
				Exclusions:   CommonExclusions,
			},
			// 3. Export as SIMPLE_VIEW
			{
				Name:         "Export as SIMPLE_VIEW",
				Method:       "GET",
				PathTemplate: "/model/export/SIMPLE_VIEW/{model}/1",
				ExpectStatus: 200,
				Exclusions:   CommonExclusions,
			},
			// 4. Set changeLevel to STRUCTURAL
			{
				Name:         "Set changeLevel to STRUCTURAL",
				Method:       "POST",
				PathTemplate: "/model/{model}/1/changeLevel/STRUCTURAL",
				ExpectStatus: 200,
				Exclusions:   ActionResultExclusions,
			},
			// 5. Lock model
			{
				Name:         "Lock model",
				Method:       "PUT",
				PathTemplate: "/model/{model}/1/lock",
				ExpectStatus: 200,
				Exclusions:   ActionResultExclusions,
			},
			// 6. Import workflow
			{
				Name:         "Import workflow",
				Method:       "POST",
				PathTemplate: "/model/{model}/1/workflow/import",
				Body:         `{"importMode":"MERGE","workflows":[{"version":"1.1","name":"TestWorkflow","initialState":"NEW","active":true,"states":{"NEW":{"transitions":[{"name":"VALIDATE","next":"VALIDATED","manual":false}]},"VALIDATED":{"transitions":[]}}}]}`,
				ExpectStatus: 200,
			},
			// 7. Export workflow
			{
				Name:         "Export workflow",
				Method:       "GET",
				PathTemplate: "/model/{model}/1/workflow/export",
				ExpectStatus: 200,
				Exclusions:   WorkflowExclusions,
			},
			// 8. Create entity with extra field (extends model via STRUCTURAL changeLevel)
			{
				Name:         "Create entity with extra field (extends model)",
				Method:       "POST",
				PathTemplate: "/entity/JSON/{model}/1?waitForConsistencyAfter=true",
				Body:         `{"name":"Bob","age":25,"extra":"field"}`,
				ExpectStatus: 200,
				Exclusions:   EntityExclusions,
			},
			// 7. Export model (SIMPLE_VIEW) — verify extended
			{
				Name:         "Export model (SIMPLE_VIEW) — verify extended",
				Method:       "GET",
				PathTemplate: "/model/export/SIMPLE_VIEW/{model}/1",
				ExpectStatus: 200,
				Exclusions:   CommonExclusions,
			},
			// 8. Lock again — expect 409
			{
				Name:         "Lock again — expect 409",
				Method:       "PUT",
				PathTemplate: "/model/{model}/1/lock",
				ExpectStatus: 409,
				Exclusions:   []string{"/instance", "/status", "/title"},
			},
			// 9. Import while locked — expect 409
			{
				Name:         "Import while locked — expect 409",
				Method:       "POST",
				PathTemplate: "/model/import/JSON/SAMPLE_DATA/{model}/1",
				Body:         `{"extra":"field"}`,
				ExpectStatus: 409,
				Exclusions:   []string{"/instance", "/status", "/title"},
			},
			// 10. Delete all entities (required before unlock)
			{
				Name:         "Delete all entities before unlock",
				Method:       "DELETE",
				PathTemplate: "/entity/{model}/1",
				ExpectStatus: 200,
				Exclusions:   []string{"/0/entityModelClassId"},
			},
			// 11. Unlock model
			{
				Name:         "Unlock model",
				Method:       "PUT",
				PathTemplate: "/model/{model}/1/unlock",
				ExpectStatus: 200,
				Exclusions:   ActionResultExclusions,
			},
			// 12. Delete model
			{
				Name:         "Delete model",
				Method:       "DELETE",
				PathTemplate: "/model/{model}/1",
				ExpectStatus: 200,
				Exclusions:   ActionResultExclusions,
			},
			// 13. Export after delete — expect 404
			{
				Name:         "Export after delete — expect 404",
				Method:       "GET",
				PathTemplate: "/model/export/JSON_SCHEMA/{model}/1",
				ExpectStatus: 404,
				Exclusions:   []string{"/instance"},
			},
		},
	}
}
