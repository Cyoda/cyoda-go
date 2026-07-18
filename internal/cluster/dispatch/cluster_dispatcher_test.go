package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

// fakeForwarder is a DispatchForwarder test double that returns a canned
// response/error without going over the wire — it isolates
// ClusterDispatcher's peer-response remint logic (B1) from the HTTP/AEAD
// plumbing already covered by the httptest-server-based forward tests.
type fakeForwarder struct {
	resp *DispatchCalloutResponse
	err  error
}

func (f *fakeForwarder) ForwardCallout(_ context.Context, _ string, _ DispatchCalloutRequest) (*DispatchCalloutResponse, error) {
	return f.resp, f.err
}

// --- fakes ---

// stubDispatcher simulates the local ProcessorDispatcher. When noMember is true,
// it returns the "no matching calculation member" error that triggers cluster lookup.
type stubDispatcher struct {
	noMember       bool
	otherErr       error
	processorResp  *spi.Entity
	criteriaResult bool
	criteriaReason string
	functionResult contract.FunctionResult
}

func (f *stubDispatcher) DispatchProcessor(_ context.Context, _ *spi.Entity, _ spi.ProcessorDefinition, _ string, _ string, _ string) (*spi.Entity, error) {
	if f.otherErr != nil {
		return nil, f.otherErr
	}
	if f.noMember {
		return nil, fmt.Errorf("%w: tags %q", internalgrpc.ErrNoMatchingMember, "python")
	}
	return f.processorResp, nil
}

func (f *stubDispatcher) DispatchCriteria(_ context.Context, _ *spi.Entity, _ json.RawMessage, _ string, _ string, _ string, _ string, _ string) (bool, string, error) {
	if f.otherErr != nil {
		return false, "", f.otherErr
	}
	if f.noMember {
		return false, "", fmt.Errorf("%w: tags %q", internalgrpc.ErrNoMatchingMember, "python")
	}
	return f.criteriaResult, f.criteriaReason, nil
}

func (f *stubDispatcher) DispatchFunction(_ context.Context, _ *spi.Entity, _ spi.ScheduleFunction, _ string, _ string, _ string) (contract.FunctionResult, error) {
	if f.otherErr != nil {
		return contract.FunctionResult{}, f.otherErr
	}
	if f.noMember {
		return contract.FunctionResult{}, fmt.Errorf("%w: tags %q", internalgrpc.ErrNoMatchingMember, "python")
	}
	return f.functionResult, nil
}

// stubNodeRegistry returns a fixed list of nodes.
type stubNodeRegistry struct {
	nodes []contract.NodeInfo
}

func (r *stubNodeRegistry) Register(_ context.Context, _ string, _ string) error { return nil }
func (r *stubNodeRegistry) Lookup(_ context.Context, _ string) (string, bool, error) {
	return "", false, nil
}
func (r *stubNodeRegistry) List(_ context.Context) ([]contract.NodeInfo, error) {
	return r.nodes, nil
}
func (r *stubNodeRegistry) Deregister(_ context.Context, _ string) error { return nil }

// testContext builds a context with UserContext set.
func testContext() context.Context {
	uc := &spi.UserContext{
		UserID: "user-1",
		Tenant: spi.Tenant{ID: "tenant-1", Name: "Test Tenant"},
		Roles:  []string{"ROLE_USER"},
	}
	return spi.WithUserContext(context.Background(), uc)
}

// testEntity builds a minimal entity for dispatch tests.
func testEntity() *spi.Entity {
	return &spi.Entity{
		Meta: spi.EntityMeta{
			ID:       "entity-1",
			TenantID: "tenant-1",
			ModelRef: spi.ModelRef{EntityName: "TestModel", ModelVersion: "1"},
			State:    "OPEN",
		},
		Data: []byte(`{"key":"value"}`),
	}
}

// testProcessor builds a processor with calculationNodesTags="python".
func testProcessor() spi.ProcessorDefinition {
	return spi.ProcessorDefinition{
		Type: "function",
		Name: "myProcessor",
		Config: spi.ProcessorConfig{
			AttachEntity:         true,
			CalculationNodesTags: "python",
		},
	}
}

// testCriterion builds criterion JSON with calculationNodesTags="python".
func testCriterion() json.RawMessage {
	return json.RawMessage(`{"type":"function","function":{"name":"myCriteria","config":{"calculationNodesTags":"python","attachEntity":true}}}`)
}

