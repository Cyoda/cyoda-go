package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	cyodapb "github.com/cyoda-platform/cyoda-go/api/grpc/cyoda"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

// grpc_attribution_test.go — on-behalf-of attribution and the own-user join
// rule through the gRPC door (EntityManage): an OBO create records the user
// and the OBO client; a compute write-back joined to the user's transaction
// records the user and the compute client; an OBO create that arms a
// scheduled transition stamps ScheduledTask.ArmedBy with the OBO user; an OBO
// callback joining another user's transaction is refused through the RPC's
// error envelope. The pass is never logged.

// createEntityGRPCAs issues an EntityCreateRequest over EntityManage under
// bearer, joined to pass when it is non-empty, and returns the response
// envelope with the created entity's id ("" on failure). It takes no
// *testing.T, so a processor may call it.
func (h *callbackHarness) createEntityGRPCAs(bearer, pass, model string, version int, payload string) (txEnvelope, string, error) {
	var data map[string]any
	if err := json.Unmarshal([]byte(payload), &data); err != nil {
		return txEnvelope{}, "", fmt.Errorf("failed to read the payload: %w", err)
	}
	reqCE, err := internalgrpc.NewCloudEvent(internalgrpc.EntityCreateRequest, map[string]any{
		"id":         "grpc-attribution-create",
		"dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": model, "version": version},
			"data":  data,
		},
	})
	if err != nil {
		return txEnvelope{}, "", fmt.Errorf("failed to build create request: %w", err)
	}
	respCE, err := cyodapb.NewCloudEventsServiceClient(h.apiConn).EntityManage(h.grpcCtxAs(bearer, pass), reqCE)
	if err != nil {
		return txEnvelope{}, "", fmt.Errorf("failed to call EntityManage: %w", err)
	}
	env, err := parseTxEnvelope(respCE)
	if err != nil || !env.Success {
		return env, "", err
	}
	_, raw, err := internalgrpc.ParseCloudEvent(respCE)
	if err != nil {
		return env, "", err
	}
	var info struct {
		TransactionInfo struct {
			EntityIDs []string `json:"entityIds"`
		} `json:"transactionInfo"`
	}
	if err := json.Unmarshal(raw, &info); err != nil || len(info.TransactionInfo.EntityIDs) == 0 {
		return env, "", fmt.Errorf("no entity id in the create response (%v)", err)
	}
	return env, info.TransactionInfo.EntityIDs[0], nil
}

// TestGRPCAttribution_OBOWrite: an EntityManage create under alice's
// on-behalf-of token records alice, of kind user, executed by the OBO client.
func TestGRPCAttribution_OBOWrite(t *testing.T) {
	h := newCalloutHarness(t, nil)
	const model = "grpc-attr-obo-write"
	h.SetupModelWithWorkflow(t, model, secondaryWorkflow)

	alice := oboTokenOn(t, h.baseURL, h.token(t), "alice")
	env, id, err := h.createEntityGRPCAs(alice, "", model, 1, `{"name":"x","amount":1,"status":"new"}`)
	if err != nil || !env.Success {
		t.Fatalf("EntityManage create as alice (OBO): %s %v", describeEnv(env), err)
	}
	assertAttribution(t, findChangeByType(h.getChanges(t, id), "CREATE"), "gRPC OBO write", "alice", "user", "service", oboClientOf(t, alice))
}

// TestGRPCAttribution_OBOWriteBack: alice's on-behalf-of token creates X; X's
// SYNC processor writes Y back over EntityManage with the compute client's own
// token and the pass. Y records alice — the transaction's origin — executed by
// the compute client.
func TestGRPCAttribution_OBOWriteBack(t *testing.T) {
	h := newCallbackHarness(t)
	const primary = "grpc-attr-obo-wb-primary"
	const secondary = "grpc-attr-obo-wb-secondary"
	h.SetupModelWithWorkflow(t, secondary, secondaryWorkflow)

	compute := h.computeBearer(t)
	computeID, _ := decodeJWTPayload(t, compute)["caas_user_id"].(string)
	if computeID == "" {
		t.Fatal("the compute client's token carries no caas_user_id")
	}
	yIDs := make(chan string, 1)
	h.RegisterProc("grpc-attr-obo-wb-proc", func(rc *reqCtx) (map[string]any, error) {
		env, id, err := h.createEntityGRPCAs(compute, rc.token, secondary, 1, `{"name":"y","amount":1,"status":"new"}`)
		if err != nil || !env.Success {
			return nil, fmt.Errorf("gRPC write-back: %s %v", describeEnv(env), err)
		}
		yIDs <- id
		return nil, nil
	})
	h.SetupModelWithWorkflow(t, primary, procCascadeWF("grpc-attr-obo-wb", "grpc-attr-obo-wb-proc", "SYNC", ""))

	alice := oboTokenOn(t, h.baseURL, h.token(t), "alice")
	if _, status, body := h.createEntityAs(t, alice, primary, 1, `{"name":"x","amount":100,"status":"new"}`); status != http.StatusOK {
		t.Fatalf("create X as alice (OBO): %d %s", status, body)
	}

	var yID string
	select {
	case yID = <-yIDs:
	case <-time.After(10 * time.Second):
		t.Fatal("timeout: write-back processor did not create Y")
	}
	assertAttribution(t, findChangeByType(h.getChanges(t, yID), "CREATE"), "Y gRPC write-back", "alice", "user", "service", computeID)
}

