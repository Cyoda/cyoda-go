package grpc

import (
	"strings"
	"testing"
	"time"

	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
)

// ---------------------------------------------------------------------------
// Temporal search filters (issue #423): creationDate chronological compare
// via LifecycleCondition, exercised through the gRPC EntitySearchCollection
// envelope. Mirrors internal/e2e/search_temporal_test.go's condition JSON
// shapes and the CLIENT_ERROR envelope convention already established by
// TestEntityStatsByStateGet_UnknownModel_ModelNotFound in search_test.go.
// ---------------------------------------------------------------------------

// lifecycleCondGRPC builds a {"type":"lifecycle",...} condition map for use
// as the "condition" field of a gRPC search request.
func lifecycleCondGRPC(field, operatorType string, value any) map[string]any {
	return map[string]any{
		"type":         "lifecycle",
		"field":        field,
		"operatorType": operatorType,
		"value":        value,
	}
}

func TestSearchTemporal_GRPC_CreationDate_GreaterThan_ChronologicalResult(t *testing.T) {
	svc, ctx := newTestEnv(t)
	const model = "grpc-search-temporal-cd-gt"
	importAndLockModel(t, svc, ctx, model, "1", map[string]any{"name": "A"})

	idA, err := svc.EntityManage(ctx, makeCE(EntityCreateRequest, map[string]any{
		"id": "gt-a", "dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": model, "version": 1},
			"data":  map[string]any{"name": "A"},
		},
	}))
	if err != nil {
		t.Fatalf("create A failed: %v", err)
	}
	_ = idA
	time.Sleep(50 * time.Millisecond)

	_, err = svc.EntityManage(ctx, makeCE(EntityCreateRequest, map[string]any{
		"id": "gt-b", "dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": model, "version": 1},
			"data":  map[string]any{"name": "B"},
		},
	}))
	if err != nil {
		t.Fatalf("create B failed: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	_, err = svc.EntityManage(ctx, makeCE(EntityCreateRequest, map[string]any{
		"id": "gt-c", "dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": model, "version": 1},
			"data":  map[string]any{"name": "C"},
		},
	}))
	if err != nil {
		t.Fatalf("create C failed: %v", err)
	}

	// Read back chronological order + creationDate strings via a
	// creationDate:asc sorted, match-all direct search.
	sortCE := makeCE(EntitySearchRequest, map[string]any{
		"id":    "gt-sort",
		"model": map[string]any{"name": model, "version": 1},
		"condition": map[string]any{
			"type": "group", "operator": "AND", "conditions": []any{},
		},
		"orderBy": []any{
			map[string]any{"path": "creationDate", "source": "meta"},
		},
	})
	sortStream := &mockEntityStream{ctx: ctx}
	if err := svc.EntitySearchCollection(sortCE, sortStream); err != nil {
		t.Fatalf("unexpected stream error: %v", err)
	}
	if len(sortStream.sent) != 3 {
		t.Fatalf("expected 3 results, got %d", len(sortStream.sent))
	}

	var names []string
	var creationDates []string
	for _, sent := range sortStream.sent {
		var typed events.EntityResponseJson
		validateResponse(t, sent, &typed)
		if !typed.Success {
			t.Fatalf("expected success=true; error: %v", typed.Error)
		}
		dataMap, ok := typed.Payload.Data.(map[string]interface{})
		if !ok {
			t.Fatalf("Payload.Data is not map[string]interface{}: %T", typed.Payload.Data)
		}
		name, _ := dataMap["name"].(string)
		names = append(names, name)

		metaMap, ok := typed.Payload.Meta.(map[string]interface{})
		if !ok {
			t.Fatalf("Payload.Meta is not map[string]interface{}: %T", typed.Payload.Meta)
		}
		cd, ok := metaMap["creationDate"].(string)
		if !ok || cd == "" {
			t.Fatalf("meta.creationDate missing or not a string: %v", metaMap)
		}
		creationDates = append(creationDates, cd)
	}
	if len(names) != 3 || names[0] != "A" || names[1] != "B" || names[2] != "C" {
		t.Fatalf("expected chronological order [A B C], got %v — entities not chronologically distinct?", names)
	}

	// GREATER_THAN A's creationDate must return exactly {B, C}.
	cond := lifecycleCondGRPC("creationDate", "GREATER_THAN", creationDates[0])
	ce := makeCE(EntitySearchRequest, map[string]any{
		"id":        "gt-search",
		"model":     map[string]any{"name": model, "version": 1},
		"condition": cond,
	})
	stream := &mockEntityStream{ctx: ctx}
	if err := svc.EntitySearchCollection(ce, stream); err != nil {
		t.Fatalf("unexpected stream error: %v", err)
	}
	if len(stream.sent) != 2 {
		t.Fatalf("expected 2 results for GREATER_THAN A's creationDate, got %d", len(stream.sent))
	}
	var gotNames []string
	for _, sent := range stream.sent {
		var typed events.EntityResponseJson
		validateResponse(t, sent, &typed)
		if !typed.Success {
			t.Fatalf("expected success=true; error: %v", typed.Error)
		}
		dataMap, ok := typed.Payload.Data.(map[string]interface{})
		if !ok {
			t.Fatalf("Payload.Data is not map[string]interface{}: %T", typed.Payload.Data)
		}
		name, _ := dataMap["name"].(string)
		gotNames = append(gotNames, name)
	}
	if !containsAll(gotNames, []string{"B", "C"}) {
		t.Errorf("expected result set {B, C}, got %v", gotNames)
	}

	// LESS_THAN A's creationDate must return nothing — proves the compare is
	// chronological (not merely "accepts the filter and returns everything").
	condNone := lifecycleCondGRPC("creationDate", "LESS_THAN", creationDates[0])
	ceNone := makeCE(EntitySearchRequest, map[string]any{
		"id":        "gt-search-none",
		"model":     map[string]any{"name": model, "version": 1},
		"condition": condNone,
	})
	streamNone := &mockEntityStream{ctx: ctx}
	if err := svc.EntitySearchCollection(ceNone, streamNone); err != nil {
		t.Fatalf("unexpected stream error: %v", err)
	}
	if len(streamNone.sent) != 0 {
		t.Fatalf("expected 0 results for LESS_THAN A's creationDate, got %d", len(streamNone.sent))
	}
}

