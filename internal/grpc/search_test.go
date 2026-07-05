package grpc

import (
	"fmt"
	"strings"
	"testing"

	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
)

// TestEntityStatsByStateGet_UnknownModel_ModelNotFound verifies that
// EntityStatsByStateGetRequest for an unregistered model returns a CLIENT_ERROR
// envelope response with MODEL_NOT_FOUND in the message, not a zero-count success.
func TestEntityStatsByStateGet_UnknownModel_ModelNotFound(t *testing.T) {
	svc, ctx := newTestEnv(t)

	ce := makeCE(EntityStatsByStateGetRequest, map[string]any{
		"id":    "stats-state-unknown",
		"model": map[string]any{"name": "does-not-exist", "version": 1},
	})
	stream := &mockEntityStream{ctx: ctx}
	if err := svc.EntitySearchCollection(ce, stream); err != nil {
		t.Fatalf("unexpected stream-level error (errors should be envelope responses): %v", err)
	}
	if len(stream.sent) == 0 {
		t.Fatal("expected an error response on the stream, got empty stream")
	}
	var typed events.EntityStatsByStateResponseJson
	validateResponse(t, stream.sent[0], &typed)
	if typed.Success {
		t.Fatal("expected success=false for unknown model")
	}
	if typed.Error == nil {
		t.Fatal("expected error block in response")
	}
	if typed.Error.Code != "CLIENT_ERROR" {
		t.Errorf("expected code=CLIENT_ERROR, got %q", typed.Error.Code)
	}
	if !strings.Contains(typed.Error.Message, "MODEL_NOT_FOUND") {
		t.Errorf("expected MODEL_NOT_FOUND in message, got %q", typed.Error.Message)
	}
}

// TestEntityStatsGet_UnknownModel_ModelNotFound verifies that EntityStatsGetRequest
// for an unregistered model returns a CLIENT_ERROR envelope response with
// MODEL_NOT_FOUND in the message, not a zero-count success.
func TestEntityStatsGet_UnknownModel_ModelNotFound(t *testing.T) {
	svc, ctx := newTestEnv(t)

	ce := makeCE(EntityStatsGetRequest, map[string]any{
		"id":    "stats-unknown",
		"model": map[string]any{"name": "does-not-exist", "version": 1},
	})
	stream := &mockEntityStream{ctx: ctx}
	if err := svc.EntitySearchCollection(ce, stream); err != nil {
		t.Fatalf("unexpected stream-level error (errors should be envelope responses): %v", err)
	}
	if len(stream.sent) == 0 {
		t.Fatal("expected an error response on the stream, got empty stream")
	}
	var typed events.EntityStatsResponseJson
	validateResponse(t, stream.sent[0], &typed)
	if typed.Success {
		t.Fatal("expected success=false for unknown model")
	}
	if typed.Error == nil {
		t.Fatal("expected error block in response")
	}
	if typed.Error.Code != "CLIENT_ERROR" {
		t.Errorf("expected code=CLIENT_ERROR, got %q", typed.Error.Code)
	}
	if !strings.Contains(typed.Error.Message, "MODEL_NOT_FOUND") {
		t.Errorf("expected MODEL_NOT_FOUND in message, got %q", typed.Error.Message)
	}
}

