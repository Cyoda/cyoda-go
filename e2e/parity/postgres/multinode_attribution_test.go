package postgres

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/parity"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// multinode_attribution_test.go — CROSS-NODE follow-on-action attribution.
// cyoda-go is primarily multi-node, so origin propagation across the cluster
// hops is first-class, not an afterthought. Three scenarios, each driven
// through a real 3-node postgres-backed cluster (MustSetupMultiNode):
//
//  1. Proxied-join cascade: a processor callback that lands on the NON-owner
//     node writes a secondary into the joined tx → attributed to the
//     originating USER, executed by the member's SERVICE identity — IDENTICAL
//     to the same-node cascade (the Join-origin-repopulation path).
//  2. Scheduled fire: a task armed on the cluster by a USER, fired (possibly
//     via peer RPC on another node under the coordinator's round-robin
//     distribution) → attributed to the user (durable ArmedBy read on the
//     firing node; ambient seed inside FireScheduledTransition), executed by
//     the system.
//  3. Callout authtype: a peer-dispatched (A→B forwarded) processor receives
//     the executor's TRUE principal kind in its AuthContext authtype (Task 7
//     forwarding) — without it the re-dispatch would fail closed with authtype
//     "".
//
// These use the pgMultiNode.ComputeUser helper (a user-kind principal in the
// compute tenant) so the causal origin (user) is observably distinct from the
// member's service identity. The tx-token is never logged/asserted (Gate 3).

// attrComputeUser is the optional capability the postgres multinode fixture
// implements to mint a user-kind principal in the compute tenant. Kept off the
// shared MultiNodeFixture interface because attribution is postgres-first.
type attrComputeUser interface {
	ComputeUser(t *testing.T, userID string, roles ...string) parity.Tenant
}

// attrMemberTag is the tag the compute-test-client's member advertises; only
// node 0 advertises it, so a processor pinned to it routes specifically there.
// Driving the transition from any other node forces the forwarded dispatch
// (A→B) and, for a joined HTTP callback, the reverse-proxy back to the owner.
const attrMemberTag = "compute-test-client"

// attrServiceID is the executor principal id of every member callback — the
// bootstrap user id the compute-test-client authenticates as (CyodaEnv:
// CYODA_BOOTSTRAP_USER_ID=compute-admin; MintM2MJWT: caas_user_id=compute-admin).
const attrServiceID = "compute-admin"

// TestMultiNodeAttribution runs the cross-node attribution scenarios against a
// fresh 3-node postgres cluster. Short-mode is handled by TestMain (os.Exit(0)
// before any test runs), matching TestMultiNode.
func TestMultiNodeAttribution(t *testing.T) {
	fix, cleanup := MustSetupMultiNode(t, 3)
	defer cleanup()

	cu, ok := fix.(attrComputeUser)
	if !ok {
		t.Fatalf("multinode fixture %T does not implement ComputeUser", fix)
	}

	t.Run("ProxiedJoinCascade", func(t *testing.T) { runAttrProxiedJoinCascade(t, fix, cu) })
	t.Run("ScheduledFire", func(t *testing.T) { runAttrScheduledFire(t, fix, cu) })
	t.Run("CalloutAuthType", func(t *testing.T) { runAttrCalloutAuthType(t, fix) })
}

// --- Scenario 1: cross-node proxied-join cascade attribution -----------------