func containsAll(got []string, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := make(map[string]int, len(got))
	for _, g := range got {
		seen[g]++
	}
	for _, w := range want {
		if seen[w] == 0 {
			return false
		}
		seen[w]--
	}
	return true
}

// TestSearchTemporal_GRPC_CreationDate_Equals_MixedPrecision proves EQUALS
// matches across mixed operand precision (a millisecond-truncated operand
// still matches the full-precision stored creationDate), exercising the
// epoch-ms flooring compare over the gRPC envelope.
func TestSearchTemporal_GRPC_CreationDate_Equals_MixedPrecision(t *testing.T) {
	svc, ctx := newTestEnv(t)
	const model = "grpc-search-temporal-cd-eq"
	importAndLockModel(t, svc, ctx, model, "1", map[string]any{"name": "A"})

	_, err := svc.EntityManage(ctx, makeCE(EntityCreateRequest, map[string]any{
		"id": "eq-a", "dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": model, "version": 1},
			"data":  map[string]any{"name": "A"},
		},
	}))
	if err != nil {
		t.Fatalf("create A failed: %v", err)
	}

	// Capture the exact stored creationDate.
	sortCE := makeCE(EntitySearchRequest, map[string]any{
		"id":    "eq-sort",
		"model": map[string]any{"name": model, "version": 1},
		"condition": map[string]any{
			"type": "group", "operator": "AND", "conditions": []any{},
		},
	})
	sortStream := &mockEntityStream{ctx: ctx}
	if err := svc.EntitySearchCollection(sortCE, sortStream); err != nil {
		t.Fatalf("unexpected stream error: %v", err)
	}
	if len(sortStream.sent) != 1 {
		t.Fatalf("expected 1 result, got %d", len(sortStream.sent))
	}
	var typed events.EntityResponseJson
	validateResponse(t, sortStream.sent[0], &typed)
	metaMap, ok := typed.Payload.Meta.(map[string]interface{})
	if !ok {
		t.Fatalf("Payload.Meta is not map[string]interface{}: %T", typed.Payload.Meta)
	}
	cd, ok := metaMap["creationDate"].(string)
	if !ok || cd == "" {
		t.Fatalf("meta.creationDate missing or not a string: %v", metaMap)
	}

	parsed, err := time.Parse(time.RFC3339Nano, cd)
	if err != nil {
		t.Fatalf("parse creationDate %q: %v", cd, err)
	}
	truncated := parsed.Truncate(time.Millisecond).Format("2006-01-02T15:04:05.000Z07:00")

	cond := lifecycleCondGRPC("creationDate", "EQUALS", truncated)
	ce := makeCE(EntitySearchRequest, map[string]any{
		"id":        "eq-search",
		"model":     map[string]any{"name": model, "version": 1},
		"condition": cond,
	})
	stream := &mockEntityStream{ctx: ctx}
	if err := svc.EntitySearchCollection(ce, stream); err != nil {
		t.Fatalf("unexpected stream error: %v", err)
	}
	if len(stream.sent) != 1 {
		t.Fatalf("expected 1 result for EQUALS mixed-precision, got %d", len(stream.sent))
	}
	var gotTyped events.EntityResponseJson
	validateResponse(t, stream.sent[0], &gotTyped)
	if !gotTyped.Success {
		t.Fatalf("expected success=true; error: %v", gotTyped.Error)
	}
}