// testFunction builds a ScheduleFunction with calculationNodesTags="python".
func testFunction() spi.ScheduleFunction {
	return spi.ScheduleFunction{
		Name:                 "myScheduleFn",
		ResultKind:           "Schedule",
		CalculationNodesTags: "python",
		AttachEntity:         true,
	}
}

// --- tests ---

func TestClusterDispatcher_LocalFirst(t *testing.T) {
	updatedEntity := &spi.Entity{
		Meta: testEntity().Meta,
		Data: []byte(`{"key":"updated"}`),
	}

	local := &stubDispatcher{
		processorResp:  updatedEntity,
		criteriaResult: true,
		functionResult: contract.FunctionResult{Kind: "Schedule", Value: json.RawMessage(`{"fireAfterMs":1000}`)},
	}
	registry := &stubNodeRegistry{}
	selector := NewRandomSelector()
	auth, _ := NewAEADPeerAuth(testSecret32, 30*time.Second)
	forwarder := NewHTTPForwarder(auth, 5*time.Second).AllowLoopbackForTesting()

	d := NewClusterDispatcher(local, registry, "self-node", selector, forwarder, 1*time.Second, nil, 0)

	t.Run("processor_local_success", func(t *testing.T) {
		ctx := testContext()
		result, err := d.DispatchProcessor(ctx, testEntity(), testProcessor(), "wf", "tr", "tx1")
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if string(result.Data) != `{"key":"updated"}` {
			t.Fatalf("expected updated data, got %s", string(result.Data))
		}
	})

	t.Run("criteria_local_success", func(t *testing.T) {
		ctx := testContext()
		matches, _, err := d.DispatchCriteria(ctx, testEntity(), testCriterion(), "TRANSITION", "wf", "tr", "proc", "tx1")
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if !matches {
			t.Fatal("expected matches=true")
		}
	})

	t.Run("function_local_success", func(t *testing.T) {
		ctx := testContext()
		result, err := d.DispatchFunction(ctx, testEntity(), testFunction(), "wf", "tr", "tx1")
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if result.Kind != "Schedule" {
			t.Fatalf("expected Kind=Schedule, got %s", result.Kind)
		}
		if string(result.Value) != `{"fireAfterMs":1000}` {
			t.Fatalf("expected fireAfterMs value, got %s", string(result.Value))
		}
	})

	t.Run("local_other_error_not_forwarded", func(t *testing.T) {
		localErr := &stubDispatcher{
			otherErr: fmt.Errorf("connection reset"),
		}
		d2 := NewClusterDispatcher(localErr, registry, "self-node", selector, forwarder, 1*time.Second, nil, 0)
		ctx := testContext()

		_, err := d2.DispatchProcessor(ctx, testEntity(), testProcessor(), "wf", "tr", "tx1")
		if err == nil {
			t.Fatal("expected error")
		}
		if err.Error() != "connection reset" {
			t.Fatalf("expected original error, got %v", err)
		}
	})
}

