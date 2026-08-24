package grpc

import (
	"strings"
	"testing"

	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
)

// gRPC is a separate entry point from HTTP and gets its own coverage per
// .claude/rules/test-coverage.md. A path whose last hop is an array wildcard
// ("$.tags[*]") addresses the array's ELEMENTS; the in-memory evaluator — the
// only evaluator that ever serves a subscripted path — resolved it to the
// array's COUNT, so the stream came back empty for an entity that holds the
// value. An empty stream is exactly what a client reads as "no matches", which
// is why this needs an envelope-level assertion rather than a unit test.

// TestEntitySearch_DirectSearch_TrailingWildcard_Resolves pins that a trailing
// wildcard selects the entity whose array contains the value, and only it.
func TestEntitySearch_DirectSearch_TrailingWildcard_Resolves(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "gadget", "1", map[string]any{
		"tags":  []any{"a"},
		"items": []any{map[string]any{"sku": "x"}},
	})

	if _, err := svc.EntityManage(ctx, makeCE(EntityCreateRequest, map[string]any{
		"id": "c-trailing-1", "dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": "gadget", "version": 1},
			"data": map[string]any{
				"tags":  []any{"red", "blue"},
				"items": []any{map[string]any{"sku": "A1"}},
			},
		},
	})); err != nil {
		t.Fatalf("create failed: %v", err)
	}

	for _, tc := range []struct {
		name      string
		value     any
		wantMatch bool
	}{
		{"first element", "red", true},
		{"second element", "blue", true},
		{"absent element", "purple", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ce := makeCE(EntitySearchRequest, map[string]any{
				"id":    "s-trailing",
				"model": map[string]any{"name": "gadget", "version": 1},
				"condition": map[string]any{
					"type": "simple", "jsonPath": "$.tags[*]",
					"operatorType": "EQUALS", "value": tc.value,
				},
			})
			stream := &mockEntityStream{ctx: ctx}
			if err := svc.EntitySearchCollection(ce, stream); err != nil {
				t.Fatalf("unexpected stream-level error: %v", err)
			}
			for _, sent := range stream.sent {
				var typed events.EntityResponseJson
				validateResponse(t, sent, &typed)
				if !typed.Success {
					t.Fatalf("expected success=true, got error %v", typed.Error)
				}
			}
			if got := len(stream.sent) > 0; got != tc.wantMatch {
				t.Errorf(`"$.tags[*]" EQUALS %v: matched=%v (%d results), want matched=%v`,
					tc.value, got, len(stream.sent), tc.wantMatch)
			}
		})
	}
}

// A trailing wildcard on an array of PURE objects names an element with
// substructure and no scalar form, so a scalar comparison on it could only ever
// be false. It must come back as an envelope error carrying INVALID_FIELD_PATH
// — not as an empty stream a client reads as "no matches".
func TestEntitySearch_DirectSearch_TrailingWildcard_ObjectArray_InvalidFieldPath(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "gadget", "1", map[string]any{
		"tags":  []any{"a"},
		"items": []any{map[string]any{"sku": "x"}},
	})

	ce := makeCE(EntitySearchRequest, map[string]any{
		"id":    "s-trailing-object",
		"model": map[string]any{"name": "gadget", "version": 1},
		"condition": map[string]any{
			"type": "simple", "jsonPath": "$.items[*]",
			"operatorType": "EQUALS", "value": "A1",
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
		t.Fatal(`"$.items[*]": expected success=false`)
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
