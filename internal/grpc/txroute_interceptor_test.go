package grpc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	cyodapb "github.com/cyoda-platform/cyoda-go/api/grpc/cyoda"
	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/proxy"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// --- test doubles -----------------------------------------------------------

// fakeRouteRegistry is a minimal contract.NodeRegistry backed by a static map.
type fakeRouteRegistry struct {
	nodes map[string]contract.NodeInfo
}

func (f fakeRouteRegistry) Register(context.Context, string, string) error { return nil }
func (f fakeRouteRegistry) Lookup(_ context.Context, nodeID string) (string, bool, error) {
	n, ok := f.nodes[nodeID]
	if !ok {
		return "", false, nil
	}
	return n.Addr, n.Alive, nil
}
func (f fakeRouteRegistry) List(_ context.Context) ([]contract.NodeInfo, error) {
	nodes := make([]contract.NodeInfo, 0, len(f.nodes))
	for _, n := range f.nodes {
		nodes = append(nodes, n)
	}
	return nodes, nil
}
func (f fakeRouteRegistry) Deregister(context.Context, string) error { return nil }

// fakeJoinTM satisfies spi.TransactionManager, injecting a joined tx into ctx.
type fakeJoinTM struct {
	spi.TransactionManager
}

func (fakeJoinTM) Join(ctx context.Context, txID string) (context.Context, error) {
	return spi.WithTransaction(ctx, &spi.TransactionState{ID: txID}), nil
}

// fakeErrTM satisfies spi.TransactionManager, failing Join with a fixed error so
// the local-join path exercises JoinFromToken's error mapping in the interceptor.
type fakeErrTM struct {
	spi.TransactionManager
	err error
}

func (f fakeErrTM) Join(context.Context, string) (context.Context, error) {
	return nil, f.err
}

// fakeServerStream is a minimal googlegrpc.ServerStream double.
type fakeServerStream struct {
	ctx  context.Context
	recv []*cepb.CloudEvent // queued messages RecvMsg will yield
	sent []*cepb.CloudEvent // messages SendMsg captured
}

func (s *fakeServerStream) SetHeader(metadata.MD) error  { return nil }
func (s *fakeServerStream) SendHeader(metadata.MD) error { return nil }
func (s *fakeServerStream) SetTrailer(metadata.MD)       {}
func (s *fakeServerStream) Context() context.Context     { return s.ctx }
func (s *fakeServerStream) SendMsg(m any) error {
	s.sent = append(s.sent, m.(*cepb.CloudEvent))
	return nil
}
func (s *fakeServerStream) RecvMsg(m any) error {
	if len(s.recv) == 0 {
		return io.EOF
	}
	proto.Merge(m.(*cepb.CloudEvent), s.recv[0])
	s.recv = s.recv[1:]
	return nil
}

// fakeClientStream satisfies googlegrpc.ServerStreamingClient[cepb.CloudEvent].
type fakeClientStream struct {
	googlegrpc.ClientStream
	frames []*cepb.CloudEvent
	idx    int
}

func (c *fakeClientStream) Recv() (*cepb.CloudEvent, error) {
	if c.idx >= len(c.frames) {
		return nil, io.EOF
	}
	f := c.frames[c.idx]
	c.idx++
	return f, nil
}

func entityManageInfo() *googlegrpc.UnaryServerInfo {
	return &googlegrpc.UnaryServerInfo{FullMethod: cyodapb.CloudEventsService_EntityManage_FullMethodName}
}

func entityManageCollectionInfo() *googlegrpc.StreamServerInfo {
	return &googlegrpc.StreamServerInfo{FullMethod: cyodapb.CloudEventsService_EntityManageCollection_FullMethodName}
}

