package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// Function conditions and pre-checks evaluated through the HTTP entry points,
// with the in-process processing service answering the callouts.

// cfEventData returns the data of the first state-machine event of type typ.
func cfEventData(t *testing.T, entityID, typ string) map[string]any {
	t.Helper()
	for _, ev := range getSMAuditEvents(t, entityID) {
		if ev["eventType"] == typ {
			d, _ := ev["data"].(map[string]any)
			return d
		}
	}
	t.Fatalf("no %s event for %s", typ, entityID)
	return nil
}

func cfFunctions(t *testing.T, data map[string]any) []map[string]any {
	t.Helper()
	raw, ok := data["functions"].([]any)
	if !ok {
		t.Fatalf("data has no functions list: %v", data)
	}
	out := make([]map[string]any, len(raw))
	for i, r := range raw {
		out[i], _ = r.(map[string]any)
	}
	return out
}

func cfAnswer(m bool, reason string) func(context.Context, *spi.Entity, json.RawMessage) (bool, string, error) {
	return func(context.Context, *spi.Entity, json.RawMessage) (bool, string, error) { return m, reason, nil }
}

func cfPayload(amount int) string {
	return fmt.Sprintf(`{"name":"X","amount":%d,"status":"draft"}`, amount)
}

func cfReasonOf(t *testing.T, entityID, typ string) string {
	t.Helper()
	r, _ := cfEventData(t, entityID, typ)["reason"].(string)
	return r
}

func TestCriterionFunction_Cascade_PreCheckFalse_NoCallout(t *testing.T) {
	const model = "e2e-cfe-pcf"
	procSvc.RegisterCriteriaReason("cf-pcf-a", cfAnswer(true, ""))
	setupModelWithWorkflow(t, model, cfWorkflow("cfe-pcf", cfFn("cf-pcf-a", cfAmountGT100)))
	id := createEntityE2E(t, model, 1, cfPayload(50))

	if s := getEntityState(t, id); s != "CREATED" {
		t.Errorf("state = %q; want CREATED", s)
	}
	if n := procSvc.CriteriaCallCount("cf-pcf-a"); n != 0 {
		t.Errorf("call count = %d; want 0", n)
	}
	d := cfEventData(t, id, "TRANSITION_NOT_MATCH_CRITERION")
	if r, _ := d["reason"].(string); r != "cf-pcf-a: pre-check did not match, not called" {
		t.Errorf("reason = %q", r)
	}
	f := cfFunctions(t, d)[0]
	if f["path"] != "" || f["called"] != false || f["preCheckMatched"] != false {
		t.Errorf("functions[0] = %v", f)
	}
	pc, _ := f["preCheck"].(map[string]any)
	if pc["jsonPath"] != "$.amount" {
		t.Errorf("preCheck = %v; want jsonPath $.amount", f["preCheck"])
	}
}

func TestCriterionFunction_Cascade_PreCheckTrue_Called(t *testing.T) {
	const model = "e2e-cfe-pct"
	procSvc.RegisterCriteriaReason("cf-pct-a", cfAnswer(true, ""))
	setupModelWithWorkflow(t, model, cfWorkflow("cfe-pct", cfFn("cf-pct-a", cfAmountGT100)))
	id := createEntityE2E(t, model, 1, cfPayload(150))

	if s := getEntityState(t, id); s != "PROMOTED" {
		t.Errorf("state = %q; want PROMOTED", s)
	}
	if n := procSvc.CriteriaCallCount("cf-pct-a"); n != 1 {
		t.Errorf("call count = %d; want 1", n)
	}
	f := cfFunctions(t, cfEventData(t, id, "TRANSITION_MAKE"))[0]
	if f["called"] != true || f["matches"] != true {
		t.Errorf("functions[0] = %v; want called and matches", f)
	}
}