// TestGRPCAttribution_ScheduledOBOArmed: an EntityManage create under alice's
// on-behalf-of token, whose workflow arms a far-future scheduled transition,
// stamps the durable ScheduledTask.ArmedBy with alice — never the OBO client —
// through the gRPC door exactly as it does through HTTP (spec §13: "scheduled
// fire armed by an OBO request, directly ... (gRPC)"). farFutureTimerWF keeps
// the timer armed (never due) for the length of the test, so the row can be
// inspected directly rather than waiting on the scheduler. This stack's
// scheduler is disabled (newCalloutHarness default) and shares the package's
// database (callback_harness_test.go), so the armed row is explicitly deleted
// at the end — nothing on this stack or the shared TestMain server would ever
// claim or cancel it otherwise (TestScheduledTaskWrites_DeleteEntity_RemovesItsTasks
// confirms deletion cancels an entity's scheduled tasks).
func TestGRPCAttribution_ScheduledOBOArmed(t *testing.T) {
	h := newCalloutHarness(t, nil)
	const model = "grpc-attr-sched-obo-armed"
	h.SetupModelWithWorkflow(t, model, farFutureTimerWF("grpc-attr-sched-obo-armed-wf"))

	alice := oboTokenOn(t, h.baseURL, h.token(t), "alice")
	env, id, err := h.createEntityGRPCAs(alice, "", model, 1, `{"name":"x","amount":1,"status":"new"}`)
	if err != nil || !env.Success {
		t.Fatalf("EntityManage create as alice (OBO): %s %v", describeEnv(env), err)
	}
	t.Cleanup(func() {
		if env, err := h.deleteEntityGRPC(id); err != nil || !env.Success {
			t.Errorf("cleanup: delete %s: %s %v", id, describeEnv(env), err)
		}
	})

	var armedID, armedKind string
	if err := dbPool.QueryRow(context.Background(),
		`SELECT armed_by_id, armed_by_kind FROM scheduled_tasks WHERE entity_id=$1`, id,
	).Scan(&armedID, &armedKind); err != nil {
		t.Fatalf("inspect scheduled_task for %s: %v", id, err)
	}
	if armedID != "alice" || armedKind != "user" {
		t.Errorf("armed timer principal = {%q,%q}; want {alice,user} (the OBO user, never the OBO client)", armedID, armedKind)
	}
}

// TestGRPCCallbackJoin_OBOOtherUser_Forbidden: alice's on-behalf-of token
// creates X; X's processor calls EntityManage with the pass of alice's
// transaction under bob's on-behalf-of token. The join is refused through the
// RPC's error envelope, as every join failure on this door is — Success false,
// the operational CLIENT_ERROR envelope code, the message carrying the domain
// code FORBIDDEN — and no Y exists.
func TestGRPCCallbackJoin_OBOOtherUser_Forbidden(t *testing.T) {
	h := newCallbackHarness(t)
	const primary = "grpc-cbj-obo-other-primary"
	const secondary = "grpc-cbj-obo-other-secondary"
	h.SetupModelWithWorkflow(t, secondary, secondaryWorkflow)

	alice := oboTokenOn(t, h.baseURL, h.token(t), "alice")
	bob := oboTokenOn(t, h.baseURL, h.token(t), "bob")
	type outcome struct {
		env txEnvelope
		err error
	}
	results := make(chan outcome, 1)
	h.RegisterProc("grpc-cbj-obo-other-proc", func(rc *reqCtx) (map[string]any, error) {
		env, _, err := h.createEntityGRPCAs(bob, rc.token, secondary, 1, `{"name":"y","amount":1,"status":"new"}`)
		results <- outcome{env, err}
		return nil, nil
	})
	h.SetupModelWithWorkflow(t, primary, procCascadeWF("grpc-cbj-obo-other", "grpc-cbj-obo-other-proc", "SYNC", ""))

	if _, status, body := h.createEntityAs(t, alice, primary, 1, `{"name":"x","amount":100,"status":"new"}`); status != http.StatusOK {
		t.Fatalf("create X as alice (OBO): %d %s", status, body)
	}

	var got outcome
	select {
	case got = <-results:
	case <-time.After(10 * time.Second):
		t.Fatal("timeout: processor did not call back")
	}
	if got.err != nil {
		t.Fatalf("EntityManage as bob: %v", got.err)
	}
	const want = "FORBIDDEN: an on-behalf-of request may join only its own user's transaction"
	if got.env.Success || got.env.Error == nil || got.env.Error.Code != "CLIENT_ERROR" || got.env.Error.Message != want {
		t.Fatalf("envelope = %s; want success=false code=CLIENT_ERROR message=%q", describeEnv(got.env), want)
	}
	if n := h.countEntities(t, secondary); n != 0 {
		t.Fatalf("%d %s entities exist; want none", n, secondary)
	}
}

// describeEnv renders an envelope for a failure message, the error's code and
// message included (a %+v would print the error as an address).
func describeEnv(env txEnvelope) string {
	if env.Error == nil {
		return fmt.Sprintf("success=%t", env.Success)
	}
	return fmt.Sprintf("success=%t code=%s message=%q", env.Success, env.Error.Code, env.Error.Message)
}
