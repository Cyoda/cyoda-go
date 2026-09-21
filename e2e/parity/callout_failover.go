package parity

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// callout_failover.go — what ends a callout and what the client is told, on
// every backend. Each case runs under a fresh tenant with a tag of its own and
// starts its compute clients one after the other, so "the client started first
// is tried first" holds and nothing carries over between scenarios. Anything
// that depends on timing or order between requests lives in internal/e2e.

const (
	calloutSample   = `{"name":"Test","amount":10,"status":"new"}`
	calloutFnSample = `{"k":1,"schedMode":"relative","offsetMs":3600000,"expireOffsetMs":0}`
	// calloutShortLimit is the responseTimeoutMs a scenario gives a compute
	// client that never answers, so a used-up try costs milliseconds.
	calloutShortLimit  = 300
	calloutAwaitWithin = 5 * time.Second
)

// calloutDoc marshals a workflow-import document at the current schema version.
func calloutDoc(name, initial string, states map[string]any) string {
	b, _ := json.Marshal(map[string]any{
		"importMode": "REPLACE",
		"workflows": []any{map[string]any{
			"version": "1.5", "name": name, "initialState": initial, "active": true, "states": states,
		}},
	})
	return string(b)
}

func calloutProcessorWF(name, proc, tag string, extra map[string]any) string {
	return ComputeClientWorkflow(name, proc, tag, "", extra)
}

func calloutCriterionWF(name, tag string, extra map[string]any) string {
	cfg := map[string]any{"calculationNodesTags": tag, "attachEntity": true}
	for k, v := range extra {
		cfg[k] = v
	}
	return calloutDoc(name, "NONE", map[string]any{
		"NONE": map[string]any{"transitions": []any{map[string]any{
			"name": "init", "next": "ACTIVE", "manual": false,
			"criterion": map[string]any{"type": "function", "function": map[string]any{"name": "always-true", "config": cfg}},
		}}},
		"ACTIVE": map[string]any{},
	})
}

func calloutFunctionWF(name, tag string, extra map[string]any) string {
	fn := map[string]any{"name": "sched-fn-resolve", "resultKind": "Schedule", "calculationNodesTags": tag, "attachEntity": true}
	for k, v := range extra {
		fn[k] = v
	}
	return calloutDoc(name, "Open", map[string]any{
		"Open": map[string]any{"transitions": []any{map[string]any{
			"name": "AutoClose", "next": "Closed", "manual": false, "schedule": map[string]any{"function": fn},
		}}},
		"Closed": map[string]any{},
	})
}

// calloutCase is one tenant, one tag, one model, and the compute clients
// started for it in order.
type calloutCase struct {
	c       *client.Client
	model   string
	clients []ComputeClient
}

func newCalloutCase(t *testing.T, fixture BackendFixture, name, sample string, workflow func(tag string) string, behaviours ...string) calloutCase {
	t.Helper()
	requireComputeClients(t, fixture)
	tenant := fixture.NewTenant(t)
	tag := "co-" + name
	cc := calloutCase{c: client.NewClient(fixture.BaseURL(), tenant.Token), model: "co-" + name}
	for _, b := range behaviours {
		cc.clients = append(cc.clients, StartComputeClientOrSkip(t, fixture, ComputeClientSpec{TenantID: tenant.ID, Tags: []string{tag}, Behaviour: b}))
	}
	cbSetupModel(t, cc.c, cc.model, sample, workflow(tag))
	return cc
}

// calloutProblem asserts status, errorCode and retryable, and returns detail.
func calloutProblem(t *testing.T, status int, body []byte, wantStatus int, wantCode string, wantRetryable bool) string {
	t.Helper()
	var pd struct {
		Detail     string         `json:"detail"`
		Properties map[string]any `json:"properties"`
	}
	if status != wantStatus {
		t.Fatalf("status = %d; want %d %s (body: %s)", status, wantStatus, wantCode, body)
	}
	if err := json.Unmarshal(body, &pd); err != nil {
		t.Fatalf("response is not a problem document: %v (body: %s)", err, body)
	}
	if got, _ := pd.Properties["errorCode"].(string); got != wantCode {
		t.Errorf("errorCode = %q; want %q (body: %s)", got, wantCode, body)
	}
	if got, _ := pd.Properties["retryable"].(bool); got != wantRetryable {
		t.Errorf("retryable = %t; want %t (body: %s)", got, wantRetryable, body)
	}
	return pd.Detail
}

