package e2e_test

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

// callout_identity_test.go — the auth context of a callout, on every path a
// callout is made from. A callout names who its work is for (authid/authtype,
// the attributed principal) and who executes it (authexecid/authexectype);
// authclaims are the executor's roles. The pnode that dispatches computes the
// pair once from the request's context. Each scenario drives the path through
// the full HTTP+gRPC stack and reads the attributes the compute member
// received.

// calloutAuth is the auth context one callout carried.
type calloutAuth struct {
	id, typ, execID, execType string
	claims                    string
	claimsPresent             bool
}

// recordCalloutAuth registers processor name on h's compute member: it
// records the auth context of each request it receives on the returned
// channel and answers with no change to the entity.
func recordCalloutAuth(h *callbackHarness, name string) <-chan calloutAuth {
	seen := make(chan calloutAuth, 4)
	h.RegisterProc(name, func(rc *reqCtx) (map[string]any, error) {
		sendCalloutAuth(seen, rc)
		return nil, nil
	})
	return seen
}

// sendCalloutAuth records rc's auth context on seen without blocking.
func sendCalloutAuth(seen chan<- calloutAuth, rc *reqCtx) {
	claims, ok := rc.attrs["authclaims"]
	select {
	case seen <- calloutAuth{
		id: rc.attrs["authid"], typ: rc.attrs["authtype"],
		execID: rc.attrs["authexecid"], execType: rc.attrs["authexectype"],
		claims: claims, claimsPresent: ok,
	}:
	default:
	}
}

// awaitCalloutAuth waits for the first callout seen on ch.
func awaitCalloutAuth(t *testing.T, ch <-chan calloutAuth, within time.Duration) calloutAuth {
	t.Helper()
	select {
	case got := <-ch:
		return got
	case <-time.After(within):
		t.Fatal("timeout: the processor was never called")
		return calloutAuth{}
	}
}

// assertCalloutAuth checks both principals of a callout's auth context.
func assertCalloutAuth(t *testing.T, label string, got calloutAuth, wantID, wantType, wantExecID, wantExecType string) {
	t.Helper()
	if got.id != wantID || got.typ != wantType {
		t.Errorf("%s: authid/authtype = %q/%q, want %q/%q", label, got.id, got.typ, wantID, wantType)
	}
	if got.execID != wantExecID || got.execType != wantExecType {
		t.Errorf("%s: authexecid/authexectype = %q/%q, want %q/%q", label, got.execID, got.execType, wantExecID, wantExecType)
	}
}

// assertClaimsHold checks that authclaims, the executor's roles, include role.
func assertClaimsHold(t *testing.T, label string, got calloutAuth, role string) {
	t.Helper()
	if !slices.Contains(strings.Split(got.claims, ","), role) {
		t.Errorf("%s: authclaims = %q (present %v), want it to hold %s", label, got.claims, got.claimsPresent, role)
	}
}

// writeBackWF is a model whose init transition runs one SYNC processor.
func writeBackWF(name, procName string) string {
	return procCascadeWF(name, procName, "SYNC", "")
}

// TestCalloutIdentity_OBORequest: a callout made by an on-behalf-of request is
// for the user and executed by the OBO client.
func TestCalloutIdentity_OBORequest(t *testing.T) {
	h := newCallbackHarness(t)
	const model = "cid-obo"
	seen := recordCalloutAuth(h, "cid-obo-proc")
	h.SetupModelWithWorkflow(t, model, writeBackWF(model, "cid-obo-proc"))

	alice := oboTokenOn(t, h.baseURL, h.token(t), "alice")
	if _, status, body := h.createEntityAs(t, alice, model, 1, `{"name":"x","amount":1,"status":"new"}`); status != http.StatusOK {
		t.Fatalf("create as alice (OBO): %d %s", status, body)
	}

	got := awaitCalloutAuth(t, seen, 10*time.Second)
	assertCalloutAuth(t, "OBO request", got, "alice", "user", oboClientOf(t, alice), "service")
	assertClaimsHold(t, "OBO request", got, "ROLE_M2M")
}

