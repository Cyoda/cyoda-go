package workflow

// One constructor per state-machine event type. Each builds exactly the
// properties of that event's member schema in api/openapi.yaml, beside the
// `eventType` the audit writer adds. An optional property whose value the
// engine does not know is left out.

// StopReason is why a run stopped, as the FINISH event's `stopReason`.
type StopReason struct {
	Type        string
	Description string
	Rejected    []RejectedTransition
}

// RejectedTransition is one automated transition the last pass refused.
type RejectedTransition struct {
	TransitionName string
	Criterion      string
	Reason         string
}

func (s StopReason) auditValue() map[string]any {
	v := map[string]any{"type": s.Type, "description": s.Description}
	if len(s.Rejected) > 0 {
		list := make([]any, len(s.Rejected))
		for i, r := range s.Rejected {
			list[i] = map[string]any{
				"transitionName": r.TransitionName,
				"criterion":      r.Criterion,
				"reason":         r.Reason,
			}
		}
		v["rejectedTransitions"] = list
	}
	return v
}

// startedData is STATE_MACHINE_START; state is empty on create.
func startedData(state string) map[string]any {
	data := map[string]any{}
	if state != "" {
		data["state"] = state
	}
	return data
}

func finishedData(state string, stop StopReason) map[string]any {
	return map[string]any{"state": state, "success": true, "stopReason": stop.auditValue()}
}

func cancelledData(reason string) map[string]any {
	return map[string]any{"reason": reason}
}

func forcedSuccessData() map[string]any {
	return map[string]any{}
}

// workflowFoundData is WORKFLOW_FOUND; out may be nil.
func workflowFoundData(workflowName string, out *criterionOutcome) map[string]any {
	data := map[string]any{"workflowName": workflowName}
	if out != nil {
		out.addFunctions(data)
	}
	return data
}

func workflowSkippedData(workflowName, reason string, out criterionOutcome) map[string]any {
	data := map[string]any{"workflowName": workflowName, "reason": reason}
	out.addFunctions(data)
	return data
}

// transitionMadeData is TRANSITION_MAKE; out may be nil.
func transitionMadeData(workflowName, transition, newState string, out *criterionOutcome) map[string]any {
	data := map[string]any{"workflowName": workflowName, "transition": transition, "newState": newState}
	if out != nil {
		out.addFunctions(data)
	}
	return data
}

func transitionNotFoundData(workflowName, transition string) map[string]any {
	return map[string]any{"workflowName": workflowName, "transition": transition}
}

func transitionNotMatchedData(workflowName, transition, criterion, reason string, out criterionOutcome) map[string]any {
	data := map[string]any{
		"workflowName": workflowName,
		"transition":   transition,
		"criterion":    criterion,
		"reason":       reason,
	}
	out.addFunctions(data)
	return data
}

// transitionAbortedData is TRANSITION_ABORTED; transition is empty on a
// loopback, which has no transition.
func transitionAbortedData(transition string, expectedTxID, actualTxID string) map[string]any {
	data := map[string]any{
		"reason":       "ENTITY_MODIFIED",
		"expectedTxId": expectedTxID,
		"actualTxId":   actualTxID,
	}
	if transition != "" {
		data["transition"] = transition
	}
	return data
}

func processingPausedData(workflowName, transition string, processNames []string) map[string]any {
	names := make([]any, len(processNames))
	for i, n := range processNames {
		names[i] = n
	}
	return map[string]any{"workflowName": workflowName, "transition": transition, "processNames": names}
}

// stateProcessResultData is STATE_PROCESS_RESULT. An empty executionMode runs
// as SYNC and is recorded so; failReason is omitted when empty.
func stateProcessResultData(processorName, executionMode string, success bool, failReason string) map[string]any {
	if executionMode == "" {
		executionMode = "SYNC"
	}
	data := map[string]any{
		"processorName": processorName,
		"executionMode": executionMode,
		"success":       success,
	}
	if failReason != "" {
		data["failReason"] = failReason
	}
	return data
}

// ScheduledArmedData is SCHEDULED_TRANSITION_ARM; scheduledTimeMs is Unix ms.
func ScheduledArmedData(transition, sourceState string, scheduledTimeMs int64) map[string]any {
	return map[string]any{"transition": transition, "sourceState": sourceState, "scheduledTime": scheduledTimeMs}
}

func ScheduledFiredData(transition, sourceState, workflowName string) map[string]any {
	return map[string]any{"transition": transition, "sourceState": sourceState, "workflowName": workflowName}
}

func ScheduledExpiredData(transition, sourceState string) map[string]any {
	return map[string]any{"transition": transition, "sourceState": sourceState}
}

// ScheduledCancelledData is SCHEDULED_TRANSITION_CANCEL; workflowName is empty
// where the selected workflow is not known.
func ScheduledCancelledData(transition, sourceState, workflowName string) map[string]any {
	data := map[string]any{"transition": transition, "sourceState": sourceState}
	if workflowName != "" {
		data["workflowName"] = workflowName
	}
	return data
}

// ScheduledFailedData is SCHEDULED_TRANSITION_FAIL; reason is an open string.
func ScheduledFailedData(transition, sourceState, reason string, attempts, lostOwners int) map[string]any {
	return map[string]any{
		"transition":  transition,
		"sourceState": sourceState,
		"reason":      reason,
		"attempts":    attempts,
		"lostOwners":  lostOwners,
	}
}
