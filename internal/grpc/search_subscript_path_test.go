package grpc

import (
	"strings"
	"testing"

	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
)

// gRPC is a separate entry point from HTTP and gets its own coverage per
// .claude/rules/test-coverage.md. A path addressing one array element by
// position is valid JSON Path that no pushdown filter can express, so the
// in-memory evaluator is the only one that ever serves it — and it did not
// resolve it, so the stream came back empty for a field holding the value. An
// empty stream is exactly what a client reads as "no matches", which is why
// this needs an envelope-level assertion rather than a unit test.

// TestEntitySearch_DirectSearch_PositionalSubscriptPath_Resolves pins that a
// positional path selects the entity that holds the value at that position.
// The seeded entity's two elements differ, so a rewrite that addressed the
// wrong element inverts the expectation rather than merely weakening it.
func TestEntitySearch_DirectSearch_PositionalSubscriptPath_Resolves(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "gadget", "1", map[string]any{"arr": []any{0, 0}})

	if _, err := svc.EntityManage(ctx, makeCE(EntityCreateRequest, map[string]any{
		"id": "c-subscript-1", "dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": "gadget", "version": 1},
			"data":  map[string]any{"arr": []any{1, 100}},
		},
	})); err != nil {
		t.Fatalf("create failed: %v", err)
	}

	for _, tc := range []struct {
		name      string
		path      string
		value     any
		wantMatch bool
	}{
		{"element 0 holds 1", "$.arr[0]", 1, true},
		{"element 1 holds 100", "$.arr[1]", 100, true},
		{"element 0 does not hold 100", "$.arr[0]", 100, false},
		{"element 1 does not hold 1", "$.arr[1]", 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ce := makeCE(EntitySearchRequest, map[string]any{
				"id":    "s-subscript",
				"model": map[string]any{"name": "gadget", "version": 1},
				"condition": map[string]any{
					"type": "simple", "jsonPath": tc.path,
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
					t.Fatalf("jsonPath %q: expected success=true, got error %v", tc.path, typed.Error)
				}
			}
			if got := len(stream.sent) > 0; got != tc.wantMatch {
				t.Errorf("jsonPath %q EQUALS %v: matched=%v (%d results), want matched=%v",
					tc.path, tc.value, got, len(stream.sent), tc.wantMatch)
			}
		})
	}
}

// TestEntitySearch_DirectSearch_PositionalSubscriptPath_TypeMismatch pins the
// error class the fix newly makes reachable on this surface: the declared type
// of an array's element is recorded under the wildcard key, so a positional
// path now type-checks like its wildcard twin instead of slipping through as
// an unknown path and answering an empty stream.
func TestEntitySearch_DirectSearch_PositionalSubscriptPath_TypeMismatch(t *testing.T) {
	for _, path := range []string{"$.arr[*]", "$.arr[0]"} {
		t.Run(path, func(t *testing.T) {
			svc, ctx := newTestEnv(t)
			importAndLockModel(t, svc, ctx, "gadget", "1", map[string]any{"arr": []any{0, 0}})

			ce := makeCE(EntitySearchRequest, map[string]any{
				"id":    "s-subscript-mismatch",
				"model": map[string]any{"name": "gadget", "version": 1},
				"condition": map[string]any{
					"type": "simple", "jsonPath": path,
					"operatorType": "GREATER_THAN", "value": "not-a-number",
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
				t.Fatalf("jsonPath %q: expected success=false", path)
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
		})
	}
}