func decodeTxResp(t *testing.T, ce *cepb.CloudEvent) events.EntityTransactionResponseJson {
	t.Helper()
	_, payload, err := ParseCloudEvent(ce)
	if err != nil {
		t.Fatalf("ParseCloudEvent: %v", err)
	}
	var r events.EntityTransactionResponseJson
	if err := json.Unmarshal(payload, &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return r
}

// --- unary ------------------------------------------------------------------

// A valid self-node token results in a joined ctx handed to the handler.
func TestTxRouteInterceptor_LocalJoin(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	tok, _ := s.Issue("local", "tx-1", time.Now().Add(time.Minute))
	ic := newTxRouteInterceptor(s, fakeRouteRegistry{}, "local", fakeJoinTM{}, 9090)
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("tx-token", tok))

	var sawTx string
	handler := func(ctx context.Context, req any) (any, error) {
		if tx := spi.GetTransaction(ctx); tx != nil {
			sawTx = tx.ID
		}
		return "ok", nil
	}
	resp, err := ic.unary()(ctx, &cepb.CloudEvent{Id: "req-1"}, entityManageInfo(), handler)
	if err != nil || sawTx != "tx-1" || resp != "ok" {
		t.Fatalf("expected local join tx-1, sawTx=%q resp=%v err=%v", sawTx, resp, err)
	}
}

// A token for a different, alive node triggers ForwardEntityManage to its gRPC addr.
// When the peer has no explicit GRPCAddr, the addr is derived from its HTTP host + local gRPC port.
func TestTxRouteInterceptor_ForeignProxies(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	tok, _ := s.Issue("node-B", "tx-9", time.Now().Add(time.Minute))
	reg := fakeRouteRegistry{nodes: map[string]contract.NodeInfo{
		"node-B": {NodeID: "node-B", Addr: "http://node-b:8080", Alive: true},
	}}
	ic := newTxRouteInterceptor(s, reg, "node-A", fakeJoinTM{}, 9090)

	var gotAddr string
	ic.forwardUnary = func(_ context.Context, _ *proxy.ClientPool, addr string, ce *cepb.CloudEvent) (*cepb.CloudEvent, error) {
		gotAddr = addr
		return &cepb.CloudEvent{Id: "forwarded"}, nil
	}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("tx-token", tok))

	handlerCalled := false
	handler := func(context.Context, any) (any, error) { handlerCalled = true; return nil, nil }
	resp, err := ic.unary()(ctx, &cepb.CloudEvent{Id: "req-1"}, entityManageInfo(), handler)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if handlerCalled {
		t.Fatal("handler must not run for a proxied call")
	}
	// The interceptor must dial the peer's derived gRPC addr (HTTP host + local gRPC port),
	// not the HTTP addr, so the forwarded call reaches the gRPC listener.
	if gotAddr != "node-b:9090" {
		t.Fatalf("expected forward to derived gRPC addr %q, got %q", "node-b:9090", gotAddr)
	}
	if ce, ok := resp.(*cepb.CloudEvent); !ok || ce.Id != "forwarded" {
		t.Fatalf("expected forwarded response verbatim, got %v", resp)
	}
}

