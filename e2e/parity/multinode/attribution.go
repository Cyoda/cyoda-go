package multinode

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/parity"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// attribution.go — CROSS-NODE follow-on-action attribution parity scenarios.
// cyoda-go is primarily multi-node, so origin propagation across the cluster
// hops is first-class, not an afterthought. Three scenarios, each driven
// through a real cluster (postgres in-tree; the commercial backend once it
// wires the optional capability below):
//
//  1. Proxied-join cascade: a processor callback that lands on the NON-owner
//     node writes a secondary into the joined tx → attributed to the
//     originating USER, executed by the member's SERVICE identity — IDENTICAL
//     to the same-node cascade (the Join-origin-repopulation path).
//  2. Scheduled fire: tasks armed on the cluster by a USER, round-robin fired
//     across all members (peer RPC) → each attributed to the user (durable
//     ArmedBy read on the firing node inside FireScheduledTransition), executed
//     by the system. Positively asserts at least one task fired on a PEER
//     (non-coordinator) node via that node's captured logs, so the scenario
//     cannot pass vacuously if scheduler distribution ever collapsed to self.
//  3. Callout authtype: a peer-dispatched (A→B forwarded) processor receives
//     the executor's TRUE principal kind in its AuthContext authtype (Task 7),
//     without which the re-dispatch would fail closed with authtype "".
//
// These live in the SHARED multinode registry (not a postgres-only test) so
// every cluster-capable backend that consumes AllTests() — including the
// out-of-tree commercial backend on its next dependency update — sees the
// coverage. A backend that has not yet wired attribution does not implement
// AttributionCapable and each scenario reports an explicit PENDING SKIP rather
// than being silently absent. Memory/sqlite cannot cluster at all, have no
// MultiNodeFixture, and never reach these — that exclusion is unchanged.
//
// The scenarios use AttributionCapable.ComputeUser (a user-kind principal in
// the compute tenant) so the causal origin (user) is observably distinct from
// the member's service identity. The tx-token is never logged/asserted (Gate 3).

func init() {
	Register(
		NamedTest{Name: "Attribution_ProxiedJoinCascade", Fn: RunAttribution_ProxiedJoinCascade},
		NamedTest{Name: "Attribution_ScheduledFire", Fn: RunAttribution_ScheduledFire},
		NamedTest{Name: "Attribution_CalloutAuthType", Fn: RunAttribution_CalloutAuthType},
	)
}

// AttributionCapable is the OPTIONAL capability a cluster fixture implements to
// run the cross-node attribution scenarios. It is deliberately NOT folded into
// MultiNodeFixture: attribution is postgres-first, and a cluster-capable backend
// that has not wired it must still compile against and consume the shared
// registry — the scenarios type-assert this interface and t.Skip when it is
// absent (PENDING), so the coverage is visible-but-pending, never invisible.
type AttributionCapable interface {
	// ComputeUser mints a USER-kind principal (caas_user_id == userID) scoped to
	// the compute-test-client's tenant — a human origin whose cascades still
	// dispatch to the registered gRPC member.
	ComputeUser(t *testing.T, userID string, roles ...string) parity.Tenant
	// NodeLogs returns node idx's captured combined stdout+stderr as a snapshot,
	// so a scenario can positively assert that a scheduled task fired on a peer
	// node (the peer-RPC fire path emits a distinctive log line). "" for an
	// out-of-range index. Never assert on token/secret material read here (Gate 3).
	NodeLogs(idx int) string
}

// attrServiceID is the executor principal id of every member callback — the
// bootstrap user id the compute-test-client authenticates as (CyodaEnv:
// CYODA_BOOTSTRAP_USER_ID=compute-admin; MintM2MJWT: caas_user_id=compute-admin).
const attrServiceID = "compute-admin"

// attrRequireCapable type-asserts the optional attribution capability, skipping
// the scenario as PENDING when the fixture has not wired it.
func attrRequireCapable(t *testing.T, fixture MultiNodeFixture) AttributionCapable {
	t.Helper()
	ac, ok := fixture.(AttributionCapable)
	if !ok {
		t.Skip("cross-node attribution parity pending on this backend")
	}
	return ac
}

