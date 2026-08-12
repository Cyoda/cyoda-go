package grpc

import (
	"context"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// Task 16: gRPC direct search — honor timeoutMillis on EntitySearchRequest.
// See .superpowers/sdd/2026-08-11-transaction-control-params-plan/task-16-brief.md.
//
// handleDirectSearchRequest (internal/grpc/search.go) reuses resolveEventTimeout
// (T14) to validate/reject/attach the deadline from req.TimeoutMillis, then
// classifies an expired-ours deadline as common.ErrCodeSearchTimeout before
// building the entityResponseError envelope this handler already uses for
// every other failure on this stream.

// --- (a) timeoutMillis: 0 -> BAD_REQUEST: envelope ---

// TestEntitySearch_DirectSearch_TimeoutMillisZero_BadRequest pins that a
// non-positive timeoutMillis is rejected before the search ever reaches the
// service layer, mirroring the write-ops' transactionTimeoutMs validation
// (common.ValidateRequestTimeoutMillis) and the HTTP door's timeoutMillis=0
// case (TestSearchEntities_TimeoutMillisZero_Returns400).
func TestEntitySearch_DirectSearch_TimeoutMillisZero_BadRequest(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"name": "Alice"})

	zero := 0
	ce := makeCE(EntitySearchRequest, map[string]any{
		"id":            "s-timeout-zero",
		"model":         map[string]any{"name": "person", "version": 1},
		"condition":     map[string]any{"type": "group", "operator": "AND", "conditions": []any{}},
		"timeoutMillis": zero,
	})
	stream := &mockEntityStream{ctx: ctx}
	if err := svc.EntitySearchCollection(ce, stream); err != nil {
		t.Fatalf("unexpected stream-level error (errors should be envelope responses): %v", err)
	}
	if len(stream.sent) != 1 {
		t.Fatalf("expected exactly 1 response, got %d", len(stream.sent))
	}
	var typed events.EntityResponseJson
	validateResponse(t, stream.sent[0], &typed)
	if typed.Success {
		t.Fatal("expected success=false for timeoutMillis=0")
	}
	if typed.Error == nil {
		t.Fatal("expected error block in response")
	}
	if typed.Error.Code != "CLIENT_ERROR" {
		t.Errorf("expected code=CLIENT_ERROR, got %q", typed.Error.Code)
	}
	if !strings.HasPrefix(typed.Error.Message, common.ErrCodeBadRequest+":") {
		t.Errorf("message = %q, want prefix %q", typed.Error.Message, common.ErrCodeBadRequest+":")
	}
}

// --- (b) tx-token'd (joined) request + field set -> same BAD_REQUEST rejection ---

// TestEntitySearch_DirectSearch_TimeoutMillis_JoinedRejected pins that a
// request carrying a joined transaction (spi.GetTransaction(ctx) != nil — how
// a routed compute-node callback presents at param-resolution time) is
// rejected with CLIENT_ERROR/BAD_REQUEST rather than silently honoring or
// silently ignoring the client-supplied deadline.
func TestEntitySearch_DirectSearch_TimeoutMillis_JoinedRejected(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"name": "Alice"})
	joinedCtx := spi.WithTransaction(ctx, &spi.TransactionState{ID: "tx-1"})

	millis := 5000
	ce := makeCE(EntitySearchRequest, map[string]any{
		"id":            "s-timeout-joined",
		"model":         map[string]any{"name": "person", "version": 1},
		"condition":     map[string]any{"type": "group", "operator": "AND", "conditions": []any{}},
		"timeoutMillis": millis,
	})
	stream := &mockEntityStream{ctx: joinedCtx}
	if err := svc.EntitySearchCollection(ce, stream); err != nil {
		t.Fatalf("unexpected stream-level error (errors should be envelope responses): %v", err)
	}
	if len(stream.sent) != 1 {
		t.Fatalf("expected exactly 1 response, got %d", len(stream.sent))
	}
	var typed events.EntityResponseJson
	validateResponse(t, stream.sent[0], &typed)
	if typed.Success {
		t.Fatal("expected success=false for a joined-transaction request")
	}
	if typed.Error == nil {
		t.Fatal("expected error block in response")
	}
	if typed.Error.Code != "CLIENT_ERROR" {
		t.Errorf("expected code=CLIENT_ERROR, got %q", typed.Error.Code)
	}
	if !strings.HasPrefix(typed.Error.Message, common.ErrCodeBadRequest+":") {
		t.Errorf("message = %q, want prefix %q", typed.Error.Message, common.ErrCodeBadRequest+":")
	}
	if !strings.Contains(typed.Error.Message, "joins an open transaction") {
		t.Errorf("message = %q, want mention of the joined transaction", typed.Error.Message)
	}
	// Names its own field, mirroring the HTTP door's parity assertion
	// (TestSearchEntities_TimeoutMillis_JoinedTxRejected400): resolveEventTimeout
	// is shared with the 5 write events whose field is transactionTimeoutMs,
	// but this event's field is timeoutMillis — the message must say so, not
	// borrow the write-events' field name.
	if !strings.Contains(typed.Error.Message, "timeoutMillis") {
		t.Errorf("message = %q, want mention of the field name timeoutMillis", typed.Error.Message)
	}
}