// A token for a peer that advertises an explicit GRPCAddr uses it verbatim.
func TestTxRouteInterceptor_ForeignProxiesAdvertisedGRPCAddr(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	tok, _ := s.Issue("node-B", "tx-10", time.Now().Add(time.Minute))
	reg := fakeRouteRegistry{nodes: map[string]contract.NodeInfo{
		"node-B": {NodeID: "node-B", Addr: "http://node-b:8080", GRPCAddr: "node-b:19090", Alive: true},
	}}
	ic := newTxRouteInterceptor(s, reg, "node-A", fakeJoinTM{}, 9090)

	var gotAddr string
	ic.forwardUnary = func(_ context.Context, _ *proxy.ClientPool, addr string, _ *cepb.CloudEvent) (*cepb.CloudEvent, error) {
		gotAddr = addr
		return &cepb.CloudEvent{Id: "fwd"}, nil
	}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("tx-token", tok))

	_, err := ic.unary()(ctx, &cepb.CloudEvent{Id: "req-adv"}, entityManageInfo(), func(context.Context, any) (any, error) {
		t.Fatal("handler must not run for a proxied call")
		return nil, nil
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if gotAddr != "node-b:19090" {
		t.Fatalf("expected advertised gRPC addr %q, got %q", "node-b:19090", gotAddr)
	}
}

// A tampered/invalid token yields the EntityManage error envelope, not a raw
// gRPC status, and never reaches the handler.
func TestTxRouteInterceptor_BadTokenEnvelope(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	ic := newTxRouteInterceptor(s, fakeRouteRegistry{}, "local", fakeJoinTM{}, 9090)
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("tx-token", "garbage.token"))

	handler := func(context.Context, any) (any, error) {
		t.Fatal("handler must not run for a bad token")
		return nil, nil
	}
	resp, err := ic.unary()(ctx, &cepb.CloudEvent{Id: "req-x"}, entityManageInfo(), handler)
	if err != nil {
		t.Fatalf("expected envelope response, got gRPC err: %v", err)
	}
	ce, ok := resp.(*cepb.CloudEvent)
	if !ok {
		t.Fatalf("expected *cepb.CloudEvent, got %T", resp)
	}
	r := decodeTxResp(t, ce)
	if r.Success {
		t.Fatal("expected Success=false")
	}
	if r.RequestID != "req-x" {
		t.Fatalf("expected RequestID req-x, got %q", r.RequestID)
	}
	if r.Error == nil || r.Error.Message == "" {
		t.Fatalf("expected error detail, got %+v", r.Error)
	}
}

// assertEnvelopeCode decodes an EntityManage error envelope and asserts it is a
// failure whose operational code (rendered as the "CODE: detail" message prefix
// on the CLIENT_ERROR class) matches wantCode. Covers the gRPC entry point's
// loud-fail contract for one callback-token error class (feature #287).
func assertEnvelopeCode(t *testing.T, resp any, wantReqID, wantCode string) {
	t.Helper()
	ce, ok := resp.(*cepb.CloudEvent)
	if !ok {
		t.Fatalf("expected *cepb.CloudEvent, got %T", resp)
	}
	r := decodeTxResp(t, ce)
	if r.Success {
		t.Fatal("expected Success=false")
	}
	if r.RequestID != wantReqID {
		t.Fatalf("RequestID = %q; want %q", r.RequestID, wantReqID)
	}
	if r.Error == nil {
		t.Fatal("expected error detail, got nil")
	}
	if !strings.HasPrefix(r.Error.Message, wantCode+":") {
		t.Fatalf("Error.Message = %q; want %q prefix", r.Error.Message, wantCode)
	}
}

// An expired token yields the EntityManage error envelope carrying
// TRANSACTION_EXPIRED (mapped from token.ErrTokenExpired), never a raw status.
func TestTxRouteInterceptor_ExpiredEnvelope(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	tok, _ := s.Issue("local", "tx-exp", time.Now().Add(-time.Second))
	ic := newTxRouteInterceptor(s, fakeRouteRegistry{}, "local", fakeJoinTM{}, 9090)
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("tx-token", tok))

	resp, err := ic.unary()(ctx, &cepb.CloudEvent{Id: "req-exp"}, entityManageInfo(), func(context.Context, any) (any, error) {
		t.Fatal("handler must not run for an expired token")
		return nil, nil
	})
	if err != nil {
		t.Fatalf("expected envelope response, got gRPC err: %v", err)
	}
	assertEnvelopeCode(t, resp, "req-exp", "TRANSACTION_EXPIRED")
}

// A token signed by a foreign secret yields UNAUTHORIZED in the envelope.
func TestTxRouteInterceptor_ForgedEnvelope(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	forger, _ := token.NewSigner([]byte("forged-secret-key-at-least-32-byte!"))
	tok, _ := forger.Issue("local", "tx-forged", time.Now().Add(time.Minute))
	ic := newTxRouteInterceptor(s, fakeRouteRegistry{}, "local", fakeJoinTM{}, 9090)
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("tx-token", tok))

	resp, err := ic.unary()(ctx, &cepb.CloudEvent{Id: "req-forged"}, entityManageInfo(), func(context.Context, any) (any, error) {
		t.Fatal("handler must not run for a forged token")
		return nil, nil
	})
	if err != nil {
		t.Fatalf("expected envelope response, got gRPC err: %v", err)
	}
	assertEnvelopeCode(t, resp, "req-forged", "UNAUTHORIZED")
}

