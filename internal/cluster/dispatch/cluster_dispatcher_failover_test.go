package dispatch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// firstSelector always picks the first candidate, making peer selection
// deterministic: with the dispatcher excluding already-tried peers from the
// candidate list, registry order dictates the failover order.
type firstSelector struct{}

func (firstSelector) Select(candidates []contract.NodeInfo) (contract.NodeInfo, error) {
	if len(candidates) == 0 {
		return contract.NodeInfo{}, errors.New("no candidates")
	}
	return candidates[0], nil
}

// scriptedForwarder is a DispatchForwarder test double with per-address
// scripted outcomes and a call log, so failover tests can degrade one peer
// and assert exactly which peers were attempted, in which order.
type scriptedForwarder struct {
	byAddr map[string]struct {
		resp *DispatchCalloutResponse
		err  error
	}
	calls []string
}

func newScriptedForwarder() *scriptedForwarder {
	return &scriptedForwarder{byAddr: map[string]struct {
		resp *DispatchCalloutResponse
		err  error
	}{}}
}

func (f *scriptedForwarder) script(addr string, resp *DispatchCalloutResponse, err error) {
	f.byAddr[addr] = struct {
		resp *DispatchCalloutResponse
		err  error
	}{resp, err}
}

func (f *scriptedForwarder) ForwardCallout(_ context.Context, addr string, _ DispatchCalloutRequest) (*DispatchCalloutResponse, error) {
	f.calls = append(f.calls, addr)
	s, ok := f.byAddr[addr]
	if !ok {
		return nil, fmt.Errorf("scriptedForwarder: no script for addr %q", addr)
	}
	return s.resp, s.err
}

// twoPeerRegistry returns a registry advertising the "python" tag for
// tenant-1 on two peers, listed bad-first so firstSelector tries the
// degraded peer before the healthy one.
func twoPeerRegistry(badAddr, goodAddr string) *stubNodeRegistry {
	return &stubNodeRegistry{
		nodes: []contract.NodeInfo{
			{NodeID: "peer-bad", Addr: badAddr, Alive: true, Tags: map[string][]string{"tenant-1": {"python"}}},
			{NodeID: "peer-good", Addr: goodAddr, Alive: true, Tags: map[string][]string{"tenant-1": {"python"}}},
		},
	}
}

func noMemberResponse() *DispatchCalloutResponse {
	return &DispatchCalloutResponse{
		Success:        false,
		Error:          "dispatch processor failed",
		ErrorCode:      common.ErrCodeNoComputeMemberForTag,
		ErrorStatus:    http.StatusServiceUnavailable,
		ErrorRetryable: true,
	}
}