// --- Scenario 1: cross-node proxied-join cascade attribution -----------------

// RunAttribution_ProxiedJoinCascade drives a SYNC callback cascade from a
// memberless owner node (forwarded dispatch + reverse-proxied callback joining
// T on the owner) and asserts the joined secondary attributes to the
// originating USER, executed by the member SERVICE — IDENTICAL to the same-node
// cascade.
func RunAttribution_ProxiedJoinCascade(t *testing.T, fixture MultiNodeFixture) {
	t.Helper()
	ac := attrRequireCapable(t, fixture)
	urls := fixture.BaseURLs()
	if len(urls) < 2 {
		t.Fatalf("proxied-join cascade attribution needs >=2 nodes, got %d", len(urls))
	}

	// Model + workflow setup uses the compute-tenant admin (locked models are
	// tenant-scoped; the user creates entities in the same tenant).
	admin := fixture.ComputeTenant(t)
	cSetup := client.NewClient(urls[0], admin.Token)

	const secondary = "attr-mn-casc-secondary"
	const primary = "attr-mn-casc-primary"
	cbRouteSetupModel(t, cSetup, secondary, `{"name":"child","amount":1,"status":"new"}`, cbRouteSecondaryWorkflow)
	cbRouteSetupModel(t, cSetup, primary, `{"name":"parent","amount":10,"status":"new"}`,
		attrPrimaryProcWorkflow("attr-mn-casc-wf", "cb-create-secondary", "SYNC",
			cbRouteContext(secondary, "attr-mn-casc-marker")))

	const userID = "cluster-alice"
	user := ac.ComputeUser(t, userID, "ROLE_USER")

	// Same-node baseline: node 0 owns T AND hosts the member. Dispatch is local,
	// the callback lands on the owner and joins locally — no cross-node hop.
	sameNodeSecID := attrDriveCascade(t, client.NewClient(urls[0], user.Token), primary)

	// Cross-node: node 1 owns T but hosts NO member. Dispatch forwards node1->
	// node0 (A->B); the member's HTTP callback lands on node0 (a NON-owner for
	// this T) and reverse-proxies back to node1, joining T there. This is the
	// Join-origin-repopulation path — origin must survive both hops.
	crossNodeSecID := attrDriveCascade(t, client.NewClient(urls[1], user.Token), primary)

	// Both secondaries must attribute IDENTICALLY: origin = the user, executor
	// = the member's service identity. Read the change from an arbitrary node
	// (shared storage) — cross-tenant/admin read via the compute-tenant admin.
	cRead := client.NewClient(urls[2%len(urls)], admin.Token)
	sameChange := attrFindChange(t, cRead, sameNodeSecID, "CREATE")
	crossChange := attrFindChange(t, cRead, crossNodeSecID, "CREATE")

	attrAssert(t, "same-node secondary", sameChange, userID, "user", "service", attrServiceID)
	attrAssert(t, "cross-node secondary", crossChange, userID, "user", "service", attrServiceID)

	// Explicit identical-attribution assertion (the primary acceptance
	// criterion): the cross-node cascade records the SAME {attributed, executor}
	// pair as the same-node one.
	if sameChange.User != crossChange.User ||
		sameChange.AttributedKind != crossChange.AttributedKind ||
		attrPrincipalKind(sameChange.ExecutedBy) != attrPrincipalKind(crossChange.ExecutedBy) {
		t.Errorf("cross-node attribution differs from same-node:\n same  = {user=%q kind=%q exec=%q}\n cross = {user=%q kind=%q exec=%q}",
			sameChange.User, sameChange.AttributedKind, attrPrincipalKind(sameChange.ExecutedBy),
			crossChange.User, crossChange.AttributedKind, attrPrincipalKind(crossChange.ExecutedBy))
	}
}