// A valid self-node token whose transaction is unknown/closed yields
// TRANSACTION_NOT_FOUND (mapped from spi.ErrTxNotFound in JoinFromToken).
func TestTxRouteInterceptor_NotFoundEnvelope(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	tok, _ := s.Issue("local", "tx-gone", time.Now().Add(time.Minute))
	ic := newTxRouteInterceptor(s, fakeRouteRegistry{}, "local", fakeErrTM{err: spi.ErrTxNotFound}, 9090)
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("tx-token", tok))

	resp, err := ic.unary()(ctx, &cepb.CloudEvent{Id: "req-nf"}, entityManageInfo(), func(context.Context, any) (any, error) {
		t.Fatal("handler must not run when the tx is not found")
		return nil, nil
	})
	if err != nil {
		t.Fatalf("expected envelope response, got gRPC err: %v", err)
	}
	assertEnvelopeCode(t, resp, "req-nf", "TRANSACTION_NOT_FOUND")
}

// A valid token naming a peer that the registry reports as dead yields
// TRANSACTION_NODE_UNAVAILABLE (503) in the envelope — the gRPC-entry-point
// counterpart of the HTTP proxy's TestHTTPProxy_TokenForDeadNode_Returns503.
// This is the "owner node down" callback case (#287): a compute-node callback
// (EntityManage) lands on a non-owner node, but the owner is unreachable, so the
// B→A forward cannot proceed and the client sees a clean operational code rather
// than a raw gRPC error. Covers classifyRouteErr's proxy.ErrNodeUnavailable arm.
func TestTxRouteInterceptor_DeadNodeUnavailableEnvelope(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	tok, _ := s.Issue("node-owner", "tx-owner-down", time.Now().Add(time.Minute))
	reg := fakeRouteRegistry{nodes: map[string]contract.NodeInfo{
		"node-owner": {NodeID: "node-owner", Addr: "http://node-owner:8080", Alive: false},
	}}
	ic := newTxRouteInterceptor(s, reg, "local", fakeJoinTM{}, 9090)
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("tx-token", tok))

	resp, err := ic.unary()(ctx, &cepb.CloudEvent{Id: "req-down"}, entityManageInfo(), func(context.Context, any) (any, error) {
		t.Fatal("handler must not run when the owner node is unavailable")
		return nil, nil
	})
	if err != nil {
		t.Fatalf("expected envelope response, got gRPC err: %v", err)
	}
	assertEnvelopeCode(t, resp, "req-down", "TRANSACTION_NODE_UNAVAILABLE")
}

// A valid self-node token for a transaction owned by a different tenant yields
// FORBIDDEN (mapped from spi.ErrTxTenantMismatch in JoinFromToken).
func TestTxRouteInterceptor_TenantMismatchEnvelope(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	tok, _ := s.Issue("local", "tx-other-tenant", time.Now().Add(time.Minute))
	ic := newTxRouteInterceptor(s, fakeRouteRegistry{}, "local", fakeErrTM{err: spi.ErrTxTenantMismatch}, 9090)
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("tx-token", tok))

	resp, err := ic.unary()(ctx, &cepb.CloudEvent{Id: "req-tenant"}, entityManageInfo(), func(context.Context, any) (any, error) {
		t.Fatal("handler must not run for a cross-tenant token")
		return nil, nil
	})
	if err != nil {
		t.Fatalf("expected envelope response, got gRPC err: %v", err)
	}
	assertEnvelopeCode(t, resp, "req-tenant", "FORBIDDEN")
}

