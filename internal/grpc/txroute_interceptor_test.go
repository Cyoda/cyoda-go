package grpc

import (
	"context"
	"encoding/json"
	"io"
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
func (f fakeRouteRegistry) List(context.Context) ([]contract.NodeInfo, error) { return nil, nil }
func (f fakeRouteRegistry) Deregister(context.Context, string) error         { return nil }

// fakeJoinTM satisfies spi.TransactionManager, injecting a joined tx into ctx.
type fakeJoinTM struct {
	spi.TransactionManager
}

func (fakeJoinTM) Join(ctx context.Context, txID string) (context.Context, error) {
	return spi.WithTransaction(ctx, &spi.TransactionState{ID: txID}), nil
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
	ic := newTxRouteInterceptor(s, fakeRouteRegistry{}, "local", fakeJoinTM{})
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

// A token for a different, alive node triggers ForwardEntityManage to its addr.
func TestTxRouteInterceptor_ForeignProxies(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	tok, _ := s.Issue("node-B", "tx-9", time.Now().Add(time.Minute))
	reg := fakeRouteRegistry{nodes: map[string]contract.NodeInfo{
		"node-B": {NodeID: "node-B", Addr: "http://node-b:8080", Alive: true},
	}}
	ic := newTxRouteInterceptor(s, reg, "node-A", fakeJoinTM{})

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
	if gotAddr != "http://node-b:8080" {
		t.Fatalf("expected forward to node-b, got %q", gotAddr)
	}
	if ce, ok := resp.(*cepb.CloudEvent); !ok || ce.Id != "forwarded" {
		t.Fatalf("expected forwarded response verbatim, got %v", resp)
	}
}

// A tampered/invalid token yields the EntityManage error envelope, not a raw
// gRPC status, and never reaches the handler.
func TestTxRouteInterceptor_BadTokenEnvelope(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	ic := newTxRouteInterceptor(s, fakeRouteRegistry{}, "local", fakeJoinTM{})
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

// Non-EntityManage methods pass through untouched (no token processing).
func TestTxRouteInterceptor_NonEntityManagePassThrough(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	ic := newTxRouteInterceptor(s, fakeRouteRegistry{}, "local", fakeJoinTM{})
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
	ic := newTxRouteInterceptor(s, fakeRouteRegistry{}, "local", fakeJoinTM{})
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

// A token for a foreign node re-issues the server-stream to the owner and
// copies frames back to the caller's stream.
func TestTxRouteInterceptor_StreamForeignProxies(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	tok, _ := s.Issue("node-B", "tx-11", time.Now().Add(time.Minute))
	reg := fakeRouteRegistry{nodes: map[string]contract.NodeInfo{
		"node-B": {NodeID: "node-B", Addr: "http://node-b:8080", Alive: true},
	}}
	ic := newTxRouteInterceptor(s, reg, "node-A", fakeJoinTM{})

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
	if gotAddr != "http://node-b:8080" {
		t.Fatalf("expected stream forward to node-b, got %q", gotAddr)
	}
	if len(ss.sent) != 2 || ss.sent[0].Id != "f1" || ss.sent[1].Id != "f2" {
		t.Fatalf("expected 2 forwarded frames f1,f2; got %+v", ss.sent)
	}
}
