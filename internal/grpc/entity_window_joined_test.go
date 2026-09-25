package grpc

import (
	"strings"
	"testing"

	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// TestRPC_TransactionWindow_JoinedRejected400: a request that joined an open
// transaction commits nothing — its owner does — so the transactionWindow of
// the two collection events is refused on it with CLIENT_ERROR/BAD_REQUEST,
// as transactionTimeoutMs is, rather than accepted and ignored.
func TestRPC_TransactionWindow_JoinedRejected400(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"name": "Alice"})
	ownerTxID, _, err := svc.txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("owner Begin: %v", err)
	}
	t.Cleanup(func() { _ = svc.txMgr.Rollback(ctx, ownerTxID) })
	joinedCtx, err := svc.txMgr.Join(ctx, ownerTxID)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}

	for _, tc := range []struct {
		name      string
		eventType string
		payload   map[string]any
	}{
		{"CreateCollection", EntityCreateCollectionRequest, map[string]any{
			"id": "test", "dataFormat": "JSON", "transactionWindow": 10,
			"payloads": []any{map[string]any{"model": map[string]any{"name": "person", "version": 1}, "data": map[string]any{"name": "A"}}},
		}},
		{"UpdateCollection", EntityUpdateCollectionRequest, map[string]any{
			"id": "test", "dataFormat": "JSON", "transactionWindow": 10,
			"payloads": []any{map[string]any{"entityId": sampleTimeoutEntityID, "data": map[string]any{"name": "B"}}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := &mockManageStream{ctx: joinedCtx}
			if err := svc.EntityManageCollection(makeCE(tc.eventType, tc.payload), stream); err != nil {
				t.Fatalf("unexpected gRPC error: %v", err)
			}
			if len(stream.sent) != 1 {
				t.Fatalf("expected 1 response, got %d", len(stream.sent))
			}
			var typed events.EntityTransactionResponseJson
			validateResponse(t, stream.sent[0], &typed)
			if typed.Success || typed.Error == nil {
				t.Fatalf("success=%t error=%v; want a refusal", typed.Success, typed.Error)
			}
			if typed.Error.Code != "CLIENT_ERROR" {
				t.Errorf("code = %q, want CLIENT_ERROR", typed.Error.Code)
			}
			msg := typed.Error.Message
			if !strings.HasPrefix(msg, common.ErrCodeBadRequest+":") || !strings.Contains(msg, "transactionWindow") ||
				!strings.Contains(msg, "joins an open transaction") {
				t.Errorf("message = %q; want BAD_REQUEST naming transactionWindow and the joined transaction", msg)
			}
		})
	}
}
