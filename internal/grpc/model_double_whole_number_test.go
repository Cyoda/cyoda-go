package grpc

import (
	"strings"
	"testing"

	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
)

// gRPC is a separate entry point from HTTP and gets its own coverage per
// .claude/rules/test-coverage.md. A leaf declared DOUBLE admits a whole
// number — INTEGER widens into DOUBLE — so the write proposes no schema
// change and must be accepted at the most restrictive change level, over
// this transport as over HTTP.
func TestRPC_EntityCreate_WholeNumberIntoDoubleLeafNeedsNoLevel(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "double-whole-grpc", "1", map[string]any{"amount": 10.5})

	if err := svc.modelHandler.SetChangeLevel(ctx, "double-whole-grpc", "1", "ARRAY_LENGTH"); err != nil {
		t.Fatalf("SetChangeLevel(ARRAY_LENGTH): %v", err)
	}

	ce := makeCE(EntityCreateRequest, map[string]any{
		"id":         "test",
		"dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": "double-whole-grpc", "version": 1},
			"data":  map[string]any{"amount": 1000},
		},
	})
	resp, err := svc.EntityManage(ctx, ce)
	if err != nil {
		t.Fatalf("unexpected transport error (this must be an envelope error): %v", err)
	}
	var typed events.EntityTransactionResponseJson
	validateResponse(t, resp, &typed)
	if !typed.Success {
		msg := ""
		if typed.Error != nil {
			msg = typed.Error.Message
		}
		if strings.Contains(msg, "type change") {
			t.Fatalf("a whole number is assignable to a DOUBLE leaf; it must not need a level: %s", msg)
		}
		t.Fatalf("expected success=true, got error %+v", typed.Error)
	}
}