// attrDriveCascade creates a primary through c (whose node becomes the tx
// owner), waits for the SYNC cascade to complete, and returns the joined
// secondary's id read from the primary's data.
func attrDriveCascade(t *testing.T, c *client.Client, primary string) uuid.UUID {
	t.Helper()
	primID, err := c.CreateEntity(t, primary, 1, `{"name":"parent","amount":100,"status":"new"}`)
	if err != nil {
		t.Fatalf("create primary (cascade): %v", err)
	}
	prim, err := c.GetEntity(t, primID)
	if err != nil {
		t.Fatalf("get primary: %v", err)
	}
	if prim.Meta.State != "ACTIVE" {
		t.Fatalf("primary state = %q; want ACTIVE (cascade did not complete): data=%+v", prim.Meta.State, prim.Data)
	}
	secID, _ := prim.Data["secondaryId"].(string)
	if secID == "" {
		t.Fatalf("primary missing secondaryId (callback did not create secondary): data=%+v", prim.Data)
	}
	id, err := uuid.Parse(secID)
	if err != nil {
		t.Fatalf("parse secondaryId %q: %v", secID, err)
	}
	return id
}

// --- Scenario 2: cross-node scheduled fire attribution -----------------------

// RunAttribution_ScheduledFire arms several tasks as a USER, waits for the
// coordinator's round-robin to fire them across all members, and asserts every
// fired change attributes to the arming user (executed by the system). It then
// POSITIVELY asserts that at least one task fired on a PEER (non-coordinator)
// node, read from that node's captured logs — the peer-RPC fire path
// (SchedulerRPCHandler) emits a distinctive line only on a node that received a
// delegated fire. Without this assertion the scenario would pass vacuously if
// scheduler distribution ever collapsed to firing everything on the coordinator.
func RunAttribution_ScheduledFire(t *testing.T, fixture MultiNodeFixture) {
	t.Helper()
	ac := attrRequireCapable(t, fixture)
	urls := fixture.BaseURLs()
	if len(urls) < 2 {
		t.Fatalf("scheduled-fire attribution needs >=2 nodes, got %d", len(urls))
	}
	admin := fixture.ComputeTenant(t)

	// Raise every node to debug so the peer-RPC fire path's (Debug-level)
	// resolved line is captured for the peer-fire assertion below; restore
	// info afterwards so unrelated scenarios stay quiet. Uses the compute-tenant
	// admin token (ROLE_ADMIN required by the admin route).
	for _, url := range urls {
		if err := client.NewClient(url, admin.Token).SetLogLevel(t, "debug"); err != nil {
			t.Fatalf("raise log level to debug on %s: %v", url, err)
		}
	}
	defer func() {
		for _, url := range urls {
			_ = client.NewClient(url, admin.Token).SetLogLevel(t, "info")
		}
	}()

	const model = "attr-mn-sched"
	cbRouteSetupModel(t, client.NewClient(urls[0], admin.Token), model,
		`{"name":"x","amount":1,"status":"new"}`, attrScheduledWorkflow)

	const userID = "cluster-bob"
	user := ac.ComputeUser(t, userID, "ROLE_USER")

	// Arm several tasks via node 1. The coordinator (lowest node id = node 0)
	// scans and round-robins each due task across all members, so some fire on
	// a PEER via SchedulerRPC. Whichever node fires, the durable ArmedBy (the
	// user) — re-read on the firing node inside FireScheduledTransition — must
	// drive the attribution. Arm enough that round-robin provably reaches every
	// member.
	const n = 9
	ids := make([]uuid.UUID, 0, n)
	cArm := client.NewClient(urls[1%len(urls)], user.Token)
	for i := 0; i < n; i++ {
		id, err := cArm.CreateEntity(t, model, 1, fmt.Sprintf(`{"name":"e%d","amount":1,"status":"new"}`, i))
		if err != nil {
			t.Fatalf("arm entity %d: %v", i, err)
		}
		ids = append(ids, id)
	}

	cRead := client.NewClient(urls[2%len(urls)], admin.Token)
	for i, id := range ids {
		attrWaitForState(t, cRead, id, "Closed", 30*time.Second)
		// The scheduled fire's change is the one executed by the SYSTEM (the
		// scheduler), attributed to the arming user. The CREATE change, by
		// contrast, is executed by the user. Select by executor kind.
		fireChange := attrFindChangeByExecutorKind(t, cRead, id, "system")
		attrAssert(t, fmt.Sprintf("scheduled fire #%d", i), fireChange, userID, "user", "system", "")
	}

	// Positive peer-execution assertion (anti-vacuity). The coordinator is
	// node-0 (lowest id); it fires its own share locally and delegates the rest
	// to peers over the peer-authenticated scheduler RPC. Only a node that
	// RECEIVED a delegated fire emits peerFireMarker, so its presence in any
	// non-coordinator node's log is unambiguous proof the cross-node peer-RPC
	// fire path executed — not a same-node collapse.
	const peerFireMarker = "scheduled task peer fire resolved"
	firedPeers := make([]int, 0, len(urls))
	for idx := 1; idx < len(urls); idx++ {
		if strings.Contains(ac.NodeLogs(idx), peerFireMarker) {
			firedPeers = append(firedPeers, idx)
		}
	}
	if len(firedPeers) == 0 {
		t.Errorf("no scheduled task observably fired on a peer (non-coordinator) node: expected at least one of nodes 1..%d to log %q; scheduler distribution may have collapsed to the coordinator",
			len(urls)-1, peerFireMarker)
	}
}