func TestCriterionFunction_Cascade_Groups(t *testing.T) {
	rows := []struct {
		name      string
		build     func(a, b string) string
		aAns      bool
		bAns      bool
		wantState string
		wantA     int
		wantB     int
		reason    string
	}{
		{"and-a-true-b-false", func(a, b string) string { return cfGroup("AND", cfFn(a, ""), cfFn(b, "")) },
			true, false, "CREATED", 1, 1,
			"cf-grp1-a at /conditions/0: answered true; cf-grp1-b at /conditions/1: answered false"},
		{"and-precheck-false", func(a, b string) string { return cfGroup("AND", cfAmountGT100, cfFn(a, "")) },
			true, false, "CREATED", 0, 0, ""},
		{"or-a-true", func(a, b string) string { return cfGroup("OR", cfFn(a, ""), cfFn(b, "")) },
			true, false, "PROMOTED", 1, 0, ""},
		{"or-a-false-b-true", func(a, b string) string { return cfGroup("OR", cfFn(a, ""), cfFn(b, "")) },
			false, true, "PROMOTED", 1, 1, ""},
		{"not-a-true", func(a, b string) string { return cfGroup("NOT", cfFn(a, "")) },
			true, false, "CREATED", 1, 0,
			"cf-grp5-a at /conditions/0: answered true"},
		{"not-and", func(a, b string) string { return cfGroup("NOT", cfGroup("AND", cfFn(a, ""), cfFn(b, ""))) },
			true, false, "PROMOTED", 1, 1, ""},
	}
	for i, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			model := fmt.Sprintf("e2e-cfe-grp%d", i+1)
			a, b := fmt.Sprintf("cf-grp%d-a", i+1), fmt.Sprintf("cf-grp%d-b", i+1)
			procSvc.RegisterCriteriaReason(a, cfAnswer(tc.aAns, ""))
			procSvc.RegisterCriteriaReason(b, cfAnswer(tc.bAns, ""))
			setupModelWithWorkflow(t, model, cfWorkflow(fmt.Sprintf("cfe-grp%d", i+1), tc.build(a, b)))
			id := createEntityE2E(t, model, 1, cfPayload(50))

			if s := getEntityState(t, id); s != tc.wantState {
				t.Errorf("state = %q; want %q", s, tc.wantState)
			}
			if n := procSvc.CriteriaCallCount(a); n != tc.wantA {
				t.Errorf("a calls = %d; want %d", n, tc.wantA)
			}
			if n := procSvc.CriteriaCallCount(b); n != tc.wantB {
				t.Errorf("b calls = %d; want %d", n, tc.wantB)
			}
			if tc.reason != "" {
				if r := cfReasonOf(t, id, "TRANSITION_NOT_MATCH_CRITERION"); r != tc.reason {
					t.Errorf("reason = %q; want %q", r, tc.reason)
				}
			}
		})
	}
}

func TestCriterionFunction_Cascade_FalseGroupThenNextTransitionFires(t *testing.T) {
	const model = "e2e-cfe-fallback"
	procSvc.RegisterCriteriaReason("cf-fb-a", cfAnswer(false, ""))
	wf := `{"importMode":"REPLACE","workflows":[{"version":"1.5","name":"cfe-fb","initialState":"NONE","active":true,
	"states":{"NONE":{"transitions":[{"name":"init","next":"CREATED","manual":false}]},
	"CREATED":{"transitions":[
		{"name":"promote","next":"PROMOTED","manual":false,"criterion":` + cfGroup("AND", cfFn("cf-fb-a", "")) + `},
		{"name":"fallback","next":"FALLBACK","manual":false}]},
	"PROMOTED":{},"FALLBACK":{}}}]}`
	setupModelWithWorkflow(t, model, wf)
	id := createEntityE2E(t, model, 1, cfPayload(50))
	if s := getEntityState(t, id); s != "FALLBACK" {
		t.Errorf("state = %q; want FALLBACK", s)
	}
}

func cfManualWorkflow(name, criterion string) string {
	return `{"importMode":"REPLACE","workflows":[{"version":"1.5","name":"` + name + `","initialState":"NONE","active":true,
	"states":{"NONE":{"transitions":[{"name":"init","next":"CREATED","manual":false}]},
	"CREATED":{"transitions":[{"name":"approve","next":"APPROVED","manual":true,"criterion":` + criterion + `}]},
	"APPROVED":{}}}]}`
}

