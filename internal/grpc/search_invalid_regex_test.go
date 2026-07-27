package grpc

import (
	"strings"
	"testing"

	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
)

// TestRPC_DirectSearch_MalformedRegex_400_InvalidCondition verifies that the
// gRPC direct-search path (EntitySearchCollection → handleDirectSearchRequest
// → SearchService.DirectSearch → Search) rejects a MATCHES_PATTERN condition
// carrying an unparsable regex ("(" — unterminated group) with the same
// domain-layer validation HTTP uses: the error envelope carries
// Error.Code == "CLIENT_ERROR" (gRPC's generic 4xx wrapper) with the domain
// code INVALID_CONDITION embedded in the message. This closes the gap left
// by Task 6's delegation to the error-free spi.MatchFilter for BOTH
// transports, not just HTTP.
func TestRPC_DirectSearch_MalformedRegex_400_InvalidCondition(t *testing.T) {
	svc, ctx := newTestEnv(t)

	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"name": "Bob"})

	ce := makeCE(EntitySearchRequest, map[string]any{
		"id":    "test",
		"model": map[string]any{"name": "person", "version": 1},
		"condition": map[string]any{
			"type":         "simple",
			"jsonPath":     "$.name",
			"operatorType": "MATCHES_PATTERN",
			"value":        "(",
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
		t.Error("expected success=false for malformed regex pattern")
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

// TestRPC_DirectSearch_ValidRegex_200 is the accept-side counterpart:
// a well-formed pattern must still search normally over gRPC (no
// accept/reject skew introduced by the new upstream validation).
func TestRPC_DirectSearch_ValidRegex_200(t *testing.T) {
	svc, ctx := newTestEnv(t)

	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"name": "Bob"})

	ce := makeCE(EntitySearchRequest, map[string]any{
		"id":    "test",
		"model": map[string]any{"name": "person", "version": 1},
		"condition": map[string]any{
			"type":         "simple",
			"jsonPath":     "$.name",
			"operatorType": "MATCHES_PATTERN",
			"value":        "^B.*$",
		},
	})

	stream := &mockEntityStream{ctx: ctx}
	err := svc.EntitySearchCollection(ce, stream)
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	// No entities were created, so the result set is empty, but the
	// request itself must be accepted rather than rejected as malformed.
	for _, sent := range stream.sent {
		var typed events.EntityResponseJson
		validateResponse(t, sent, &typed)
		if !typed.Success {
			t.Fatalf("expected success=true for valid pattern, got error: %v", typed.Error)
		}
	}
}

// TestRPC_SnapshotSearch_MalformedRegex_400_InvalidCondition mirrors the
// direct-search case for the async snapshot-submit path
// (EntitySearch → handleSnapshotSearchRequest → SubmitAsyncSearch →
// SubmitAsync): no snapshot job should ever be created for a malformed
// pattern.
func TestRPC_SnapshotSearch_MalformedRegex_400_InvalidCondition(t *testing.T) {
	svc, ctx := newTestEnv(t)

	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"name": "Bob"})

	ce := makeCE(EntitySnapshotSearchRequest, map[string]any{
		"id":    "test",
		"model": map[string]any{"name": "person", "version": 1},
		"condition": map[string]any{
			"type":         "simple",
			"jsonPath":     "$.name",
			"operatorType": "MATCHES_PATTERN",
			"value":        "(",
		},
	})

	resp, err := svc.EntitySearch(ctx, ce)
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if resp.Type != EntitySnapshotSearchResponse {
		t.Errorf("expected type %s, got %s", EntitySnapshotSearchResponse, resp.Type)
	}

	var typed events.EntitySnapshotSearchResponseJson
	validateResponse(t, resp, &typed)
	if typed.Success {
		t.Error("expected success=false for malformed regex pattern")
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
