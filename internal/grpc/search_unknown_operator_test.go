package grpc

import (
	"strings"
	"testing"

	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
)

// TestRPC_DirectSearch_UnknownOperator_400_InvalidCondition verifies that
// the gRPC direct-search path rejects an unknown operatorType with the same
// classification HTTP now uses: Error.Code == "CLIENT_ERROR" (gRPC's
// generic 4xx wrapper) with INVALID_CONDITION embedded in the message.
// operator-semantics.md §4: "An operator name outside this set is 400
// INVALID_CONDITION, on every surface that carries a condition."
func TestRPC_DirectSearch_UnknownOperator_400_InvalidCondition(t *testing.T) {
	svc, ctx := newTestEnv(t)

	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"name": "Bob"})

	ce := makeCE(EntitySearchRequest, map[string]any{
		"id":    "test",
		"model": map[string]any{"name": "person", "version": 1},
		"condition": map[string]any{
			"type":         "simple",
			"jsonPath":     "$.name",
			"operatorType": "NOT_EQUALS",
			"value":        "Bob",
		},
	})

	stream := &mockEntityStream{ctx: ctx}
	err := svc.EntitySearchCollection(ce, stream)
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if len(stream.sent) != 1 {
		t.Fatalf("expected exactly 1 response sent, got %d", len(stream.sent))
	}
	if stream.sent[0].Type != EntityResponse {
		t.Errorf("expected type %s, got %s", EntityResponse, stream.sent[0].Type)
	}

	var typed events.EntityResponseJson
	validateResponse(t, stream.sent[0], &typed)
	if typed.Success {
		t.Error("expected success=false for unknown operatorType")
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
}
