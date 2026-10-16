package workflow

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"

	"github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/e2e/openapivalidator"
)

func TestConstructors_ValidateAgainstTheirMember(t *testing.T) {
	doc, err := api.GetSwagger()
	if err != nil {
		t.Fatalf("GetSwagger: %v", err)
	}
	if err := openapivalidator.ApplyView(doc); err != nil {
		t.Fatalf("ApplyView: %v", err)
	}
	// The `data` property of a state-machine audit event is a use of the base
	// StateMachineEventDto; ApplyView turns that use into a discriminated
	// oneOf. The base schema itself is not rewritten, so validating against
	// it would accept any object.
	schema := doc.Components.Schemas["StateMachineAuditEventDto"].Value.AllOf[1].Value.Properties["data"].Value

	withFn := criterionOutcome{functions: []functionRecord{
		{path: "/conditions/0", name: "f", hasPreCheck: true, preCheck: map[string]any{"type": "simple", "jsonPath": "$.a", "operatorType": "EQUALS", "value": 1}, preCheckMatched: true, called: true, matches: false, reason: "too small"},
		{path: "/conditions/1", name: "g", hasPreCheck: true, preCheck: map[string]any{"type": "simple", "jsonPath": "$.b", "operatorType": "EQUALS", "value": 2}},
	}}
	emptyFn := criterionOutcome{functions: []functionRecord{}}
	noFn := criterionOutcome{}

	stop := StopReason{Type: "COMPLETED_ALL_CRITERIA_REJECTED", Description: "refused", Rejected: []RejectedTransition{
		{TransitionName: "t1", Criterion: "simple", Reason: "criterion did not match"},
	}}

	cases := []struct {
		name  string
		event string
		data  map[string]any
		keys  []string
	}{
		{"started with state", "STATE_MACHINE_START", startedData("NEW"), []string{"state"}},
		{"started on create", "STATE_MACHINE_START", startedData(""), nil},
		{"finished with rejections", "STATE_MACHINE_FINISH", finishedData("NEW", stop), []string{"state", "stopReason", "success"}},
		{"finished without rejections", "STATE_MACHINE_FINISH", finishedData("NEW", StopReason{Type: "COMPLETED_NO_AUTO_TRANSITIONS", Description: "none"}), []string{"state", "stopReason", "success"}},
		{"cancelled", "CANCEL", cancelledData("by caller"), []string{"reason"}},
		{"forced success", "FORCE_SUCCESS", forcedSuccessData(), nil},
		{"workflow found, nil outcome", "WORKFLOW_FOUND", workflowFoundData("wf", nil), []string{"workflowName"}},
		{"workflow found, no function", "WORKFLOW_FOUND", workflowFoundData("wf", &noFn), []string{"workflowName"}},
		{"workflow found, empty functions", "WORKFLOW_FOUND", workflowFoundData("wf", &emptyFn), []string{"functions", "workflowName"}},
		{"workflow found, functions", "WORKFLOW_FOUND", workflowFoundData("wf", &withFn), []string{"functions", "workflowName"}},
		{"workflow skipped, functions", "WORKFLOW_SKIP", workflowSkippedData("wf", "no match", withFn), []string{"functions", "reason", "workflowName"}},
		{"workflow skipped, no function", "WORKFLOW_SKIP", workflowSkippedData("wf", "no match", noFn), []string{"reason", "workflowName"}},
		{"transition made, functions", "TRANSITION_MAKE", transitionMadeData("wf", "go", "NEXT", &withFn), []string{"functions", "newState", "transition", "workflowName"}},
		{"transition made, nil outcome", "TRANSITION_MAKE", transitionMadeData("wf", "go", "NEXT", nil), []string{"newState", "transition", "workflowName"}},
		{"transition not found", "TRANSITION_NOT_FOUND", transitionNotFoundData("wf", "go"), []string{"transition", "workflowName"}},
		{"transition not matched, functions", "TRANSITION_NOT_MATCH_CRITERION", transitionNotMatchedData("wf", "go", "f", "too small", withFn), []string{"criterion", "functions", "reason", "transition", "workflowName"}},
		{"transition not matched, no function", "TRANSITION_NOT_MATCH_CRITERION", transitionNotMatchedData("wf", "go", "simple", "criterion did not match", noFn), []string{"criterion", "reason", "transition", "workflowName"}},
		{"transition aborted", "TRANSITION_ABORTED", transitionAbortedData("go", "tx-1", "tx-2"), []string{"actualTxId", "expectedTxId", "reason", "transition"}},
		{"transition aborted on loopback", "TRANSITION_ABORTED", transitionAbortedData("", "tx-1", "tx-2"), []string{"actualTxId", "expectedTxId", "reason"}},
		{"processing paused", "PAUSE_FOR_PROCESSING", processingPausedData("wf", "go", []string{"p1", "p2"}), []string{"processNames", "transition", "workflowName"}},
		{"state process result ok", "STATE_PROCESS_RESULT", stateProcessResultData("p1", "ASYNC_NEW_TX", true, ""), []string{"executionMode", "processorName", "success"}},
		{"state process result failed", "STATE_PROCESS_RESULT", stateProcessResultData("p1", "SYNC", false, "bad input"), []string{"executionMode", "failReason", "processorName", "success"}},
		{"scheduled armed", "SCHEDULED_TRANSITION_ARM", ScheduledArmedData("go", "NEW", 1760000000000), []string{"scheduledTime", "sourceState", "transition"}},
		{"scheduled fired", "SCHEDULED_TRANSITION_FIRE", ScheduledFiredData("go", "NEW", "wf"), []string{"sourceState", "transition", "workflowName"}},
		{"scheduled expired", "SCHEDULED_TRANSITION_EXPIRE", ScheduledExpiredData("go", "NEW"), []string{"sourceState", "transition"}},
		{"scheduled cancelled with workflow", "SCHEDULED_TRANSITION_CANCEL", ScheduledCancelledData("go", "NEW", "wf"), []string{"sourceState", "transition", "workflowName"}},
		{"scheduled cancelled without workflow", "SCHEDULED_TRANSITION_CANCEL", ScheduledCancelledData("go", "NEW", ""), []string{"sourceState", "transition"}},
		{"scheduled failed", "SCHEDULED_TRANSITION_FAIL", ScheduledFailedData("go", "NEW", "RUN_PANICKED", 3, 0), []string{"attempts", "lostOwners", "reason", "sourceState", "transition"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// What the wire carries: marshal, then unmarshal into JSON values.
			raw, err := json.Marshal(tc.data)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var wire map[string]any
			if err := json.Unmarshal(raw, &wire); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			got := make([]string, 0, len(wire))
			for k := range wire {
				got = append(got, k)
			}
			sort.Strings(got)
			want := append([]string{}, tc.keys...)
			sort.Strings(want)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("keys = %v, want %v", got, want)
			}
			wire["eventType"] = tc.event
			if err := schema.VisitJSON(wire); err != nil {
				t.Errorf("%s data does not validate against its member: %v\n%s", tc.event, err, raw)
			}
		})
	}
}

func TestStateProcessResultData_EmptyModeIsSync(t *testing.T) {
	got := stateProcessResultData("p", "", true, "")
	if got["executionMode"] != "SYNC" {
		t.Errorf("executionMode = %v, want SYNC", got["executionMode"])
	}
}