// --- Scenario 3: cross-node callout authtype ---------------------------------

// RunAttribution_CalloutAuthType drives a SYNC processor from a memberless owner
// node (forwarded dispatch A→B) and asserts the forwarded processor observes the
// executor's TRUE principal kind ("service") in its CloudEvents authtype — Task
// 7 reconstructing PrincipalKind on the member-hosting node.
func RunAttribution_CalloutAuthType(t *testing.T, fixture MultiNodeFixture) {
	t.Helper()
	_ = attrRequireCapable(t, fixture)
	urls := fixture.BaseURLs()
	if len(urls) < 2 {
		t.Fatalf("callout authtype needs >=2 nodes, got %d", len(urls))
	}

	// The executor here is the compute-tenant SERVICE principal that drives the
	// transition; its true kind must reach the forwarded processor's authtype.
	svc := fixture.ComputeTenant(t)

	const model = "attr-mn-authtype"
	cbRouteSetupModel(t, client.NewClient(urls[0], svc.Token), model,
		`{"name":"x","amount":1,"status":"new","observedAuthType":""}`,
		attrPrimaryProcWorkflow("attr-mn-authtype-wf", "record-authtype", "SYNC", ""))

	// Drive from node 1 (owner, no member) → dispatch forwards node1->node0
	// (A→B). Node 0 re-dispatches to its local member, reconstructing the
	// executor's PrincipalKind (Task 7) into the CloudEvents authtype. The
	// processor records what it observed.
	c := client.NewClient(urls[1], svc.Token)
	id, err := c.CreateEntity(t, model, 1, `{"name":"x","amount":1,"status":"new","observedAuthType":""}`)
	if err != nil {
		t.Fatalf("create entity (authtype): %v", err)
	}
	ent, err := c.GetEntity(t, id)
	if err != nil {
		t.Fatalf("get entity: %v", err)
	}
	if ent.Meta.State != "ACTIVE" {
		t.Fatalf("state = %q; want ACTIVE (forwarded dispatch did not complete): data=%+v", ent.Meta.State, ent.Data)
	}
	got, _ := ent.Data["observedAuthType"].(string)
	if got != "service" {
		t.Errorf("forwarded processor observed authtype = %q; want %q (Task 7 must reconstruct the executor's true kind on the member-hosting node)", got, "service")
	}
}

// --- workflow builders -------------------------------------------------------

// attrScheduledWorkflow: NONE -> (init) -> Open -> (AutoClose, scheduled) ->
// Closed. Creating an entity arms AutoClose with ArmedBy = the creating origin;
// the scan loop fires it after delayMs.
const attrScheduledWorkflow = `{
	"importMode": "REPLACE",
	"workflows": [{
		"version": "1.1", "name": "attr-mn-sched-wf", "initialState": "NONE", "active": true,
		"states": {
			"NONE":   {"transitions": [{"name": "init", "next": "Open", "manual": false}]},
			"Open":   {"transitions": [{"name": "AutoClose", "next": "Closed", "manual": false, "schedule": {"delayMs": 1500}}]},
			"Closed": {}
		}
	}]
}`