func TestClusterDispatcher_ForwardsToPeer(t *testing.T) {
	auth, _ := NewAEADPeerAuth(testSecret32, 30*time.Second)

	t.Run("processor_forwarded_to_peer", func(t *testing.T) {
		// Set up a peer httptest server that acts as a dispatch handler.
		peerLocal := &stubDispatcher{
			processorResp: &spi.Entity{
				Meta: testEntity().Meta,
				Data: []byte(`{"key":"peer-processed"}`),
			},
		}
		handler := NewDispatchHandler(peerLocal, auth)
		mux := http.NewServeMux()
		handler.Register(mux)
		peer := httptest.NewServer(mux)
		defer peer.Close()

		// Local fails with "no matching calculation member".
		local := &stubDispatcher{noMember: true}
		registry := &stubNodeRegistry{
			nodes: []contract.NodeInfo{
				{NodeID: "self-node", Addr: "http://localhost:9999", Alive: true, Tags: map[string][]string{"tenant-1": {"python"}}},
				{NodeID: "peer-1", Addr: peer.URL, Alive: true, Tags: map[string][]string{"tenant-1": {"python"}}},
			},
		}
		selector := NewRandomSelector()
		forwarder := NewHTTPForwarder(auth, 5*time.Second).AllowLoopbackForTesting()

		d := NewClusterDispatcher(local, registry, "self-node", selector, forwarder, 1*time.Second, nil, 0)

		ctx := testContext()
		result, err := d.DispatchProcessor(ctx, testEntity(), testProcessor(), "wf", "tr", "tx1")
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if string(result.Data) != `{"key":"peer-processed"}` {
			t.Fatalf("expected peer-processed data, got %s", string(result.Data))
		}
	})

	t.Run("criteria_forwarded_to_peer", func(t *testing.T) {
		peerLocal := &stubDispatcher{
			criteriaResult: true,
			criteriaReason: "peer-evaluated reason",
		}
		handler := NewDispatchHandler(peerLocal, auth)
		mux := http.NewServeMux()
		handler.Register(mux)
		peer := httptest.NewServer(mux)
		defer peer.Close()

		local := &stubDispatcher{noMember: true}
		registry := &stubNodeRegistry{
			nodes: []contract.NodeInfo{
				{NodeID: "self-node", Addr: "http://localhost:9999", Alive: true, Tags: map[string][]string{"tenant-1": {"python"}}},
				{NodeID: "peer-1", Addr: peer.URL, Alive: true, Tags: map[string][]string{"tenant-1": {"python"}}},
			},
		}
		selector := NewRandomSelector()
		forwarder := NewHTTPForwarder(auth, 5*time.Second).AllowLoopbackForTesting()

		d := NewClusterDispatcher(local, registry, "self-node", selector, forwarder, 1*time.Second, nil, 0)

		ctx := testContext()
		matches, reason, err := d.DispatchCriteria(ctx, testEntity(), testCriterion(), "TRANSITION", "wf", "tr", "proc", "tx1")
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if !matches {
			t.Fatal("expected matches=true from peer")
		}
		// The peer-evaluated reason must survive the full forward round-trip
		// (producer -> DispatchCriteriaResponse.Reason -> peer-branch return).
		if reason != "peer-evaluated reason" {
			t.Fatalf("expected peer reason propagated, got %q", reason)
		}
	})

	t.Run("function_forwarded_to_peer", func(t *testing.T) {
		peerLocal := &stubDispatcher{
			functionResult: contract.FunctionResult{
				Kind:  "Schedule",
				Value: json.RawMessage(`{"fireAfterMs":2000}`),
			},
		}
		handler := NewDispatchHandler(peerLocal, auth)
		mux := http.NewServeMux()
		handler.Register(mux)
		peer := httptest.NewServer(mux)
		defer peer.Close()

		local := &stubDispatcher{noMember: true}
		registry := &stubNodeRegistry{
			nodes: []contract.NodeInfo{
				{NodeID: "self-node", Addr: "http://localhost:9999", Alive: true, Tags: map[string][]string{"tenant-1": {"python"}}},
				{NodeID: "peer-1", Addr: peer.URL, Alive: true, Tags: map[string][]string{"tenant-1": {"python"}}},
			},
		}
		selector := NewRandomSelector()
		forwarder := NewHTTPForwarder(auth, 5*time.Second).AllowLoopbackForTesting()

		d := NewClusterDispatcher(local, registry, "self-node", selector, forwarder, 1*time.Second, nil, 0)

		ctx := testContext()
		result, err := d.DispatchFunction(ctx, testEntity(), testFunction(), "wf", "tr", "tx1")
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		// Result and ResultKind must survive the full forward round-trip
		// (producer -> DispatchCalloutResponse.{Result,ResultKind} -> peer-branch return).
		if result.Kind != "Schedule" {
			t.Fatalf("expected Kind=Schedule propagated from peer, got %q", result.Kind)
		}
		if string(result.Value) != `{"fireAfterMs":2000}` {
			t.Fatalf("expected peer result value propagated, got %s", string(result.Value))
		}
	})
}

