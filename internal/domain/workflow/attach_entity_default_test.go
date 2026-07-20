package workflow

import (
	"encoding/json"
	"testing"
)

// TestApplyAttachEntityDefaults_HeterogeneousTransitions is a direct unit
// test guarding applyAttachEntityDefaults' raw-JSON re-decode/index-align
// probe for schedule.function.attachEntity. It covers a case the existing
// single-transition, full-HTTP-import tests
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
// once loosely into attachEntityProbe — rather than hand-building the
// probe's unexported anonymous-struct literals, so the test input is
// guaranteed aligned the same way production input is.
func TestApplyAttachEntityDefaults_HeterogeneousTransitions(t *testing.T) {
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
	var probe attachEntityProbe
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("decode probe: %v", err)
	}

	applyAttachEntityDefaults(req.Workflows, probe)

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

// TestApplyAttachEntityDefaults_Processors is the processor companion to the
// schedule.function test above: it exercises the processor-index alignment of
// applyAttachEntityDefaults. One transition carries three processors whose
// config.attachEntity is omitted / explicit-false / explicit-true; a second
// transition carries none. Decodes the same raw bytes twice, mirroring the
// handler, so the probe indices line up the same way production input does.
func TestApplyAttachEntityDefaults_Processors(t *testing.T) {
	raw := []byte(`{
		"importMode": "REPLACE",
		"workflows": [
			{
				"version": "1.3",
				"name": "wf",
				"initialState": "S",
				"states": {
					"S": {
						"transitions": [
							{
								"name": "T_NO_PROC",
								"next": "S",
								"manual": false
							},
							{
								"name": "T_PROCS",
								"next": "S",
								"manual": false,
								"processors": [
									{"name": "p_omitted", "type": "externalized", "config": {"calculationNodesTags": "billing"}},
									{"name": "p_false", "type": "externalized", "config": {"attachEntity": false}},
									{"name": "p_true", "type": "externalized", "config": {"attachEntity": true}}
								]
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
	var probe attachEntityProbe
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("decode probe: %v", err)
	}

	applyAttachEntityDefaults(req.Workflows, probe)

	trs := req.Workflows[0].States["S"].Transitions
	if len(trs) != 2 {
		t.Fatalf("expected 2 transitions, got %d", len(trs))
	}
	if len(trs[0].Processors) != 0 {
		t.Errorf("T_NO_PROC: expected no processors, got %d", len(trs[0].Processors))
	}
	procs := trs[1].Processors
	if len(procs) != 3 {
		t.Fatalf("T_PROCS: expected 3 processors, got %d", len(procs))
	}
	if got := procs[0].Config.AttachEntity; !got {
		t.Errorf("p_omitted: AttachEntity = %v, want true (defaulted)", got)
	}
	if got := procs[1].Config.AttachEntity; got {
		t.Errorf("p_false: AttachEntity = %v, want false (preserved)", got)
	}
	if got := procs[2].Config.AttachEntity; !got {
		t.Errorf("p_true: AttachEntity = %v, want true (preserved)", got)
	}
}
