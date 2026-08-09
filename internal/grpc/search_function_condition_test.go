package grpc

import (
	"strings"
	"testing"

	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
)

// gRPC is a separate entry point from HTTP and must reject a `function`
// clause the same way. It previously did not: the clause reached
// match.Prepare, and the raw error surfaced as a SERVER_ERROR envelope with a
// ticket while the gRPC status stayed OK — a 5xx for a client-supplied
// condition, over a transport where it is easy to miss.

func functionConditionCE() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":   "approval-check",
			"config": map[string]any{"calculationNodesTags": "approval-service"},
		},
	}
}

func TestRPC_DirectSearch_FunctionCondition_400_InvalidCondition(t *testing.T) {
	svc, ctx := newTestEnv(t)

	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"name": "Bob"})

	ce := makeCE(EntitySearchRequest, map[string]any{
		"id":        "test",
		"model":     map[string]any{"name": "person", "version": 1},
		"condition": functionConditionCE(),
	})

	stream := &mockEntityStream{ctx: ctx}
	if err := svc.EntitySearchCollection(ce, stream); err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if len(stream.sent) != 1 {
		t.Fatalf("expected exactly 1 response sent, got %d", len(stream.sent))
	}

	var typed events.EntityResponseJson
	validateResponse(t, stream.sent[0], &typed)
	if typed.Success {
		t.Error("expected success=false for a function condition")
	}
	if typed.Error == nil {
		t.Fatal("expected error in response")
	}
	if typed.Error.Code != "CLIENT_ERROR" {
		t.Errorf("expected envelope code CLIENT_ERROR, got %s", typed.Error.Code)
	}
	if !strings.Contains(typed.Error.Message, "INVALID_CONDITION") {
		t.Errorf("expected message to contain INVALID_CONDITION, got %s", typed.Error.Message)
	}
	// The 5xx this replaces carried a ticket UUID and no domain detail.
	if strings.Contains(typed.Error.Message, "SERVER_ERROR") {
		t.Errorf("a client-supplied condition must not surface as SERVER_ERROR: %s", typed.Error.Message)
	}
}

// The snapshot path is the gRPC twin of async submit: no job may be created
// for a condition that can never execute.
func TestRPC_SnapshotSearch_FunctionCondition_400_InvalidCondition(t *testing.T) {
	svc, ctx := newTestEnv(t)

	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"name": "Bob"})

	ce := makeCE(EntitySnapshotSearchRequest, map[string]any{
		"id":        "test",
		"model":     map[string]any{"name": "person", "version": 1},
		"condition": functionConditionCE(),
	})

	resp, err := svc.EntitySearch(ctx, ce)
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}

	var typed events.EntitySnapshotSearchResponseJson
	validateResponse(t, resp, &typed)
	if typed.Success {
		t.Error("expected success=false for a function condition")
	}
	if typed.Error == nil {
		t.Fatal("expected error in response")
	}
	if typed.Error.Code != "CLIENT_ERROR" {
		t.Errorf("expected envelope code CLIENT_ERROR, got %s", typed.Error.Code)
	}
	if !strings.Contains(typed.Error.Message, "INVALID_CONDITION") {
		t.Errorf("expected message to contain INVALID_CONDITION, got %s", typed.Error.Message)
	}
	if typed.Status.SnapshotID != nilUUID {
		t.Errorf("expected no snapshot job to be created, got snapshotId=%s", typed.Status.SnapshotID)
	}
}