// TestCalloutIdentity_ClientsOwnRequest: a client acting for itself is both
// principals of its callouts.
func TestCalloutIdentity_ClientsOwnRequest(t *testing.T) {
	h := newCallbackHarness(t)
	const model = "cid-own"
	seen := recordCalloutAuth(h, "cid-own-proc")
	h.SetupModelWithWorkflow(t, model, writeBackWF(model, "cid-own-proc"))

	if _, status, body := h.CreateEntity(t, model, 1, `{"name":"x","amount":1,"status":"new"}`); status != http.StatusOK {
		t.Fatalf("create as the suite client: %d %s", status, body)
	}

	got := awaitCalloutAuth(t, seen, 10*time.Second)
	assertCalloutAuth(t, "client's own request", got, attrServiceID, "service", attrServiceID, "service")
	assertClaimsHold(t, "client's own request", got, "ROLE_M2M")
}

// TestCalloutIdentity_WriteBackCascade: alice's on-behalf-of request creates
// X; X's processor A — called for alice, executed by the OBO client — writes
// Y back with the compute client's own token, joined to alice's transaction;
// Y's processor B is called within the same request. B's callout is for
// alice, the transaction's origin, and executed by the compute client.
func TestCalloutIdentity_WriteBackCascade(t *testing.T) {
	h := newCallbackHarness(t)
	const primary = "cid-wb-primary"
	const secondary = "cid-wb-secondary"
	seen := recordCalloutAuth(h, "cid-wb-b")
	h.SetupModelWithWorkflow(t, secondary, writeBackWF(secondary, "cid-wb-b"))

	compute := h.computeBearer(t)
	computeID, _ := decodeJWTPayload(t, compute)["caas_user_id"].(string)
	if computeID == "" {
		t.Fatal("the compute client's token carries no caas_user_id")
	}
	seenA := make(chan calloutAuth, 4)
	h.RegisterProc("cid-wb-a", func(rc *reqCtx) (map[string]any, error) {
		sendCalloutAuth(seenA, rc)
		res, err := rc.CreateEntityAs(compute, secondary, 1, `{"name":"y","amount":1,"status":"new"}`)
		if err != nil {
			return nil, fmt.Errorf("write-back create: %w", err)
		}
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("write-back create status=%d body=%s", res.StatusCode, res.Body)
		}
		return nil, nil
	})
	h.SetupModelWithWorkflow(t, primary, writeBackWF(primary, "cid-wb-a"))

	alice := oboTokenOn(t, h.baseURL, h.token(t), "alice")
	if _, status, body := h.createEntityAs(t, alice, primary, 1, `{"name":"x","amount":1,"status":"new"}`); status != http.StatusOK {
		t.Fatalf("create X as alice (OBO): %d %s", status, body)
	}

	gotA := awaitCalloutAuth(t, seenA, 10*time.Second)
	assertCalloutAuth(t, "OBO request's own callout", gotA, "alice", "user", oboClientOf(t, alice), "service")
	got := awaitCalloutAuth(t, seen, 10*time.Second)
	assertCalloutAuth(t, "write-back cascade", got, "alice", "user", computeID, "service")
	assertClaimsHold(t, "write-back cascade", got, "ROLE_M2M")
}

// TestCalloutIdentity_OBOCascade: alice's on-behalf-of request creates X; X's
// processor writes Y back with alice's own on-behalf-of token, joined to her
// transaction (an on-behalf-of request may join its own user's); Y's
// processor is called within the same request. Every callout of an
// on-behalf-of request and its cascades is for alice, executed by the OBO
// client.
func TestCalloutIdentity_OBOCascade(t *testing.T) {
	h := newCallbackHarness(t)
	const primary = "cid-obo-casc-primary"
	const secondary = "cid-obo-casc-secondary"
	seen := recordCalloutAuth(h, "cid-obo-casc-b")
	h.SetupModelWithWorkflow(t, secondary, writeBackWF(secondary, "cid-obo-casc-b"))

	alice := oboTokenOn(t, h.baseURL, h.token(t), "alice")
	seenA := make(chan calloutAuth, 4)
	h.RegisterProc("cid-obo-casc-a", func(rc *reqCtx) (map[string]any, error) {
		sendCalloutAuth(seenA, rc)
		res, err := rc.CreateEntityAs(alice, secondary, 1, `{"name":"y","amount":1,"status":"new"}`)
		if err != nil {
			return nil, fmt.Errorf("OBO cascade create: %w", err)
		}
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("OBO cascade create status=%d body=%s", res.StatusCode, res.Body)
		}
		return nil, nil
	})
	h.SetupModelWithWorkflow(t, primary, writeBackWF(primary, "cid-obo-casc-a"))

	if _, status, body := h.createEntityAs(t, alice, primary, 1, `{"name":"x","amount":1,"status":"new"}`); status != http.StatusOK {
		t.Fatalf("create X as alice (OBO): %d %s", status, body)
	}

	obo := oboClientOf(t, alice)
	assertCalloutAuth(t, "OBO request's callout", awaitCalloutAuth(t, seenA, 10*time.Second), "alice", "user", obo, "service")
	got := awaitCalloutAuth(t, seen, 10*time.Second)
	assertCalloutAuth(t, "OBO cascade's callout", got, "alice", "user", obo, "service")
	assertClaimsHold(t, "OBO cascade's callout", got, "ROLE_M2M")
}

