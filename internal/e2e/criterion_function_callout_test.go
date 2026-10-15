package e2e_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// Function conditions inside criterion groups, through the callout path:
// a member failure, an unserved tag, and scheduled firing.

// cfTaggedFn is a function condition whose callout goes to the nodes
// serving tag.
func cfTaggedFn(name, tag, preCheck string) string {
	cfg := `"config":{"calculationNodesTags":"` + tag + `","attachEntity":true}`
	if preCheck == "" {
		return `{"type":"function","function":{"name":"` + name + `",` + cfg + `}}`
	}
	return `{"type":"function","function":{"name":"` + name + `",` + cfg + `,"criterion":` + preCheck + `}}`
}

// scriptByName answers each callout by the function name it carries.
func scriptByName(replies map[string]cnodeReply) cnodeScript {
	return func(_ context.Context, call receivedCallout, _ *reqCtx) cnodeReply { return replies[call.Name] }
}

func TestCriterionFunctionCallout_NoMemberInGroup_503(t *testing.T) {
	h := newCallbackHarness(t)
	model, tag := uniq("cfc-nomem"), uniq("cfc-nomem-tag")
	h.SetupModelWithWorkflow(t, model, cfWorkflow("cfc-nomem-wf", cfGroup("AND", cfAmountGT10, cfTaggedFn("x", tag, ""))))

	_, status, body := h.CreateEntity(t, model, 1, workflowSampleModel)
	assertProblem(t, status, body, http.StatusServiceUnavailable, "NO_COMPUTE_MEMBER_FOR_TAG", true)
}

func TestCriterionFunctionCallout_MemberFailsInGroup_400(t *testing.T) {
	h := newCallbackHarness(t)
	model, tag := uniq("cfc-fail"), uniq("cfc-fail-tag")
	cn := h.AttachCnode(t, cnodeSpec{name: "fns", tags: []string{tag}, script: scriptByName(map[string]cnodeReply{
		"a": answerMatches(false),
		"b": answerFail("b down"),
	})})
	h.SetupModelWithWorkflow(t, model, cfWorkflow("cfc-fail-wf", cfGroup("OR", cfTaggedFn("a", tag, ""), cfTaggedFn("b", tag, ""))))

	_, status, body := h.CreateEntity(t, model, 1, workflowSampleModel)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400 (body: %s)", status, body)
	}
	if got := problemErrorCode(body); got != "WORKFLOW_FAILED" {
		t.Errorf("errorCode = %q; want WORKFLOW_FAILED (body: %s)", got, body)
	}
	got := cn.Received()
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "b" {
		t.Errorf("callouts = %v; want a, then b", got)
	}
}

func TestCriterionFunctionCallout_Scheduled_PreCheckFalseDeclined(t *testing.T) {
	h, s := newSchedulerHarness(t, nil)
	model, tag := uniq("cfc-decl"), uniq("cfc-decl-tag")
	cn := h.AttachCnode(t, cnodeSpec{name: "crit", tags: []string{tag}, script: scriptAlways(answerMatches(true))})
	preCheck := `{"type":"simple","jsonPath":"$.amount","operatorType":"GREATER_THAN","value":1000}`
	h.SetupModelWithWorkflow(t, model, schedDoc("cfc-decl-wf", map[string]any{
		"Open": map[string]any{"transitions": []any{map[string]any{
			"name": "Fire", "next": "Done", "manual": false, "schedule": map[string]any{"delayMs": 100},
			"criterion": json.RawMessage(cfTaggedFn("c", tag, preCheck)),
		}}},
		"Done": map[string]any{},
	}))
	id := createOpen(t, h, model, workflowSampleModel)

	awaitCallbackSMEventType(t, h, id, "TRANSITION_NOT_MATCH_CRITERION", "Open", scheduledFireTimeout)
	s.awaitTask(t, id, "Fire", scheduledFireTimeout, "removal", func(_ taskRow, ok bool) bool { return !ok })
	requireState(t, h, id, "Open")
	if n := len(cn.Received()); n != 0 {
		t.Errorf("the cnode received %d callouts; want 0 (the pre-check does not match)", n)
	}
}

func TestCriterionFunctionCallout_Scheduled_FailureInGroupRetried(t *testing.T) {
	h, s := newSchedulerHarness(t, nil)
	model, tag := uniq("cfc-retry"), uniq("cfc-retry-tag")
	h.AttachCnode(t, cnodeSpec{name: "crit", tags: []string{tag}, script: scriptAlways(answerFail("crit boom"))})
	h.SetupModelWithWorkflow(t, model, schedDoc("cfc-retry-wf", map[string]any{
		"Open": map[string]any{"transitions": []any{map[string]any{
			"name": "Fire", "next": "Done", "manual": false, "schedule": map[string]any{"delayMs": 100},
			"criterion": json.RawMessage(cfGroup("AND", cfAmountGT10, cfTaggedFn("c", tag, ""))),
		}}},
		"Done": map[string]any{},
	}))
	id := createOpen(t, h, model, workflowSampleModel)

	r := firstAttempt(t, s, id, "Fire")
	if r.Status != "WAITING" || r.Attempts != 1 || r.FailureReason != "" || r.LastError != "crit boom" {
		t.Errorf("after the failed callout: %+v; want WAITING, attempts 1, lastError \"crit boom\"", r)
	}
	requireState(t, h, id, "Open")
}