// TestSearchTemporal_GRPC_StringOpOnTemporalField_ConditionTypeMismatch
// verifies that a string operator (CONTAINS) applied to the temporal
// creationDate field is rejected as a CLIENT_ERROR envelope with
// CONDITION_TYPE_MISMATCH in the message — not silently accepted or
// gRPC-transport-errored.
func TestSearchTemporal_GRPC_StringOpOnTemporalField_ConditionTypeMismatch(t *testing.T) {
	svc, ctx := newTestEnv(t)
	const model = "grpc-search-temporal-400-string-op"
	importAndLockModel(t, svc, ctx, model, "1", map[string]any{"name": "A"})

	_, err := svc.EntityManage(ctx, makeCE(EntityCreateRequest, map[string]any{
		"id": "so-a", "dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": model, "version": 1},
			"data":  map[string]any{"name": "A"},
		},
	}))
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	cond := lifecycleCondGRPC("creationDate", "CONTAINS", "2021")
	ce := makeCE(EntitySearchRequest, map[string]any{
		"id":        "so-search",
		"model":     map[string]any{"name": model, "version": 1},
		"condition": cond,
	})
	stream := &mockEntityStream{ctx: ctx}
	if err := svc.EntitySearchCollection(ce, stream); err != nil {
		t.Fatalf("unexpected stream-level error (errors should be envelope responses): %v", err)
	}
	if len(stream.sent) == 0 {
		t.Fatal("expected an error response on the stream, got empty stream")
	}
	var typed events.EntityResponseJson
	validateResponse(t, stream.sent[0], &typed)
	if typed.Success {
		t.Fatal("expected success=false for string op on temporal field")
	}
	if typed.Error == nil {
		t.Fatal("expected error block in response")
	}
	if typed.Error.Code != "CLIENT_ERROR" {
		t.Errorf("expected code=CLIENT_ERROR, got %q", typed.Error.Code)
	}
	if !strings.Contains(typed.Error.Message, "CONDITION_TYPE_MISMATCH") {
		t.Errorf("expected CONDITION_TYPE_MISMATCH in message, got %q", typed.Error.Message)
	}
}