// TestClusterDispatcher_FailoverOnTransportError: the selected peer is
// unreachable at the transport level; the dispatcher must retry the other
// healthy tag-matching peer instead of surfacing DISPATCH_FORWARD_FAILED.
func TestClusterDispatcher_FailoverOnTransportError(t *testing.T) {
	local := &stubDispatcher{noMember: true}

	newDispatcher := func(fwd DispatchForwarder) *ClusterDispatcher {
		return NewClusterDispatcher(local, twoPeerRegistry("http://bad", "http://good"), "self-node", firstSelector{}, fwd, 1*time.Second, nil, 0)
	}

	t.Run("processor", func(t *testing.T) {
		fwd := newScriptedForwarder()
		fwd.script("http://bad", nil, errors.New("connection refused"))
		fwd.script("http://good", &DispatchCalloutResponse{Success: true, EntityData: []byte(`{"key":"peer-processed"}`)}, nil)
		d := newDispatcher(fwd)

		result, err := d.DispatchProcessor(testContext(), testEntity(), testProcessor(), "wf", "tr", "tx1")
		if err != nil {
			t.Fatalf("expected failover success, got %v", err)
		}
		if string(result.Data) != `{"key":"peer-processed"}` {
			t.Fatalf("expected peer-processed data, got %s", string(result.Data))
		}
		if len(fwd.calls) != 2 || fwd.calls[0] != "http://bad" || fwd.calls[1] != "http://good" {
			t.Fatalf("expected [bad, good] attempt order, got %v", fwd.calls)
		}
	})

	t.Run("criteria", func(t *testing.T) {
		fwd := newScriptedForwarder()
		fwd.script("http://bad", nil, errors.New("connection refused"))
		matches := true
		fwd.script("http://good", &DispatchCalloutResponse{Success: true, Matches: &matches, Reason: "peer reason"}, nil)
		d := newDispatcher(fwd)

		got, reason, err := d.DispatchCriteria(testContext(), testEntity(), testCriterion(), "TRANSITION", "wf", "tr", "proc", "tx1")
		if err != nil {
			t.Fatalf("expected failover success, got %v", err)
		}
		if !got || reason != "peer reason" {
			t.Fatalf("expected matches=true with peer reason, got %v %q", got, reason)
		}
		if len(fwd.calls) != 2 {
			t.Fatalf("expected 2 attempts, got %v", fwd.calls)
		}
	})

	t.Run("function", func(t *testing.T) {
		fwd := newScriptedForwarder()
		fwd.script("http://bad", nil, errors.New("connection refused"))
		fwd.script("http://good", &DispatchCalloutResponse{Success: true, ResultKind: "Schedule", Result: []byte(`{"fireAfterMs":1000}`)}, nil)
		d := newDispatcher(fwd)

		result, err := d.DispatchFunction(testContext(), testEntity(), testFunction(), "wf", "tr", "tx1")
		if err != nil {
			t.Fatalf("expected failover success, got %v", err)
		}
		if result.Kind != "Schedule" {
			t.Fatalf("expected Kind=Schedule, got %q", result.Kind)
		}
		if len(fwd.calls) != 2 {
			t.Fatalf("expected 2 attempts, got %v", fwd.calls)
		}
	})
}

// TestClusterDispatcher_FailoverOnPeerNoMember: the peer answered but had
// lost its matching calculation member between gossip advertisement and
// forward (NO_COMPUTE_MEMBER_FOR_TAG — nothing executed). The dispatcher
// must try the next tag-matching peer.
func TestClusterDispatcher_FailoverOnPeerNoMember(t *testing.T) {
	local := &stubDispatcher{noMember: true}
	fwd := newScriptedForwarder()
	fwd.script("http://bad", noMemberResponse(), nil)
	fwd.script("http://good", &DispatchCalloutResponse{Success: true, EntityData: []byte(`{"key":"peer-processed"}`)}, nil)
	d := NewClusterDispatcher(local, twoPeerRegistry("http://bad", "http://good"), "self-node", firstSelector{}, fwd, 1*time.Second, nil, 0)

	result, err := d.DispatchProcessor(testContext(), testEntity(), testProcessor(), "wf", "tr", "tx1")
	if err != nil {
		t.Fatalf("expected failover success, got %v", err)
	}
	if string(result.Data) != `{"key":"peer-processed"}` {
		t.Fatalf("expected peer-processed data, got %s", string(result.Data))
	}
	if len(fwd.calls) != 2 || fwd.calls[0] != "http://bad" || fwd.calls[1] != "http://good" {
		t.Fatalf("expected [bad, good] attempt order, got %v", fwd.calls)
	}
}

