package parity

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// criterion_function.go — function conditions inside criteria groups, and the
// function pre-check, on every backend. Each case runs under a fresh tenant
// with a tag of its own and one compute client serving the catalog, so the
// criterion requests that client received are exactly the callouts the
// evaluation made. The catalog criteria give no reason, so every answer is
// listed bare.

const (
	cfLow  = `{"name":"Test","amount":50,"status":"new"}`
	cfHigh = `{"name":"Test","amount":150,"status":"new"}`
	// cfAmountGT100 is a local condition, used as a pre-check.
	cfAmountGT100 = `{"type":"simple","jsonPath":"$.amount","operatorType":"GREATER_THAN","value":100}`
)

// cfFn is a function condition calling the catalog criterion name on tag, with
// the pre-check preCheck when it is not empty.
func cfFn(name, tag, preCheck string) string {
	fn := `{"name":"` + name + `","config":{"calculationNodesTags":"` + tag + `","attachEntity":true}`
	if preCheck != "" {
		fn += `,"criterion":` + preCheck
	}
	return `{"type":"function","function":` + fn + `}}`
}

// cfGroup is a criteria group of the given operator over members.
func cfGroup(op string, members ...string) string {
	return `{"type":"group","operator":"` + op + `","conditions":[` + strings.Join(members, ",") + `]}`
}

// cfCase is one tenant, one tag, one model and the compute client that serves
// the tag.
type cfCase struct {
	c     *client.Client
	model string
	tag   string
	cc    ComputeClient
}

// newCFCase starts a catalog compute client on a tag of its own under a fresh
// tenant, then imports the model with the workflow document workflow(tag)
// builds. A fixture that cannot start compute clients skips here, before the
// server is touched.
func newCFCase(t *testing.T, fixture BackendFixture, name string, workflow func(tag string) string) cfCase {
	t.Helper()
	requireComputeClients(t, fixture)
	tenant := fixture.NewTenant(t)
	k := cfCase{c: client.NewClient(fixture.BaseURL(), tenant.Token), model: "cf-" + name, tag: "cf-" + name}
	k.cc = StartComputeClientOrSkip(t, fixture, ComputeClientSpec{TenantID: tenant.ID, Tags: []string{k.tag}})
	cbSetupModel(t, k.c, k.model, cfLow, workflow(k.tag))
	return k
}

// create creates one entity and returns its id and state.
func (k cfCase) create(t *testing.T, body string) (uuid.UUID, string) {
	t.Helper()
	id, err := k.c.CreateEntity(t, k.model, 1, body)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	ent, err := k.c.GetEntity(t, id)
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}
	return id, ent.Meta.State
}

// requireCriteria asserts the names of the criterion requests the compute
// client has received, in arrival order. The operation under test has ended,
// so nothing more can arrive.
func (k cfCase) requireCriteria(t *testing.T, want ...string) {
	t.Helper()
	if got := CriterionRequestNames(t, k.cc); !slices.Equal(got, want) {
		t.Errorf("criterion requests = %q; want %q", got, want)
	}
}

// cfPromoteWF is a workflow whose entity starts in CREATED and moves to
// PROMOTED by the automated transition promote, guarded by criterion.
func cfPromoteWF(name, criterion string) string {
	return `{"importMode":"REPLACE","workflows":[{"version":"1.5","name":"` + name + `","initialState":"CREATED","active":true,
		"states":{
			"CREATED":{"transitions":[{"name":"promote","next":"PROMOTED","manual":false,"criterion":` + criterion + `}]},
			"PROMOTED":{}
		}}]}`
}

// cfSelectionWF is two workflows in declaration order: wf-fn, selected by
// criterion, leads to A; wf-plain has no criterion and leads to B.
func cfSelectionWF(criterion string) string {
	return `{"importMode":"REPLACE","workflows":[
		{"version":"1.5","name":"wf-fn","initialState":"NONE","active":true,"criterion":` + criterion + `,
			"states":{"NONE":{"transitions":[{"name":"to-a","next":"A","manual":false}]},"A":{}}},
		{"version":"1.5","name":"wf-plain","initialState":"NONE","active":true,
			"states":{"NONE":{"transitions":[{"name":"to-b","next":"B","manual":false}]},"B":{}}}
	]}`
}