func TestCriterionFunction_Manual_Refused_ReasonListsCallouts(t *testing.T) {
	const model = "e2e-cfe-manref"
	procSvc.RegisterCriteriaReason("cf-man-a", cfAnswer(true, ""))
	procSvc.RegisterCriteriaReason("cf-man-b", cfAnswer(false, "too small"))
	setupModelWithWorkflow(t, model, cfManualWorkflow("cfe-manref",
		cfGroup("AND", cfFn("cf-man-a", ""), cfFn("cf-man-b", ""))))
	id := createEntityE2E(t, model, 1, cfPayload(50))

	resp := doAuth(t, http.MethodPut, fmt.Sprintf("/api/entity/JSON/%s/approve", id), cfPayload(50))
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400: %s", resp.StatusCode, body)
	}
	if got := problemErrorCode(body); got != "WORKFLOW_FAILED" {
		t.Errorf("errorCode = %q; want WORKFLOW_FAILED (%s)", got, body)
	}
	want := "criterion not matched: cf-man-a at /conditions/0: answered true; cf-man-b at /conditions/1: answered false: too small"
	if !strings.Contains(body, want) {
		t.Errorf("body missing %q: %s", want, body)
	}
}

func TestCriterionFunction_Manual_Allowed_TransitionMakeFunctions(t *testing.T) {
	const model = "e2e-cfe-manok"
	procSvc.RegisterCriteriaReason("cf-mok-a", cfAnswer(true, ""))
	setupModelWithWorkflow(t, model, cfManualWorkflow("cfe-manok", cfGroup("OR", cfFn("cf-mok-a", ""))))
	id := createEntityE2E(t, model, 1, cfPayload(50))

	resp := doAuth(t, http.MethodPut, fmt.Sprintf("/api/entity/JSON/%s/approve", id), cfPayload(50))
	if body := readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200: %s", resp.StatusCode, body)
	}
	if n := len(cfFunctions(t, cfEventData(t, id, "TRANSITION_MAKE"))); n != 1 {
		t.Errorf("TRANSITION_MAKE functions length = %d; want 1", n)
	}
}

func cfSelectionWorkflows(fnCriterion string) string {
	return `{"importMode":"REPLACE","workflows":[
	{"version":"1.5","name":"wf-fn","initialState":"NONE","active":true,"criterion":` + fnCriterion + `,
	 "states":{"NONE":{"transitions":[{"name":"go","next":"A","manual":false}]},"A":{}}},
	{"version":"1.5","name":"wf-plain","initialState":"NONE","active":true,
	 "states":{"NONE":{"transitions":[{"name":"go","next":"B","manual":false}]},"B":{}}}]}`
}

func TestCriterionFunction_Selection_PreCheck(t *testing.T) {
	const model = "e2e-cfe-selpc"
	procSvc.RegisterCriteriaReason("cf-selpc-sel", cfAnswer(true, ""))
	setupModelWithWorkflow(t, model, cfSelectionWorkflows(cfFn("cf-selpc-sel", cfAmountGT100)))

	low := createEntityE2E(t, model, 1, cfPayload(50))
	if s := getEntityState(t, low); s != "B" {
		t.Errorf("amount 50: state = %q; want B", s)
	}
	if n := procSvc.CriteriaCallCount("cf-selpc-sel"); n != 0 {
		t.Errorf("amount 50: call count = %d; want 0", n)
	}
	if f := cfFunctions(t, cfEventData(t, low, "WORKFLOW_SKIP"))[0]; f["called"] != false {
		t.Errorf("WORKFLOW_SKIP functions[0] = %v; want called false", f)
	}

	high := createEntityE2E(t, model, 1, cfPayload(150))
	if s := getEntityState(t, high); s != "A" {
		t.Errorf("amount 150: state = %q; want A", s)
	}
	if f := cfFunctions(t, cfEventData(t, high, "WORKFLOW_FOUND"))[0]; f["called"] != true {
		t.Errorf("WORKFLOW_FOUND functions[0] = %v; want called true", f)
	}
}

