package e2e_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// callout_failover_test.go drives the failover rules through the client-facing
// doors: two cnodes serve one tag, and what the first does with the work
// decides whether the second is asked at all.

// TestCalloutFailover_Processor: two cnodes serve one tag; the one attached
// first is tried first. What the first does with the work, and whether the
// processor is declared idempotent, decides whether the second is asked.
func TestCalloutFailover_Processor(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(3, 100*time.Millisecond))
	sfx := randSuffix(t) // repeated runs (go test -count=N) share this package's Postgres testcontainer

	cases := []struct {
		name          string
		first         cnodeReply
		config        map[string]any
		wantStatus    int
		wantCode      string
		wantRetryable bool
		wantDetail    string
		wantSecond    int // callouts the second cnode receives
	}{
		{"no-answer-not-idempotent", neverAnswer(), nil,
			http.StatusServiceUnavailable, "DISPATCH_TIMEOUT", true, "", 0},
		{"no-answer-idempotent", neverAnswer(), map[string]any{"idempotent": true},
			http.StatusOK, "", false, "", 1},
		{"drop-not-idempotent", closeStream(), nil,
			http.StatusServiceUnavailable, "COMPUTE_MEMBER_DISCONNECTED", true, "", 0},
		{"drop-idempotent", closeStream(), map[string]any{"idempotent": true},
			http.StatusOK, "", false, "", 1},
		{"failed-verdict-true", answerFailVerdict("s1 boom", true), map[string]any{"idempotent": true},
			http.StatusBadRequest, "WORKFLOW_FAILED", true, "processor s1-proc failed: s1 boom", 0},
		{"failed-verdict-false", answerFailVerdict("s1 boom", false), map[string]any{"idempotent": true},
			http.StatusBadRequest, "WORKFLOW_FAILED", false, "processor s1-proc failed: s1 boom", 0},
		{"failed-no-verdict", answerFail("s1 boom"), map[string]any{"idempotent": true},
			http.StatusBadRequest, "WORKFLOW_FAILED", false, "processor s1-proc failed: s1 boom", 0},
		{"terminal", answerMalformedPayload(), map[string]any{"idempotent": true},
			http.StatusBadRequest, "WORKFLOW_FAILED", false, "", 0},
		{"retry-policy-none", neverAnswer(), map[string]any{"idempotent": true, "retryPolicy": "NONE"},
			http.StatusServiceUnavailable, "DISPATCH_TIMEOUT", true, "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tag, model := "s1-"+sfx+"-"+tc.name, "s1-model-"+sfx+"-"+tc.name
			first := h.AttachCnode(t, cnodeSpec{name: "first", tags: []string{tag}, script: scriptAlways(tc.first)})
			second := h.AttachCnode(t, cnodeSpec{name: "second", tags: []string{tag}})
			defer second.Detach(t)
			defer first.Detach(t)

			cfg := map[string]any{"calculationNodesTags": tag, "responseTimeoutMs": 300}
			for k, v := range tc.config {
				cfg[k] = v
			}
			h.SetupModelWithWorkflow(t, model, chainWorkflowJSON("s1-wf-"+tc.name, procSpec{"s1-proc", "SYNC", cfg}))

			_, status, body := h.CreateEntity(t, model, 1, workflowSampleModel)
			if tc.wantStatus == http.StatusOK {
				if status != http.StatusOK {
					t.Fatalf("create: %d %s; want 200 (the second cnode answers)", status, body)
				}
			} else {
				pd := assertProblem(t, status, body, tc.wantStatus, tc.wantCode, tc.wantRetryable)
				if tc.wantDetail != "" && !strings.Contains(pd.Detail, tc.wantDetail) {
					t.Errorf("detail = %q; want it to contain %q", pd.Detail, tc.wantDetail)
				}
				if strings.Contains(pd.Detail, "processor dispatch failed:") {
					t.Errorf("detail = %q; the inner \"processor dispatch failed:\" segment is gone", pd.Detail)
				}
				if n := h.countEntities(t, model); n != 0 {
					t.Errorf("%d entities committed by a failed SYNC create; want 0", n)
				}
			}

			got1, got2 := first.Received(), second.Received()
			if len(got1) != 1 {
				t.Fatalf("first cnode received %d callouts; want 1 (it was attached first): %v", len(got1), got1)
			}
			if len(got2) != tc.wantSecond {
				t.Fatalf("second cnode received %d callouts; want %d: %v", len(got2), tc.wantSecond, got2)
			}
			if tc.wantSecond == 1 {
				if got1[0].RequestID == "" || got1[0].RequestID != got2[0].RequestID {
					t.Errorf("request ids differ across tries: %q then %q", got1[0].RequestID, got2[0].RequestID)
				}
				if got1[0].EventID == got2[0].EventID {
					t.Errorf("both tries carry CloudEvent id %q; the envelope id is unique per event", got1[0].EventID)
				}
				if got1[0].Pass() == got2[0].Pass() {
					t.Error("both tries carry the same pass; every try mints its own")
				}
			}
		})
	}
}

