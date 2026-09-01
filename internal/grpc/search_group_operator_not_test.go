package grpc

import (
	"strings"
	"testing"

	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
)

// search_group_operator_not_test.go covers Task 12 of the NOT-node plan on
// the gRPC surface, mirroring search_unknown_operator_test.go's envelope
// assertions (Success, Error.Code=="CLIENT_ERROR", INVALID_CONDITION in the
// message) for the two GroupCondition{Operator:"NOT"} arity failures, plus
// the accepted case (exactly one condition) and the async/snapshot
// no-job-issued guarantee TestRPC_SnapshotSearch_MalformedRegex_400_InvalidCondition
// already pins for a malformed pattern.

func notGroup(conditions ...map[string]any) map[string]any {
	return map[string]any{
		"type":       "group",
		"operator":   "NOT",
		"conditions": conditions,
	}
}

func nameEqualsBob() map[string]any {
	return map[string]any{"type": "simple", "jsonPath": "$.name", "operatorType": "EQUALS", "value": "Bob"}
}

func amountGreaterThan1000() map[string]any {
	return map[string]any{"type": "simple", "jsonPath": "$.amount", "operatorType": "GREATER_THAN", "value": 1000}
}

// --- Direct search ---

// TestRPC_DirectSearch_GroupOperatorNOT_OneCondition_Accepted is the
// positive control: a NOT group with exactly one condition must be accepted
// rather than rejected as a malformed request.
func TestRPC_DirectSearch_GroupOperatorNOT_OneCondition_Accepted(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"name": "Bob"})

	ce := makeCE(EntitySearchRequest, map[string]any{
		"id":        "test",
		"model":     map[string]any{"name": "person", "version": 1},
		"condition": notGroup(nameEqualsBob()),
	})

	stream := &mockEntityStream{ctx: ctx}
	if err := svc.EntitySearchCollection(ce, stream); err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	for _, sent := range stream.sent {
		var typed events.EntityResponseJson
		validateResponse(t, sent, &typed)
		if !typed.Success {
			t.Fatalf("expected success=true for NOT with exactly one condition, got error: %v", typed.Error)
		}
	}
}

func TestRPC_DirectSearch_GroupOperatorNOT_ZeroConditions_400_InvalidCondition(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"name": "Bob"})

	ce := makeCE(EntitySearchRequest, map[string]any{
		"id":        "test",
		"model":     map[string]any{"name": "person", "version": 1},
		"condition": notGroup(),
	})

	stream := &mockEntityStream{ctx: ctx}
	if err := svc.EntitySearchCollection(ce, stream); err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if len(stream.sent) != 1 {
		t.Fatalf("expected exactly 1 response sent, got %d", len(stream.sent))
	}
	var typed events.EntityResponseJson
	validateResponse(t, stream.sent[0], &typed)
	if typed.Success {
		t.Error("expected success=false for NOT with zero conditions")
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

func TestRPC_DirectSearch_GroupOperatorNOT_TwoConditions_400_InvalidCondition(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"name": "Bob"})

	ce := makeCE(EntitySearchRequest, map[string]any{
		"id":        "test",
		"model":     map[string]any{"name": "person", "version": 1},
		"condition": notGroup(nameEqualsBob(), amountGreaterThan1000()),
	})

	stream := &mockEntityStream{ctx: ctx}
	if err := svc.EntitySearchCollection(ce, stream); err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if len(stream.sent) != 1 {
		t.Fatalf("expected exactly 1 response sent, got %d", len(stream.sent))
	}
	var typed events.EntityResponseJson
	validateResponse(t, stream.sent[0], &typed)
	if typed.Success {
		t.Error("expected success=false for NOT with two conditions")
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

// --- Snapshot (async) search: no job issued on rejection ---

// TestRPC_SnapshotSearch_GroupOperatorNOT_ZeroConditions_NoJobIssued mirrors
// TestRPC_SnapshotSearch_MalformedRegex_400_InvalidCondition's no-job-issued
// assertion for the NOT zero-arity rejection: SnapshotID must stay the nil
// UUID, not merely the response being non-success.
func TestRPC_SnapshotSearch_GroupOperatorNOT_ZeroConditions_NoJobIssued(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"name": "Bob"})

	ce := makeCE(EntitySnapshotSearchRequest, map[string]any{
		"id":        "test",
		"model":     map[string]any{"name": "person", "version": 1},
		"condition": notGroup(),
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
		t.Error("expected success=false for NOT with zero conditions")
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

// TestRPC_SnapshotSearch_GroupOperatorNOT_TwoConditions_NoJobIssued is the
// sibling arity failure for the same no-job-issued guarantee.
func TestRPC_SnapshotSearch_GroupOperatorNOT_TwoConditions_NoJobIssued(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"name": "Bob"})

	ce := makeCE(EntitySnapshotSearchRequest, map[string]any{
		"id":        "test",
		"model":     map[string]any{"name": "person", "version": 1},
		"condition": notGroup(nameEqualsBob(), amountGreaterThan1000()),
	})

	resp, err := svc.EntitySearch(ctx, ce)
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}

	var typed events.EntitySnapshotSearchResponseJson
	validateResponse(t, resp, &typed)
	if typed.Success {
		t.Error("expected success=false for NOT with two conditions")
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
