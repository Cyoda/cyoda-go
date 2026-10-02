package multinode

import (
	"encoding/base64"
	"encoding/json"
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
//     to the same-node cascade (the Join-origin-repopulation path). The
//     secondary's own processor, forwarded to the member-hosting node, is
//     called for that user and executed by the member's client.
//  2. Scheduled fire: tasks armed on the cluster by a USER; whichever pnode
//     claims each, the claimed task carries ArmedBy, so every fired change
//     attributes to the arming user, executed by the system — and so does the
//     fire's callout, wherever it is forwarded.
//  3. Callout auth context: a peer-dispatched (A→B forwarded) processor
//     receives the identity the dispatching pnode computed — the attributed
//     principal and the executor — as computed, never recomputed by the peer.
//
// The callout scenarios read what the forwarded processor received from the
// entity data its record-authtype processor writes: observedAuthType and
// observedAuthID (the attributed principal), observedAuthExecType and
// observedAuthExecID (the executor).
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

// attrSampleCreateSecondary is the cascade primary's model sample, seeding
// zero-valued declarations for the fields cb-create-secondary stamps back onto
// the primary. Processor output passes the SAME model checks a client write
// does, so the model must declare them; seeding rather than loosening the
// model's changeLevel keeps the model strict.
const attrSampleCreateSecondary = `{"name":"parent","amount":10,"status":"new","secondaryId":"","secondaryTxId":"","tokenWasEmpty":false}`

// attrObservedFields is the record-authtype processor's output, seeded with
// zero values so a strict model accepts it.
const attrObservedFields = `"observedAuthType":"","observedAuthID":"","observedAuthExecType":"","observedAuthExecID":""`

// attrSampleSecondaryObserved is the cascade secondary's model sample: the
// fields cb-create-secondary writes, and the fields record-authtype writes.
const attrSampleSecondaryObserved = `{"name":"child","amount":1,"status":"new",` + attrObservedFields + `}`

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
	// ComputeServiceID is the executor principal id of every callback of the
	// fixture's own compute client: the id of the M2M client it
	// authenticates as.
	ComputeServiceID() string
}

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
	// The secondary runs record-authtype on a second compute client on node 0,
	// tagged for it alone: a compute client answers one request at a time, and
	// the fixture's own client is still waiting on the callback that creates
	// the secondary. Created in T on the owner node, the secondary's callout is
	// forwarded to node 0 when the owner is another node. The second client
	// serves the same catalog as the fixture's own, so a callout of the
	// compute tenant with no tags that reaches it is answered the same way.
	const recordTag = "attr-mn-casc-rec"
	StartComputeClientOrSkip(t, fixture, 0, parity.ComputeClientSpec{TenantID: admin.ID, Tags: []string{recordTag}})
	cbRouteSetupModel(t, cSetup, secondary, attrSampleSecondaryObserved,
		attrProcWorkflow("attr-mn-casc-sec-wf", "record-authtype", "SYNC", recordTag, ""))
	cbRouteSetupModel(t, cSetup, primary, attrSampleCreateSecondary,
		attrPrimaryProcWorkflow("attr-mn-casc-wf", "cb-create-secondary", "SYNC",
			cbRouteContext(secondary, "attr-mn-casc-marker")))

	const userID = "cluster-alice"
	user := ac.ComputeUser(t, userID, "ROLE_USER", "ROLE_M2M")

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

	serviceID := ac.ComputeServiceID()
	if serviceID == "" {
		t.Fatal("the fixture names no compute client id")
	}
	attrAssert(t, "same-node secondary", sameChange, userID, "user", "service", serviceID)
	attrAssert(t, "cross-node secondary", crossChange, userID, "user", "service", serviceID)

	// The secondary's callout is a write-back cascade: for the transaction's
	// origin, executed by the member's client — forwarded or not.
	attrAssertCallout(t, "same-node secondary callout", attrObserved(t, cRead, sameNodeSecID), userID, "user", serviceID, "service")
	attrAssertCallout(t, "cross-node secondary callout", attrObserved(t, cRead, crossNodeSecID), userID, "user", serviceID, "service")

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