// --- (c) blocking search + timeoutMillis: 1 -> SEARCH_TIMEOUT:, Retryable, no elements ---

// searchTimeoutEntityStore blocks Search until ctx is Done and returns
// ctx.Err() — deterministic by construction, never races a wall-clock sleep
// against the deadline. Mirrors internal/domain/search's handler_timeout_test.go
// searcherEntityStore fake and internal/grpc's own blockingEntityStore
// (entity_timeout_test.go), adapted to the Searcher interface DirectSearch uses.
type searchTimeoutEntityStore struct {
	spi.EntityStore
	searchCalls int
}

func (s *searchTimeoutEntityStore) Search(ctx context.Context, _ spi.Filter, _ spi.SearchOptions) ([]*spi.Entity, error) {
	s.searchCalls++
	<-ctx.Done()
	return nil, ctx.Err()
}

// searchTimeoutStoreFactory wraps a real spi.StoreFactory but always hands
// out the given searchTimeoutEntityStore from EntityStore(); every other
// accessor delegates to the wrapped factory.
type searchTimeoutStoreFactory struct {
	spi.StoreFactory
	entityStore *searchTimeoutEntityStore
}

func (f *searchTimeoutStoreFactory) EntityStore(_ context.Context) (spi.EntityStore, error) {
	return f.entityStore, nil
}

// TestEntitySearch_DirectSearch_TimeoutExpires_SearchTimeoutNoElements pins
// the D2/D8 contract end to end through the gRPC door: a timeoutMillis:1
// deadline that genuinely expires while the store's Search blocks surfaces as
// a CLIENT_ERROR envelope with a SEARCH_TIMEOUT: prefixed message and
// Retryable:true — and, because DirectSearch collects all results before this
// handler streams any of them, the failure must be the ONLY element sent (no
// result elements precede or follow it).
func TestEntitySearch_DirectSearch_TimeoutExpires_SearchTimeoutNoElements(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()
	ctx := grpcTenantCtx()
	ref := spi.ModelRef{EntityName: "timeoutmodel", ModelVersion: "1"}
	saveMinimalModelGRPC(t, ctx, base, ref)

	blocking := &searchTimeoutEntityStore{}
	factory := &searchTimeoutStoreFactory{StoreFactory: base, entityStore: blocking}
	searchStore, _ := base.AsyncSearchStore(context.Background())
	svc := &CloudEventsServiceImpl{searchService: search.NewSearchService(factory, common.NewDefaultUUIDGenerator(), searchStore)}

	millis := 1
	ce := makeCE(EntitySearchRequest, map[string]any{
		"id":            "s-timeout-expire",
		"model":         map[string]any{"name": "timeoutmodel", "version": 1},
		"condition":     map[string]any{"type": "group", "operator": "AND", "conditions": []any{}},
		"timeoutMillis": millis,
	})
	stream := &mockEntityStream{ctx: ctx}
	if err := svc.EntitySearchCollection(ce, stream); err != nil {
		t.Fatalf("unexpected stream-level error (errors should be envelope responses): %v", err)
	}
	if len(stream.sent) != 1 {
		t.Fatalf("expected exactly 1 response (no result elements), got %d", len(stream.sent))
	}
	var typed events.EntityResponseJson
	validateResponse(t, stream.sent[0], &typed)
	if typed.Success {
		t.Fatal("expected success=false for an expired timeoutMillis deadline")
	}
	if typed.Error == nil {
		t.Fatal("expected error block in response")
	}
	if typed.Error.Code != "CLIENT_ERROR" {
		t.Errorf("expected code=CLIENT_ERROR, got %q", typed.Error.Code)
	}
	if !strings.HasPrefix(typed.Error.Message, common.ErrCodeSearchTimeout+":") {
		t.Errorf("message = %q, want prefix %q", typed.Error.Message, common.ErrCodeSearchTimeout+":")
	}
	if typed.Error.Retryable == nil || !*typed.Error.Retryable {
		t.Errorf("Retryable = %v, want true", typed.Error.Retryable)
	}
	if blocking.searchCalls != 1 {
		t.Errorf("Search called %d times, want exactly 1", blocking.searchCalls)
	}
}
