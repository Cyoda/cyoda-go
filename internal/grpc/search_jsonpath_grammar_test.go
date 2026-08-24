package grpc

import (
	"fmt"
	"strings"
	"testing"

	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
)

// grpcNonJSONPathSpellings mirrors the HTTP e2e table. gRPC is a separate
// entry point from HTTP and gets its own coverage per
// .claude/rules/test-coverage.md; both funnel through SearchService, and this
// is what proves the boundary sits in the service rather than in an HTTP
// handler.
var grpcNonJSONPathSpellings = []struct {
	name string
	path string
}{
	{"bare identifier", "amount"},
	{"leader only", "$."},
	{"bracket quoted", "$['amount']"},
	{"trailing dot", "$.amount."},
	{"empty segment", "$..amount"},
	// Malformed BRACKET spellings. The grammar used to stop scanning at the
	// first '[', so these classified as "valid but unpushdownable" and the
	// engine fell back to in-memory evaluation — which resolves none of them.
	// On this surface that is an empty stream, which a client reads as "no
	// matches" rather than "your path is malformed".
	{"unclosed subscript", "$.amount["},
	{"unmatched close", "$.amount]"},
	{"subscript without field", "$.[0]"},
	{"empty subscript", "$.amount[]"},
	{"negative index", "$.amount[-1]"},
	{"slice", "$.amount[0:2]"},
	{"double-quoted subscript", `$.amount["x"]`},
	{"sql tail after subscript", "$.amount[0];DROP"},
}

// TestEntitySearch_DirectSearch_NonJSONPathCondition_InvalidFieldPath pins the
// gRPC envelope for the streaming (direct) search surface: a jsonPath that is
// not JSON Path nomenclature must come back as an envelope error carrying
// INVALID_FIELD_PATH — not as a gRPC transport error, and above all not as an
// empty stream that a client reads as "no matches".
func TestEntitySearch_DirectSearch_NonJSONPathCondition_InvalidFieldPath(t *testing.T) {
	for _, tc := range grpcNonJSONPathSpellings {
		t.Run(tc.name, func(t *testing.T) {
			svc, ctx := newTestEnv(t)
			importAndLockModel(t, svc, ctx, "gadget", "1", map[string]any{"amount": 0})

			ce := makeCE(EntitySearchRequest, map[string]any{
				"id":    "search-badpath",
				"model": map[string]any{"name": "gadget", "version": 1},
				"condition": map[string]any{
					"type": "simple", "jsonPath": tc.path,
					"operatorType": "GREATER_THAN", "value": 50,
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
				t.Fatalf("jsonPath %q: expected success=false", tc.path)
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
		})
	}
}

// TestEntitySearch_SnapshotSearch_NonJSONPathCondition_InvalidFieldPath pins
// the same rejection on the async/snapshot surface, and additionally that no
// snapshot ID is issued — a rejected submit must not leave a job behind for a
// caller to poll.
func TestEntitySearch_SnapshotSearch_NonJSONPathCondition_InvalidFieldPath(t *testing.T) {
	for _, tc := range grpcNonJSONPathSpellings {
		t.Run(tc.name, func(t *testing.T) {
			svc, ctx := newTestEnv(t)
			importAndLockModel(t, svc, ctx, "gadget", "1", map[string]any{"amount": 0})

			ce := makeCE(EntitySnapshotSearchRequest, map[string]any{
				"id":    fmt.Sprintf("snap-badpath-%s", tc.name),
				"model": map[string]any{"name": "gadget", "version": 1},
				"condition": map[string]any{
					"type": "simple", "jsonPath": tc.path,
					"operatorType": "GREATER_THAN", "value": 50,
				},
			})
			resp, err := svc.EntitySearch(ctx, ce)
			if err != nil {
				t.Fatalf("unexpected transport error (a bad path must envelope-error): %v", err)
			}
			var typed events.EntitySnapshotSearchResponseJson
			validateResponse(t, resp, &typed)
			if typed.Success {
				t.Fatalf("jsonPath %q: expected success=false", tc.path)
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
			if typed.Status.SnapshotID != nilUUID {
				t.Errorf("expected nilUUID for failed submit, got %q", typed.Status.SnapshotID)
			}
		})
	}
}

// TestEntitySearch_DirectSearch_ArraySubscriptPath_NotRejected is the
// gRPC-side positive control: an array-subscripted path is valid JSON Path
// that no pushdown filter can express, and must still be served through the
// in-memory fallback rather than rejected alongside the malformed spellings
// above.
func TestEntitySearch_DirectSearch_ArraySubscriptPath_NotRejected(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "gadget", "1", map[string]any{"amount": 0, "tags": []any{"a"}})

	ce := makeCE(EntitySearchRequest, map[string]any{
		"id":    "search-subscript",
		"model": map[string]any{"name": "gadget", "version": 1},
		"condition": map[string]any{
			"type": "simple", "jsonPath": "$.tags[*]",
			"operatorType": "NOT_NULL", "value": nil,
		},
	})
	stream := &mockEntityStream{ctx: ctx}
	if err := svc.EntitySearchCollection(ce, stream); err != nil {
		t.Fatalf("unexpected stream-level error: %v", err)
	}
	for _, sent := range stream.sent {
		var typed events.EntityResponseJson
		validateResponse(t, sent, &typed)
		if !typed.Success && typed.Error != nil && strings.Contains(typed.Error.Message, "INVALID_FIELD_PATH") {
			t.Fatalf("array-subscript path was rejected as an invalid path: %q; it is valid JSON Path and must reach the in-memory fallback",
				typed.Error.Message)
		}
	}
}