func TestCriterionFunction_Selection_FunctionInGroup(t *testing.T) {
	const model = "e2e-cfe-selgrp"
	procSvc.RegisterCriteriaReason("cf-selgrp-a", cfAnswer(false, ""))
	procSvc.RegisterCriteriaReason("cf-selgrp-b", cfAnswer(true, ""))
	setupModelWithWorkflow(t, model, cfSelectionWorkflows(
		cfGroup("OR", cfFn("cf-selgrp-a", ""), cfFn("cf-selgrp-b", ""))))
	id := createEntityE2E(t, model, 1, cfPayload(50))

	if s := getEntityState(t, id); s != "A" {
		t.Errorf("state = %q; want A", s)
	}
	fs := cfFunctions(t, cfEventData(t, id, "WORKFLOW_FOUND"))
	if len(fs) != 2 || fs[0]["path"] != "/conditions/0" || fs[1]["path"] != "/conditions/1" {
		t.Errorf("functions = %v; want paths /conditions/0 and /conditions/1", fs)
	}
	for i, f := range fs {
		if f["called"] != true {
			t.Errorf("functions[%d] = %v; want called true", i, f)
		}
		if f["matches"] != (i == 1) {
			t.Errorf("functions[%d] = %v; want matches %v", i, f, i == 1)
		}
	}
	for _, name := range []string{"cf-selgrp-a", "cf-selgrp-b"} {
		if n := procSvc.CriteriaCallCount(name); n != 1 {
			t.Errorf("%s call count = %d; want 1", name, n)
		}
	}
}

func TestCriterionFunction_TransitionsRead(t *testing.T) {
	const model = "e2e-cfe-trread"
	procSvc.RegisterCriteriaReason("cf-tr-tr", cfAnswer(true, ""))
	wf := `{"importMode":"REPLACE","workflows":[{"version":"1.5","name":"cfe-tr","initialState":"NONE","active":true,
	"criterion":` + cfGroup("AND", cfAmountGT10, cfFn("cf-tr-tr", "")) + `,
	"states":{"NONE":{"transitions":[{"name":"init","next":"CREATED","manual":false}]},
	"CREATED":{"transitions":[{"name":"approve","next":"APPROVED","manual":true}]},
	"APPROVED":{}}}]}`
	setupModelWithWorkflow(t, model, wf)
	id := createEntityE2E(t, model, 1, cfPayload(50))
	n := procSvc.CriteriaCallCount("cf-tr-tr")

	path := fmt.Sprintf("/api/entity/%s/transitions", id)
	resp := doAuth(t, http.MethodGet, path, "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("transitions: status = %d; want 200: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "approve") {
		t.Errorf("transitions body lacks approve: %s", body)
	}
	if got := procSvc.CriteriaCallCount("cf-tr-tr"); got != n+1 {
		t.Errorf("call count = %d; want %d", got, n+1)
	}

	procSvc.RegisterCriteriaReason("cf-tr-tr", func(context.Context, *spi.Entity, json.RawMessage) (bool, string, error) {
		return false, "", fmt.Errorf("callout down")
	})
	resp = doAuth(t, http.MethodGet, path, "")
	body = readBody(t, resp)
	if got := procSvc.CriteriaCallCount("cf-tr-tr"); got != n+2 {
		t.Errorf("call count after failing callout = %d; want %d", got, n+2)
	}
	if !strings.Contains(body, "callout down") {
		t.Errorf("detail lacks the callout's error text: %s", body)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("transitions with failing function: status = %d; want 400: %s", resp.StatusCode, body)
	}
	if got := problemErrorCode(body); got != "WORKFLOW_FAILED" {
		t.Errorf("errorCode = %q; want WORKFLOW_FAILED (%s)", got, body)
	}
}

const cfBadPathSimple = `{"type":"simple","jsonPath":"$.amonut","operatorType":"GREATER_THAN","value":1}`