// TestClusterDispatcher_NoFailoverOnExecutedCalloutFailure: any peer failure
// other than NO_COMPUTE_MEMBER_FOR_TAG means the callout was actually
// dispatched on the peer (e.g. DISPATCH_TIMEOUT — the compute member ran or
// may have run). The dispatcher must NOT re-execute it on another peer; the
// peer's classification propagates unchanged.
func TestClusterDispatcher_NoFailoverOnExecutedCalloutFailure(t *testing.T) {
	local := &stubDispatcher{noMember: true}
	fwd := newScriptedForwarder()
	fwd.script("http://bad", &DispatchCalloutResponse{
		Success:        false,
		Error:          "dispatch processor failed",
		ErrorCode:      common.ErrCodeDispatchTimeout,
		ErrorStatus:    http.StatusServiceUnavailable,
		ErrorRetryable: true,
	}, nil)
	fwd.script("http://good", &DispatchCalloutResponse{Success: true}, nil)
	d := NewClusterDispatcher(local, twoPeerRegistry("http://bad", "http://good"), "self-node", firstSelector{}, fwd, 1*time.Second, nil, 0)

	_, err := d.DispatchProcessor(testContext(), testEntity(), testProcessor(), "wf", "tr", "tx1")
	if err == nil {
		t.Fatal("expected error")
	}
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *common.AppError, got %T: %v", err, err)
	}
	if appErr.Code != common.ErrCodeDispatchTimeout {
		t.Fatalf("expected code %s, got %s", common.ErrCodeDispatchTimeout, appErr.Code)
	}
	if len(fwd.calls) != 1 || fwd.calls[0] != "http://bad" {
		t.Fatalf("expected exactly one attempt against the first peer, got %v", fwd.calls)
	}
}

// TestClusterDispatcher_FailoverExhaustion: when every tag-matching peer
// fails, each is tried at most once and the LAST failure surfaces with the
// same taxonomy the single-attempt path produced.
func TestClusterDispatcher_FailoverExhaustion(t *testing.T) {
	local := &stubDispatcher{noMember: true}

	t.Run("all_transport_errors_surface_forward_failed", func(t *testing.T) {
		fwd := newScriptedForwarder()
		fwd.script("http://bad", nil, errors.New("connection refused"))
		fwd.script("http://good", nil, errors.New("connection refused"))
		d := NewClusterDispatcher(local, twoPeerRegistry("http://bad", "http://good"), "self-node", firstSelector{}, fwd, 1*time.Second, nil, 0)

		_, err := d.DispatchProcessor(testContext(), testEntity(), testProcessor(), "wf", "tr", "tx1")
		if err == nil {
			t.Fatal("expected error")
		}
		var appErr *common.AppError
		if !errors.As(err, &appErr) {
			t.Fatalf("expected *common.AppError, got %T: %v", err, err)
		}
		if appErr.Code != common.ErrCodeDispatchForwardFailed {
			t.Fatalf("expected code %s, got %s", common.ErrCodeDispatchForwardFailed, appErr.Code)
		}
		if appErr.Status != http.StatusServiceUnavailable || !appErr.Retryable {
			t.Fatalf("expected retryable 503, got status=%d retryable=%v", appErr.Status, appErr.Retryable)
		}
		if len(fwd.calls) != 2 {
			t.Fatalf("expected each peer tried exactly once, got %v", fwd.calls)
		}
	})

	t.Run("all_peers_lost_member_surface_no_compute_member", func(t *testing.T) {
		fwd := newScriptedForwarder()
		fwd.script("http://bad", noMemberResponse(), nil)
		fwd.script("http://good", noMemberResponse(), nil)
		d := NewClusterDispatcher(local, twoPeerRegistry("http://bad", "http://good"), "self-node", firstSelector{}, fwd, 1*time.Second, nil, 0)

		_, err := d.DispatchProcessor(testContext(), testEntity(), testProcessor(), "wf", "tr", "tx1")
		if err == nil {
			t.Fatal("expected error")
		}
		var appErr *common.AppError
		if !errors.As(err, &appErr) {
			t.Fatalf("expected *common.AppError, got %T: %v", err, err)
		}
		if appErr.Code != common.ErrCodeNoComputeMemberForTag {
			t.Fatalf("expected code %s, got %s", common.ErrCodeNoComputeMemberForTag, appErr.Code)
		}
		if appErr.Status != http.StatusServiceUnavailable || !appErr.Retryable {
			t.Fatalf("expected retryable 503, got status=%d retryable=%v", appErr.Status, appErr.Retryable)
		}
		if len(fwd.calls) != 2 {
			t.Fatalf("expected each peer tried exactly once, got %v", fwd.calls)
		}
	})
}