// TestCalloutFailover_CriterionAndFunction: a criterion and a schedule function
// go to the next cnode after a try that got no answer, with no declaration by
// the author; retryPolicy NONE keeps each to one try.
func TestCalloutFailover_CriterionAndFunction(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(3, 100*time.Millisecond))
	sfx := randSuffix(t) // repeated runs (go test -count=N) share this package's Postgres testcontainer

	fnJSON := func(name, tag, policy string) string {
		fn := map[string]any{"name": name, "resultKind": "Schedule", "calculationNodesTags": tag, "responseTimeoutMs": 300}
		if policy != "" {
			fn["retryPolicy"] = policy
		}
		b, _ := json.Marshal(fn)
		return string(b)
	}
	critCfg := func(tag, policy string) map[string]any {
		cfg := map[string]any{"calculationNodesTags": tag, "responseTimeoutMs": 300}
		if policy != "" {
			cfg["retryPolicy"] = policy
		}
		return cfg
	}
	schedule := answerResult("Schedule", map[string]any{"fireAfterMs": int64(3600000)})

	cases := []struct {
		name       string
		workflow   func(tag string) string
		second     cnodeReply
		wantOK     bool
		wantState  string
		wantSecond int
	}{
		{"criterion", func(tag string) string { return criterionWorkflowJSON("s2-crit-wf", "s2-crit", critCfg(tag, "")) },
			answerMatches(true), true, "DONE", 1},
		{"criterion-none", func(tag string) string {
			return criterionWorkflowJSON("s2-crit-none-wf", "s2-crit", critCfg(tag, "NONE"))
		},
			answerMatches(true), false, "", 0},
		{"function", func(tag string) string { return scheduleFunctionWorkflowJSON("s2-fn-wf", fnJSON("s2-fn", tag, "")) },
			schedule, true, "Open", 1},
		{"function-none", func(tag string) string {
			return scheduleFunctionWorkflowJSON("s2-fn-none-wf", fnJSON("s2-fn", tag, "NONE"))
		},
			schedule, false, "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tag, model := "s2-"+sfx+"-"+tc.name, "s2-model-"+sfx+"-"+tc.name
			first := h.AttachCnode(t, cnodeSpec{name: "first", tags: []string{tag}, script: scriptAlways(neverAnswer())})
			second := h.AttachCnode(t, cnodeSpec{name: "second", tags: []string{tag}, script: scriptAlways(tc.second)})
			defer second.Detach(t)
			defer first.Detach(t)
			h.SetupModelWithWorkflow(t, model, tc.workflow(tag))

			id, status, body := h.CreateEntity(t, model, 1, workflowSampleModel)
			if tc.wantOK {
				if status != http.StatusOK {
					t.Fatalf("create: %d %s; want 200", status, body)
				}
				if st, _ := h.GetEntityState(t, id); st != tc.wantState {
					t.Errorf("state = %q; want %q", st, tc.wantState)
				}
			} else {
				assertProblem(t, status, body, http.StatusServiceUnavailable, "DISPATCH_TIMEOUT", true)
			}
			got1, got2 := first.Received(), second.Received()
			if len(got1) != 1 || len(got2) != tc.wantSecond {
				t.Fatalf("first received %d, second %d; want 1 and %d", len(got1), len(got2), tc.wantSecond)
			}
			if tc.wantSecond == 1 && got1[0].RequestID != got2[0].RequestID {
				t.Errorf("request ids differ across tries: %q then %q", got1[0].RequestID, got2[0].RequestID)
			}
		})
	}
}