func TestClusterDispatcher_NoMemberAnywhere(t *testing.T) {
	local := &stubDispatcher{noMember: true}
	registry := &stubNodeRegistry{
		nodes: []contract.NodeInfo{
			{NodeID: "self-node", Addr: "http://localhost:9999", Alive: true, Tags: map[string][]string{}},
			{NodeID: "peer-1", Addr: "http://localhost:9998", Alive: true, Tags: map[string][]string{"other-tenant": {"python"}}},
			{NodeID: "dead-peer", Addr: "http://localhost:9997", Alive: false, Tags: map[string][]string{"tenant-1": {"python"}}},
		},
	}
	selector := NewRandomSelector()
	auth, _ := NewAEADPeerAuth(testSecret32, 30*time.Second)
	forwarder := NewHTTPForwarder(auth, 5*time.Second).AllowLoopbackForTesting()

	// Use a very short wait timeout so the test completes quickly.
	d := NewClusterDispatcher(local, registry, "self-node", selector, forwarder, 500*time.Millisecond, nil, 0)

	t.Run("processor_no_member_anywhere", func(t *testing.T) {
		ctx := testContext()
		start := time.Now()
		_, err := d.DispatchProcessor(ctx, testEntity(), testProcessor(), "wf", "tr", "tx1")
		elapsed := time.Since(start)
		if err == nil {
			t.Fatal("expected error")
		}
		var appErr *common.AppError
		if !errors.As(err, &appErr) {
			t.Fatalf("expected *common.AppError, got %T: %v", err, err)
		}
		if appErr.Status != http.StatusServiceUnavailable {
			t.Fatalf("expected status 503, got %d", appErr.Status)
		}
		if appErr.Code != common.ErrCodeNoComputeMemberForTag {
			t.Fatalf("expected code %s, got %s", common.ErrCodeNoComputeMemberForTag, appErr.Code)
		}
		if !appErr.Retryable {
			t.Fatal("expected retryable=true")
		}
		// Should have polled for approximately the wait timeout.
		if elapsed < 400*time.Millisecond {
			t.Fatalf("expected polling to take ~500ms, took %v", elapsed)
		}
	})

	t.Run("criteria_no_member_anywhere", func(t *testing.T) {
		ctx := testContext()
		_, _, err := d.DispatchCriteria(ctx, testEntity(), testCriterion(), "TRANSITION", "wf", "tr", "proc", "tx1")
		if err == nil {
			t.Fatal("expected error")
		}
		var appErr *common.AppError
		if !errors.As(err, &appErr) {
			t.Fatalf("expected *common.AppError, got %T: %v", err, err)
		}
		if appErr.Status != http.StatusServiceUnavailable {
			t.Fatalf("expected status 503, got %d", appErr.Status)
		}
		if appErr.Code != common.ErrCodeNoComputeMemberForTag {
			t.Fatalf("expected code %s, got %s", common.ErrCodeNoComputeMemberForTag, appErr.Code)
		}
		if !appErr.Retryable {
			t.Fatal("expected retryable=true")
		}
	})
}

// TestClusterDispatcher_ForwardFailure covers the terminal case where a peer
// IS selected (it advertises the required tag) but the transport-level
// forward to that peer fails (e.g. connection refused). This must surface a
// retryable 503 DISPATCH_FORWARD_FAILED, distinct from the no-peer-found
// case (NO_COMPUTE_MEMBER_FOR_TAG).
func TestClusterDispatcher_ForwardFailure(t *testing.T) {
	registry := &stubNodeRegistry{
		nodes: []contract.NodeInfo{
			// Peer advertises the tag but is unreachable — forward transport fails.
			{NodeID: "peer-1", Addr: "http://localhost:1", Alive: true, Tags: map[string][]string{"tenant-1": {"python"}}},
		},
	}
	selector := NewRandomSelector()
	auth, _ := NewAEADPeerAuth(testSecret32, 30*time.Second)
	forwarder := NewHTTPForwarder(auth, 2*time.Second).AllowLoopbackForTesting()

	local := &stubDispatcher{noMember: true}
	d := NewClusterDispatcher(local, registry, "self-node", selector, forwarder, 1*time.Second, nil, 0)

	// assertForwardFailureSanitized checks the shared taxonomy assertions
	// AND (B2) that the client-facing Message carries no peer topology —
	// no IP/host:port, no route path, no scheme — only the generic text.
	assertForwardFailureSanitized := func(t *testing.T, err error) {
		t.Helper()
		if err == nil {
			t.Fatal("expected error")
		}
		var appErr *common.AppError
		if !errors.As(err, &appErr) {
			t.Fatalf("expected *common.AppError, got %T: %v", err, err)
		}
		if appErr.Status != http.StatusServiceUnavailable {
			t.Fatalf("expected status 503, got %d", appErr.Status)
		}
		if appErr.Code != common.ErrCodeDispatchForwardFailed {
			t.Fatalf("expected code %s, got %s", common.ErrCodeDispatchForwardFailed, appErr.Code)
		}
		if !appErr.Retryable {
			t.Fatal("expected retryable=true")
		}
		for _, leak := range []string{"localhost:1", "://", "/internal/dispatch/callout", "peer-1"} {
			if strings.Contains(appErr.Message, leak) {
				t.Errorf("client-facing message leaks peer topology (%q found): %q", leak, appErr.Message)
			}
		}
	}

	t.Run("processor_forward_transport_fails", func(t *testing.T) {
		ctx := testContext()
		_, err := d.DispatchProcessor(ctx, testEntity(), testProcessor(), "wf", "tr", "tx1")
		assertForwardFailureSanitized(t, err)
	})

	t.Run("criteria_forward_transport_fails", func(t *testing.T) {
		ctx := testContext()
		_, _, err := d.DispatchCriteria(ctx, testEntity(), testCriterion(), "TRANSITION", "wf", "tr", "proc", "tx1")
		assertForwardFailureSanitized(t, err)
	})

	t.Run("function_forward_transport_fails", func(t *testing.T) {
		ctx := testContext()
		_, err := d.DispatchFunction(ctx, testEntity(), testFunction(), "wf", "tr", "tx1")
		assertForwardFailureSanitized(t, err)
	})
}