// RunCriterionFunction_GroupStopRule: group members are evaluated in
// declaration order and evaluation stops as soon as the group's value is
// known, so the requests a compute client receives are exactly the members
// reached.
func RunCriterionFunction_GroupStopRule(t *testing.T, fixture BackendFixture) {
	requireComputeClients(t, fixture)
	cases := []struct {
		name      string
		criterion func(tag string) string
		wantState string
		wantCalls []string
	}{
		{"and-stops-at-false", func(tag string) string {
			return cfGroup("AND", cfFn("always-false", tag, ""), cfFn("always-true", tag, ""))
		}, "CREATED", []string{"always-false"}},
		{"or-stops-at-true", func(tag string) string {
			return cfGroup("OR", cfFn("always-true", tag, ""), cfFn("always-false", tag, ""))
		}, "PROMOTED", []string{"always-true"}},
		{"not-inverts", func(tag string) string {
			return cfGroup("NOT", cfFn("always-true", tag, ""))
		}, "CREATED", []string{"always-true"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k := newCFCase(t, fixture, "stop-"+tc.name, func(tag string) string {
				return cfPromoteWF("cf-stop-wf", tc.criterion(tag))
			})
			if _, state := k.create(t, cfLow); state != tc.wantState {
				t.Errorf("state = %q; want %q", state, tc.wantState)
			}
			k.requireCriteria(t, tc.wantCalls...)
		})
	}
}

// RunCriterionFunction_SelectionPreCheck: a workflow criterion that is one
// function condition with a pre-check. When the pre-check does not match the
// workflow is skipped without a callout; when it matches the callout decides.
func RunCriterionFunction_SelectionPreCheck(t *testing.T, fixture BackendFixture) {
	k := newCFCase(t, fixture, "sel-precheck", func(tag string) string {
		return cfSelectionWF(cfFn("always-true", tag, cfAmountGT100))
	})

	low, state := k.create(t, cfLow)
	if state != "B" {
		t.Errorf("amount 50: state = %q; want B (wf-fn skipped, wf-plain selected)", state)
	}
	k.requireCriteria(t)
	skip := FunctionRecords(t, StateMachineEventData(t, k.c, low, "WORKFLOW_SKIP"))
	if len(skip) != 1 {
		t.Fatalf("WORKFLOW_SKIP functions = %v; want one entry", skip)
	}
	if skip[0]["called"] != false || skip[0]["preCheckMatched"] != false || skip[0]["path"] != "" || skip[0]["name"] != "always-true" {
		t.Errorf("WORKFLOW_SKIP functions[0] = %v; want path \"\", name always-true, preCheckMatched false, called false", skip[0])
	}

	high, state := k.create(t, cfHigh)
	if state != "A" {
		t.Errorf("amount 150: state = %q; want A (wf-fn selected)", state)
	}
	k.requireCriteria(t, "always-true")
	found := FunctionRecords(t, StateMachineEventData(t, k.c, high, "WORKFLOW_FOUND"))
	if len(found) != 1 || found[0]["called"] != true || found[0]["matches"] != true || found[0]["preCheckMatched"] != true {
		t.Errorf("WORKFLOW_FOUND functions = %v; want one entry with preCheckMatched, called and matches true", found)
	}
}

// RunCriterionFunction_SelectionGroup: a workflow criterion that is an OR of
// two function conditions selects the workflow on the second member's answer,
// and the audit lists both members by position.
func RunCriterionFunction_SelectionGroup(t *testing.T, fixture BackendFixture) {
	k := newCFCase(t, fixture, "sel-group", func(tag string) string {
		return cfSelectionWF(cfGroup("OR", cfFn("always-false", tag, ""), cfFn("always-true", tag, "")))
	})

	id, state := k.create(t, cfLow)
	if state != "A" {
		t.Errorf("state = %q; want A (wf-fn selected by its second member)", state)
	}
	k.requireCriteria(t, "always-false", "always-true")
	found := FunctionRecords(t, StateMachineEventData(t, k.c, id, "WORKFLOW_FOUND"))
	if len(found) != 2 {
		t.Fatalf("WORKFLOW_FOUND functions = %v; want two entries", found)
	}
	for i, want := range []struct {
		path, name string
		matches    bool
	}{{"/conditions/0", "always-false", false}, {"/conditions/1", "always-true", true}} {
		if f := found[i]; f["path"] != want.path || f["name"] != want.name || f["called"] != true || f["matches"] != want.matches {
			t.Errorf("WORKFLOW_FOUND functions[%d] = %v; want path %s, name %s, called true, matches %t", i, f, want.path, want.name, want.matches)
		}
	}
}

