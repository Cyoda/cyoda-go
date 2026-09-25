package grpc

import (
	"fmt"
	"strings"
	"testing"

	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
)

// cbdRPCWorkflowJSON carries a COMMIT_BEFORE_DISPATCH processor on the create's
// automated transition, or — onTransition — on a manual transition "go" out of
// Ready, which a create reaches with no processor.
func cbdRPCWorkflowJSON(wfName, procName string, onTransition bool) string {
	cbd := fmt.Sprintf(`"processors": [{"type": "calculator", "name": %q, "executionMode": "COMMIT_BEFORE_DISPATCH",
		"config": {"attachEntity": true, "calculationNodesTags": "no-member-needed"}}]`, procName)
	states := fmt.Sprintf(`"Open": {"transitions": [{"name": "init", "next": "Done", "manual": false, %s}]}, "Done": {}`, cbd)
	if onTransition {
		states = fmt.Sprintf(`"Open": {"transitions": [{"name": "store", "next": "Ready", "manual": false}]},
			"Ready": {"transitions": [{"name": "go", "next": "Done", "manual": true, %s}]}, "Done": {}`, cbd)
	}
	return fmt.Sprintf(`{"importMode": "REPLACE", "workflows": [{
		"version": "1.1", "name": %q, "initialState": "Open", "active": true, "states": {%s}}]}`, wfName, states)
}

// TestRPC_JoinedWrite_CommitBeforeDispatch_Envelope: a write on the gRPC door
// that joined its transaction and reaches a COMMIT_BEFORE_DISPATCH processor is
// refused with the CLIENT_ERROR envelope carrying COMMIT_IN_JOINED_TRANSACTION,
// not retryable, naming the processor — and the joined transaction is still
// open afterwards. No compute member is registered: the refusal comes before
// any dispatch.
func TestRPC_JoinedWrite_CommitBeforeDispatch_Envelope(t *testing.T) {
	for _, tc := range []struct {
		name         string
		onTransition bool
	}{
		{"EntityCreateRequest", false},
		{"EntityTransitionRequest", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			modelName := "grpc-joined-cbd-" + strings.ToLower(tc.name)
			svc, wfHandler, ctx := newTestEnvWithDispatch(t)
			setupScheduledWorkflowRPCEnv(t, svc, wfHandler, ctx, modelName,
				cbdRPCWorkflowJSON("joined-cbd-wf", "segmenter", tc.onTransition))

			entityID := ""
			if tc.onTransition {
				created := createScheduledEntity(t, svc, ctx, modelName)
				if !created.Success {
					t.Fatalf("setup create failed: %+v", created.Error)
				}
				entityID = created.TransactionInfo.EntityIds[0]
			}

			ownerTxID, _, err := svc.txMgr.Begin(ctx)
			if err != nil {
				t.Fatalf("owner Begin: %v", err)
			}
			t.Cleanup(func() { _ = svc.txMgr.Rollback(ctx, ownerTxID) })
			joinedCtx, err := svc.txMgr.Join(ctx, ownerTxID)
			if err != nil {
				t.Fatalf("Join: %v", err)
			}

			// The two response types carry distinct but identically shaped error
			// objects; read both into one shape.
			var success bool
			var code, message string
			var retryable *bool
			if tc.onTransition {
				resp, err := svc.EntityManage(joinedCtx, makeCE(EntityTransitionRequest, map[string]any{
					"id": "transition-1", "entityId": entityID, "transition": "go",
				}))
				if err != nil {
					t.Fatalf("unexpected gRPC transport error: %v", err)
				}
				var tr events.EntityTransitionResponseJson
				validateResponse(t, resp, &tr)
				success = tr.Success
				if tr.Error != nil {
					code, message, retryable = tr.Error.Code, tr.Error.Message, tr.Error.Retryable
				}
			} else {
				cr := createScheduledEntity(t, svc, joinedCtx, modelName)
				success = cr.Success
				if cr.Error != nil {
					code, message, retryable = cr.Error.Code, cr.Error.Message, cr.Error.Retryable
				}
			}

			if success {
				t.Fatal("the joined write succeeded; want a refusal")
			}
			if code != "CLIENT_ERROR" {
				t.Errorf("Error.Code = %q; want CLIENT_ERROR", code)
			}
			if !strings.HasPrefix(message, "COMMIT_IN_JOINED_TRANSACTION:") {
				t.Errorf("message = %q; want the domain code as its prefix", message)
			}
			if !strings.Contains(message, `"segmenter"`) {
				t.Errorf("message does not name the processor: %s", message)
			}
			if retryable != nil && *retryable {
				t.Error("the refusal is not retryable")
			}
			if _, err := svc.txMgr.Join(ctx, ownerTxID); err != nil {
				t.Fatalf("the joined transaction is no longer open after the refusal: %v", err)
			}
		})
	}
}
