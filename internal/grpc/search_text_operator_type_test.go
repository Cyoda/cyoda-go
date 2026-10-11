package grpc

import (
	"strings"
	"testing"

	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
)

// A string or pattern operator on a field with no text type can never match a
// stored value. Both gRPC search entry points refuse it with a CLIENT_ERROR
// envelope naming CONDITION_TYPE_MISMATCH, instead of an empty stream.

func textOperatorOnNumericCondition() map[string]any {
	return map[string]any{
		"type": "simple", "jsonPath": "$.price",
		"operatorType": "ICONTAINS", "value": "1",
	}
}

func TestRPC_DirectSearch_TextOperatorOnNumericField_ConditionTypeMismatch(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "priced", "1", map[string]any{"price": 10.5})

	ce := makeCE(EntitySearchRequest, map[string]any{
		"id":        "s-textop",
		"model":     map[string]any{"name": "priced", "version": 1},
		"condition": textOperatorOnNumericCondition(),
	})
	stream := &mockEntityStream{ctx: ctx}
	if err := svc.EntitySearchCollection(ce, stream); err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if len(stream.sent) != 1 {
		t.Fatalf("expected exactly 1 error response, got %d", len(stream.sent))
	}
	var typed events.EntityResponseJson
	validateResponse(t, stream.sent[0], &typed)
	if typed.Success || typed.Error == nil {
		t.Fatalf("expected an error envelope, got success=%v error=%v", typed.Success, typed.Error)
	}
	if typed.Error.Code != "CLIENT_ERROR" {
		t.Errorf("expected envelope code CLIENT_ERROR, got %s", typed.Error.Code)
	}
	if !strings.Contains(typed.Error.Message, "CONDITION_TYPE_MISMATCH") {
		t.Errorf("expected message to contain CONDITION_TYPE_MISMATCH, got %s", typed.Error.Message)
	}
}

func TestRPC_SnapshotSearch_TextOperatorOnNumericField_ConditionTypeMismatch(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "priced", "1", map[string]any{"price": 10.5})

	ce := makeCE(EntitySnapshotSearchRequest, map[string]any{
		"id":        "s-textop-snap",
		"model":     map[string]any{"name": "priced", "version": 1},
		"condition": textOperatorOnNumericCondition(),
	})
	resp, err := svc.EntitySearch(ctx, ce)
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	var typed events.EntitySnapshotSearchResponseJson
	validateResponse(t, resp, &typed)
	if typed.Success || typed.Error == nil {
		t.Fatalf("expected an error envelope, got success=%v error=%v", typed.Success, typed.Error)
	}
	if typed.Error.Code != "CLIENT_ERROR" {
		t.Errorf("expected envelope code CLIENT_ERROR, got %s", typed.Error.Code)
	}
	if !strings.Contains(typed.Error.Message, "CONDITION_TYPE_MISMATCH") {
		t.Errorf("expected message to contain CONDITION_TYPE_MISMATCH, got %s", typed.Error.Message)
	}
	if typed.Status.SnapshotID != nilUUID {
		t.Errorf("expected no snapshot job, got snapshotId=%s", typed.Status.SnapshotID)
	}
}

func TestRPC_DirectSearch_OrderingOperatorOnBooleanField_ConditionTypeMismatch(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "flagged", "1", map[string]any{"active": true})

	ce := makeCE(EntitySearchRequest, map[string]any{
		"id":    "s-ordop",
		"model": map[string]any{"name": "flagged", "version": 1},
		"condition": map[string]any{
			"type": "simple", "jsonPath": "$.active",
			"operatorType": "GREATER_THAN", "value": "false",
		},
	})
	stream := &mockEntityStream{ctx: ctx}
	if err := svc.EntitySearchCollection(ce, stream); err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if len(stream.sent) != 1 {
		t.Fatalf("expected exactly 1 error response, got %d", len(stream.sent))
	}
	var typed events.EntityResponseJson
	validateResponse(t, stream.sent[0], &typed)
	if typed.Success || typed.Error == nil {
		t.Fatalf("expected an error envelope, got success=%v error=%v", typed.Success, typed.Error)
	}
	if typed.Error.Code != "CLIENT_ERROR" {
		t.Errorf("expected envelope code CLIENT_ERROR, got %s", typed.Error.Code)
	}
	if !strings.Contains(typed.Error.Message, "CONDITION_TYPE_MISMATCH") {
		t.Errorf("expected message to contain CONDITION_TYPE_MISMATCH, got %s", typed.Error.Message)
	}
}
