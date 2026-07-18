package externalapi

// External API Scenario Suite — 08-workflow-import-export
//
// Schema adaptation notes (dictionary shape → cyoda-go accepted shape):
//
//   Field "to"         → "next"       (TransitionDefinition.Next)
//   Field "automated"  → "manual"     (boolean inverted: automated=true ↔ manual=false)
//   Criterion type "jsonpath" with "path"/"equals"
//              → type "simple" with "jsonPath"/"operatorType"/"value"
//   Criterion group "clauses" → "conditions"
//   Processor config  → requires "type" field; bare config map not accepted.
//
// Each deviation is annotated with // dictionary uses X; cyoda-go accepts Y —
// different_naming_same_level, per the parity-suite convention.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/externalapi/driver"
	"github.com/cyoda-platform/cyoda-go/e2e/externalapi/errorcontract"
	"github.com/cyoda-platform/cyoda-go/e2e/parity"
)

func init() {
	parity.Register(
		parity.NamedTest{Name: "ExternalAPI_08_01_SimpleAutomatedTransition", Fn: RunExternalAPI_08_01_SimpleAutomatedTransition},
		parity.NamedTest{Name: "ExternalAPI_08_02_DefaultsAppliedAndReturned", Fn: RunExternalAPI_08_02_DefaultsAppliedAndReturned},
		parity.NamedTest{Name: "ExternalAPI_08_03_AdvancedCriteriaAndProcessors", Fn: RunExternalAPI_08_03_AdvancedCriteriaAndProcessors},
		parity.NamedTest{Name: "ExternalAPI_08_04_StrategyReplace", Fn: RunExternalAPI_08_04_StrategyReplace},
		parity.NamedTest{Name: "ExternalAPI_08_05_StrategyActivate", Fn: RunExternalAPI_08_05_StrategyActivate},
		parity.NamedTest{Name: "ExternalAPI_08_06_StrategyMerge", Fn: RunExternalAPI_08_06_StrategyMerge},
		parity.NamedTest{Name: "ExternalAPI_08_07_ScheduledTransitionRoundtrip", Fn: RunExternalAPI_08_07_ScheduledTransitionRoundtrip},
		parity.NamedTest{Name: "ExternalAPI_08_08_ScheduledTransitionRejects", Fn: RunExternalAPI_08_08_ScheduledTransitionRejects},
		parity.NamedTest{Name: "ExternalAPI_08_09_AllowCyclesBypass", Fn: RunExternalAPI_08_09_AllowCyclesBypass},
		parity.NamedTest{Name: "ExternalAPI_08_10_AsyncResultTrueRejects", Fn: RunExternalAPI_08_10_AsyncResultTrueRejects},
		parity.NamedTest{Name: "ExternalAPI_08_11_CrossoverOrphanRejects", Fn: RunExternalAPI_08_11_CrossoverOrphanRejects},
	)
}

// minimalWorkflow08 returns a two-state workflow with one automated transition
// guarded by a simple JSONPath criterion. Used by 08/01 and the strategy
// scenarios as a baseline workflow.
//
// Schema adaptations applied vs the dictionary's proposed shape:
//   - "to" → "next"          — different_naming_same_level
//   - "automated":true → "manual":false  — different_naming_same_level (inverted)
//   - criterion "type":"jsonpath","path","equals"
//     → "type":"simple","jsonPath","operatorType","value"  — different_naming_same_level
func minimalWorkflow08(name string) string {
	return `{
		"workflows": [{
			"name": "` + name + `",
			"version": "1.1",
			"initialState": "draft",
			"states": {
				"draft": {
					"transitions": [{
						"name": "PUBLISH",
						"next": "published",
						"manual": false,
						"criterion": {"type": "simple", "jsonPath": "$.publish", "operatorType": "EQUALS", "value": true}
					}]
				},
				"published": {"transitions": []}
			}
		}]
	}`
}