// TestSearchTemporal_GRPC_BadOperand_ConditionTypeMismatch verifies that a
// non-parseable operand ("not-a-date") on a temporal field is rejected as a
// CLIENT_ERROR envelope with CONDITION_TYPE_MISMATCH in the message.
func TestSearchTemporal_GRPC_BadOperand_ConditionTypeMismatch(t *testing.T) {
	svc, ctx := newTestEnv(t)
	const model = "grpc-search-temporal-400-bad-operand"
	importAndLockModel(t, svc, ctx, model, "1", map[string]any{"name": "A"})

	_, err := svc.EntityManage(ctx, makeCE(EntityCreateRequest, map[string]any{
		"id": "bo-a", "dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": model, "version": 1},
			"data":  map[string]any{"name": "A"},
		},
	}))
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	cond := lifecycleCondGRPC("creationDate", "GREATER_THAN", "not-a-date")
	ce := makeCE(EntitySearchRequest, map[string]any{
		"id":        "bo-search",
		"model":     map[string]any{"name": model, "version": 1},
		"condition": cond,
	})
	stream := &mockEntityStream{ctx: ctx}
	if err := svc.EntitySearchCollection(ce, stream); err != nil {
		t.Fatalf("unexpected stream-level error (errors should be envelope responses): %v", err)
	}
	if len(stream.sent) == 0 {
		t.Fatal("expected an error response on the stream, got empty stream")
	}
	var typed events.EntityResponseJson
	validateResponse(t, stream.sent[0], &typed)
	if typed.Success {
		t.Fatal("expected success=false for bad temporal operand")
	}
	if typed.Error == nil {
		t.Fatal("expected error block in response")
	}
	if typed.Error.Code != "CLIENT_ERROR" {
		t.Errorf("expected code=CLIENT_ERROR, got %q", typed.Error.Code)
	}
	if !strings.Contains(typed.Error.Message, "CONDITION_TYPE_MISMATCH") {
		t.Errorf("expected CONDITION_TYPE_MISMATCH in message, got %q", typed.Error.Message)
	}
}

// TestSearchTemporal_GRPC_UnknownMetaField_InvalidFieldPath verifies that a
// lifecycle condition on an unknown meta field is rejected as a
// CLIENT_ERROR envelope with INVALID_FIELD_PATH in the message.
func TestSearchTemporal_GRPC_UnknownMetaField_InvalidFieldPath(t *testing.T) {
	svc, ctx := newTestEnv(t)
	const model = "grpc-search-temporal-400-unknown-field"
	importAndLockModel(t, svc, ctx, model, "1", map[string]any{"name": "A"})

	_, err := svc.EntityManage(ctx, makeCE(EntityCreateRequest, map[string]any{
		"id": "uf-a", "dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": model, "version": 1},
			"data":  map[string]any{"name": "A"},
		},
	}))
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	cond := lifecycleCondGRPC("bogusField", "EQUALS", "x")
	ce := makeCE(EntitySearchRequest, map[string]any{
		"id":        "uf-search",
		"model":     map[string]any{"name": model, "version": 1},
		"condition": cond,
	})
	stream := &mockEntityStream{ctx: ctx}
	if err := svc.EntitySearchCollection(ce, stream); err != nil {
		t.Fatalf("unexpected stream-level error (errors should be envelope responses): %v", err)
	}
	if len(stream.sent) == 0 {
		t.Fatal("expected an error response on the stream, got empty stream")
	}
	var typed events.EntityResponseJson
	validateResponse(t, stream.sent[0], &typed)
	if typed.Success {
		t.Fatal("expected success=false for unknown meta field")
	}
	if typed.Error == nil {
		t.Fatal("expected error block in response")
	}
	if typed.Error.Code != "CLIENT_ERROR" {
		t.Errorf("expected code=CLIENT_ERROR, got %q", typed.Error.Code)
	}
	if !strings.Contains(typed.Error.Message, "INVALID_FIELD_PATH") {
		t.Errorf("expected INVALID_FIELD_PATH in message, got %q", typed.Error.Message)
	}
}
