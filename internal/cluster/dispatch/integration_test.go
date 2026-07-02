package dispatch

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// TestIntegration_ClusterDispatch_FullFlow tests the complete end-to-end flow:
//  1. Create a "peer" Node B with a local dispatcher that returns success.
//  2. Create Node B's internal dispatch handler (with HMAC auth) and start it as httptest.NewServer.
//  3. Create Node A's ClusterDispatcher: local dispatcher fails (no member),
//     registry knows about the peer with matching tags.
//  4. Call DispatchProcessor on Node A's ClusterDispatcher.
//  5. Verify the result comes from the peer (forwarded successfully).
func TestIntegration_ClusterDispatch_FullFlow(t *testing.T) {
	auth, _ := NewAEADPeerAuth(testSecret32, 30*time.Second)

	t.Run("processor_full_flow", func(t *testing.T) {
		// Node B: local dispatcher returns peer-processed result.
		nodeBLocal := &stubDispatcher{
			processorResp: &spi.Entity{
				Meta: testEntity().Meta,
				Data: []byte(`{"result":"from-peer-processor"}`),
			},
		}
		handler := NewDispatchHandler(nodeBLocal, auth)
		mux := http.NewServeMux()
		handler.Register(mux)
		nodeBServer := httptest.NewServer(mux)
		defer nodeBServer.Close()

		// Node A: local dispatcher has no matching member.
		nodeALocal := &stubDispatcher{noMember: true}
		registry := &stubNodeRegistry{
			nodes: []contract.NodeInfo{
				{NodeID: "node-a", Addr: "http://localhost:9999", Alive: true, Tags: map[string][]string{}},
				{NodeID: "node-b", Addr: nodeBServer.URL, Alive: true, Tags: map[string][]string{"tenant-1": {"python"}}},
			},
		}
		selector := NewRandomSelector()
		forwarder := NewHTTPForwarder(auth, 5*time.Second).AllowLoopbackForTesting()
		d := NewClusterDispatcher(nodeALocal, registry, "node-a", selector, forwarder, 2*time.Second, nil, 0)

		ctx := testContext()
		result, err := d.DispatchProcessor(ctx, testEntity(), testProcessor(), "wf", "tr", "tx-integration-1")
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if string(result.Data) != `{"result":"from-peer-processor"}` {
			t.Fatalf("expected peer result, got %s", string(result.Data))
		}
	})

	t.Run("criteria_full_flow", func(t *testing.T) {
		// Node B: local dispatcher returns criteria match = true.
		nodeBLocal := &stubDispatcher{
			criteriaResult: true,
		}
		handler := NewDispatchHandler(nodeBLocal, auth)
		mux := http.NewServeMux()
		handler.Register(mux)
		nodeBServer := httptest.NewServer(mux)
		defer nodeBServer.Close()

		// Node A: local dispatcher has no matching member.
		nodeALocal := &stubDispatcher{noMember: true}
		registry := &stubNodeRegistry{
			nodes: []contract.NodeInfo{
				{NodeID: "node-a", Addr: "http://localhost:9999", Alive: true, Tags: map[string][]string{}},
				{NodeID: "node-b", Addr: nodeBServer.URL, Alive: true, Tags: map[string][]string{"tenant-1": {"python"}}},
			},
		}
		selector := NewRandomSelector()
		forwarder := NewHTTPForwarder(auth, 5*time.Second).AllowLoopbackForTesting()
		d := NewClusterDispatcher(nodeALocal, registry, "node-a", selector, forwarder, 2*time.Second, nil, 0)

		ctx := testContext()
		matches, err := d.DispatchCriteria(ctx, testEntity(), testCriterion(), "TRANSITION", "wf", "tr", "proc", "tx-integration-2")
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if !matches {
			t.Fatal("expected matches=true from peer")
		}
	})
}

// TestIntegration_ClusterDispatch_NoMemberTimeout tests the timeout behaviour:
//  1. Local dispatcher fails (no member).
//  2. Registry has no peers with matching tags.
//  3. Wait timeout is short (300ms).
//  4. Verify error is returned after waiting.
//  5. Verify elapsed time shows the poll waited.
func TestIntegration_ClusterDispatch_NoMemberTimeout(t *testing.T) {
	local := &stubDispatcher{noMember: true}

	// Registry contains no peers with the required "python" tag for tenant-1.
	registry := &stubNodeRegistry{
		nodes: []contract.NodeInfo{
			{NodeID: "node-a", Addr: "http://localhost:9999", Alive: true, Tags: map[string][]string{}},
			{NodeID: "node-b", Addr: "http://localhost:9998", Alive: true, Tags: map[string][]string{"other-tenant": {"python"}}},
			{NodeID: "node-c", Addr: "http://localhost:9997", Alive: false, Tags: map[string][]string{"tenant-1": {"python"}}},
		},
	}
	selector := NewRandomSelector()
	timeoutAuth, _ := NewAEADPeerAuth(testSecret32, 30*time.Second)
	forwarder := NewHTTPForwarder(timeoutAuth, 5*time.Second).AllowLoopbackForTesting()

	const waitTimeout = 300 * time.Millisecond
	d := NewClusterDispatcher(local, registry, "node-a", selector, forwarder, waitTimeout, nil, 0)

	t.Run("processor_timeout", func(t *testing.T) {
		ctx := testContext()
		start := time.Now()
		_, err := d.DispatchProcessor(ctx, testEntity(), testProcessor(), "wf", "tr", "tx-timeout-1")
		elapsed := time.Since(start)

		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !containsStr(err.Error(), common.ErrCodeNoComputeMemberForTag) {
			t.Fatalf("expected NO_COMPUTE_MEMBER_FOR_TAG error, got %v", err)
		}
		// Should have polled for approximately the wait timeout.
		if elapsed < 250*time.Millisecond {
			t.Fatalf("expected elapsed >= 250ms (poll waited), got %v", elapsed)
		}
	})

	t.Run("criteria_timeout", func(t *testing.T) {
		ctx := testContext()
		start := time.Now()
		_, err := d.DispatchCriteria(ctx, testEntity(), testCriterion(), "TRANSITION", "wf", "tr", "proc", "tx-timeout-2")
		elapsed := time.Since(start)

		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !containsStr(err.Error(), common.ErrCodeNoComputeMemberForTag) {
			t.Fatalf("expected NO_COMPUTE_MEMBER_FOR_TAG error, got %v", err)
		}
		if elapsed < 250*time.Millisecond {
			t.Fatalf("expected elapsed >= 250ms (poll waited), got %v", elapsed)
		}
	})
}