// TestCalloutIdentity_GRPCOBORequest: the gRPC door. An entity created over
// EntityManage with alice's on-behalf-of token runs a processor whose callout
// is for alice, executed by the OBO client.
func TestCalloutIdentity_GRPCOBORequest(t *testing.T) {
	h := newCallbackHarness(t)
	const model = "cid-grpc-obo"
	seen := recordCalloutAuth(h, "cid-grpc-obo-proc")
	h.SetupModelWithWorkflow(t, model, writeBackWF(model, "cid-grpc-obo-proc"))

	alice := oboTokenOn(t, h.baseURL, h.token(t), "alice")
	if env, _, err := h.createEntityGRPCAs(alice, "", model, 1, `{"name":"x","amount":1,"status":"new"}`); err != nil || !env.Success {
		t.Fatalf("EntityManage create as alice (OBO): %s %v", describeEnv(env), err)
	}

	got := awaitCalloutAuth(t, seen, 10*time.Second)
	assertCalloutAuth(t, "gRPC OBO request", got, "alice", "user", oboClientOf(t, alice), "service")
	assertClaimsHold(t, "gRPC OBO request", got, "ROLE_M2M")
}

// TestCalloutIdentity_CBDDetachedCallback: a commit-before-dispatch processor
// calls back with no pass — an ordinary request of the compute client, in a
// transaction of its own. The callouts that request makes are the compute
// client's, in both principals.
func TestCalloutIdentity_CBDDetachedCallback(t *testing.T) {
	h := newCallbackHarness(t)
	const primary = "cid-cbd-primary"
	const secondary = "cid-cbd-secondary"
	seen := recordCalloutAuth(h, "cid-cbd-b")
	h.SetupModelWithWorkflow(t, secondary, writeBackWF(secondary, "cid-cbd-b"))

	compute := h.computeBearer(t)
	computeID, _ := decodeJWTPayload(t, compute)["caas_user_id"].(string)
	if computeID == "" {
		t.Fatal("the compute client's token carries no caas_user_id")
	}
	h.RegisterProc("cid-cbd-a", func(rc *reqCtx) (map[string]any, error) {
		if rc.token != "" {
			return nil, fmt.Errorf("a commit-before-dispatch callout carried a pass")
		}
		res, err := rc.CreateEntityAs(compute, secondary, 1, `{"name":"y","amount":1,"status":"new"}`)
		if err != nil {
			return nil, fmt.Errorf("detached create: %w", err)
		}
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("detached create status=%d body=%s", res.StatusCode, res.Body)
		}
		return nil, nil
	})
	h.SetupModelWithWorkflow(t, primary, procCascadeWF(primary, "cid-cbd-a", "COMMIT_BEFORE_DISPATCH", ""))

	alice := oboTokenOn(t, h.baseURL, h.token(t), "alice")
	if _, status, body := h.createEntityAs(t, alice, primary, 1, `{"name":"x","amount":1,"status":"new"}`); status != http.StatusOK {
		t.Fatalf("create X as alice (OBO): %d %s", status, body)
	}

	got := awaitCalloutAuth(t, seen, 10*time.Second)
	assertCalloutAuth(t, "CBD-detached callback", got, computeID, "service", computeID, "service")
	assertClaimsHold(t, "CBD-detached callback", got, "ROLE_M2M")
}