// TestEntityGetAll_UnknownModel_ModelNotFound verifies that EntityGetAllRequest
// for an unregistered model returns a CLIENT_ERROR envelope response with
// MODEL_NOT_FOUND in the message, not an empty stream.
func TestEntityGetAll_UnknownModel_ModelNotFound(t *testing.T) {
	svc, ctx := newTestEnv(t)

	ce := makeCE(EntityGetAllRequest, map[string]any{
		"id":    "getall-unknown",
		"model": map[string]any{"name": "does-not-exist", "version": 1},
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
		t.Fatal("expected success=false for unknown model")
	}
	if typed.Error == nil {
		t.Fatal("expected error block in response")
	}
	if typed.Error.Code != "CLIENT_ERROR" {
		t.Errorf("expected code=CLIENT_ERROR, got %q", typed.Error.Code)
	}
	if !strings.Contains(typed.Error.Message, "MODEL_NOT_FOUND") {
		t.Errorf("expected MODEL_NOT_FOUND in message, got %q", typed.Error.Message)
	}
}

// TestEntitySearch_DirectSearch_OrderBy_SourceMeta verifies that a direct search
// with source:"meta" on the canonical meta field "creationDate" succeeds,
// exercising the SourceMeta mapping branch in handleDirectSearchRequest.
func TestEntitySearch_DirectSearch_OrderBy_SourceMeta(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"surname": "Smith"})

	_, err := svc.EntityManage(ctx, makeCE(EntityCreateRequest, map[string]any{
		"id": "c-meta-1", "dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": "person", "version": 1},
			"data":  map[string]any{"surname": "Smith"},
		},
	}))
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	ce := makeCE(EntitySearchRequest, map[string]any{
		"id":    "s-meta-1",
		"model": map[string]any{"name": "person", "version": 1},
		"condition": map[string]any{
			"type": "group", "operator": "AND", "conditions": []any{},
		},
		"orderBy": []any{
			map[string]any{"path": "creationDate", "source": "meta"},
		},
	})
	stream := &mockEntityStream{ctx: ctx}
	if err := svc.EntitySearchCollection(ce, stream); err != nil {
		t.Fatalf("unexpected stream error: %v", err)
	}
	if len(stream.sent) == 0 {
		t.Fatal("expected at least one response")
	}
	var typed events.EntityResponseJson
	validateResponse(t, stream.sent[0], &typed)
	if !typed.Success {
		t.Errorf("expected success=true with source:meta; error: %v", typed.Error)
	}
}

// TestEntitySearch_SnapshotSearch_OrderBy_SourceMeta verifies that an async
// snapshot search with source:"meta" on the canonical meta field "creationDate"
// is accepted and returns a snapshot ID, exercising the SourceMeta mapping
// branch in handleSnapshotSearchRequest.
func TestEntitySearch_SnapshotSearch_OrderBy_SourceMeta(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"surname": "Smith"})

	ce := makeCE(EntitySnapshotSearchRequest, map[string]any{
		"id":    "snap-meta-1",
		"model": map[string]any{"name": "person", "version": 1},
		"condition": map[string]any{
			"type": "group", "operator": "AND", "conditions": []any{},
		},
		"orderBy": []any{
			map[string]any{"path": "creationDate", "source": "meta"},
		},
	})
	resp, err := svc.EntitySearch(ctx, ce)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var typed events.EntitySnapshotSearchResponseJson
	validateResponse(t, resp, &typed)
	if !typed.Success {
		t.Fatalf("expected success=true with source:meta; error: %v", typed.Error)
	}
	if typed.Status.SnapshotID == "" {
		t.Error("expected non-empty snapshotId")
	}
}

// TestEntitySearch_DirectSearch_OrderBy_DescOrdering verifies that Path and
// Desc are wired end-to-end through the real search engine: two entities with
// distinct "tag" values are seeded and the streamed result order must reflect
// the requested direction (desc, then asc).
func TestEntitySearch_DirectSearch_OrderBy_DescOrdering(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "item", "1", map[string]any{"tag": "x"})

	for _, tag := range []string{"aaa", "zzz"} {
		_, err := svc.EntityManage(ctx, makeCE(EntityCreateRequest, map[string]any{
			"id": "c-ord-" + tag, "dataFormat": "JSON",
			"payload": map[string]any{
				"model": map[string]any{"name": "item", "version": 1},
				"data":  map[string]any{"tag": tag},
			},
		}))
		if err != nil {
			t.Fatalf("create %q failed: %v", tag, err)
		}
	}

	getTagOrder := func(t *testing.T, desc bool) []string {
		t.Helper()
		ce := makeCE(EntitySearchRequest, map[string]any{
			"id":    "s-ord-desc",
			"model": map[string]any{"name": "item", "version": 1},
			"condition": map[string]any{
				"type": "group", "operator": "AND", "conditions": []any{},
			},
			"orderBy": []any{
				map[string]any{"path": "tag", "desc": desc},
			},
		})
		stream := &mockEntityStream{ctx: ctx}
		if err := svc.EntitySearchCollection(ce, stream); err != nil {
			t.Fatalf("unexpected stream error: %v", err)
		}
		if len(stream.sent) != 2 {
			t.Fatalf("expected 2 results, got %d", len(stream.sent))
		}
		var tags []string
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
			tag, ok := dataMap["tag"].(string)
			if !ok {
				t.Fatalf("tag field is not a string: %T", dataMap["tag"])
			}
			tags = append(tags, tag)
		}
		return tags
	}

	// desc:true — lexicographically larger value must come first.
	if got := getTagOrder(t, true); len(got) == 2 && (got[0] != "zzz" || got[1] != "aaa") {
		t.Errorf("desc order: expected [zzz aaa], got %v", got)
	}

	// desc:false — lexicographically smaller value must come first.
	if got := getTagOrder(t, false); len(got) == 2 && (got[0] != "aaa" || got[1] != "zzz") {
		t.Errorf("asc order: expected [aaa zzz], got %v", got)
	}
}

