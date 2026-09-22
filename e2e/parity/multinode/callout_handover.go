package multinode

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// callout_handover.go — the hand-over on a real cluster. A pnode whose own
// cnodes cannot take a callout hands the tries that are left to a peer; the
// peer runs its local procedure with the owner's tries and answer limit and
// mints each try's pass in the OWNER's name, so the cnode's callbacks are
// routed to the owner and admitted by the owner's fence.
//
// Each scenario runs under a tenant of its own with tags of its own, so nothing
// carries over. No scenario stops a pnode or a cnode mid-flight, and each has
// exactly one peer advertising its tag — so which peer the owner's random peer
// order picks never matters.

func init() {
	Register(
		NamedTest{Name: "Callout_HandOverSucceeds", Fn: RunCallout_HandOverSucceeds},
		NamedTest{Name: "Callout_HandOverTwoTriesInOneExchange", Fn: RunCallout_HandOverTwoTriesInOneExchange},
		NamedTest{Name: "Callout_HandOverCarriesMessageAndVerdict", Fn: RunCallout_HandOverCarriesMessageAndVerdict},
		NamedTest{Name: "Callout_TwoTenantsShareATag", Fn: RunCallout_TwoTenantsShareATag},
	)
}

const mnSample = `{"name":"Test","amount":10,"status":"new"}`

// mnProc builds one processor entry routed to tag.
func mnProc(name, mode, tag, contextValue string, extra map[string]any) map[string]any {
	cfg := map[string]any{"attachEntity": true, "calculationNodesTags": tag, "context": contextValue}
	for k, v := range extra {
		cfg[k] = v
	}
	return map[string]any{"type": "calculator", "name": name, "executionMode": mode, "config": cfg}
}

// mnWorkflow builds a NONE -> ACTIVE workflow whose automated transition runs
// procs in order.
func mnWorkflow(wfName string, procs ...map[string]any) string {
	list := make([]any, len(procs))
	for i, p := range procs {
		list[i] = p
	}
	b, _ := json.Marshal(map[string]any{
		"importMode": "REPLACE",
		"workflows": []any{map[string]any{
			"version": "1.5", "name": wfName, "initialState": "NONE", "active": true,
			"states": map[string]any{
				"NONE":   map[string]any{"transitions": []any{map[string]any{"name": "init", "next": "ACTIVE", "manual": false, "processors": list}}},
				"ACTIVE": map[string]any{},
			},
		}},
	})
	return string(b)
}

// mnRequire skips unless the fixture can start compute clients and has the
// pnodes the scenario needs.
func mnRequire(t *testing.T, fixture MultiNodeFixture, pnodes int) []string {
	t.Helper()
	if _, ok := fixture.(ComputeClientCapable); !ok {
		t.Skip("cluster fixture cannot start further compute clients; scenario pending on this backend")
	}
	urls := fixture.BaseURLs()
	if len(urls) < pnodes {
		t.Fatalf("needs at least %d pnodes, got %d", pnodes, len(urls))
	}
	return urls
}

