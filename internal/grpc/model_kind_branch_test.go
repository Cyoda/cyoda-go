package grpc

import (
	"strings"
	"testing"

	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
)

// gRPC is a separate entry point from HTTP and gets its own coverage per
// .claude/rules/test-coverage.md. The extension door is reachable here through
// the same shared entity handler, so the level rules for adding a kind must
// answer identically over this transport.

// TestRPC_EntityCreate_AddingAKindFollowsTheLevel — below STRUCTURAL a write
// proposing a second kind for a declared path is refused, and the message
// names the level that resolves it; at STRUCTURAL it is accepted.
func TestRPC_EntityCreate_AddingAKindFollowsTheLevel(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "kind-branch-grpc", "1", map[string]any{"s": "x"})

	send := func(t *testing.T) *events.EntityTransactionResponseJson {
		t.Helper()
		ce := makeCE(EntityCreateRequest, map[string]any{
			"id":         "test",
			"dataFormat": "JSON",
			"payload": map[string]any{
				"model": map[string]any{"name": "kind-branch-grpc", "version": 1},
				"data":  map[string]any{"s": []any{"A"}},
			},
		})
		resp, err := svc.EntityManage(ctx, ce)
		if err != nil {
			t.Fatalf("unexpected transport error (this must be an envelope error): %v", err)
		}
		var typed events.EntityTransactionResponseJson
		validateResponse(t, resp, &typed)
		return &typed
	}

	if err := svc.modelHandler.SetChangeLevel(ctx, "kind-branch-grpc", "1", "TYPE"); err != nil {
		t.Fatalf("SetChangeLevel(TYPE): %v", err)
	}
	typed := send(t)
	if typed.Success {
		t.Error("expected success=false: adding a kind below STRUCTURAL is refused")
	}
	if typed.Error == nil || typed.Error.Code != "CLIENT_ERROR" {
		t.Fatalf("expected CLIENT_ERROR envelope, got %+v", typed.Error)
	}
	if !strings.Contains(typed.Error.Message, "VALIDATION_FAILED") {
		t.Errorf("expected VALIDATION_FAILED in message, got %s", typed.Error.Message)
	}
	if !strings.Contains(typed.Error.Message, "STRUCTURAL") {
		t.Errorf("message must name the level that resolves it, got %s", typed.Error.Message)
	}

	if err := svc.modelHandler.SetChangeLevel(ctx, "kind-branch-grpc", "1", "STRUCTURAL"); err != nil {
		t.Fatalf("SetChangeLevel(STRUCTURAL): %v", err)
	}
	typed = send(t)
	if !typed.Success {
		t.Errorf("expected success=true at STRUCTURAL, got error %+v", typed.Error)
	}
}

// TestRPC_EntityCreate_DeclaredBranchAcceptedAtType — once a path declares two
// kinds, a write of either is admissible at a level that permits no schema
// change of its own, because neither proposes one.
func TestRPC_EntityCreate_DeclaredBranchAcceptedAtType(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "kind-branch-declared-grpc", "1", map[string]any{"s": "x"})

	if err := svc.modelHandler.SetChangeLevel(ctx, "kind-branch-declared-grpc", "1", "STRUCTURAL"); err != nil {
		t.Fatalf("SetChangeLevel(STRUCTURAL): %v", err)
	}
	send := func(t *testing.T, data map[string]any) *events.EntityTransactionResponseJson {
		t.Helper()
		ce := makeCE(EntityCreateRequest, map[string]any{
			"id":         "test",
			"dataFormat": "JSON",
			"payload": map[string]any{
				"model": map[string]any{"name": "kind-branch-declared-grpc", "version": 1},
				"data":  data,
			},
		})
		resp, err := svc.EntityManage(ctx, ce)
		if err != nil {
			t.Fatalf("unexpected transport error: %v", err)
		}
		var typed events.EntityTransactionResponseJson
		validateResponse(t, resp, &typed)
		return &typed
	}

	// Establish the array branch alongside the scalar one.
	if typed := send(t, map[string]any{"s": []any{"A"}}); !typed.Success {
		t.Fatalf("precondition: STRUCTURAL write must add the kind, got %+v", typed.Error)
	}

	if err := svc.modelHandler.SetChangeLevel(ctx, "kind-branch-declared-grpc", "1", "TYPE"); err != nil {
		t.Fatalf("SetChangeLevel(TYPE): %v", err)
	}
	for _, data := range []map[string]any{
		{"s": "plain"},
		{"s": []any{"A", "B"}},
	} {
		if typed := send(t, data); !typed.Success {
			t.Errorf("%v: a declared branch must be accepted at TYPE, got %+v", data, typed.Error)
		}
	}
}