func runAttrProxiedJoinCascade(t *testing.T, fix interface {
	BaseURLs() []string
	ComputeTenant(t *testing.T) parity.Tenant
}, cu attrComputeUser) {
	urls := fix.BaseURLs()
	if len(urls) < 2 {
		t.Fatalf("proxied-join cascade attribution needs >=2 nodes, got %d", len(urls))
	}

	// Model + workflow setup uses the compute-tenant admin (locked models are
	// tenant-scoped; the user creates entities in the same tenant).
	admin := fix.ComputeTenant(t)
	cSetup := client.NewClient(urls[0], admin.Token)

	const secondary = "attr-mn-casc-secondary"
	const primary = "attr-mn-casc-primary"
	attrSetupModel(t, cSetup, secondary, `{"name":"child","amount":1,"status":"new"}`, attrSecondaryWorkflow)
	attrSetupModel(t, cSetup, primary, `{"name":"parent","amount":10,"status":"new"}`,
		attrPrimaryProcWorkflow("attr-mn-casc-wf", "cb-create-secondary", "SYNC",
			attrCbContext(secondary, "attr-mn-casc-marker")))

	const userID = "cluster-alice"
	user := cu.ComputeUser(t, userID, "ROLE_USER")

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
		principalKind(sameChange.ExecutedBy) != principalKind(crossChange.ExecutedBy) {
		t.Errorf("cross-node attribution differs from same-node:\n same  = {user=%q kind=%q exec=%q}\n cross = {user=%q kind=%q exec=%q}",
			sameChange.User, sameChange.AttributedKind, principalKind(sameChange.ExecutedBy),
			crossChange.User, crossChange.AttributedKind, principalKind(crossChange.ExecutedBy))
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

func runAttrScheduledFire(t *testing.T, fix interface {
	BaseURLs() []string
	ComputeTenant(t *testing.T) parity.Tenant
}, cu attrComputeUser) {
	urls := fix.BaseURLs()
	admin := fix.ComputeTenant(t)

	const model = "attr-mn-sched"
	attrSetupModel(t, client.NewClient(urls[0], admin.Token), model,
		`{"name":"x","amount":1,"status":"new"}`, attrScheduledWorkflow)

	const userID = "cluster-bob"
	user := cu.ComputeUser(t, userID, "ROLE_USER")

	// Arm several tasks via node 1. The coordinator (lowest node id = node 0)
	// scans and round-robins each due task across all members, so at least some
	// fire on a PEER via SchedulerRPC. Whichever node fires, the durable ArmedBy
	// (the user) — re-read on the firing node inside FireScheduledTransition —
	// must drive the attribution. Assert every fired change records the user.
	const n = 4
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
}

// --- Scenario 3: cross-node callout authtype ---------------------------------

func runAttrCalloutAuthType(t *testing.T, fix interface {
	BaseURLs() []string
	ComputeTenant(t *testing.T) parity.Tenant
}) {
	urls := fix.BaseURLs()
	if len(urls) < 2 {
		t.Fatalf("callout authtype needs >=2 nodes, got %d", len(urls))
	}

	// The executor here is the compute-tenant SERVICE principal that drives the
	// transition; its true kind must reach the forwarded processor's authtype.
	svc := fix.ComputeTenant(t)

	const model = "attr-mn-authtype"
	attrSetupModel(t, client.NewClient(urls[0], svc.Token), model,
		`{"name":"x","amount":1,"status":"new","observedAuthType":""}`,
		attrPrimaryProcWorkflow("attr-mn-authtype-wf", "record-authtype", "SYNC", ""))

	// Drive from node 1 (owner, no member) → dispatch forwards node1->node0
	// (A->B). Node 0 re-dispatches to its local member, reconstructing the
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

// attrSecondaryWorkflow is a trivial NONE->STORED workflow (no processors) so a
// secondary entity created inside a joined callback runs a minimal cascade.
const attrSecondaryWorkflow = `{
	"importMode": "REPLACE",
	"workflows": [{
		"version": "1.1", "name": "attr-mn-secondary-wf", "initialState": "NONE", "active": true,
		"states": {
			"NONE":   {"transitions": [{"name": "store", "next": "STORED", "manual": false}]},
			"STORED": {}
		}
	}]
}`

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
// transition carries one processor pinned to attrMemberTag (so dispatch is
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
	}`, wfName, procName, execMode, attrMemberTag, ctxField)
}

// attrCbContext builds the pass-through ProcessorConfig.context JSON-string the
// cb-create-secondary processor decodes into {secondaryModel, secondaryVersion,
// marker} — a JSON-encoded string, matching the compute-test-client contract.
func attrCbContext(secondaryModel, marker string) string {
	inner, _ := json.Marshal(map[string]any{
		"secondaryModel":   secondaryModel,
		"secondaryVersion": 1,
		"marker":           marker,
	})
	quoted, _ := json.Marshal(string(inner))
	return string(quoted)
}

// --- setup + assertion helpers (replicated locally; the shared cbRoute*
// helpers live in the multinode package this postgres package cannot import) --

func attrSetupModel(t *testing.T, c *client.Client, modelName, sampleDoc, workflowJSON string) {
	t.Helper()
	if err := c.ImportModel(t, modelName, 1, sampleDoc); err != nil {
		t.Fatalf("ImportModel %s: %v", modelName, err)
	}
	if err := c.LockModel(t, modelName, 1); err != nil {
		t.Fatalf("LockModel %s: %v", modelName, err)
	}
	if err := c.ImportWorkflow(t, modelName, 1, workflowJSON); err != nil {
		t.Fatalf("ImportWorkflow %s: %v", modelName, err)
	}
}

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
		if principalKind(ch.ExecutedBy) == execKind {
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

// principalKind returns the executor kind or "" for a nil principal.
func principalKind(p *client.ChangePrincipal) string {
	if p == nil {
		return ""
	}
	return p.Kind
}
