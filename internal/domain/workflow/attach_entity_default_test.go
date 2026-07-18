package workflow

import (
	"encoding/json"
	"testing"
)

// TestApplyScheduleFunctionAttachEntityDefault_HeterogeneousTransitions is a
// direct unit test (C1, final review) guarding
// applyScheduleFunctionAttachEntityDefault's raw-JSON re-decode/index-align
// probe. The logic itself is unchanged — this only adds coverage the
// existing single-transition, full-HTTP-import tests
// (TestImport_ScheduleFunction_AttachEntityOmitted_DefaultsToTrue /
// ...ExplicitFalse_Preserved in handler_test.go) don't reach: a
// multi-workflow request with a heterogeneous transition mix in one state
// (a no-schedule transition, a delayMs-only transition, and three
// schedule.function transitions with attachEntity omitted/explicit-false/
// explicit-true), which exercises the index-alignment between workflows
// and probe.Workflows at both the workflow level and the per-state
// transition level.
//
// Mirrors ImportEntityModelWorkflow's own mechanism (handler.go): decode
// the SAME raw JSON bytes twice — once strictly into []workflowImportDef,
// once loosely into scheduleFunctionAttachEntityProbe — rather than
// hand-building the probe's unexported anonymous-struct literals, so the
// test input is guaranteed aligned the same way production input is.
func TestApplyScheduleFunctionAttachEntityDefault_HeterogeneousTransitions(t *testing.T) {
	raw := []byte(`{
		"importMode": "REPLACE",
		"workflows": [
			{
				"version": "1.1",
				"name": "wf1",
				"initialState": "S",
				"states": {
					"S": {
						"transitions": [
							{
								"name": "T_NO_SCHEDULE",
								"next": "S",
								"manual": false
							},
							{
								"name": "T_DELAY_ONLY",
								"next": "S",
								"manual": false,
								"schedule": {"delayMs": 5000}
							},
							{
								"name": "T_FN_OMITTED",
								"next": "S",
								"manual": false,
								"schedule": {
									"function": {
										"name": "f1",
										"resultKind": "Schedule",
										"calculationNodesTags": "billing"
									}
								}
							},
							{
								"name": "T_FN_EXPLICIT_FALSE",
								"next": "S",
								"manual": false,
								"schedule": {
									"function": {
										"name": "f2",
										"resultKind": "Schedule",
										"calculationNodesTags": "billing",
										"attachEntity": false
									}
								}
							},
							{
								"name": "T_FN_EXPLICIT_TRUE",
								"next": "S",
								"manual": false,
								"schedule": {
									"function": {
										"name": "f3",
										"resultKind": "Schedule",
										"calculationNodesTags": "billing",
										"attachEntity": true
									}
								}
							}
						]
					}
				}
			},
			{
				"version": "1.1",
				"name": "wf2",
				"initialState": "S2",
				"states": {
					"S2": {
						"transitions": [
							{
								"name": "T2_NO_SCHEDULE",
								"next": "S2",
								"manual": false
							},
							{
								"name": "T2_FN_OMITTED",
								"next": "S2",
								"manual": false,
								"schedule": {
									"function": {
										"name": "g1",
										"resultKind": "Schedule",
										"calculationNodesTags": "billing"
									}
								}
							}
						]
					}
				}
			}
		]
	}`)

	var req importRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("decode importRequest: %v", err)
	}
	var probe scheduleFunctionAttachEntityProbe
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("decode probe: %v", err)
	}

	applyScheduleFunctionAttachEntityDefault(req.Workflows, probe)

	wf1Trs := req.Workflows[0].States["S"].Transitions
	if len(wf1Trs) != 5 {
		t.Fatalf("wf1: expected 5 transitions, got %d", len(wf1Trs))
	}
	if wf1Trs[0].Schedule != nil {
		t.Errorf("T_NO_SCHEDULE: expected Schedule to stay nil, got %+v", wf1Trs[0].Schedule)
	}
	if wf1Trs[1].Schedule == nil || wf1Trs[1].Schedule.Function != nil {
		t.Errorf("T_DELAY_ONLY: expected Schedule.Function to stay nil, got %+v", wf1Trs[1].Schedule)
	}
	if got := wf1Trs[2].Schedule.Function.AttachEntity; !got {
		t.Errorf("T_FN_OMITTED: AttachEntity = %v, want true (defaulted)", got)
	}
	if got := wf1Trs[3].Schedule.Function.AttachEntity; got {
		t.Errorf("T_FN_EXPLICIT_FALSE: AttachEntity = %v, want false (preserved)", got)
	}
	if got := wf1Trs[4].Schedule.Function.AttachEntity; !got {
		t.Errorf("T_FN_EXPLICIT_TRUE: AttachEntity = %v, want true (preserved)", got)
	}

	wf2Trs := req.Workflows[1].States["S2"].Transitions
	if len(wf2Trs) != 2 {
		t.Fatalf("wf2: expected 2 transitions, got %d", len(wf2Trs))
	}
	if wf2Trs[0].Schedule != nil {
		t.Errorf("T2_NO_SCHEDULE: expected Schedule to stay nil, got %+v", wf2Trs[0].Schedule)
	}
	if got := wf2Trs[1].Schedule.Function.AttachEntity; !got {
		t.Errorf("T2_FN_OMITTED: AttachEntity = %v, want true (defaulted) — workflow-index alignment broken", got)
	}
}