// RunExternalAPI_08_01_SimpleAutomatedTransition — dictionary 08/01.
// Import a simple workflow with one automated (non-manual) transition and
// export it; assert the transition name survives the round-trip.
func RunExternalAPI_08_01_SimpleAutomatedTransition(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)

	if err := d.CreateModelFromSample("wf1", 1, `{"k":1,"publish":false}`); err != nil {
		t.Fatalf("create model: %v", err)
	}
	if err := d.ImportWorkflow("wf1", 1, minimalWorkflow08("simple")); err != nil {
		t.Fatalf("ImportWorkflow: %v", err)
	}
	raw, err := d.ExportWorkflow("wf1", 1)
	if err != nil {
		t.Fatalf("ExportWorkflow: %v", err)
	}
	if !strings.Contains(string(raw), `"PUBLISH"`) {
		t.Errorf("export missing transition name PUBLISH: %s", string(raw))
	}
}

// RunExternalAPI_08_02_DefaultsAppliedAndReturned — dictionary 08/02.
// Import a partially-specified workflow (transition omits manual/criterion);
// export must round-trip the workflow with the transition name intact.
// cyoda-go stores what was sent and applies active=true as the only server
// default; omitted boolean fields default to false (Go zero value).
func RunExternalAPI_08_02_DefaultsAppliedAndReturned(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)

	if err := d.CreateModelFromSample("wf2", 1, `{"k":1}`); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Import a workflow that omits "manual" on the transition.
	// dictionary uses "automated" field; cyoda-go uses "manual" (inverted) —
	// different_naming_same_level. Omitting both → manual defaults to false
	// (i.e., automated=true in dictionary terms).
	body := `{
		"workflows": [{
			"name": "defaults",
			"version": "1.1",
			"initialState": "s1",
			"states": {
				"s1": {"transitions": [{"name": "MOVE", "next": "s2"}]},
				"s2": {"transitions": []}
			}
		}]
	}`
	if err := d.ImportWorkflow("wf2", 1, body); err != nil {
		t.Fatalf("ImportWorkflow: %v", err)
	}
	raw, err := d.ExportWorkflow("wf2", 1)
	if err != nil {
		t.Fatalf("ExportWorkflow: %v", err)
	}
	var shape map[string]any
	if err := json.Unmarshal(raw, &shape); err != nil {
		t.Fatalf("decode export: %v", err)
	}
	wfs, ok := shape["workflows"].([]any)
	if !ok || len(wfs) == 0 {
		t.Fatalf("export missing workflows: %v", shape)
	}
	// Assert the transition name survived the round-trip.
	if !strings.Contains(string(raw), `"MOVE"`) {
		t.Errorf("export missing transition name MOVE: %s", string(raw))
	}
}

// RunExternalAPI_08_03_AdvancedCriteriaAndProcessors — dictionary 08/03.
// Import a workflow with a group criterion (AND) and a processor.
// Export must round-trip both structures.
//
// Schema adaptations:
//   - Criterion: "type":"group","operator":"AND","clauses":[...]
//     → "type":"group","operator":"AND","conditions":[...]  — different_naming_same_level
//   - Inner criteria: "type":"jsonpath","path","equals"
//     → "type":"simple","jsonPath","operatorType","value"  — different_naming_same_level
//   - Processor: bare {"name","config"} → requires "type" field — different_naming_same_level
func RunExternalAPI_08_03_AdvancedCriteriaAndProcessors(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)

	if err := d.CreateModelFromSample("wf3", 1, `{"flag":true,"value":42}`); err != nil {
		t.Fatalf("create: %v", err)
	}
	// dictionary uses "clauses"; cyoda-go predicate.GroupCondition uses "conditions" —
	// different_naming_same_level.
	// dictionary uses criterion type "jsonpath" with "path"/"equals";
	// cyoda-go uses type "simple" with "jsonPath"/"operatorType"/"value" —
	// different_naming_same_level.
	// dictionary processor omits "type"; cyoda-go ProcessorDefinition.Type is required —
	// different_naming_same_level.
	body := `{
		"workflows": [{
			"name": "advanced",
			"version": "1.1",
			"initialState": "init",
			"states": {
				"init": {
					"transitions": [{
						"name": "ADVANCE",
						"next": "done",
						"manual": false,
						"criterion": {
							"type": "group",
							"operator": "AND",
							"conditions": [
								{"type": "simple", "jsonPath": "$.flag", "operatorType": "EQUALS", "value": true},
								{"type": "simple", "jsonPath": "$.value", "operatorType": "GREATER_THAN", "value": 10}
							]
						},
						"processors": [{"name": "noop", "type": "FUNCTION"}]
					}]
				},
				"done": {"transitions": []}
			}
		}]
	}`
	if err := d.ImportWorkflow("wf3", 1, body); err != nil {
		t.Fatalf("ImportWorkflow: %v", err)
	}
	raw, err := d.ExportWorkflow("wf3", 1)
	if err != nil {
		t.Fatalf("ExportWorkflow: %v", err)
	}
	if !strings.Contains(string(raw), `"ADVANCE"`) {
		t.Errorf("export missing transition ADVANCE: %s", string(raw))
	}
	// Assert the group criterion and processor survived the round-trip.
	if !strings.Contains(string(raw), `"group"`) {
		t.Errorf("export missing group criterion type: %s", string(raw))
	}
	if !strings.Contains(string(raw), `"noop"`) {
		t.Errorf("export missing processor name: %s", string(raw))
	}
}