// Non-EntityManage methods pass through untouched (no token processing).
func TestTxRouteInterceptor_NonEntityManagePassThrough(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	ic := newTxRouteInterceptor(s, fakeRouteRegistry{}, "local", fakeJoinTM{}, 9090)
	// A bad token present, but on a method we don't route: must be ignored.
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("tx-token", "garbage.token"))

	called := false
	handler := func(ctx context.Context, req any) (any, error) {
		called = true
		if spi.GetTransaction(ctx) != nil {
			t.Fatal("no join expected for non-routed method")
		}
		return "ok", nil
	}
	info := &googlegrpc.UnaryServerInfo{FullMethod: cyodapb.CloudEventsService_EntitySearch_FullMethodName}
	resp, err := ic.unary()(ctx, nil, info, handler)
	if err != nil || !called || resp != "ok" {
		t.Fatalf("expected passthrough, called=%v resp=%v err=%v", called, resp, err)
	}
}

// --- stream -----------------------------------------------------------------

// A valid self-node token joins the tx onto the stream context.
func TestTxRouteInterceptor_StreamLocalJoin(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	tok, _ := s.Issue("local", "tx-7", time.Now().Add(time.Minute))
	ic := newTxRouteInterceptor(s, fakeRouteRegistry{}, "local", fakeJoinTM{}, 9090)
	baseCtx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("tx-token", tok))
	ss := &fakeServerStream{ctx: baseCtx}

	var sawTx string
	handler := func(_ any, stream googlegrpc.ServerStream) error {
		if tx := spi.GetTransaction(stream.Context()); tx != nil {
			sawTx = tx.ID
		}
		return nil
	}
	if err := ic.stream()(nil, ss, entityManageCollectionInfo(), handler); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if sawTx != "tx-7" {
		t.Fatalf("expected joined tx-7 on stream ctx, got %q", sawTx)
	}
}

// A non-EntityManageCollection stream method with a bad token must pass through
// untouched — no routing, no envelope, handler invoked directly.
func TestTxRouteInterceptor_StreamNonEntityManagePassThrough(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	ic := newTxRouteInterceptor(s, fakeRouteRegistry{}, "local", fakeJoinTM{}, 9090)
	baseCtx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("tx-token", "garbage.token"))
	ss := &fakeServerStream{ctx: baseCtx}

	called := false
	handler := func(_ any, stream googlegrpc.ServerStream) error {
		called = true
		if spi.GetTransaction(stream.Context()) != nil {
			t.Fatal("no join expected for non-routed stream method")
		}
		return nil
	}
	info := &googlegrpc.StreamServerInfo{FullMethod: cyodapb.CloudEventsService_EntitySearch_FullMethodName}
	if err := ic.stream()(nil, ss, info, handler); err != nil {
		t.Fatalf("expected passthrough, got err: %v", err)
	}
	if !called {
		t.Fatal("expected handler to be called for non-routed stream method")
	}
	if len(ss.sent) != 0 {
		t.Fatalf("expected no frames sent for passthrough, got %d", len(ss.sent))
	}
}

// A token for a foreign node re-issues the server-stream to the owner's gRPC addr
// (derived from HTTP host + local gRPC port) and copies frames back to the caller's stream.
func TestTxRouteInterceptor_StreamForeignProxies(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	tok, _ := s.Issue("node-B", "tx-11", time.Now().Add(time.Minute))
	reg := fakeRouteRegistry{nodes: map[string]contract.NodeInfo{
		"node-B": {NodeID: "node-B", Addr: "http://node-b:8080", Alive: true},
	}}
	ic := newTxRouteInterceptor(s, reg, "node-A", fakeJoinTM{}, 9090)

	var gotAddr string
	ic.forwardStream = func(_ context.Context, _ *proxy.ClientPool, addr string, _ *cepb.CloudEvent) (googlegrpc.ServerStreamingClient[cepb.CloudEvent], error) {
		gotAddr = addr
		return &fakeClientStream{frames: []*cepb.CloudEvent{{Id: "f1"}, {Id: "f2"}}}, nil
	}
	baseCtx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("tx-token", tok))
	ss := &fakeServerStream{ctx: baseCtx, recv: []*cepb.CloudEvent{{Id: "req-2"}}}

	handler := func(any, googlegrpc.ServerStream) error {
		t.Fatal("handler must not run for a proxied stream")
		return nil
	}
	if err := ic.stream()(nil, ss, entityManageCollectionInfo(), handler); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	// Stream must also use the derived gRPC addr, not the HTTP addr.
	if gotAddr != "node-b:9090" {
		t.Fatalf("expected stream forward to derived gRPC addr %q, got %q", "node-b:9090", gotAddr)
	}
	if len(ss.sent) != 2 || ss.sent[0].Id != "f1" || ss.sent[1].Id != "f2" {
		t.Fatalf("expected 2 forwarded frames f1,f2; got %+v", ss.sent)
	}
}

