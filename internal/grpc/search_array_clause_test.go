package grpc

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
)

func scoresEnv(t *testing.T) (*CloudEventsServiceImpl, context.Context) {
	t.Helper()
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "scored", "1", map[string]any{"name": "S", "scores": []any{1, 2, 3}})
	for i, scores := range [][]any{{90, 40, 75}, {10, 95, 60}} {
		if _, err := svc.EntityManage(ctx, makeCE(EntityCreateRequest, map[string]any{
			"id": fmt.Sprintf("c-scores-%d", i), "dataFormat": "JSON",
			"payload": map[string]any{
				"model": map[string]any{"name": "scored", "version": 1},
				"data":  map[string]any{"name": []string{"A", "B"}[i], "scores": scores},
			},
		})); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	return svc, ctx
}

func directSearchCE(cond map[string]any) *cepb.CloudEvent {
	return makeCE(EntitySearchRequest, map[string]any{
		"id": "s-array", "model": map[string]any{"name": "scored", "version": 1}, "condition": cond,
	})
}

func TestRPC_DirectSearch_ArrayClauseOperator(t *testing.T) {
	svc, ctx := scoresEnv(t)
	stream := &mockEntityStream{ctx: ctx}
	err := svc.EntitySearchCollection(directSearchCE(map[string]any{
		"type": "array", "jsonPath": "$.scores[*]", "operatorType": "GREATER_THAN", "values": []any{50, nil, 70},
	}), stream)
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if len(stream.sent) != 1 {
		t.Fatalf("want exactly A, got %d results", len(stream.sent))
	}
	var typed events.EntityResponseJson
	validateResponse(t, stream.sent[0], &typed)
	if !typed.Success || typed.Payload.Data.(map[string]any)["name"] != "A" {
		t.Errorf("want A, got %+v", typed)
	}
}

func TestRPC_DirectSearch_ArrayClauseRefusals(t *testing.T) {
	svc, ctx := scoresEnv(t)
	for _, tc := range []struct {
		name string
		cond map[string]any
		code string
	}{
		{"missing operator", map[string]any{"type": "array", "jsonPath": "$.scores[*]", "values": []any{50}}, "INVALID_CONDITION"},
		{"all null", map[string]any{"type": "array", "jsonPath": "$.scores[*]", "operatorType": "EQUALS", "values": []any{nil}}, "INVALID_CONDITION"},
		{"earlier wildcard", map[string]any{"type": "array", "jsonPath": "$.o[*].scores[*]", "operatorType": "EQUALS", "values": []any{1}}, "INVALID_FIELD_PATH"},
		{"text operator on numbers", map[string]any{"type": "array", "jsonPath": "$.scores[*]", "operatorType": "CONTAINS", "values": []any{"9"}}, "CONDITION_TYPE_MISMATCH"},
	} {
		stream := &mockEntityStream{ctx: ctx}
		if err := svc.EntitySearchCollection(directSearchCE(tc.cond), stream); err != nil {
			t.Fatalf("%s: transport error: %v", tc.name, err)
		}
		if len(stream.sent) != 1 {
			t.Fatalf("%s: want 1 error response, got %d", tc.name, len(stream.sent))
		}
		var typed events.EntityResponseJson
		validateResponse(t, stream.sent[0], &typed)
		if typed.Success || typed.Error == nil || typed.Error.Code != "CLIENT_ERROR" || !strings.Contains(typed.Error.Message, tc.code) {
			t.Errorf("%s: want CLIENT_ERROR naming %s, got %+v", tc.name, tc.code, typed.Error)
		}
	}
}

func TestRPC_DirectSearch_ArrayClauseValuesNotArray_InvalidArgument(t *testing.T) {
	svc, ctx := scoresEnv(t)
	err := svc.EntitySearchCollection(directSearchCE(map[string]any{
		"type": "array", "jsonPath": "$.scores[*]", "operatorType": "EQUALS", "values": "x",
	}), &mockEntityStream{ctx: ctx})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
}

func TestRPC_SnapshotSearch_ArrayClause(t *testing.T) {
	svc, ctx := scoresEnv(t)
	ce := func(cond map[string]any) *cepb.CloudEvent {
		return makeCE(EntitySnapshotSearchRequest, map[string]any{
			"id": "s-array-snap", "model": map[string]any{"name": "scored", "version": 1}, "condition": cond,
		})
	}
	resp, err := svc.EntitySearch(ctx, ce(map[string]any{
		"type": "array", "jsonPath": "$.scores[*]", "operatorType": "GREATER_THAN", "values": []any{50, nil, 70},
	}))
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	var ok events.EntitySnapshotSearchResponseJson
	validateResponse(t, resp, &ok)
	if !ok.Success || ok.Status.SnapshotID == nilUUID {
		t.Fatalf("want an accepted snapshot job, got %+v", ok)
	}

	resp, err = svc.EntitySearch(ctx, ce(map[string]any{
		"type": "array", "jsonPath": "$.scores[*]", "values": []any{50},
	}))
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	var bad events.EntitySnapshotSearchResponseJson
	validateResponse(t, resp, &bad)
	if bad.Success || bad.Error == nil || bad.Error.Code != "CLIENT_ERROR" || !strings.Contains(bad.Error.Message, "INVALID_CONDITION") {
		t.Errorf("missing operator: want CLIENT_ERROR naming INVALID_CONDITION, got %+v", bad.Error)
	}
	if bad.Status.SnapshotID != nilUUID {
		t.Errorf("refused condition must not create a job, got %s", bad.Status.SnapshotID)
	}
}