// requireCounts asserts the exact number of calculation requests each compute
// client of the case received, in the order they were started. The first count
// is awaited; the rest are read once the operation has ended and nothing more
// can arrive.
func (cc calloutCase) requireCounts(t *testing.T, want ...int) {
	t.Helper()
	if len(want) != len(cc.clients) {
		t.Fatalf("requireCounts wants one count per client: %d counts, %d clients", len(want), len(cc.clients))
	}
	for i, n := range want {
		var got []ReceivedCallout
		if n > 0 {
			got = AwaitReceived(t, cc.clients[i], n, calloutAwaitWithin)
		} else {
			got = cc.clients[i].Received(t)
		}
		if len(got) != n {
			t.Errorf("compute client %d (%s) received %d requests; want %d: %+v", i, cc.clients[i].MemberID(), len(got), n, got)
		}
	}
}

// requireNoEntity asserts the failed operation committed nothing.
func (cc calloutCase) requireNoEntity(t *testing.T) {
	t.Helper()
	if list, err := cc.c.ListEntitiesByModel(t, cc.model, 1); err != nil || len(list) != 0 {
		t.Errorf("entities after the failed create: %d err=%v; want 0", len(list), err)
	}
}

// failAndExpect drives one create that must fail, and requires that the spare
// client (the last one started) was never asked and nothing was committed.
func (cc calloutCase) failAndExpect(t *testing.T, sample string, wantStatus int, wantCode string, wantRetryable bool) string {
	t.Helper()
	status, body, err := cc.c.CreateEntityRaw(t, cc.model, 1, sample)
	if err != nil {
		t.Fatalf("CreateEntityRaw: %v", err)
	}
	detail := calloutProblem(t, status, body, wantStatus, wantCode, wantRetryable)
	cc.requireCounts(t, 1, 0)
	cc.requireNoEntity(t)
	return detail
}

// succeedOnSecond drives one create that must succeed on the second client
// under the first try's request id, and returns the new entity.
func (cc calloutCase) succeedOnSecond(t *testing.T, sample, wantKind string) client.EntityResult {
	t.Helper()
	id, err := cc.c.CreateEntity(t, cc.model, 1, sample)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	cc.requireCounts(t, 1, 1)
	first, second := cc.clients[0].Received(t), cc.clients[1].Received(t)
	if first[0].Kind != wantKind || second[0].Kind != wantKind {
		t.Errorf("kinds = %q then %q; want %q", first[0].Kind, second[0].Kind, wantKind)
	}
	if first[0].RequestID == "" || first[0].RequestID != second[0].RequestID {
		t.Errorf("request ids differ across tries: %q then %q", first[0].RequestID, second[0].RequestID)
	}
	ent, err := cc.c.GetEntity(t, id)
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}
	return ent
}

// RunCalloutNoAnswerNotIdempotentStops: a processor not declared idempotent is
// not repeated after a try that got no answer: 503 with the try's own code.
func RunCalloutNoAnswerNotIdempotentStops(t *testing.T, fixture BackendFixture) {
	cc := newCalloutCase(t, fixture, "noanswer-stop", calloutSample, func(tag string) string {
		return calloutProcessorWF("co-noanswer-stop-wf", "noop", tag, map[string]any{"responseTimeoutMs": calloutShortLimit})
	}, ComputeBehaviourStall, ComputeBehaviourCatalog)
	cc.failAndExpect(t, calloutSample, http.StatusServiceUnavailable, "DISPATCH_TIMEOUT", true)
}

// RunCalloutCriterionFailsOver: a criterion is repeat-safe by rule.
func RunCalloutCriterionFailsOver(t *testing.T, fixture BackendFixture) {
	cc := newCalloutCase(t, fixture, "crit-failover", calloutSample, func(tag string) string {
		return calloutCriterionWF("co-crit-failover-wf", tag, map[string]any{"responseTimeoutMs": calloutShortLimit})
	}, ComputeBehaviourStall, ComputeBehaviourCatalog)
	if ent := cc.succeedOnSecond(t, calloutSample, "criterion"); ent.Meta.State != "ACTIVE" {
		t.Errorf("state = %q; want ACTIVE (the criterion matched on the second compute client)", ent.Meta.State)
	}
}

// RunCalloutFunctionFailsOver: a schedule function is repeat-safe by rule.
func RunCalloutFunctionFailsOver(t *testing.T, fixture BackendFixture) {
	cc := newCalloutCase(t, fixture, "fn-failover", calloutFnSample, func(tag string) string {
		return calloutFunctionWF("co-fn-failover-wf", tag, map[string]any{"responseTimeoutMs": calloutShortLimit})
	}, ComputeBehaviourStall, ComputeBehaviourCatalog)
	if ent := cc.succeedOnSecond(t, calloutFnSample, "function"); ent.Meta.State != "Open" {
		t.Errorf("state = %q; want Open (armed an hour out, not fired)", ent.Meta.State)
	}
}