// RunExternalAPI_08_04_StrategyReplace — dictionary 08/04.
// importMode=REPLACE removes all previous workflows before adding new ones.
func RunExternalAPI_08_04_StrategyReplace(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)

	if err := d.CreateModelFromSample("wf4", 1, `{"k":1,"publish":false}`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := d.ImportWorkflow("wf4", 1, minimalWorkflow08("first")); err != nil {
		t.Fatalf("first import: %v", err)
	}
	// Import with REPLACE — should drop the first workflow entirely.
	replaceBody := `{
		"importMode": "REPLACE",
		"workflows": [{
			"name": "second",
			"version": "1.1",
			"initialState": "draft",
			"states": {"draft": {"transitions": []}}
		}]
	}`
	if err := d.ImportWorkflow("wf4", 1, replaceBody); err != nil {
		t.Fatalf("REPLACE import: %v", err)
	}
	raw, err := d.ExportWorkflow("wf4", 1)
	if err != nil {
		t.Fatalf("ExportWorkflow after REPLACE: %v", err)
	}
	if strings.Contains(string(raw), `"first"`) {
		t.Errorf("REPLACE did not drop first workflow: %s", string(raw))
	}
	if !strings.Contains(string(raw), `"second"`) {
		t.Errorf("REPLACE did not add second workflow: %s", string(raw))
	}
}

// RunExternalAPI_08_05_StrategyActivate — dictionary 08/05.
// importMode=ACTIVATE keeps existing workflows but deactivates those not in the
// import list, and activates the incoming workflows.
func RunExternalAPI_08_05_StrategyActivate(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)

	if err := d.CreateModelFromSample("wf5", 1, `{"k":1,"publish":false}`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := d.ImportWorkflow("wf5", 1, minimalWorkflow08("first")); err != nil {
		t.Fatalf("first import: %v", err)
	}
	activateBody := `{
		"importMode": "ACTIVATE",
		"workflows": [{
			"name": "second",
			"version": "1.1",
			"initialState": "draft",
			"states": {"draft": {"transitions": []}}
		}]
	}`
	if err := d.ImportWorkflow("wf5", 1, activateBody); err != nil {
		t.Fatalf("ACTIVATE import: %v", err)
	}
	raw, err := d.ExportWorkflow("wf5", 1)
	if err != nil {
		t.Fatalf("ExportWorkflow after ACTIVATE: %v", err)
	}
	// ACTIVATE keeps both workflows; "first" is deactivated, "second" is active.
	// Both names must appear in the export.
	for _, name := range []string{"first", "second"} {
		if !strings.Contains(string(raw), `"`+name+`"`) {
			t.Errorf("ACTIVATE missing %s workflow in export: %s", name, string(raw))
		}
	}
}