// TestEntitySearch_DirectSearch_OrderBy_ValidField verifies that a direct
// search with a valid orderBy path resolves successfully.
func TestEntitySearch_DirectSearch_OrderBy_ValidField(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"surname": "Smith"})

	// Create an entity so search has something to return.
	_, err := svc.EntityManage(ctx, makeCE(EntityCreateRequest, map[string]any{
		"id": "c1", "dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": "person", "version": 1},
			"data":  map[string]any{"surname": "Smith"},
		},
	}))
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	ce := makeCE(EntitySearchRequest, map[string]any{
		"id":    "s1",
		"model": map[string]any{"name": "person", "version": 1},
		"condition": map[string]any{
			"type": "group", "operator": "AND", "conditions": []any{},
		},
		"orderBy": []any{
			map[string]any{"path": "surname", "desc": true},
		},
	})
	stream := &mockEntityStream{ctx: ctx}
	if err := svc.EntitySearchCollection(ce, stream); err != nil {
		t.Fatalf("unexpected stream error: %v", err)
	}
	if len(stream.sent) == 0 {
		t.Fatal("expected at least one response")
	}
	var typed events.EntityResponseJson
	validateResponse(t, stream.sent[0], &typed)
	if !typed.Success {
		t.Errorf("expected success=true; error: %v", typed.Error)
	}
}

// TestEntitySearch_DirectSearch_OrderBy_InvalidField verifies that a direct
// search with an unknown sort path returns CLIENT_ERROR / INVALID_FIELD_PATH.
func TestEntitySearch_DirectSearch_OrderBy_InvalidField(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"surname": "Smith"})

	ce := makeCE(EntitySearchRequest, map[string]any{
		"id":    "s2",
		"model": map[string]any{"name": "person", "version": 1},
		"condition": map[string]any{
			"type": "group", "operator": "AND", "conditions": []any{},
		},
		"orderBy": []any{
			map[string]any{"path": "nope"},
		},
	})
	stream := &mockEntityStream{ctx: ctx}
	if err := svc.EntitySearchCollection(ce, stream); err != nil {
		t.Fatalf("unexpected stream error (errors should be envelope responses): %v", err)
	}
	if len(stream.sent) == 0 {
		t.Fatal("expected an error response on the stream")
	}
	var typed events.EntityResponseJson
	validateResponse(t, stream.sent[0], &typed)
	if typed.Success {
		t.Fatal("expected success=false for unknown sort field")
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

// TestEntitySearch_SnapshotSearch_OrderBy_ValidField verifies that an async
// snapshot search with a valid orderBy is accepted and returns a snapshot ID.
func TestEntitySearch_SnapshotSearch_OrderBy_ValidField(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"surname": "Smith"})

	ce := makeCE(EntitySnapshotSearchRequest, map[string]any{
		"id":    "snap1",
		"model": map[string]any{"name": "person", "version": 1},
		"condition": map[string]any{
			"type": "group", "operator": "AND", "conditions": []any{},
		},
		"orderBy": []any{
			map[string]any{"path": "surname", "desc": true},
		},
	})
	resp, err := svc.EntitySearch(ctx, ce)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var typed events.EntitySnapshotSearchResponseJson
	validateResponse(t, resp, &typed)
	if !typed.Success {
		t.Fatalf("expected success=true; error: %v", typed.Error)
	}
	if typed.Status.SnapshotID == "" {
		t.Error("expected non-empty snapshotId")
	}
}