// TestClusterDispatcher_RemintsPeerErrorTaxonomy unit-tests the peer-response
// remint logic in isolation: given a DispatchCalloutResponse with the
// ErrorCode/ErrorStatus/ErrorRetryable trio set, ClusterDispatcher.Dispatch*
// must re-mint the SAME *common.AppError taxonomy — a 5xx classified as
// Operational.AsRetryable() stays Operational/retryable, and a 500 classified
// as Internal stays Internal — matching how single-node dispatch mints the
// identical code (B1, final review). An empty ErrorCode (older/unclassified
// peer) falls back to the historical plain error.
func TestClusterDispatcher_RemintsPeerErrorTaxonomy(t *testing.T) {
	registry := &stubNodeRegistry{
		nodes: []contract.NodeInfo{
			{NodeID: "peer-1", Addr: "http://peer", Alive: true, Tags: map[string][]string{"tenant-1": {"python"}}},
		},
	}
	selector := NewRandomSelector()
	local := &stubDispatcher{noMember: true}

	t.Run("503_retryable_code_reminted_operational_retryable", func(t *testing.T) {
		fwd := &fakeForwarder{resp: &DispatchCalloutResponse{
			Success:        false,
			Error:          "dispatch processor failed",
			ErrorCode:      common.ErrCodeDispatchTimeout,
			ErrorStatus:    http.StatusServiceUnavailable,
			ErrorRetryable: true,
		}}
		d := NewClusterDispatcher(local, registry, "self-node", selector, fwd, 1*time.Second, nil, 0)
		ctx := testContext()
		_, err := d.DispatchProcessor(ctx, testEntity(), testProcessor(), "wf", "tr", "tx1")
		if err == nil {
			t.Fatal("expected error")
		}
		var appErr *common.AppError
		if !errors.As(err, &appErr) {
			t.Fatalf("expected *common.AppError, got %T: %v", err, err)
		}
		if appErr.Status != http.StatusServiceUnavailable {
			t.Errorf("Status = %d, want 503", appErr.Status)
		}
		if appErr.Code != common.ErrCodeDispatchTimeout {
			t.Errorf("Code = %s, want %s", appErr.Code, common.ErrCodeDispatchTimeout)
		}
		if !appErr.Retryable {
			t.Error("Retryable = false, want true")
		}
		if appErr.Level != common.LevelOperational {
			t.Errorf("Level = %v, want LevelOperational", appErr.Level)
		}
	})

	t.Run("500_code_reminted_internal_with_code", func(t *testing.T) {
		fwd := &fakeForwarder{resp: &DispatchCalloutResponse{
			Success:     false,
			Error:       "dispatch function failed",
			ErrorCode:   common.ErrCodeScheduleFunctionInvalidResult,
			ErrorStatus: http.StatusInternalServerError,
		}}
		d := NewClusterDispatcher(local, registry, "self-node", selector, fwd, 1*time.Second, nil, 0)
		ctx := testContext()
		_, err := d.DispatchFunction(ctx, testEntity(), testFunction(), "wf", "tr", "tx1")
		if err == nil {
			t.Fatal("expected error")
		}
		var appErr *common.AppError
		if !errors.As(err, &appErr) {
			t.Fatalf("expected *common.AppError, got %T: %v", err, err)
		}
		if appErr.Status != http.StatusInternalServerError {
			t.Errorf("Status = %d, want 500", appErr.Status)
		}
		if appErr.Code != common.ErrCodeScheduleFunctionInvalidResult {
			t.Errorf("Code = %s, want %s", appErr.Code, common.ErrCodeScheduleFunctionInvalidResult)
		}
		if appErr.Level != common.LevelInternal {
			t.Errorf("Level = %v, want LevelInternal", appErr.Level)
		}
	})

	t.Run("empty_error_code_falls_back_to_plain_error", func(t *testing.T) {
		fwd := &fakeForwarder{resp: &DispatchCalloutResponse{
			Success: false,
			Error:   "dispatch criteria failed",
		}}
		d := NewClusterDispatcher(local, registry, "self-node", selector, fwd, 1*time.Second, nil, 0)
		ctx := testContext()
		_, _, err := d.DispatchCriteria(ctx, testEntity(), testCriterion(), "TRANSITION", "wf", "tr", "proc", "tx1")
		if err == nil {
			t.Fatal("expected error")
		}
		var appErr *common.AppError
		if errors.As(err, &appErr) {
			t.Fatalf("expected plain fallback error (no AppError), got %+v", appErr)
		}
	})
}