// RunExternalAPI_08_06_StrategyMerge — dictionary 08/06.
// importMode=MERGE updates existing workflows by name and appends new ones;
// workflows not mentioned in the import are left untouched.
func RunExternalAPI_08_06_StrategyMerge(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)

	if err := d.CreateModelFromSample("wf6", 1, `{"k":1,"publish":false}`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := d.ImportWorkflow("wf6", 1, minimalWorkflow08("baseline")); err != nil {
		t.Fatalf("baseline import: %v", err)
	}
	// MERGE: update "baseline" in place and add a new "newone" workflow.
	// dictionary uses "automated":false → cyoda-go uses "manual":true —
	// different_naming_same_level.
	mergeBody := `{
		"importMode": "MERGE",
		"workflows": [
			{
				"name": "baseline",
				"version": "1.1",
				"initialState": "draft",
				"states": {
					"draft": {"transitions": [{"name": "PUBLISH", "next": "published", "manual": true}]},
					"published": {"transitions": []}
				}
			},
			{
				"name": "newone",
				"version": "1.1",
				"initialState": "s",
				"states": {"s": {"transitions": []}}
			}
		]
	}`
	if err := d.ImportWorkflow("wf6", 1, mergeBody); err != nil {
		t.Fatalf("MERGE import: %v", err)
	}
	raw, err := d.ExportWorkflow("wf6", 1)
	if err != nil {
		t.Fatalf("ExportWorkflow after MERGE: %v", err)
	}
	for _, name := range []string{"baseline", "newone"} {
		if !strings.Contains(string(raw), `"`+name+`"`) {
			t.Errorf("MERGE missing %s workflow in export: %s", name, string(raw))
		}
	}
}

// rfc9457Detail extracts the "detail" string from an RFC 9457 Problem
// Details body. Returns "" if the body is empty or doesn't parse.
// Used by negative-path scenarios 08/08 and 08/09 to assert on the
// human-readable validator message.
func rfc9457Detail(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var p struct {
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return ""
	}
	return p.Detail
}

