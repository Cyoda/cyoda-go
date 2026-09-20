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
// capturedReq records the request from the most recent ForwardCallout call
// so tests can assert what gets forwarded to a peer (e.g. principal kind
// propagation).
type fakeForwarder struct {
	resp        *DispatchCalloutResponse
	err         error
	capturedReq DispatchCalloutRequest
}

func (f *fakeForwarder) ForwardCallout(_ context.Context, _ string, req DispatchCalloutRequest) (*DispatchCalloutResponse, error) {
	f.capturedReq = req
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

// The shared fixtures — stubNodeRegistry, testContext, testEntity,
// testProcessor, testCriterion, testFunction — live in fixtures_test.go.

// testAnswerLimit stands for (*grpc.ProcessorDispatcher).ResolveAnswerLimit in
// the tests that still drive ClusterDispatcher.
func testAnswerLimit(ms int64) (time.Duration, *contract.CalloutFailure) {
	if ms > 0 {
		return time.Duration(ms) * time.Millisecond, nil
	}
	return 5 * time.Second, nil
}

func newTestClusterDispatcher(t *testing.T, local contract.ExternalProcessingService, registry contract.NodeRegistry, selfNodeID string, selector PeerSelector, fwd DispatchForwarder, wait time.Duration) *ClusterDispatcher {
	t.Helper()
	router, err := NewPeerRouter(registry, selfNodeID, selector, fwd, nil)
	if err != nil {
		t.Fatalf("NewPeerRouter: %v", err)
	}
	return NewClusterDispatcher(local, router, testAnswerLimit, wait, time.Second)
}

// stubRunner lets a stubDispatcher stand for the peer's local procedure.
type stubRunner struct{ stub *stubDispatcher }

func (s stubRunner) RunLocal(ctx context.Context, call internalgrpc.Callout, _ int) internalgrpc.LocalResult {
	src := call.Source
	var res internalgrpc.CalloutResult
	var err error
	switch call.Kind {
	case internalgrpc.ProcessorCallout:
		res.Entity, err = s.stub.DispatchProcessor(ctx, src.Entity, *src.Processor, src.WorkflowName, src.TransitionName, call.TxID)
	case internalgrpc.CriteriaCallout:
		res.Matches, res.Reason, err = s.stub.DispatchCriteria(ctx, src.Entity, src.Criterion, src.Target, src.WorkflowName, src.TransitionName, src.ProcessorName, call.TxID)
	default:
		res.Function, err = s.stub.DispatchFunction(ctx, src.Entity, *src.Function, src.WorkflowName, src.TransitionName, call.TxID)
	}
	switch {
	case err == nil:
		return internalgrpc.LocalResult{Result: res, TriesUsed: 1}
	case errors.Is(err, internalgrpc.ErrNoMatchingMember):
		return internalgrpc.LocalResult{Failure: &contract.CalloutFailure{Kind: contract.NoHandOff, Code: common.ErrCodeNoComputeMemberForTag, Message: err.Error(), Err: err}}
	default:
		return internalgrpc.LocalResult{TriesUsed: 1,
			Failure:  &contract.CalloutFailure{Kind: contract.NoAnswer, Message: err.Error(), Err: err},
			Attempts: []contract.CalloutAttempt{{MemberID: "m1", Kind: contract.NoAnswer, Cause: err.Error()}}}
	}
}

func (s stubRunner) ResolveAnswerLimit(ms int64) (time.Duration, *contract.CalloutFailure) {
	return testAnswerLimit(ms)
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

	d := newTestClusterDispatcher(t, local, registry, "self-node", selector, forwarder, 1*time.Second)

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
		d2 := newTestClusterDispatcher(t, localErr, registry, "self-node", selector, forwarder, 1*time.Second)
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
		handler := NewDispatchHandler(stubRunner{peerLocal}, auth, testMaxTries)
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

		d := newTestClusterDispatcher(t, local, registry, "self-node", selector, forwarder, 1*time.Second)

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
		handler := NewDispatchHandler(stubRunner{peerLocal}, auth, testMaxTries)
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

		d := newTestClusterDispatcher(t, local, registry, "self-node", selector, forwarder, 1*time.Second)

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
		handler := NewDispatchHandler(stubRunner{peerLocal}, auth, testMaxTries)
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

		d := newTestClusterDispatcher(t, local, registry, "self-node", selector, forwarder, 1*time.Second)

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

// TestClusterDispatcher_ForwardsPrincipalKind guards that the originating
// principal's explicit Kind is carried across the cross-node dispatch wire —
// the peer reconstructs a UserContext from this request (handler.go
// buildContext), and AttachAuthContext fails the dispatch closed if Kind is
// unset. Without this field on the wire, every forwarded callout would fail
// auth-context attachment on the peer regardless of the originating
// principal's real kind.
func TestClusterDispatcher_ForwardsPrincipalKind(t *testing.T) {
	registry := &stubNodeRegistry{
		nodes: []contract.NodeInfo{
			{NodeID: "peer-1", Addr: "http://peer", Alive: true, Tags: map[string][]string{"tenant-1": {"python"}}},
		},
	}
	selector := NewRandomSelector()
	local := &stubDispatcher{noMember: true}

	tests := []struct {
		name string
		kind spi.PrincipalKind
	}{
		{"user", spi.PrincipalUser},
		{"service", spi.PrincipalService},
		{"system", spi.PrincipalSystem},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := spi.WithUserContext(context.Background(), &spi.UserContext{
				UserID: "principal-1",
				Kind:   tt.kind,
				Tenant: spi.Tenant{ID: "tenant-1", Name: "Test Tenant"},
			})

			t.Run("processor", func(t *testing.T) {
				fwd := &fakeForwarder{resp: &DispatchCalloutResponse{Outcome: OutcomeOK, TriesUsed: intPtr(1), EntityData: []byte(`{}`)}}
				d := newTestClusterDispatcher(t, local, registry, "self-node", selector, fwd, 1*time.Second)
				if _, err := d.DispatchProcessor(ctx, testEntity(), testProcessor(), "wf", "tr", "tx1"); err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if fwd.capturedReq.PrincipalKind != tt.kind {
					t.Errorf("PrincipalKind = %q, want %q", fwd.capturedReq.PrincipalKind, tt.kind)
				}
			})

			t.Run("criteria", func(t *testing.T) {
				fwd := &fakeForwarder{resp: &DispatchCalloutResponse{Outcome: OutcomeOK, TriesUsed: intPtr(1), Matches: new(bool)}}
				d := newTestClusterDispatcher(t, local, registry, "self-node", selector, fwd, 1*time.Second)
				if _, _, err := d.DispatchCriteria(ctx, testEntity(), testCriterion(), "TRANSITION", "wf", "tr", "proc", "tx1"); err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if fwd.capturedReq.PrincipalKind != tt.kind {
					t.Errorf("PrincipalKind = %q, want %q", fwd.capturedReq.PrincipalKind, tt.kind)
				}
			})

			t.Run("function", func(t *testing.T) {
				fwd := &fakeForwarder{resp: &DispatchCalloutResponse{Outcome: OutcomeOK, TriesUsed: intPtr(1)}}
				d := newTestClusterDispatcher(t, local, registry, "self-node", selector, fwd, 1*time.Second)
				if _, err := d.DispatchFunction(ctx, testEntity(), testFunction(), "wf", "tr", "tx1"); err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if fwd.capturedReq.PrincipalKind != tt.kind {
					t.Errorf("PrincipalKind = %q, want %q", fwd.capturedReq.PrincipalKind, tt.kind)
				}
			})
		})
	}
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
	d := newTestClusterDispatcher(t, local, registry, "self-node", selector, forwarder, 500*time.Millisecond)

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

// TestClusterDispatcher_ForwardFailure covers the case where a peer IS selected
// (it advertises the required tag) and the hand-over's answer is lost after the
// connection was opened. That must surface a retryable 503
// DISPATCH_FORWARD_FAILED, distinct from the no-peer-found case
// (NO_COMPUTE_MEMBER_FOR_TAG) — and carry none of the peer's topology.
func TestClusterDispatcher_ForwardFailure(t *testing.T) {
	registry := &stubNodeRegistry{
		nodes: []contract.NodeInfo{
			{NodeID: "peer-1", Addr: "http://localhost:1", Alive: true, Tags: map[string][]string{"tenant-1": {"python"}}},
		},
	}
	selector := NewRandomSelector()
	// The answer is lost after the connection was opened: the peer may have the
	// work, so it is not routed around and the try is spent.
	forwarder := &fakeForwarder{err: &ForwardError{Stage: StageAfterConnect,
		Err: errors.New("dispatch forward: HTTP POST http://localhost:1/internal/dispatch/callout: read: connection reset")}}

	local := &stubDispatcher{noMember: true}
	d := newTestClusterDispatcher(t, local, registry, "self-node", selector, forwarder, 1*time.Second)

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
			Outcome:        "no_answer",
			TriesUsed:      intPtr(1),
			ErrorCode:      common.ErrCodeDispatchTimeout,
			ErrorStatus:    http.StatusServiceUnavailable,
			ErrorRetryable: true,
		}}
		d := newTestClusterDispatcher(t, local, registry, "self-node", selector, fwd, 1*time.Second)
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
			Outcome:     "terminal",
			TriesUsed:   intPtr(1),
			ErrorCode:   common.ErrCodeScheduleFunctionInvalidResult,
			ErrorStatus: http.StatusInternalServerError,
		}}
		d := newTestClusterDispatcher(t, local, registry, "self-node", selector, fwd, 1*time.Second)
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

	// A failure the cnode itself declared carries no code of the answering
	// pnode's: its own message and verdict travel instead, and they reach the
	// caller unchanged. Before this they were replaced by a generic text.
	t.Run("the_cnodes_own_message_and_verdict_reach_the_caller", func(t *testing.T) {
		no := false
		answer := func() *DispatchCalloutResponse {
			return &DispatchCalloutResponse{Outcome: "member_failed", TriesUsed: intPtr(1),
				MemberError: "boom", MemberRetryable: &no}
		}
		assertMemberFailed := func(t *testing.T, err error) {
			t.Helper()
			if err == nil {
				t.Fatal("expected error")
			}
			var failure *contract.CalloutFailure
			if !errors.As(err, &failure) {
				t.Fatalf("expected a *contract.CalloutFailure, got %T: %v", err, err)
			}
			if failure.Kind != contract.MemberFailed || failure.Message != "boom" {
				t.Errorf("failure = %+v, want the cnode's own message", failure)
			}
			var appErr *common.AppError
			if errors.As(err, &appErr) {
				t.Errorf("a cnode's own failure was minted as this node's error: %+v", appErr)
			}
			// Cluster topology stays out of what the caller sees.
			if strings.Contains(err.Error(), "peer-1") {
				t.Errorf("the caller's error leaks the peer node id: %q", err.Error())
			}
		}

		t.Run("criteria", func(t *testing.T) {
			d := newTestClusterDispatcher(t, local, registry, "self-node", selector, &fakeForwarder{resp: answer()}, 1*time.Second)
			_, _, err := d.DispatchCriteria(testContext(), testEntity(), testCriterion(), "TRANSITION", "wf", "tr", "proc", "tx1")
			assertMemberFailed(t, err)
		})
		t.Run("processor", func(t *testing.T) {
			d := newTestClusterDispatcher(t, local, registry, "self-node", selector, &fakeForwarder{resp: answer()}, 1*time.Second)
			_, err := d.DispatchProcessor(testContext(), testEntity(), testProcessor(), "wf", "tr", "tx1")
			assertMemberFailed(t, err)
		})
		t.Run("function", func(t *testing.T) {
			d := newTestClusterDispatcher(t, local, registry, "self-node", selector, &fakeForwarder{resp: answer()}, 1*time.Second)
			_, err := d.DispatchFunction(testContext(), testEntity(), testFunction(), "wf", "tr", "tx1")
			assertMemberFailed(t, err)
		})
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
		handler := NewDispatchHandler(stubRunner{peerLocal}, auth, testMaxTries)
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
		return newTestClusterDispatcher(t, local, registry, "self-node", selector, forwarder, 1*time.Second)
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
