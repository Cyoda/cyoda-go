package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

// --- fakes ---

// stubDispatcher simulates the local ProcessorDispatcher. When noMember is true,
// it returns the "no matching calculation member" error that triggers cluster lookup.
type stubDispatcher struct {
	noMember       bool
	otherErr       error
	processorResp  *spi.Entity
	criteriaResult bool
	criteriaReason string
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

// --- tests ---

func TestClusterDispatcher_LocalFirst(t *testing.T) {
	updatedEntity := &spi.Entity{
		Meta: testEntity().Meta,
		Data: []byte(`{"key":"updated"}`),
	}

	local := &stubDispatcher{
		processorResp:  updatedEntity,
		criteriaResult: true,
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

	t.Run("processor_forward_transport_fails", func(t *testing.T) {
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
			t.Fatalf("expected status 503, got %d", appErr.Status)
		}
		if appErr.Code != common.ErrCodeDispatchForwardFailed {
			t.Fatalf("expected code %s, got %s", common.ErrCodeDispatchForwardFailed, appErr.Code)
		}
		if !appErr.Retryable {
			t.Fatal("expected retryable=true")
		}
	})

	t.Run("criteria_forward_transport_fails", func(t *testing.T) {
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
		if appErr.Code != common.ErrCodeDispatchForwardFailed {
			t.Fatalf("expected code %s, got %s", common.ErrCodeDispatchForwardFailed, appErr.Code)
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