// RunAttribution_ScheduledFire arms several tasks as a USER through node 1,
// waits for them to fire, and asserts every fired change attributes to the
// arming user (executed by the system). Whichever node claims a task runs it
// and reads ArmedBy from the claimed record.
func RunAttribution_ScheduledFire(t *testing.T, fixture MultiNodeFixture) {
	t.Helper()
	ac := attrRequireCapable(t, fixture)
	urls := fixture.BaseURLs()
	if len(urls) < 2 {
		t.Fatalf("scheduled-fire attribution needs >=2 nodes, got %d", len(urls))
	}
	admin := fixture.ComputeTenant(t)

	const model = "attr-mn-sched"
	cbRouteSetupModel(t, client.NewClient(urls[0], admin.Token), model,
		`{"name":"x","amount":1,"status":"new",`+attrObservedFields+`}`, attrScheduledWorkflow())

	const userID = "cluster-bob"
	user := ac.ComputeUser(t, userID, "ROLE_USER", "ROLE_M2M")

	// Arm several tasks through node 1. Any node may claim and run them; the
	// durable ArmedBy (the user) drives the attribution wherever the task runs.
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
		// The fire's callout, made by whichever node claimed the task and
		// forwarded to the member-hosting node from any other: for the arming
		// user, executed by the system.
		attrAssertCallout(t, fmt.Sprintf("scheduled fire #%d callout", i), attrObserved(t, cRead, id), userID, "user", "system", "system")
	}
}

// --- Scenario 3: cross-node callout auth context -----------------------------

// RunAttribution_CalloutAuthType drives a SYNC processor from a memberless owner
// node (forwarded dispatch A→B) and asserts the forwarded processor receives
// the identity the owner computed: a client acting for itself is both the
// attributed principal and the executor.
func RunAttribution_CalloutAuthType(t *testing.T, fixture MultiNodeFixture) {
	t.Helper()
	_ = attrRequireCapable(t, fixture)
	urls := fixture.BaseURLs()
	if len(urls) < 2 {
		t.Fatalf("callout authtype needs >=2 nodes, got %d", len(urls))
	}

	// The compute-tenant SERVICE principal drives the transition for itself.
	svc := fixture.ComputeTenant(t)
	svcID := attrTokenUser(t, svc.Token)

	const model = "attr-mn-authtype"
	sample := `{"name":"x","amount":1,"status":"new",` + attrObservedFields + `}`
	cbRouteSetupModel(t, client.NewClient(urls[0], svc.Token), model, sample,
		attrPrimaryProcWorkflow("attr-mn-authtype-wf", "record-authtype", "SYNC", ""))

	// Drive from node 1 (owner, no member) → dispatch forwards node1->node0
	// (A→B). Node 0 sends the callout to its local member with the identity
	// node 1 computed. The processor records what it observed.
	c := client.NewClient(urls[1], svc.Token)
	id, err := c.CreateEntity(t, model, 1, sample)
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
	attrAssertCallout(t, "forwarded callout", attrObservedOf(ent.Data), svcID, "service", svcID, "service")
}

// --- workflow builders -------------------------------------------------------

// attrScheduledWorkflow: NONE -> (init) -> Open -> (AutoClose, scheduled) ->
// Closed. Creating an entity arms AutoClose with ArmedBy = the creating origin;
// a node's scheduler claims and fires it after delayMs. AutoClose runs
// record-authtype pinned to computeMemberTag, so the fire's callout is
// forwarded to the member-hosting node from any other node.
func attrScheduledWorkflow() string {
	b, _ := json.Marshal(map[string]any{
		"importMode": "REPLACE",
		"workflows": []any{map[string]any{
			"version": "1.1", "name": "attr-mn-sched-wf", "initialState": "NONE", "active": true,
			"states": map[string]any{
				"NONE": map[string]any{"transitions": []any{map[string]any{"name": "init", "next": "Open", "manual": false}}},
				"Open": map[string]any{"transitions": []any{map[string]any{
					"name": "AutoClose", "next": "Closed", "manual": false,
					"schedule": map[string]any{"delayMs": 1500},
					"processors": []any{map[string]any{
						"type": "calculator", "name": "record-authtype", "executionMode": "SYNC",
						"config": map[string]any{"attachEntity": true, "calculationNodesTags": computeMemberTag},
					}},
				}}},
				"Closed": map[string]any{},
			},
		}},
	})
	return string(b)
}