// attrPrimaryProcWorkflow builds a NONE->ACTIVE auto-transition workflow whose
// transition carries one processor pinned to computeMemberTag (so dispatch is
// forwarded to the member-hosting node when driven from any other node).
// contextValue may be "" for processors that need no pass-through context.
func attrPrimaryProcWorkflow(wfName, procName, execMode, contextValue string) string {
	ctxField := ""
	if contextValue != "" {
		ctxField = ", \"context\": " + contextValue
	}
	return fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": %q, "initialState": "NONE", "active": true,
			"states": {
				"NONE":   {"transitions": [{"name": "init", "next": "ACTIVE", "manual": false,
					"processors": [{"type": "calculator", "name": %q, "executionMode": %q,
						"config": {"attachEntity": true, "calculationNodesTags": %q%s}}]
				}]},
				"ACTIVE": {}
			}
		}]
	}`, wfName, procName, execMode, computeMemberTag, ctxField)
}

// --- assertion helpers -------------------------------------------------------

// attrFindChange returns the newest change of the given changeType for entityID.
func attrFindChange(t *testing.T, c *client.Client, entityID uuid.UUID, changeType string) client.EntityChangeMeta {
	t.Helper()
	changes, err := c.GetEntityChanges(t, entityID)
	if err != nil {
		t.Fatalf("GetEntityChanges %s: %v", entityID, err)
	}
	for _, ch := range changes {
		if ch.ChangeType == changeType {
			return ch
		}
	}
	t.Fatalf("no %s change for %s (changes=%+v)", changeType, entityID, changes)
	return client.EntityChangeMeta{}
}

// attrFindChangeByExecutorKind returns the newest change whose executor kind
// matches — used to select the scheduled-fire write (executor=system) apart
// from the create (executor=user).
func attrFindChangeByExecutorKind(t *testing.T, c *client.Client, entityID uuid.UUID, execKind string) client.EntityChangeMeta {
	t.Helper()
	changes, err := c.GetEntityChanges(t, entityID)
	if err != nil {
		t.Fatalf("GetEntityChanges %s: %v", entityID, err)
	}
	for _, ch := range changes {
		if attrPrincipalKind(ch.ExecutedBy) == execKind {
			return ch
		}
	}
	t.Fatalf("no change with executor kind %q for %s (changes=%+v)", execKind, entityID, changes)
	return client.EntityChangeMeta{}
}

// attrWaitForState polls until the entity reaches wantState or the deadline
// elapses.
func attrWaitForState(t *testing.T, c *client.Client, entityID uuid.UUID, wantState string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		ent, err := c.GetEntity(t, entityID)
		if err == nil {
			last = ent.Meta.State
			if last == wantState {
				return
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("entity %s did not reach state %q within %v (last=%q)", entityID, wantState, timeout, last)
}

// attrAssert asserts the {attributed, executor} pair on a change entry.
// wantExecID may be "" to skip the executor-id check.
func attrAssert(t *testing.T, label string, ch client.EntityChangeMeta, wantUser, wantAttrKind, wantExecKind, wantExecID string) {
	t.Helper()
	if ch.User != wantUser {
		t.Errorf("%s: attributed user = %q; want %q (entry=%+v)", label, ch.User, wantUser, ch)
	}
	if ch.AttributedKind != wantAttrKind {
		t.Errorf("%s: attributedKind = %q; want %q (entry=%+v)", label, ch.AttributedKind, wantAttrKind, ch)
	}
	if ch.ExecutedBy == nil {
		t.Fatalf("%s: executedBy missing (entry=%+v)", label, ch)
	}
	if ch.ExecutedBy.Kind != wantExecKind {
		t.Errorf("%s: executedBy.kind = %q; want %q (entry=%+v)", label, ch.ExecutedBy.Kind, wantExecKind, ch)
	}
	if wantExecID != "" && ch.ExecutedBy.ID != wantExecID {
		t.Errorf("%s: executedBy.id = %q; want %q (entry=%+v)", label, ch.ExecutedBy.ID, wantExecID, ch)
	}
}

// attrPrincipalKind returns the executor kind or "" for a nil principal.
func attrPrincipalKind(p *client.ChangePrincipal) string {
	if p == nil {
		return ""
	}
	return p.Kind
}