// TestClusterDispatcher_PeerLocalDispatchErrorTaxonomyPropagatesOverWire
// drives the full forward path (real HTTP + AEAD, via DispatchHandler) where
// the PEER node's own local dispatch fails, and asserts the forwarding
// ClusterDispatcher surfaces the SAME AppError taxonomy the peer would
// surface if the caller had dispatched locally — not a plain error that
// classifyWorkflowError would collapse into 400 WORKFLOW_FAILED (B1).
func TestClusterDispatcher_PeerLocalDispatchErrorTaxonomyPropagatesOverWire(t *testing.T) {
	auth, _ := NewAEADPeerAuth(testSecret32, 30*time.Second)

	newForwardingDispatcher := func(t *testing.T, peerLocal *stubDispatcher) *ClusterDispatcher {
		t.Helper()
		handler := NewDispatchHandler(peerLocal, auth)
		mux := http.NewServeMux()
		handler.Register(mux)
		peer := httptest.NewServer(mux)
		t.Cleanup(peer.Close)

		local := &stubDispatcher{noMember: true}
		registry := &stubNodeRegistry{
			nodes: []contract.NodeInfo{
				{NodeID: "peer-1", Addr: peer.URL, Alive: true, Tags: map[string][]string{"tenant-1": {"python"}}},
			},
		}
		selector := NewRandomSelector()
		forwarder := NewHTTPForwarder(auth, 5*time.Second).AllowLoopbackForTesting()
		return NewClusterDispatcher(local, registry, "self-node", selector, forwarder, 1*time.Second, nil, 0)
	}

	t.Run("peer_dispatch_timeout_appError_propagates_as_503_retryable", func(t *testing.T) {
		peerLocal := &stubDispatcher{
			otherErr: common.Operational(http.StatusServiceUnavailable, common.ErrCodeDispatchTimeout,
				"processor dispatch timed out after 3000ms").AsRetryable(),
		}
		d := newForwardingDispatcher(t, peerLocal)

		ctx := testContext()
		_, err := d.DispatchProcessor(ctx, testEntity(), testProcessor(), "wf", "tr", "tx1")
		if err == nil {
			t.Fatal("expected error")
		}
		var appErr *common.AppError
		if !errors.As(err, &appErr) {
			t.Fatalf("expected *common.AppError, got %T: %v", err, err)
		}
		if appErr.Status != http.StatusServiceUnavailable {
			t.Fatalf("expected 503, got %d", appErr.Status)
		}
		if appErr.Code != common.ErrCodeDispatchTimeout {
			t.Fatalf("expected code %s, got %s", common.ErrCodeDispatchTimeout, appErr.Code)
		}
		if !appErr.Retryable {
			t.Fatal("expected retryable=true")
		}
	})

	t.Run("peer_no_matching_member_between_gossip_and_forward_propagates_as_503_retryable", func(t *testing.T) {
		peerLocal := &stubDispatcher{noMember: true}
		d := newForwardingDispatcher(t, peerLocal)

		ctx := testContext()
		_, err := d.DispatchProcessor(ctx, testEntity(), testProcessor(), "wf", "tr", "tx1")
		if err == nil {
			t.Fatal("expected error")
		}
		var appErr *common.AppError
		if !errors.As(err, &appErr) {
			t.Fatalf("expected *common.AppError, got %T: %v", err, err)
		}
		if appErr.Status != http.StatusServiceUnavailable {
			t.Fatalf("expected 503, got %d", appErr.Status)
		}
		if appErr.Code != common.ErrCodeNoComputeMemberForTag {
			t.Fatalf("expected code %s, got %s", common.ErrCodeNoComputeMemberForTag, appErr.Code)
		}
		if !appErr.Retryable {
			t.Fatal("expected retryable=true")
		}
	})
}

func containsStr(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && contains(s, substr))
}

func contains(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
