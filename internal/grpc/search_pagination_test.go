package grpc

import (
	"math"
	"strings"
	"testing"

	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
)

// Regression tests for PR #149 follow-up: gRPC search/getAll handlers
// must apply the same pagination caps and overflow guard as the HTTP
// search handler. Validation must happen BEFORE the storage / job
// lookup — asserted by passing an unknown model or unknown snapshot ID
// alongside oversized params and confirming the response surfaces a
// CLIENT_ERROR with a pagination message rather than an internal /
// not-found.

// TestRPC_EntityGetAll_PageNumberExceedsCap_RejectedBeforeStorage —
// handleEntityGetAllRequest forwards req.PageNumber/PageSize to
// ListEntities, where `start := PageNumber * PageSize` panics with a
// slice-bounds error for attacker-supplied MaxInt32. The handler must
// reject the request first.
func TestRPC_EntityGetAll_PageNumberExceedsCap_RejectedBeforeStorage(t *testing.T) {
	svc, ctx := newTestEnv(t)

	ce := makeCE(EntityGetAllRequest, map[string]any{
		"id":         "test",
		"model":      map[string]any{"name": "person", "version": 1},
		"pageSize":   10,
		"pageNumber": math.MaxInt32, // far above MaxPageNumber=214748
	})

	stream := &mockEntityStream{ctx: ctx}
	if err := svc.EntitySearchCollection(ce, stream); err != nil {
		t.Fatalf("unexpected stream error (validation should produce a CloudEvent error response, not a stream error): %v", err)
	}
	requireClientError(t, stream.sent, "pagenumber")
}

// TestRPC_EntityGetAll_PageSizeExceedsCap_RejectedBeforeStorage —
// pageSize above MaxPageSize must surface as a CLIENT_ERROR.
func TestRPC_EntityGetAll_PageSizeExceedsCap_RejectedBeforeStorage(t *testing.T) {
	svc, ctx := newTestEnv(t)

	ce := makeCE(EntityGetAllRequest, map[string]any{
		"id":       "test",
		"model":    map[string]any{"name": "person", "version": 1},
		"pageSize": 100000, // above MaxPageSize=10000
	})

	stream := &mockEntityStream{ctx: ctx}
	if err := svc.EntitySearchCollection(ce, stream); err != nil {
		t.Fatalf("unexpected stream error: %v", err)
	}
	requireClientError(t, stream.sent, "pagesize")
}

// TestRPC_SnapshotGetResults_PageNumberOverflow_RejectedBeforeJobLookup —
// handleSnapshotGetRequestStreaming previously passed PageNumber/PageSize
// straight through to GetAsyncSearchResults with no cap. An attacker
// supplying MaxInt64 caused the same overflow class as PR #149 fixed in
// HTTP. The handler must validate before the snapshot lookup, surfacing a
// CLIENT_ERROR rather than an internal/not-found.
func TestRPC_SnapshotGetResults_PageNumberOverflow_RejectedBeforeJobLookup(t *testing.T) {
	svc, ctx := newTestEnv(t)

	ce := makeCE(SnapshotGetRequest, map[string]any{
		"id":         "test",
		"snapshotId": "00000000-0000-0000-0000-000000000000",
		"pageNumber": math.MaxInt32,
		"pageSize":   10,
	})

	stream := &mockEntityStream{ctx: ctx}
	if err := svc.EntitySearchCollection(ce, stream); err != nil {
		t.Fatalf("unexpected stream error: %v", err)
	}
	requireClientError(t, stream.sent, "pagenumber")
}

// requireClientError asserts the streamed response is a single
// EntityResponse with success=false and a CLIENT_ERROR error code whose
// message contains the expected substring (lowercase compare).
func requireClientError(t *testing.T, sent []*cepb.CloudEvent, msgFragment string) {
	t.Helper()
	if len(sent) == 0 {
		t.Fatalf("expected an error response on the stream, got 0 events")
	}
	var typed events.EntityResponseJson
	validateResponse(t, sent[0], &typed)
	if typed.Success {
		t.Fatalf("expected success=false, got success=true")
	}
	if typed.Error == nil {
		t.Fatalf("expected error block, got nil")
	}
	if typed.Error.Code != "CLIENT_ERROR" {
		t.Errorf("expected code=CLIENT_ERROR, got %q", typed.Error.Code)
	}
	if !strings.Contains(strings.ToLower(typed.Error.Message), msgFragment) {
		t.Errorf("expected error message to contain %q, got %q", msgFragment, typed.Error.Message)
	}
}