// TestCalloutIdentity_ScheduledFire: a timer armed by alice's on-behalf-of
// request fires; the fire's callout is for alice, the arming principal, and
// executed by the system, which holds no roles.
func TestCalloutIdentity_ScheduledFire(t *testing.T) {
	h, _ := newSchedulerCallbackHarness(t, nil)
	model := uniq("cid-sched")
	seen := recordCalloutAuth(h, "cid-sched-proc")
	h.SetupModelWithWorkflow(t, model, fireOpenToDone("cid-sched-wf", 300, 0, sProc("cid-sched-proc", "SYNC", "", true)))

	alice := oboTokenOn(t, h.baseURL, h.token(t), "alice")
	id, status, body := h.createEntityAs(t, alice, model, 1, workflowSampleModel)
	if status != http.StatusOK {
		t.Fatalf("create as alice (OBO): %d %s", status, body)
	}

	got := awaitCalloutAuth(t, seen, scheduledFireTimeout)
	assertCalloutAuth(t, "scheduled fire", got, "alice", "user", firedSchedExecID, firedSchedExecKind)
	if got.claimsPresent {
		t.Errorf("scheduled fire: authclaims = %q, want absent: the system holds no roles", got.claims)
	}
	awaitCallbackEntityState(t, h, id, "Done", scheduledFireTimeout)
}

// TestCalloutIdentity_ScheduledFireCBDOutsideTx: a timer armed by alice's
// on-behalf-of request fires a commit-before-dispatch processor with
// startNewTxOnDispatch false, so the processor is dispatched with no
// transaction. Its callout is still for alice, the arming principal, and
// executed by the system; the write that applies the processor's result is
// alice's, executed by the system.
func TestCalloutIdentity_ScheduledFireCBDOutsideTx(t *testing.T) {
	h, _ := newSchedulerCallbackHarness(t, nil)
	model := uniq("cid-sched-cbd")
	seen := make(chan calloutAuth, 4)
	h.RegisterProc("cid-sched-cbd-proc", func(rc *reqCtx) (map[string]any, error) {
		sendCalloutAuth(seen, rc)
		return map[string]any{"name": "Test Order", "amount": 777, "status": "draft"}, nil
	})
	proc := sProc("cid-sched-cbd-proc", "COMMIT_BEFORE_DISPATCH", "", true)
	proc["config"].(map[string]any)["startNewTxOnDispatch"] = false
	h.SetupModelWithWorkflow(t, model, fireOpenToDone("cid-sched-cbd-wf", 300, 0, proc))

	alice := oboTokenOn(t, h.baseURL, h.token(t), "alice")
	id, status, body := h.createEntityAs(t, alice, model, 1, workflowSampleModel)
	if status != http.StatusOK {
		t.Fatalf("create as alice (OBO): %d %s", status, body)
	}

	got := awaitCalloutAuth(t, seen, scheduledFireTimeout)
	assertCalloutAuth(t, "scheduled fire, CBD outside a transaction", got, "alice", "user", firedSchedExecID, firedSchedExecKind)
	if got.claimsPresent {
		t.Errorf("scheduled fire, CBD outside a transaction: authclaims = %q, want absent: the system holds no roles", got.claims)
	}

	awaitCallbackEntityState(t, h, id, "Done", scheduledFireTimeout)
	if amount, _ := h.GetEntityData(t, id)["amount"].(float64); amount != 777 {
		t.Fatalf("amount = %v, want 777: the processor's result was not applied", amount)
	}
	// Every write of the fire — TX_pre's and the one applying the result in
	// TX_post — is alice's, executed by the system.
	updates := 0
	for _, c := range h.getChanges(t, id) {
		if ct, _ := c["changeType"].(string); ct != "UPDATE" {
			continue
		}
		updates++
		assertAttribution(t, c, "scheduled fire write", "alice", "user", firedSchedExecKind, firedSchedExecID)
	}
	if updates == 0 {
		t.Fatal("no UPDATE change recorded for the fire")
	}
}