// cancellingForwarder cancels the request context and then fails at the
// transport level, simulating the caller's deadline expiring mid-forward.
type cancellingForwarder struct {
	cancel context.CancelFunc
	calls  int
}

func (f *cancellingForwarder) ForwardCallout(_ context.Context, _ string, _ DispatchCalloutRequest) (*DispatchCalloutResponse, error) {
	f.calls++
	f.cancel()
	return nil, errors.New("context canceled")
}

// TestClusterDispatcher_CtxCancelledMidForwardKeepsTaxonomy: a context that
// dies DURING the forward attempt must surface the same retryable 503
// DISPATCH_FORWARD_FAILED the single-attempt path always produced — not a
// bare ctx error that classifyWorkflowError collapses into a non-retryable
// 400 WORKFLOW_FAILED. The dispatcher must also stop failing over: no
// further peers are attempted on a dead context.
func TestClusterDispatcher_CtxCancelledMidForwardKeepsTaxonomy(t *testing.T) {
	local := &stubDispatcher{noMember: true}
	ctx, cancel := context.WithCancel(testContext())
	defer cancel()

	fwd := &cancellingForwarder{cancel: cancel}
	d := NewClusterDispatcher(local, twoPeerRegistry("http://bad", "http://good"), "self-node", firstSelector{}, fwd, 1*time.Second, nil, 0)

	_, err := d.DispatchProcessor(ctx, testEntity(), testProcessor(), "wf", "tr", "tx1")
	if err == nil {
		t.Fatal("expected error")
	}
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *common.AppError, got %T: %v", err, err)
	}
	if appErr.Code != common.ErrCodeDispatchForwardFailed {
		t.Fatalf("expected code %s, got %s", common.ErrCodeDispatchForwardFailed, appErr.Code)
	}
	if appErr.Status != http.StatusServiceUnavailable || !appErr.Retryable {
		t.Fatalf("expected retryable 503, got status=%d retryable=%v", appErr.Status, appErr.Retryable)
	}
	if fwd.calls != 1 {
		t.Fatalf("expected no failover on a dead context, got %d attempts", fwd.calls)
	}
}

// TestClusterDispatcher_FailoverOverWire is the acceptance-criterion test:
// one degraded peer (listener closed — connection refused) and one healthy
// peer both advertise the tag; cross-node dispatch succeeds via failover
// through the real HTTP + AEAD forward path.
func TestClusterDispatcher_FailoverOverWire(t *testing.T) {
	auth, _ := NewAEADPeerAuth(testSecret32, 30*time.Second)

	peerLocal := &stubDispatcher{
		processorResp: &spi.Entity{
			Meta: testEntity().Meta,
			Data: []byte(`{"key":"peer-processed"}`),
		},
	}
	handler := NewDispatchHandler(peerLocal, auth)
	mux := http.NewServeMux()
	handler.Register(mux)
	healthy := httptest.NewServer(mux)
	defer healthy.Close()

	// A server that is immediately closed: its address refuses connections.
	degraded := httptest.NewServer(http.NotFoundHandler())
	degradedURL := degraded.URL
	degraded.Close()

	local := &stubDispatcher{noMember: true}
	registry := twoPeerRegistry(degradedURL, healthy.URL)
	forwarder := NewHTTPForwarder(auth, 5*time.Second).AllowLoopbackForTesting()
	d := NewClusterDispatcher(local, registry, "self-node", firstSelector{}, forwarder, 1*time.Second, nil, 0)

	result, err := d.DispatchProcessor(testContext(), testEntity(), testProcessor(), "wf", "tr", "tx1")
	if err != nil {
		t.Fatalf("expected failover success, got %v", err)
	}
	if string(result.Data) != `{"key":"peer-processed"}` {
		t.Fatalf("expected peer-processed data, got %s", string(result.Data))
	}
}