// RunExternalAPI_08_07_ScheduledTransitionRoundtrip — dictionary 08/07.
// A scheduled transition (manual=false, schedule.delayMs > 0) is a
// sibling primitive on TransitionDefinition, distinct from processors[].
// Schedule.TimeoutMs is a pointer on the wire so the three semantic
// states are distinguishable through import/export:
//   - non-nil positive ("fire by deadline; drop if late")
//   - non-nil zero     ("strictest; drop the moment we are late")
//   - absent / nil     ("no timeout; fire indefinitely late")
//
// All three states must round-trip exactly — non-nil zero must be
// preserved in the exported JSON (key present with value 0), and the
// nil case must have no `timeoutMs` key at all.
func RunExternalAPI_08_07_ScheduledTransitionRoundtrip(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)

	if err := d.CreateModelFromSample("wfSchedRT", 1, `{"k":1}`); err != nil {
		t.Fatalf("create model: %v", err)
	}

	body := `{"workflows":[{
		"version":"1.1","name":"Scheduled Round-Trip Workflow",
		"desc":"Three scheduled transitions covering the TimeoutMs pointer states",
		"initialState":"start","active":true,
		"states":{
			"start":{"transitions":[
				{"name":"WithPositiveTimeout","next":"a","manual":false,
				 "schedule":{"delayMs":1000,"timeoutMs":90000000}},
				{"name":"WithZeroTimeout","next":"b","manual":false,
				 "schedule":{"delayMs":1000,"timeoutMs":0}},
				{"name":"WithoutTimeout","next":"c","manual":false,
				 "schedule":{"delayMs":1000}}
			]},
			"a":{"transitions":[]},
			"b":{"transitions":[]},
			"c":{"transitions":[]}
		}
	}]}`

	if err := d.ImportWorkflow("wfSchedRT", 1, body); err != nil {
		t.Fatalf("ImportWorkflow: %v", err)
	}

	raw, err := d.ExportWorkflow("wfSchedRT", 1)
	if err != nil {
		t.Fatalf("ExportWorkflow: %v", err)
	}

	// Parse the exported workflow and verify the three pointer states preserved.
	var doc struct {
		Workflows []struct {
			States map[string]struct {
				Transitions []struct {
					Name     string                 `json:"name"`
					Schedule map[string]interface{} `json:"schedule"`
				} `json:"transitions"`
			} `json:"states"`
		} `json:"workflows"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse exported workflow: %v\nraw: %s", err, raw)
	}
	if len(doc.Workflows) == 0 {
		t.Fatalf("export missing workflows: %s", raw)
	}
	startState, ok := doc.Workflows[0].States["start"]
	if !ok {
		t.Fatalf("export missing 'start' state: %s", raw)
	}
	byName := make(map[string]map[string]interface{}, 3)
	for _, tr := range startState.Transitions {
		byName[tr.Name] = tr.Schedule
	}

	// WithPositiveTimeout: non-nil positive → "timeoutMs": 90000000 must be present.
	posSched := byName["WithPositiveTimeout"]
	if posSched == nil {
		t.Fatal("WithPositiveTimeout schedule missing from export")
	}
	if v, ok := posSched["timeoutMs"]; !ok {
		t.Errorf("WithPositiveTimeout: expected timeoutMs field present; got schedule=%v", posSched)
	} else if f, _ := v.(float64); int64(f) != 90000000 {
		t.Errorf("WithPositiveTimeout: expected timeoutMs=90000000; got %v", v)
	}

	// WithZeroTimeout: non-nil zero → "timeoutMs": 0 must be present (NOT omitted).
	zeroSched := byName["WithZeroTimeout"]
	if zeroSched == nil {
		t.Fatal("WithZeroTimeout schedule missing from export")
	}
	if v, ok := zeroSched["timeoutMs"]; !ok {
		t.Errorf("WithZeroTimeout: expected timeoutMs=0 present (non-nil zero must survive omitempty); got missing in %v", zeroSched)
	} else if f, _ := v.(float64); int64(f) != 0 {
		t.Errorf("WithZeroTimeout: expected timeoutMs=0; got %v", v)
	}

	// WithoutTimeout: nil → no timeoutMs key.
	withoutSched := byName["WithoutTimeout"]
	if withoutSched == nil {
		t.Fatal("WithoutTimeout schedule missing from export")
	}
	if v, ok := withoutSched["timeoutMs"]; ok {
		t.Errorf("WithoutTimeout: expected no timeoutMs key (nil pointer should be omitted); got %v", v)
	}
}

// RunExternalAPI_08_08_ScheduledTransitionRejects — dictionary 08/08.
// Validator rejection matrix:
//  1. manual+schedule mutually exclusive — HTTP 400 + VALIDATION_FAILED
//     with "manual and scheduled are mutually exclusive" in the detail.
//  2. schedule.delayMs<=0 with no schedule.function — HTTP 400 +
//     VALIDATION_FAILED with "exactly one of schedule.delayMs or
//     schedule.function is required" in the detail. delayMs=0 is falsy
//     under the delayMs/function XOR (hasDelay := DelayMs > 0), so this
//     is the "neither present" shape, not a dedicated delayMs<=0 message.
func RunExternalAPI_08_08_ScheduledTransitionRejects(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)

	if err := d.CreateModelFromSample("wfSchedRej", 1, `{"k":1}`); err != nil {
		t.Fatalf("create model: %v", err)
	}

	// Sub-case 1: manual+schedule.
	manualPlusSchedule := `{"workflows":[{
		"version":"1.1","name":"Manual Plus Schedule",
		"initialState":"start","active":true,
		"states":{
			"start":{"transitions":[{
				"name":"BadManualScheduled","next":"end","manual":true,
				"schedule":{"delayMs":1000}
			}]},
			"end":{"transitions":[]}
		}
	}]}`
	status, body, err := d.ImportWorkflowRaw("wfSchedRej", 1, manualPlusSchedule)
	if err != nil {
		t.Fatalf("ImportWorkflowRaw (manual+schedule): %v", err)
	}
	errorcontract.Match(t, status, body, errorcontract.ExpectedError{
		HTTPStatus: http.StatusBadRequest,
		ErrorCode:  "VALIDATION_FAILED",
	})
	if detail := rfc9457Detail(body); !strings.Contains(detail, "manual and scheduled are mutually exclusive") {
		t.Errorf("manual+schedule: expected detail substring 'manual and scheduled are mutually exclusive'; got %q (body=%s)", detail, string(body))
	}

	// Sub-case 2: delayMs:0.
	zeroDelay := `{"workflows":[{
		"version":"1.1","name":"Zero Delay Schedule",
		"initialState":"start","active":true,
		"states":{
			"start":{"transitions":[{
				"name":"BadZeroDelay","next":"end","manual":false,
				"schedule":{"delayMs":0}
			}]},
			"end":{"transitions":[]}
		}
	}]}`
	status, body, err = d.ImportWorkflowRaw("wfSchedRej", 1, zeroDelay)
	if err != nil {
		t.Fatalf("ImportWorkflowRaw (zero delay): %v", err)
	}
	errorcontract.Match(t, status, body, errorcontract.ExpectedError{
		HTTPStatus: http.StatusBadRequest,
		ErrorCode:  "VALIDATION_FAILED",
	})
	if detail := rfc9457Detail(body); !strings.Contains(detail, "exactly one of schedule.delayMs or schedule.function is required") {
		t.Errorf("zero-delay: expected detail substring 'exactly one of schedule.delayMs or schedule.function is required'; got %q (body=%s)", detail, string(body))
	}
}

// RunExternalAPI_08_09_AllowCyclesBypass — dictionary 08/09.
// The validator rejects unguarded cycles in automated transitions to
// prevent FSM runaway loops. A scheduled-polling pattern (S1 -> S2 -> S1)
// is structurally a cycle but legitimate because each hop is rate-limited
// by schedule.delayMs. The import envelope carries an `allowCycles` flag
// (default false) that opts in to permitting such cycles.
//
// Default allowCycles=false (omitted) must reject the polling workflow
// with HTTP 400 + VALIDATION_FAILED. Explicit allowCycles=true must
// accept the same workflow.
func RunExternalAPI_08_09_AllowCyclesBypass(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)

	if err := d.CreateModelFromSample("wfAllowCycles", 1, `{"k":1}`); err != nil {
		t.Fatalf("create model: %v", err)
	}

	// Sub-case 1: WITHOUT allowCycles → cycle rejection.
	defaultBody := `{"workflows":[{
		"version":"1.1","name":"Polling Scheduled Workflow",
		"initialState":"S1","active":true,
		"states":{
			"S1":{"transitions":[{"name":"to-S2","next":"S2","manual":false,
			                      "schedule":{"delayMs":1000}}]},
			"S2":{"transitions":[{"name":"to-S1","next":"S1","manual":false,
			                      "schedule":{"delayMs":1000}}]}
		}
	}]}`
	status, body, err := d.ImportWorkflowRaw("wfAllowCycles", 1, defaultBody)
	if err != nil {
		t.Fatalf("ImportWorkflowRaw (default): %v", err)
	}
	errorcontract.Match(t, status, body, errorcontract.ExpectedError{
		HTTPStatus: http.StatusBadRequest,
		ErrorCode:  "VALIDATION_FAILED",
	})

	// Sub-case 2: WITH allowCycles=true → accept. Driver's ImportWorkflow
	// passes the body verbatim, so allowCycles slots in as a top-level
	// envelope field.
	allowBody := `{"allowCycles":true,"workflows":[{
		"version":"1.1","name":"Polling Scheduled Workflow",
		"initialState":"S1","active":true,
		"states":{
			"S1":{"transitions":[{"name":"to-S2","next":"S2","manual":false,
			                      "schedule":{"delayMs":1000}}]},
			"S2":{"transitions":[{"name":"to-S1","next":"S1","manual":false,
			                      "schedule":{"delayMs":1000}}]}
		}
	}]}`
	if err := d.ImportWorkflow("wfAllowCycles", 1, allowBody); err != nil {
		t.Fatalf("ImportWorkflow (allowCycles=true): expected acceptance; got: %v", err)
	}

	raw, err := d.ExportWorkflow("wfAllowCycles", 1)
	if err != nil {
		t.Fatalf("ExportWorkflow: %v", err)
	}
	var exp struct {
		Workflows []struct {
			Name string `json:"name"`
		} `json:"workflows"`
	}
	if err := json.Unmarshal(raw, &exp); err != nil {
		t.Fatalf("parse export: %v\nraw: %s", err, raw)
	}
	if len(exp.Workflows) != 1 {
		t.Errorf("expected 1 workflow in export; got %d (raw=%s)", len(exp.Workflows), raw)
	} else if exp.Workflows[0].Name != "Polling Scheduled Workflow" {
		t.Errorf("expected workflow name 'Polling Scheduled Workflow'; got %q", exp.Workflows[0].Name)
	}
}

// RunExternalAPI_08_10_AsyncResultTrueRejects — dictionary 08/10.
// Workflow import with asyncResult=true on a processor config is
// rejected with HTTP 400 + VALIDATION_FAILED and a detail containing
// "asyncResult=true is not supported".
func RunExternalAPI_08_10_AsyncResultTrueRejects(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)

	if err := d.CreateModelFromSample("wfAsyncRej", 1, `{"k":1}`); err != nil {
		t.Fatalf("create model: %v", err)
	}

	body := `{"workflows":[{
		"version":"1.1","name":"Async True Reject",
		"initialState":"start","active":true,
		"states":{
			"start":{"transitions":[{
				"name":"bad","next":"end","manual":false,
				"processors":[{"type":"externalized","name":"p","executionMode":"SYNC",
					"config":{"asyncResult":true}}]
			}]},
			"end":{"transitions":[]}
		}
	}]}`

	status, respBody, err := d.ImportWorkflowRaw("wfAsyncRej", 1, body)
	if err != nil {
		t.Fatalf("ImportWorkflowRaw: %v", err)
	}
	errorcontract.Match(t, status, respBody, errorcontract.ExpectedError{
		HTTPStatus: http.StatusBadRequest,
		ErrorCode:  "VALIDATION_FAILED",
	})
	if detail := rfc9457Detail(respBody); !strings.Contains(detail, "asyncResult=true is not supported") {
		t.Errorf("asyncResult=true: expected detail substring 'asyncResult=true is not supported'; got %q (body=%s)", detail, string(respBody))
	}
}

// RunExternalAPI_08_11_CrossoverOrphanRejects — dictionary 08/11.
// Workflow import with crossoverToAsyncMs set without asyncResult=true
// is rejected with HTTP 400 + VALIDATION_FAILED and a detail containing
// "crossoverToAsyncMs is not supported".
func RunExternalAPI_08_11_CrossoverOrphanRejects(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)

	if err := d.CreateModelFromSample("wfCrossOrphanRej", 1, `{"k":1}`); err != nil {
		t.Fatalf("create model: %v", err)
	}

	body := `{"workflows":[{
		"version":"1.1","name":"Crossover Orphan Reject",
		"initialState":"start","active":true,
		"states":{
			"start":{"transitions":[{
				"name":"bad","next":"end","manual":false,
				"processors":[{"type":"externalized","name":"p","executionMode":"SYNC",
					"config":{"crossoverToAsyncMs":5000}}]
			}]},
			"end":{"transitions":[]}
		}
	}]}`

	status, respBody, err := d.ImportWorkflowRaw("wfCrossOrphanRej", 1, body)
	if err != nil {
		t.Fatalf("ImportWorkflowRaw: %v", err)
	}
	errorcontract.Match(t, status, respBody, errorcontract.ExpectedError{
		HTTPStatus: http.StatusBadRequest,
		ErrorCode:  "VALIDATION_FAILED",
	})
	if detail := rfc9457Detail(respBody); !strings.Contains(detail, "crossoverToAsyncMs is not supported") {
		t.Errorf("crossover-orphan: expected detail substring 'crossoverToAsyncMs is not supported'; got %q (body=%s)", detail, string(respBody))
	}
}