func TestCriterionFunction_Unevaluable_NoCallout(t *testing.T) {
	assertWorkflowFailed := func(t *testing.T, status int, body string) {
		t.Helper()
		if status != http.StatusBadRequest {
			t.Fatalf("status = %d; want 400: %s", status, body)
		}
		if got := problemErrorCode(body); got != "WORKFLOW_FAILED" {
			t.Errorf("errorCode = %q; want WORKFLOW_FAILED (%s)", got, body)
		}
		// Only an engine that evaluates function conditions reaches the
		// undeclared-field check; one that refuses them says "not supported".
		if !strings.Contains(body, "unknown field path(s): $.amonut") {
			t.Errorf("detail does not name the undeclared path: %s", body)
		}
		for _, bad := range []string{"not supported", "not implemented"} {
			if strings.Contains(body, bad) {
				t.Errorf("detail contains %q: %s", bad, body)
			}
		}
	}
	post := func(t *testing.T, model string) (int, string) {
		t.Helper()
		resp := doAuth(t, http.MethodPost, fmt.Sprintf("/api/entity/JSON/%s/1", model), cfPayload(50))
		return resp.StatusCode, readBody(t, resp)
	}

	// Behaviour guard: the member beside the function is a local leaf, which
	// the pre-change engine also checked before refusing the group.
	t.Run("undeclared-path-beside-function", func(t *testing.T) {
		const model = "e2e-cfe-unev1"
		procSvc.RegisterCriteriaReason("cf-unev1-a", cfAnswer(true, ""))
		setupModelWithWorkflow(t, model, cfWorkflow("cfe-unev1", cfGroup("OR", cfFn("cf-unev1-a", ""), cfBadPathSimple)))
		status, body := post(t, model)
		assertWorkflowFailed(t, status, body)
		if n := procSvc.CriteriaCallCount("cf-unev1-a"); n != 0 {
			t.Errorf("call count = %d; want 0", n)
		}
	})

	// Behaviour guard as above for a pre-check inside a group.
	t.Run("undeclared-path-in-precheck", func(t *testing.T) {
		const model = "e2e-cfe-unev2"
		procSvc.RegisterCriteriaReason("cf-unev2-a", cfAnswer(true, ""))
		setupModelWithWorkflow(t, model, cfWorkflow("cfe-unev2", cfGroup("OR", cfFn("cf-unev2-a", cfBadPathSimple))))
		status, body := post(t, model)
		assertWorkflowFailed(t, status, body)
		if n := procSvc.CriteriaCallCount("cf-unev2-a"); n != 0 {
			t.Errorf("call count = %d; want 0", n)
		}
	})

	// Discriminating: the pre-change engine never saw pre-checks and made the
	// callout; the current engine refuses before any callout.
	t.Run("undeclared-path-in-precheck-whole-criterion", func(t *testing.T) {
		const model = "e2e-cfe-unev4"
		procSvc.RegisterCriteriaReason("cf-unev4-a", cfAnswer(true, ""))
		setupModelWithWorkflow(t, model, cfWorkflow("cfe-unev4", cfFn("cf-unev4-a", cfBadPathSimple)))
		status, body := post(t, model)
		assertWorkflowFailed(t, status, body)
		if n := procSvc.CriteriaCallCount("cf-unev4-a"); n != 0 {
			t.Errorf("call count = %d; want 0", n)
		}
	})

	t.Run("manual-transition", func(t *testing.T) {
		const model = "e2e-cfe-unev3"
		procSvc.RegisterCriteriaReason("cf-unev3-a", cfAnswer(true, ""))
		setupModelWithWorkflow(t, model, cfManualWorkflow("cfe-unev3", cfGroup("OR", cfFn("cf-unev3-a", ""), cfBadPathSimple)))
		id := createEntityE2E(t, model, 1, cfPayload(50))
		resp := doAuth(t, http.MethodPut, fmt.Sprintf("/api/entity/JSON/%s/approve", id), cfPayload(50))
		body := readBody(t, resp)
		assertWorkflowFailed(t, resp.StatusCode, body)
		if n := procSvc.CriteriaCallCount("cf-unev3-a"); n != 0 {
			t.Errorf("call count = %d; want 0", n)
		}
		if s := getEntityState(t, id); s != "CREATED" {
			t.Errorf("state after refusal = %q; want CREATED", s)
		}
	})
}