// mnWarmUp starts a healthy compute client on pnode node carrying warmTag plus
// extraTags, and returns once a create routed to warmTag succeeds through
// owner — so the owner knows that pnode's list, with every tag attached to it
// before this call. Call it LAST for each peer.
//
// A tag attached on one pnode reaches the others by the membership protocol,
// some time later; a scenario that drove its create at once would measure that
// delay against the fixture's own patience. Lists travel whole, so once the
// warm-up tag is routable every tag attached before it on that pnode is known
// to the owner as well.
func mnWarmUp(t *testing.T, fixture MultiNodeFixture, owner *client.Client, tenant parity.Tenant, node int, warmTag string, extraTags ...string) parity.ComputeClient {
	t.Helper()
	model := "mn-warm-" + warmTag
	cbRouteSetupModel(t, owner, model, cbRouteSampleNoWriteback, mnWorkflow(model+"-wf", mnProc("noop", "SYNC", warmTag, "", nil)))
	cc := StartComputeClientOrSkip(t, fixture, node, parity.ComputeClientSpec{TenantID: tenant.ID, Tags: append([]string{warmTag}, extraTags...)})
	deadline := time.Now().Add(30 * time.Second)
	for {
		status, body, err := owner.CreateEntityRaw(t, model, 1, mnSample)
		if err != nil {
			t.Fatalf("warm-up create: %v", err)
		}
		if status == http.StatusOK {
			return cc
		}
		if time.Now().After(deadline) {
			t.Fatalf("30s after it attached to pnode %d the owner still cannot route to tag %s: %d %s", node, warmTag, status, body)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// mnProblem asserts status, errorCode and retryable, and returns detail.
func mnProblem(t *testing.T, status int, body []byte, wantStatus int, wantCode string, wantRetryable bool) string {
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

// mnHasRequest reports whether cc received a calculation request with requestID.
func mnHasRequest(t *testing.T, cc parity.ComputeClient, requestID string) bool {
	t.Helper()
	for _, r := range cc.Received(t) {
		if r.RequestID == requestID {
			return true
		}
	}
	return false
}

// RunCallout_HandOverSucceeds: the owner's own compute node drops with the work
// in its hands; the processor is idempotent; the callout is handed over to the
// pnode that has another, under the same request id.
func RunCallout_HandOverSucceeds(t *testing.T, fixture MultiNodeFixture) {
	urls := mnRequire(t, fixture, 2)
	tenant := fixture.NewTenant(t)
	owner := client.NewClient(urls[0], tenant.Token)
	const model, tag = "mn-ho-ok", "mn-ho-ok"

	local := StartComputeClientOrSkip(t, fixture, 0, parity.ComputeClientSpec{TenantID: tenant.ID, Tags: []string{tag}, Behaviour: parity.ComputeBehaviourDrop})
	remote := mnWarmUp(t, fixture, owner, tenant, 1, "mn-ho-ok-w", tag)
	cbRouteSetupModel(t, owner, model, cbRouteSampleNoWriteback,
		mnWorkflow("mn-ho-ok-wf", mnProc("noop", "SYNC", tag, "", map[string]any{"idempotent": true})))

	if _, err := owner.CreateEntity(t, model, 1, mnSample); err != nil {
		t.Fatalf("create through the owner: %v", err)
	}
	first := parity.AwaitReceived(t, local, 1, 5*time.Second)
	if len(first) != 1 {
		t.Errorf("the owner's own compute client received %d requests; want 1", len(first))
	}
	if !mnHasRequest(t, remote, first[0].RequestID) {
		t.Errorf("the other pnode's compute client did not receive request %s; received: %+v", first[0].RequestID, remote.Received(t))
	}
}

// RunCallout_HandOverTwoTriesInOneExchange: the owner has no compute node of
// its own; the other pnode has a silent one and a healthy one. One hand-over
// covers both tries, and the silent one is given up after the processor's own
// limit.
func RunCallout_HandOverTwoTriesInOneExchange(t *testing.T, fixture MultiNodeFixture) {
	urls := mnRequire(t, fixture, 2)
	tenant := fixture.NewTenant(t)
	owner := client.NewClient(urls[0], tenant.Token)
	const model, tag = "mn-ho-two", "mn-ho-two"

	silent := StartComputeClientOrSkip(t, fixture, 1, parity.ComputeClientSpec{TenantID: tenant.ID, Tags: []string{tag}, Behaviour: parity.ComputeBehaviourStall})
	healthy := mnWarmUp(t, fixture, owner, tenant, 1, "mn-ho-two-w", tag)
	cbRouteSetupModel(t, owner, model, cbRouteSampleNoWriteback,
		mnWorkflow("mn-ho-two-wf", mnProc("noop", "SYNC", tag, "", map[string]any{"idempotent": true, "responseTimeoutMs": 500})))

	start := time.Now()
	if _, err := owner.CreateEntity(t, model, 1, mnSample); err != nil {
		t.Fatalf("create through the owner: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < 500*time.Millisecond || elapsed > 10*time.Second {
		t.Errorf("create took %v; want at least the 500ms limit of the silent try and far less than the 30s default", elapsed)
	}
	first := parity.AwaitReceived(t, silent, 1, 5*time.Second)
	if len(first) != 1 {
		t.Errorf("the silent compute client received %d requests; want 1", len(first))
	}
	if !mnHasRequest(t, healthy, first[0].RequestID) {
		t.Errorf("the healthy compute client did not receive request %s", first[0].RequestID)
	}
}

// RunCallout_HandOverCarriesMessageAndVerdict: the compute node that fails the
// processor is on another pnode; its own message and its verdict reach the
// client of the owner.
func RunCallout_HandOverCarriesMessageAndVerdict(t *testing.T, fixture MultiNodeFixture) {
	urls := mnRequire(t, fixture, 2)
	tenant := fixture.NewTenant(t)
	owner := client.NewClient(urls[0], tenant.Token)
	const model, tag = "mn-ho-verdict", "mn-ho-verdict"

	failing := StartComputeClientOrSkip(t, fixture, 1, parity.ComputeClientSpec{TenantID: tenant.ID, Tags: []string{tag}, Behaviour: parity.ComputeBehaviourFailRetryable})
	mnWarmUp(t, fixture, owner, tenant, 1, "mn-ho-verdict-w")
	cbRouteSetupModel(t, owner, model, cbRouteSampleNoWriteback,
		mnWorkflow("mn-ho-verdict-wf", mnProc("noop", "SYNC", tag, "", map[string]any{"idempotent": true})))

	status, body, err := owner.CreateEntityRaw(t, model, 1, mnSample)
	if err != nil {
		t.Fatalf("CreateEntityRaw: %v", err)
	}
	detail := mnProblem(t, status, body, http.StatusBadRequest, "WORKFLOW_FAILED", true)
	if want := "processor noop failed: scripted failure: fail-retryable"; !strings.Contains(detail, want) {
		t.Errorf("detail = %q; want it to contain %q", detail, want)
	}
	if got := parity.AwaitReceived(t, failing, 1, 5*time.Second); len(got) != 1 {
		t.Errorf("the failing compute client received %d requests; want 1 — a \"failed\" answer ends the callout", len(got))
	}
}

// mnMemberRe matches one entry of a CALLOUT_FAILED failure list and captures
// the compute member it names. The angle brackets are the rendering's own.
var mnMemberRe = regexp.MustCompile(`\[member ?<?([^>:\]\s]+)>?:`)

// RunCallout_TwoTenantsShareATag: two tenants attach compute nodes under one
// tag to one pnode. Each tenant's callout, handed over from another pnode,
// goes only to its own; a failure names only its own.
func RunCallout_TwoTenantsShareATag(t *testing.T, fixture MultiNodeFixture) {
	urls := mnRequire(t, fixture, 2)
	x, y := fixture.NewTenant(t), fixture.NewTenant(t)
	ownerX, ownerY := client.NewClient(urls[0], x.Token), client.NewClient(urls[0], y.Token)
	const model, tag = "mn-two-tenants", "mn-shared"

	x1 := StartComputeClientOrSkip(t, fixture, 1, parity.ComputeClientSpec{TenantID: x.ID, Tags: []string{tag}, Behaviour: parity.ComputeBehaviourStall})
	x2 := StartComputeClientOrSkip(t, fixture, 1, parity.ComputeClientSpec{TenantID: x.ID, Tags: []string{tag}, Behaviour: parity.ComputeBehaviourStall})
	mnWarmUp(t, fixture, ownerX, x, 1, "mn-shared-wx")
	yHealthy := mnWarmUp(t, fixture, ownerY, y, 1, "mn-shared-wy", tag)

	wf := mnWorkflow("mn-two-tenants-wf", mnProc("noop", "SYNC", tag, "", map[string]any{"idempotent": true, "responseTimeoutMs": 300}))
	cbRouteSetupModel(t, ownerX, model, cbRouteSampleNoWriteback, wf)
	cbRouteSetupModel(t, ownerY, model, cbRouteSampleNoWriteback, wf)

	yBefore := len(yHealthy.Received(t))
	status, body, err := ownerX.CreateEntityRaw(t, model, 1, mnSample)
	if err != nil {
		t.Fatalf("tenant X CreateEntityRaw: %v", err)
	}
	detail := mnProblem(t, status, body, http.StatusServiceUnavailable, "CALLOUT_FAILED", true)
	named := map[string]bool{}
	for _, m := range mnMemberRe.FindAllStringSubmatch(detail, -1) {
		named[m[1]] = true
	}
	if !named[x1.MemberID()] || !named[x2.MemberID()] || len(named) != 2 {
		t.Errorf("detail = %q; want it to name exactly tenant X's members %s and %s", detail, x1.MemberID(), x2.MemberID())
	}
	if strings.Contains(detail, yHealthy.MemberID()) {
		t.Errorf("detail = %q; it names tenant Y's compute node", detail)
	}
	if got := len(yHealthy.Received(t)); got != yBefore {
		t.Errorf("tenant Y's healthy compute node received %d of tenant X's requests", got-yBefore)
	}

	if _, err := ownerY.CreateEntity(t, model, 1, mnSample); err != nil {
		t.Fatalf("tenant Y create: %v", err)
	}
	if got := len(yHealthy.Received(t)); got != yBefore+1 {
		t.Errorf("tenant Y's compute node received %d requests for tenant Y's create; want 1", got-yBefore)
	}
	if len(x1.Received(t))+len(x2.Received(t)) > 4 {
		t.Errorf("tenant X's compute nodes received more requests than tenant X's tries allow")
	}
}