// RunCriterionFunction_ManualRefusedReason: a manual transition refused by a
// group of function conditions answers 400 WORKFLOW_FAILED, and the message
// lists every callout made with its answer.
func RunCriterionFunction_ManualRefusedReason(t *testing.T, fixture BackendFixture) {
	k := newCFCase(t, fixture, "manual-refused", func(tag string) string {
		crit := cfGroup("AND", cfFn("always-true", tag, ""), cfFn("always-false", tag, ""))
		return `{"importMode":"REPLACE","workflows":[{"version":"1.5","name":"cf-manual-wf","initialState":"CREATED","active":true,
			"states":{
				"CREATED":{"transitions":[{"name":"approve","next":"APPROVED","manual":true,"criterion":` + crit + `}]},
				"APPROVED":{}
			}}]}`
	})

	id, state := k.create(t, cfLow)
	if state != "CREATED" {
		t.Fatalf("state after create = %q; want CREATED", state)
	}
	k.requireCriteria(t)

	status, body, err := k.c.UpdateEntityRaw(t, id, "approve", cfLow)
	if err != nil {
		t.Fatalf("UpdateEntityRaw: %v", err)
	}
	if status != http.StatusBadRequest || !containsErrorCode(body, "WORKFLOW_FAILED") {
		t.Fatalf("approve: got %d %s; want 400 WORKFLOW_FAILED", status, body)
	}
	var pd struct {
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(body, &pd); err != nil {
		t.Fatalf("response is not a problem document: %v (body: %s)", err, body)
	}
	const wantDetail = `WORKFLOW_FAILED: transition "approve" criterion not matched: ` +
		"always-true at /conditions/0: answered true; always-false at /conditions/1: answered false"
	if pd.Detail != wantDetail {
		t.Errorf("approve: detail\n got %q\nwant %q", pd.Detail, wantDetail)
	}
	k.requireCriteria(t, "always-true", "always-false")
	ent, err := k.c.GetEntity(t, id)
	if err != nil {
		t.Fatalf("GetEntity after the refused approve: %v", err)
	}
	if ent.Meta.State != "CREATED" {
		t.Errorf("after the refused approve: state = %q; want CREATED", ent.Meta.State)
	}
}

// RunCriterionFunction_AuditKeepsPreCheckNumbers: the audit lists a function
// condition's pre-check as written, so a number in it keeps every digit on
// every backend, including one a float64 cannot hold.
func RunCriterionFunction_AuditKeepsPreCheckNumbers(t *testing.T, fixture BackendFixture) {
	const big = "12345678901234567890"
	preCheck := `{"type":"simple","jsonPath":"$.amount","operatorType":"LESS_THAN","value":` + big + `}`
	k := newCFCase(t, fixture, "audit-numbers", func(tag string) string {
		return cfSelectionWF(cfFn("always-true", tag, preCheck))
	})

	id, state := k.create(t, cfLow)
	if state != "A" {
		t.Errorf("state = %q; want A (pre-check matched, wf-fn selected)", state)
	}
	k.requireCriteria(t, "always-true")
	found := FunctionRecords(t, StateMachineEventData(t, k.c, id, "WORKFLOW_FOUND"))
	if len(found) != 1 {
		t.Fatalf("WORKFLOW_FOUND functions = %v; want one entry", found)
	}
	pc, ok := found[0]["preCheck"].(map[string]any)
	if !ok {
		t.Fatalf("WORKFLOW_FOUND functions[0].preCheck = %v; want the pre-check object", found[0]["preCheck"])
	}
	if got, ok := pc["value"].(json.Number); !ok || got.String() != big {
		t.Errorf("WORKFLOW_FOUND functions[0].preCheck.value = %v (%T); want the number %s with every digit", pc["value"], pc["value"], big)
	}
}