// A transport error from forwardUnary must be enveloped (not returned as a raw
// gRPC status), and the envelope must echo the original RequestID.
func TestTxRouteInterceptor_ForeignProxiesForwardErr(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	tok, _ := s.Issue("node-B", "tx-99", time.Now().Add(time.Minute))
	reg := fakeRouteRegistry{nodes: map[string]contract.NodeInfo{
		"node-B": {NodeID: "node-B", Addr: "http://node-b:8080", Alive: true},
	}}
	ic := newTxRouteInterceptor(s, reg, "node-A", fakeJoinTM{}, 9090)
	ic.forwardUnary = func(_ context.Context, _ *proxy.ClientPool, _ string, _ *cepb.CloudEvent) (*cepb.CloudEvent, error) {
		return nil, errors.New("peer unreachable")
	}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("tx-token", tok))

	resp, err := ic.unary()(ctx, &cepb.CloudEvent{Id: "req-fwd-err"}, entityManageInfo(), func(context.Context, any) (any, error) {
		t.Fatal("handler must not run")
		return nil, nil
	})
	if err != nil {
		t.Fatalf("expected envelope response, got raw gRPC err: %v", err)
	}
	ce, ok := resp.(*cepb.CloudEvent)
	if !ok {
		t.Fatalf("expected *cepb.CloudEvent, got %T", resp)
	}
	r := decodeTxResp(t, ce)
	if r.Success {
		t.Fatal("expected Success=false")
	}
	if r.RequestID != "req-fwd-err" {
		t.Fatalf("expected RequestID req-fwd-err, got %q", r.RequestID)
	}
}

// A transport error from forwardStream must be enveloped with the RequestID
// of the already-consumed inbound message (not empty from a second RecvMsg).
func TestTxRouteInterceptor_StreamForwardErrPreservesRequestID(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	tok, _ := s.Issue("node-B", "tx-88", time.Now().Add(time.Minute))
	reg := fakeRouteRegistry{nodes: map[string]contract.NodeInfo{
		"node-B": {NodeID: "node-B", Addr: "http://node-b:8080", Alive: true},
	}}
	ic := newTxRouteInterceptor(s, reg, "node-A", fakeJoinTM{}, 9090)
	ic.forwardStream = func(_ context.Context, _ *proxy.ClientPool, _ string, _ *cepb.CloudEvent) (googlegrpc.ServerStreamingClient[cepb.CloudEvent], error) {
		return nil, errors.New("peer unreachable")
	}
	baseCtx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("tx-token", tok))
	ss := &fakeServerStream{ctx: baseCtx, recv: []*cepb.CloudEvent{{Id: "stream-req-42"}}}

	handler := func(any, googlegrpc.ServerStream) error {
		t.Fatal("handler must not run")
		return nil
	}
	if err := ic.stream()(nil, ss, entityManageCollectionInfo(), handler); err != nil {
		t.Fatalf("expected envelope on stream, got raw err: %v", err)
	}
	if len(ss.sent) != 1 {
		t.Fatalf("expected 1 envelope frame, got %d", len(ss.sent))
	}
	r := decodeTxResp(t, ss.sent[0])
	if r.Success {
		t.Fatal("expected Success=false")
	}
	if r.RequestID != "stream-req-42" {
		t.Fatalf("expected RequestID stream-req-42, got %q", r.RequestID)
	}
}