// attrPrimaryProcWorkflow builds a NONE->ACTIVE auto-transition workflow whose
// transition carries one processor pinned to computeMemberTag (so dispatch is
// forwarded to the member-hosting node when driven from any other node).
// contextValue may be "" for processors that need no pass-through context.
func attrPrimaryProcWorkflow(wfName, procName, execMode, contextValue string) string {
	return attrProcWorkflow(wfName, procName, execMode, computeMemberTag, contextValue)
}

// attrProcWorkflow is attrPrimaryProcWorkflow with the processor pinned to tag.
func attrProcWorkflow(wfName, procName, execMode, tag, contextValue string) string {
	// Built with an encoder, not a string template. contextValue arrives as a
	// plain JSON string from cbRouteContext and is encoded here — splicing it
	// raw would emit a nested object where the DTO expects a string.
	cfg := map[string]any{"attachEntity": true, "calculationNodesTags": tag}
	if contextValue != "" {
		cfg["context"] = contextValue
	}
	b, _ := json.Marshal(map[string]any{
		"importMode": "REPLACE",
		"workflows": []any{map[string]any{
			"version": "1.1", "name": wfName, "initialState": "NONE", "active": true,
			"states": map[string]any{
				"NONE": map[string]any{"transitions": []any{map[string]any{
					"name": "init", "next": "ACTIVE", "manual": false,
					"processors": []any{map[string]any{
						"type": "calculator", "name": procName, "executionMode": execMode, "config": cfg,
					}},
				}}},
				"ACTIVE": map[string]any{},
			},
		}},
	})
	return string(b)
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

// attrObservation is the auth context a record-authtype processor observed.
type attrObservation struct {
	ID, Type, ExecID, ExecType string
}

// attrObservedOf reads the auth context record-authtype wrote into data.
func attrObservedOf(data map[string]any) attrObservation {
	str := func(k string) string { v, _ := data[k].(string); return v }
	return attrObservation{
		ID: str("observedAuthID"), Type: str("observedAuthType"),
		ExecID: str("observedAuthExecID"), ExecType: str("observedAuthExecType"),
	}
}

// attrObserved reads entityID and returns the auth context its record-authtype
// processor observed.
func attrObserved(t *testing.T, c *client.Client, entityID uuid.UUID) attrObservation {
	t.Helper()
	ent, err := c.GetEntity(t, entityID)
	if err != nil {
		t.Fatalf("get entity %s: %v", entityID, err)
	}
	return attrObservedOf(ent.Data)
}

// attrAssertCallout asserts both principals of the auth context a callout
// carried.
func attrAssertCallout(t *testing.T, label string, got attrObservation, wantID, wantType, wantExecID, wantExecType string) {
	t.Helper()
	if got.ID != wantID || got.Type != wantType {
		t.Errorf("%s: authid/authtype = %q/%q; want %q/%q", label, got.ID, got.Type, wantID, wantType)
	}
	if got.ExecID != wantExecID || got.ExecType != wantExecType {
		t.Errorf("%s: authexecid/authexectype = %q/%q; want %q/%q", label, got.ExecID, got.ExecType, wantExecID, wantExecType)
	}
}

// attrTokenUser returns the caas_user_id claim of a JWT: the principal id the
// token authenticates. The signature is not checked; the token is the test's
// own.
func attrTokenUser(t *testing.T, token string) string {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatal("the token is not a JWT")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode the token's claims: %v", err)
	}
	var claims struct {
		User string `json:"caas_user_id"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil || claims.User == "" {
		t.Fatalf("the token names no caas_user_id (%v)", err)
	}
	return claims.User
}

// attrPrincipalKind returns the executor kind or "" for a nil principal.
func attrPrincipalKind(p *client.ChangePrincipal) string {
	if p == nil {
		return ""
	}
	return p.Kind
}