// TestEntitySearch_DirectSearch_OrderBy_ExceedsCap verifies that a direct
// search with more than 16 sort keys (the default cap) is rejected with
// CLIENT_ERROR / INVALID_FIELD_PATH via the gRPC envelope. The model is
// seeded with 17 scalar fields so all sort keys are otherwise valid — the
// only reason for rejection must be the cap.
func TestEntitySearch_DirectSearch_OrderBy_ExceedsCap(t *testing.T) {
	svc, ctx := newTestEnv(t)

	// Build a model with 17 scalar string fields (field0 … field16) so that
	// every sort key in the request is schema-valid. The cap (16) is the
	// only reason the request should be rejected.
	sampleData := make(map[string]any, 17)
	for i := 0; i < 17; i++ {
		sampleData[fmt.Sprintf("field%d", i)] = "value"
	}
	importAndLockModel(t, svc, ctx, "widget", "1", sampleData)

	// 17 sort keys — one beyond the default cap of 16.
	orderBy := make([]any, 17)
	for i := range orderBy {
		orderBy[i] = map[string]any{"path": fmt.Sprintf("field%d", i)}
	}

	ce := makeCE(EntitySearchRequest, map[string]any{
		"id":    "s-cap-1",
		"model": map[string]any{"name": "widget", "version": 1},
		"condition": map[string]any{
			"type": "group", "operator": "AND", "conditions": []any{},
		},
		"orderBy": orderBy,
	})
	stream := &mockEntityStream{ctx: ctx}
	if err := svc.EntitySearchCollection(ce, stream); err != nil {
		t.Fatalf("unexpected stream error: %v", err)
	}
	if len(stream.sent) == 0 {
		t.Fatal("expected at least one response")
	}
	var typed events.EntityResponseJson
	validateResponse(t, stream.sent[0], &typed)
	if typed.Success {
		t.Fatal("expected success=false for exceeding sort key cap")
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

// TestEntitySearch_SnapshotSearch_OrderBy_ExceedsCap verifies that an async
// snapshot search with more than 16 sort keys (the default cap) is rejected
// synchronously at submit, surfaces CLIENT_ERROR / INVALID_FIELD_PATH, and
// issues no snapshot ID. The model is seeded with 17 scalar fields so all
// sort keys are otherwise valid — the only reason for rejection must be the cap.
func TestEntitySearch_SnapshotSearch_OrderBy_ExceedsCap(t *testing.T) {
	svc, ctx := newTestEnv(t)

	// Build a model with 17 scalar string fields (field0 … field16) so that
	// every sort key in the request is schema-valid. The cap (16) is the
	// only reason the request should be rejected.
	sampleData := make(map[string]any, 17)
	for i := 0; i < 17; i++ {
		sampleData[fmt.Sprintf("field%d", i)] = "value"
	}
	importAndLockModel(t, svc, ctx, "gadget", "1", sampleData)

	// 17 sort keys — one beyond the default cap of 16.
	orderBy := make([]any, 17)
	for i := range orderBy {
		orderBy[i] = map[string]any{"path": fmt.Sprintf("field%d", i)}
	}

	ce := makeCE(EntitySnapshotSearchRequest, map[string]any{
		"id":    "snap-cap-1",
		"model": map[string]any{"name": "gadget", "version": 1},
		"condition": map[string]any{
			"type": "group", "operator": "AND", "conditions": []any{},
		},
		"orderBy": orderBy,
	})
	resp, err := svc.EntitySearch(ctx, ce)
	if err != nil {
		t.Fatalf("unexpected transport error (bad sort must envelope-error, not gRPC-error): %v", err)
	}
	var typed events.EntitySnapshotSearchResponseJson
	validateResponse(t, resp, &typed)
	if typed.Success {
		t.Fatal("expected success=false for exceeding sort key cap")
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
	// No snapshot ID must be issued when submit fails.
	if typed.Status.SnapshotID != nilUUID {
		t.Errorf("expected nilUUID for failed submit, got %q", typed.Status.SnapshotID)
	}
}

// TestEntitySearch_DirectSearch_UnknownModel_ModelNotFound verifies that
// EntitySearchRequest for an unregistered model returns a CLIENT_ERROR
// envelope response with MODEL_NOT_FOUND in the message, not an empty
// stream.
func TestEntitySearch_DirectSearch_UnknownModel_ModelNotFound(t *testing.T) {
	svc, ctx := newTestEnv(t)

	ce := makeCE(EntitySearchRequest, map[string]any{
		"id":    "search-unknown",
		"model": map[string]any{"name": "does-not-exist", "version": 1},
		"condition": map[string]any{
			"type": "group", "operator": "AND", "conditions": []any{},
		},
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
		t.Fatal("expected success=false for unknown model")
	}
	if typed.Error == nil {
		t.Fatal("expected error block in response")
	}
	if typed.Error.Code != "CLIENT_ERROR" {
		t.Errorf("expected code=CLIENT_ERROR, got %q", typed.Error.Code)
	}
	if !strings.Contains(typed.Error.Message, "MODEL_NOT_FOUND") {
		t.Errorf("expected MODEL_NOT_FOUND in message, got %q", typed.Error.Message)
	}
}

// TestEntitySearch_SnapshotSearch_UnknownModel_ModelNotFound verifies that
// EntitySnapshotSearchRequest for an unregistered model returns a CLIENT_ERROR
// envelope response with MODEL_NOT_FOUND in the message at submit time,
// and issues no snapshot ID.
func TestEntitySearch_SnapshotSearch_UnknownModel_ModelNotFound(t *testing.T) {
	svc, ctx := newTestEnv(t)

	ce := makeCE(EntitySnapshotSearchRequest, map[string]any{
		"id":    "snap-unknown",
		"model": map[string]any{"name": "does-not-exist", "version": 1},
		"condition": map[string]any{
			"type": "group", "operator": "AND", "conditions": []any{},
		},
	})
	resp, err := svc.EntitySearch(ctx, ce)
	if err != nil {
		t.Fatalf("unexpected transport error (errors should be envelope responses): %v", err)
	}
	var typed events.EntitySnapshotSearchResponseJson
	validateResponse(t, resp, &typed)
	if typed.Success {
		t.Fatal("expected success=false for unknown model")
	}
	if typed.Error == nil {
		t.Fatal("expected error block in response")
	}
	if typed.Error.Code != "CLIENT_ERROR" {
		t.Errorf("expected code=CLIENT_ERROR, got %q", typed.Error.Code)
	}
	if !strings.Contains(typed.Error.Message, "MODEL_NOT_FOUND") {
		t.Errorf("expected MODEL_NOT_FOUND in message, got %q", typed.Error.Message)
	}
	// No snapshot ID must be issued when submit fails.
	if typed.Status.SnapshotID != nilUUID {
		t.Errorf("expected nilUUID for failed submit, got %q", typed.Status.SnapshotID)
	}
}

// TestEntitySearch_SnapshotSearch_OrderBy_InvalidField verifies that an async
// snapshot search with an unknown sort path is rejected synchronously at submit,
// surfaces CLIENT_ERROR / INVALID_FIELD_PATH, and issues no snapshot ID.
func TestEntitySearch_SnapshotSearch_OrderBy_InvalidField(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"surname": "Smith"})

	ce := makeCE(EntitySnapshotSearchRequest, map[string]any{
		"id":    "snap2",
		"model": map[string]any{"name": "person", "version": 1},
		"condition": map[string]any{
			"type": "group", "operator": "AND", "conditions": []any{},
		},
		"orderBy": []any{
			map[string]any{"path": "nope"},
		},
	})
	resp, err := svc.EntitySearch(ctx, ce)
	if err != nil {
		t.Fatalf("unexpected transport error (bad sort must envelope-error, not gRPC-error): %v", err)
	}
	var typed events.EntitySnapshotSearchResponseJson
	validateResponse(t, resp, &typed)
	if typed.Success {
		t.Fatal("expected success=false for unknown sort field")
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
	// No snapshot ID must be issued when submit fails.
	if typed.Status.SnapshotID != nilUUID {
		t.Errorf("expected nilUUID for failed submit, got %q", typed.Status.SnapshotID)
	}
}