// RunCalloutMemberFailedStops: a compute node that answers "failed" ends the
// callout — declared idempotent or not — and its message and verdict reach the
// client.
func RunCalloutMemberFailedStops(t *testing.T, fixture BackendFixture) {
	requireComputeClients(t, fixture)
	cases := []struct {
		name          string
		first         string
		proc          string
		wantRetryable bool
		wantDetail    string
	}{
		{"verdict-true", ComputeBehaviourFailRetryable, "noop", true, "processor noop failed: scripted failure: fail-retryable"},
		{"no-verdict", ComputeBehaviourFail, "noop", false, "processor noop failed: scripted failure: fail"},
		{"verdict-false", ComputeBehaviourCatalog, "inject-error-not-retryable", false,
			"processor inject-error-not-retryable failed: inject-error-not-retryable: deliberate failure"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cc := newCalloutCase(t, fixture, "failed-"+tc.name, calloutSample, func(tag string) string {
				return calloutProcessorWF("co-failed-wf", tc.proc, tag, map[string]any{"idempotent": true})
			}, tc.first, ComputeBehaviourCatalog)
			detail := cc.failAndExpect(t, calloutSample, http.StatusBadRequest, "WORKFLOW_FAILED", tc.wantRetryable)
			if want := "WORKFLOW_FAILED: " + tc.wantDetail; detail != want {
				t.Errorf("detail = %q; want %q", detail, want)
			}
			if strings.Contains(detail, "processor dispatch failed:") {
				t.Errorf("detail = %q; the inner \"processor dispatch failed:\" segment is gone", detail)
			}
		})
	}
}

// RunCalloutRetryPolicyNoneOneTry: retryPolicy NONE is one try on a processor,
// a criterion and a function, repeat-safe or not.
func RunCalloutRetryPolicyNoneOneTry(t *testing.T, fixture BackendFixture) {
	requireComputeClients(t, fixture)
	none := func(extra map[string]any) map[string]any {
		extra["retryPolicy"] = "NONE"
		extra["responseTimeoutMs"] = calloutShortLimit
		return extra
	}
	cases := []struct {
		name, sample string
		workflow     func(tag string) string
	}{
		{"processor", calloutSample, func(tag string) string {
			return calloutProcessorWF("co-none-proc-wf", "noop", tag, none(map[string]any{"idempotent": true}))
		}},
		{"criterion", calloutSample, func(tag string) string { return calloutCriterionWF("co-none-crit-wf", tag, none(map[string]any{})) }},
		{"function", calloutFnSample, func(tag string) string { return calloutFunctionWF("co-none-fn-wf", tag, none(map[string]any{})) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cc := newCalloutCase(t, fixture, "none-"+tc.name, tc.sample, tc.workflow, ComputeBehaviourStall, ComputeBehaviourCatalog)
			cc.failAndExpect(t, tc.sample, http.StatusServiceUnavailable, "DISPATCH_TIMEOUT", true)
		})
	}
}

// RunCalloutEveryTryUsed: both compute nodes of the tag stay silent on an
// idempotent processor. The local procedure tries each of them once, the
// patience then runs out without a membership change — which starts no further
// pass — so exactly two attempts are on record and the client is told so:
// 503 CALLOUT_FAILED, retryable, in the full shape the contract fixes, naming
// only this tenant's two compute nodes.
//
// The collapsed "(k times)" form of an entry needs the same compute node to
// fail identically in two passes, which needs a membership change inside the
// patience; staging one here would mean racing a compute client's start against
// a running callout. It is pinned where it can be staged without a race, in
// internal/callout's rendering tests.
func RunCalloutEveryTryUsed(t *testing.T, fixture BackendFixture) {
	cc := newCalloutCase(t, fixture, "all-used", calloutSample, func(tag string) string {
		return calloutProcessorWF("co-all-used-wf", "noop", tag, map[string]any{"idempotent": true, "responseTimeoutMs": calloutShortLimit})
	}, ComputeBehaviourStall, ComputeBehaviourStall)

	status, body, err := cc.c.CreateEntityRaw(t, cc.model, 1, calloutSample)
	if err != nil {
		t.Fatalf("CreateEntityRaw: %v", err)
	}
	detail := calloutProblem(t, status, body, http.StatusServiceUnavailable, "CALLOUT_FAILED", true)
	cause := fmt.Sprintf("DISPATCH_TIMEOUT: processor dispatch timed out after %dms: no response", calloutShortLimit)
	want := fmt.Sprintf("CALLOUT_FAILED: the callout could not be completed, got 2 failures: [member<%s>: %s], [member<%s>: %s]",
		cc.clients[0].MemberID(), cause, cc.clients[1].MemberID(), cause)
	if detail != want {
		t.Errorf("detail\n got %q\nwant %q", detail, want)
	}
	cc.requireCounts(t, 1, 1)
	cc.requireNoEntity(t)
}
