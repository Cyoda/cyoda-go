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
	"github.com/cyoda-platform/cyoda-go/internal/domain/txjoin"
	"github.com/cyoda-platform/cyoda-go/internal/fence"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
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
func (f fakeRouteRegistry) Changed() <-chan struct{}                 { return nil }

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

// fakeServerStream is a minimal googlegrpc.ServerStream double. request is the
// one request message of a server-streaming RPC (recv is the same queue, for
// tests that read like a sequence); onRecv and onSend observe each end of the
// stream as it is used.
type fakeServerStream struct {
	ctx     context.Context
	request *cepb.CloudEvent   // the single request message, yielded once
	recv    []*cepb.CloudEvent // queued messages RecvMsg will yield
	sent    []*cepb.CloudEvent // messages SendMsg captured
	onRecv  func()
	onSend  func()
}

func newFakeServerStream(ctx context.Context) *fakeServerStream {
	return &fakeServerStream{ctx: ctx}
}

func (s *fakeServerStream) SetHeader(metadata.MD) error  { return nil }
func (s *fakeServerStream) SendHeader(metadata.MD) error { return nil }
func (s *fakeServerStream) SetTrailer(metadata.MD)       {}
func (s *fakeServerStream) Context() context.Context     { return s.ctx }
func (s *fakeServerStream) SendMsg(m any) error {
	if s.onSend != nil {
		s.onSend()
	}
	s.sent = append(s.sent, m.(*cepb.CloudEvent))
	return nil
}
func (s *fakeServerStream) RecvMsg(m any) error {
	if s.onRecv != nil {
		s.onRecv()
	}
	if s.request != nil {
		proto.Merge(m.(*cepb.CloudEvent), s.request)
		s.request = nil
		return nil
	}
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

// gatedFence returns a fence and the gate registry its wait takes: the join
// layer must take the lock the fence waits on, so the two share one registry.
func gatedFence() (*fence.Fence, *txgate.Registry) {
	gate := txgate.New()
	return fence.New(gate), gate
}

// mustJoiner builds the Joiner the interceptor takes, over signer, txMgr and a
// fence whose wait takes gate's locks. A nil meter (the no-op meter).
func mustJoiner(t *testing.T, s *token.Signer, txMgr spi.TransactionManager, f *fence.Fence, gate *txgate.Registry) *txjoin.Joiner {
	t.Helper()
	j, err := txjoin.NewJoiner(s, txMgr, f, gate, nil)
	if err != nil {
		t.Fatalf("NewJoiner: %v", err)
	}
	return j
}

// noCalloutJoiner is the join layer over a fence that knows no callout.
func noCalloutJoiner(t *testing.T, s *token.Signer, txMgr spi.TransactionManager) *txjoin.Joiner {
	t.Helper()
	f, gate := gatedFence()
	return mustJoiner(t, s, txMgr, f, gate)
}

// liveRouteFence returns a fence on which the callout of txID is in progress at
// major 1, the gate its wait takes, and the pass claims that name it.
func liveRouteFence(t *testing.T, txID string) (*fence.Fence, *txgate.Registry, token.Claims) {
	t.Helper()
	f, gate := gatedFence()
	calloutID := "req-" + txID
	_, end := f.Begin(context.Background(), calloutID, txID, nil)
	t.Cleanup(end)
	f.Advance(calloutID, 1)
	return f, gate, token.Claims{NodeID: "local", TxRef: txID, ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: calloutID, Major: 1}
}

func entityManageInfo() *googlegrpc.UnaryServerInfo {
	return &googlegrpc.UnaryServerInfo{FullMethod: cyodapb.CloudEventsService_EntityManage_FullMethodName}
}

func entityManageCollectionInfo() *googlegrpc.StreamServerInfo {
	return &googlegrpc.StreamServerInfo{FullMethod: cyodapb.CloudEventsService_EntityManageCollection_FullMethodName}
}

func entitySearchInfo() *googlegrpc.UnaryServerInfo {
	return &googlegrpc.UnaryServerInfo{FullMethod: cyodapb.CloudEventsService_EntitySearch_FullMethodName}
}

func entitySearchCollectionInfo() *googlegrpc.StreamServerInfo {
	return &googlegrpc.StreamServerInfo{FullMethod: cyodapb.CloudEventsService_EntitySearchCollection_FullMethodName}
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

// decodeEntityResp decodes a unary interceptor result as an EntityResponse — the
// error-envelope shape used by the search RPCs (EntitySearch / EntitySearchCollection).
func decodeEntityResp(t *testing.T, resp any) events.EntityResponseJson {
	t.Helper()
	ce, ok := resp.(*cepb.CloudEvent)
	if !ok {
		t.Fatalf("expected *cepb.CloudEvent, got %T", resp)
	}
	return decodeEntityRespCE(t, ce)
}

func decodeEntityRespCE(t *testing.T, ce *cepb.CloudEvent) events.EntityResponseJson {
	t.Helper()
	_, payload, err := ParseCloudEvent(ce)
	if err != nil {
		t.Fatalf("ParseCloudEvent: %v", err)
	}
	var r events.EntityResponseJson
	if err := json.Unmarshal(payload, &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return r
}

// --- unary ------------------------------------------------------------------

// A valid self-node token results in a joined ctx handed to the handler.
func TestTxRouteInterceptor_LocalJoin(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	f, gate, claims := liveRouteFence(t, "tx-1")
	tok, _ := s.Issue(claims)
	ic := newTxRouteInterceptor(s, fakeRouteRegistry{}, "local", mustJoiner(t, s, fakeJoinTM{}, f, gate), 9090, true)
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

// A valid self-node token on EntitySearch (unary) joins the tx onto the handler
// context, symmetric with EntityManage: a compute-node callback that searches
// within its still-open transaction must observe its own uncommitted writes.
func TestTxRouteInterceptor_SearchLocalJoin(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	f, gate, claims := liveRouteFence(t, "tx-search-1")
	tok, _ := s.Issue(claims)
	ic := newTxRouteInterceptor(s, fakeRouteRegistry{}, "local", mustJoiner(t, s, fakeJoinTM{}, f, gate), 9090, true)
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("tx-token", tok))

	var sawTx string
	handler := func(ctx context.Context, req any) (any, error) {
		if tx := spi.GetTransaction(ctx); tx != nil {
			sawTx = tx.ID
		}
		return "ok", nil
	}
	resp, err := ic.unary()(ctx, &cepb.CloudEvent{Id: "req-s1"}, entitySearchInfo(), handler)
	if err != nil || sawTx != "tx-search-1" || resp != "ok" {
		t.Fatalf("expected search local join tx-search-1, sawTx=%q resp=%v err=%v", sawTx, resp, err)
	}
}

// A valid self-node token on EntitySearchCollection (stream) joins the tx onto
// the stream context, symmetric with EntityManageCollection.
func TestTxRouteInterceptor_SearchStreamLocalJoin(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	f, gate, claims := liveRouteFence(t, "tx-search-7")
	tok, _ := s.Issue(claims)
	ic := newTxRouteInterceptor(s, fakeRouteRegistry{}, "local", mustJoiner(t, s, fakeJoinTM{}, f, gate), 9090, true)
	baseCtx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("tx-token", tok))
	// The interceptor receives the one request message before it takes the
	// transaction's lock, and replays it to the handler.
	ss := &fakeServerStream{ctx: baseCtx, request: &cepb.CloudEvent{Id: "req-s7"}}

	var sawTx string
	handler := func(_ any, stream googlegrpc.ServerStream) error {
		if tx := spi.GetTransaction(stream.Context()); tx != nil {
			sawTx = tx.ID
		}
		return nil
	}
	if err := ic.stream()(nil, ss, entitySearchCollectionInfo(), handler); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if sawTx != "tx-search-7" {
		t.Fatalf("expected joined tx-search-7 on search stream ctx, got %q", sawTx)
	}
}

// A search token for a foreign, alive node forwards the EntitySearch (unary) to
// the owner via the search forward seam — mirroring the write path, so a
// peer-owned transaction's reads execute on the owner (where T is live).
func TestTxRouteInterceptor_SearchForeignProxies(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	tok, _ := s.Issue(token.Claims{NodeID: "node-B", TxRef: "tx-search-9", ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-tx-search-9", Major: 1})
	reg := fakeRouteRegistry{nodes: map[string]contract.NodeInfo{
		"node-B": {NodeID: "node-B", Addr: "http://node-b:8080", Alive: true},
	}}
	ic := newTxRouteInterceptor(s, reg, "node-A", noCalloutJoiner(t, s, fakeJoinTM{}), 9090, true)

	var gotAddr string
	writeForwardCalled := false
	ic.forwardUnary = func(context.Context, *proxy.ClientPool, string, *cepb.CloudEvent) (*cepb.CloudEvent, error) {
		writeForwardCalled = true // the search path must NOT use the write forward
		return nil, nil
	}
	ic.forwardSearchUnary = func(_ context.Context, _ *proxy.ClientPool, addr string, _ *cepb.CloudEvent) (*cepb.CloudEvent, error) {
		gotAddr = addr
		return &cepb.CloudEvent{Id: "search-forwarded"}, nil
	}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("tx-token", tok))

	resp, err := ic.unary()(ctx, &cepb.CloudEvent{Id: "req-s9"}, entitySearchInfo(), func(context.Context, any) (any, error) {
		t.Fatal("handler must not run for a proxied search")
		return nil, nil
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if writeForwardCalled {
		t.Fatal("search proxy must use the search forward, not the write forward")
	}
	if gotAddr != "node-b:9090" {
		t.Fatalf("expected search forward to derived gRPC addr %q, got %q", "node-b:9090", gotAddr)
	}
	if ce, ok := resp.(*cepb.CloudEvent); !ok || ce.Id != "search-forwarded" {
		t.Fatalf("expected forwarded search response verbatim, got %v", resp)
	}
}

// A search token for a foreign node re-issues EntitySearchCollection (stream) to
// the owner via the search stream forward seam and copies frames back.
func TestTxRouteInterceptor_SearchStreamForeignProxies(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	tok, _ := s.Issue(token.Claims{NodeID: "node-B", TxRef: "tx-search-11", ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-tx-search-11", Major: 1})
	reg := fakeRouteRegistry{nodes: map[string]contract.NodeInfo{
		"node-B": {NodeID: "node-B", Addr: "http://node-b:8080", Alive: true},
	}}
	ic := newTxRouteInterceptor(s, reg, "node-A", noCalloutJoiner(t, s, fakeJoinTM{}), 9090, true)

	var gotAddr string
	ic.forwardStream = func(context.Context, *proxy.ClientPool, string, *cepb.CloudEvent) (googlegrpc.ServerStreamingClient[cepb.CloudEvent], error) {
		t.Fatal("search stream proxy must use the search forward, not the write forward")
		return nil, nil
	}
	ic.forwardSearchStream = func(_ context.Context, _ *proxy.ClientPool, addr string, _ *cepb.CloudEvent) (googlegrpc.ServerStreamingClient[cepb.CloudEvent], error) {
		gotAddr = addr
		return &fakeClientStream{frames: []*cepb.CloudEvent{{Id: "sf1"}, {Id: "sf2"}}}, nil
	}
	baseCtx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("tx-token", tok))
	ss := &fakeServerStream{ctx: baseCtx, recv: []*cepb.CloudEvent{{Id: "req-search-2"}}}

	handler := func(any, googlegrpc.ServerStream) error {
		t.Fatal("handler must not run for a proxied search stream")
		return nil
	}
	if err := ic.stream()(nil, ss, entitySearchCollectionInfo(), handler); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if gotAddr != "node-b:9090" {
		t.Fatalf("expected search stream forward to derived gRPC addr %q, got %q", "node-b:9090", gotAddr)
	}
	if len(ss.sent) != 2 || ss.sent[0].Id != "sf1" || ss.sent[1].Id != "sf2" {
		t.Fatalf("expected 2 forwarded frames sf1,sf2; got %+v", ss.sent)
	}
}

// A bad token on EntitySearch (unary) yields the SEARCH error envelope
// (EntityResponse, not the transaction envelope), never a raw gRPC status, and
// never reaches the handler — the loud-fail contract, symmetric with writes.
func TestTxRouteInterceptor_SearchBadTokenEnvelope(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	ic := newTxRouteInterceptor(s, fakeRouteRegistry{}, "local", noCalloutJoiner(t, s, fakeJoinTM{}), 9090, true)
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("tx-token", "garbage.token"))

	resp, err := ic.unary()(ctx, &cepb.CloudEvent{Id: "req-sx"}, entitySearchInfo(), func(context.Context, any) (any, error) {
		t.Fatal("handler must not run for a bad search token")
		return nil, nil
	})
	if err != nil {
		t.Fatalf("expected envelope response, got gRPC err: %v", err)
	}
	r := decodeEntityResp(t, resp)
	if r.Success {
		t.Fatal("expected Success=false")
	}
	if r.RequestID != "req-sx" {
		t.Fatalf("expected RequestID req-sx, got %q", r.RequestID)
	}
	if r.Error == nil || r.Error.Message == "" {
		t.Fatalf("expected error detail, got %+v", r.Error)
	}
}

// A bad token on EntitySearchCollection (stream) yields the SEARCH error
// envelope frame (EntityResponse), never a raw gRPC status.
func TestTxRouteInterceptor_SearchStreamBadTokenEnvelope(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	ic := newTxRouteInterceptor(s, fakeRouteRegistry{}, "local", noCalloutJoiner(t, s, fakeJoinTM{}), 9090, true)
	baseCtx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("tx-token", "garbage.token"))
	ss := &fakeServerStream{ctx: baseCtx}

	handler := func(any, googlegrpc.ServerStream) error {
		t.Fatal("handler must not run for a bad search token")
		return nil
	}
	if err := ic.stream()(nil, ss, entitySearchCollectionInfo(), handler); err != nil {
		t.Fatalf("expected envelope on stream, got raw err: %v", err)
	}
	if len(ss.sent) != 1 {
		t.Fatalf("expected 1 envelope frame, got %d", len(ss.sent))
	}
	r := decodeEntityRespCE(t, ss.sent[0])
	if r.Success {
		t.Fatal("expected Success=false")
	}
	if r.Error == nil || r.Error.Message == "" {
		t.Fatalf("expected error detail, got %+v", r.Error)
	}
}

// A token for a different, alive node triggers ForwardEntityManage to its gRPC addr.
// When the peer has no explicit GRPCAddr, the addr is derived from its HTTP host + local gRPC port.
func TestTxRouteInterceptor_ForeignProxies(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	tok, _ := s.Issue(token.Claims{NodeID: "node-B", TxRef: "tx-9", ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-tx-9", Major: 1})
	reg := fakeRouteRegistry{nodes: map[string]contract.NodeInfo{
		"node-B": {NodeID: "node-B", Addr: "http://node-b:8080", Alive: true},
	}}
	ic := newTxRouteInterceptor(s, reg, "node-A", noCalloutJoiner(t, s, fakeJoinTM{}), 9090, true)

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
	tok, _ := s.Issue(token.Claims{NodeID: "node-B", TxRef: "tx-10", ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-tx-10", Major: 1})
	reg := fakeRouteRegistry{nodes: map[string]contract.NodeInfo{
		"node-B": {NodeID: "node-B", Addr: "http://node-b:8080", GRPCAddr: "node-b:19090", Alive: true},
	}}
	ic := newTxRouteInterceptor(s, reg, "node-A", noCalloutJoiner(t, s, fakeJoinTM{}), 9090, true)

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
	ic := newTxRouteInterceptor(s, fakeRouteRegistry{}, "local", noCalloutJoiner(t, s, fakeJoinTM{}), 9090, true)
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
// loud-fail contract for one callback-token error class.
//
// Unlike assertRefusalEnvelope, which reads the fields both envelope shapes
// share, this one decodes as EntityTransactionResponseJson and so also pins the
// write RPCs' response shape: a refusal that came back in some other shape
// fails here rather than passing on its code alone.
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
	tok, _ := s.Issue(token.Claims{NodeID: "local", TxRef: "tx-exp", ExpiresAt: time.Now().Add(-time.Second).Unix(), Callout: "req-tx-exp", Major: 1})
	ic := newTxRouteInterceptor(s, fakeRouteRegistry{}, "local", noCalloutJoiner(t, s, fakeJoinTM{}), 9090, true)
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
	tok, _ := forger.Issue(token.Claims{NodeID: "local", TxRef: "tx-forged", ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-tx-forged", Major: 1})
	ic := newTxRouteInterceptor(s, fakeRouteRegistry{}, "local", noCalloutJoiner(t, s, fakeJoinTM{}), 9090, true)
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
	tok, _ := s.Issue(token.Claims{NodeID: "local", TxRef: "tx-gone", ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-tx-gone", Major: 1})
	ic := newTxRouteInterceptor(s, fakeRouteRegistry{}, "local", noCalloutJoiner(t, s, fakeErrTM{err: spi.ErrTxNotFound}), 9090, true)
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
// This is the "owner node down" callback case: a compute-node callback
// (EntityManage) lands on a non-owner node, but the owner is unreachable, so the
// B→A forward cannot proceed and the client sees a clean operational code rather
// than a raw gRPC error. Covers classifyRouteErr's proxy.ErrNodeUnavailable arm.
func TestTxRouteInterceptor_DeadNodeUnavailableEnvelope(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	tok, _ := s.Issue(token.Claims{NodeID: "node-owner", TxRef: "tx-owner-down", ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-tx-owner-down", Major: 1})
	reg := fakeRouteRegistry{nodes: map[string]contract.NodeInfo{
		"node-owner": {NodeID: "node-owner", Addr: "http://node-owner:8080", Alive: false},
	}}
	ic := newTxRouteInterceptor(s, reg, "local", noCalloutJoiner(t, s, fakeJoinTM{}), 9090, true)
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

// A valid token naming a peer the registry does not list at all — exactly what
// registry.Gossip.List yields for a member whose gossip metadata does not
// parse (internal/cluster/registry/gossip_badmeta_internal_test.go covers the
// HTTP door for the same condition) — also yields TRANSACTION_NODE_UNAVAILABLE
// (503) in the envelope. This exercises ResolveNodeInfo's fall-through when the
// token's node is missing from List entirely, distinct from
// TestTxRouteInterceptor_DeadNodeUnavailableEnvelope above, where the node is
// listed but marked not-Alive.
func TestTxRouteInterceptor_NodeMissingFromRegistryUnavailableEnvelope(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	tok, _ := s.Issue(token.Claims{NodeID: "node-ghost", TxRef: "tx-ghost", ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-tx-ghost", Major: 1})
	reg := fakeRouteRegistry{nodes: map[string]contract.NodeInfo{
		"node-other": {NodeID: "node-other", Addr: "http://node-other:8080", Alive: true},
	}}
	ic := newTxRouteInterceptor(s, reg, "local", noCalloutJoiner(t, s, fakeJoinTM{}), 9090, true)
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("tx-token", tok))

	resp, err := ic.unary()(ctx, &cepb.CloudEvent{Id: "req-ghost"}, entityManageInfo(), func(context.Context, any) (any, error) {
		t.Fatal("handler must not run when the token names a node the registry does not list")
		return nil, nil
	})
	if err != nil {
		t.Fatalf("expected envelope response, got gRPC err: %v", err)
	}
	assertEnvelopeCode(t, resp, "req-ghost", "TRANSACTION_NODE_UNAVAILABLE")
}

// A valid self-node token for a transaction owned by a different tenant yields
// FORBIDDEN (mapped from spi.ErrTxTenantMismatch in JoinFromToken).
func TestTxRouteInterceptor_TenantMismatchEnvelope(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	tok, _ := s.Issue(token.Claims{NodeID: "local", TxRef: "tx-other-tenant", ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-tx-other-tenant", Major: 1})
	ic := newTxRouteInterceptor(s, fakeRouteRegistry{}, "local", noCalloutJoiner(t, s, fakeErrTM{err: spi.ErrTxTenantMismatch}), 9090, true)
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

// A pass presented by the wrong tenant is answered FORBIDDEN on this door too,
// whatever the fence knows about the callout it names — current, superseded or
// never registered — so a stolen pass tells another tenant nothing about which
// callouts exist.
func TestTxRouteInterceptor_TenantMismatchIsOneAnswerWhateverTheFenceKnows(t *testing.T) {
	for name, setup := range map[string]func(*testing.T) (*fence.Fence, *txgate.Registry, token.Claims){
		"callout current": func(t *testing.T) (*fence.Fence, *txgate.Registry, token.Claims) {
			return liveRouteFence(t, "tx-1")
		},
		"callout superseded": func(t *testing.T) (*fence.Fence, *txgate.Registry, token.Claims) {
			f, gate, claims := liveRouteFence(t, "tx-1")
			f.Advance(claims.Callout, 2)
			return f, gate, claims
		},
		"callout unknown": func(t *testing.T) (*fence.Fence, *txgate.Registry, token.Claims) {
			f, gate := gatedFence()
			return f, gate, token.Claims{NodeID: "local", TxRef: "tx-1", ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-tx-1", Major: 1}
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := token.NewSigner(make32(t))
			f, gate, claims := setup(t)
			tok, _ := s.Issue(claims)
			ic := newTxRouteInterceptor(s, fakeRouteRegistry{}, "local", mustJoiner(t, s, fakeErrTM{err: spi.ErrTxTenantMismatch}, f, gate), 9090, true)
			ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("tx-token", tok))

			resp, err := ic.unary()(ctx, &cepb.CloudEvent{Id: "req-stolen"}, entityManageInfo(), func(context.Context, any) (any, error) {
				t.Fatal("handler must not run for a cross-tenant token")
				return nil, nil
			})
			if err != nil {
				t.Fatalf("expected envelope response, got gRPC err: %v", err)
			}
			assertEnvelopeCode(t, resp, "req-stolen", "FORBIDDEN")
		})
	}
}

// envelopeRefusal is the shape-agnostic view of an error envelope: the write
// RPCs answer with the transaction envelope and the search RPCs with the
// entity-response envelope, and both carry these fields alike.
type envelopeRefusal struct {
	Success   bool   `json:"success"`
	RequestID string `json:"requestId"`
	Error     *struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		Retryable *bool  `json:"retryable"`
	} `json:"error"`
}

// assertRefusalEnvelope asserts ce is a failure envelope of either shape whose
// operational code (the "CODE: detail" message prefix of the CLIENT_ERROR
// class) is wantCode, and that it is not retryable.
func assertRefusalEnvelope(t *testing.T, ce *cepb.CloudEvent, wantReqID, wantCode string) {
	t.Helper()
	_, payload, err := ParseCloudEvent(ce)
	if err != nil {
		t.Fatalf("ParseCloudEvent: %v", err)
	}
	var env envelopeRefusal
	if err := json.Unmarshal(payload, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if env.Success {
		t.Fatal("expected Success=false")
	}
	if env.RequestID != wantReqID {
		t.Fatalf("RequestID = %q; want %q", env.RequestID, wantReqID)
	}
	if env.Error == nil || !strings.HasPrefix(env.Error.Message, wantCode+":") {
		t.Fatalf("Error = %+v; want %q prefix", env.Error, wantCode)
	}
	if env.Error.Retryable != nil && *env.Error.Retryable {
		t.Fatalf("%s must not be retryable", wantCode)
	}
}

// assertStreamEnvelopeCode asserts the stream carries exactly one refusal
// envelope with wantCode, echoing wantReqID. A refusal taken before the request
// message was read — a token the interceptor cannot resolve at all — has no
// request id; a joined request is refused after its one message has been
// received, so its refusal names it.
func assertStreamEnvelopeCode(t *testing.T, ss *fakeServerStream, wantReqID, wantCode string) {
	t.Helper()
	if len(ss.sent) != 1 {
		t.Fatalf("expected 1 envelope frame, got %d", len(ss.sent))
	}
	assertRefusalEnvelope(t, ss.sent[0], wantReqID, wantCode)
}

// Owner gave the work to a second cnode of its own: the first cnode's callback
// is refused at once, while the callout is still in progress — on the write
// RPC and on the read RPC.
func TestTxRouteInterceptor_SupersededWhileCalloutInProgress(t *testing.T) {
	for name, info := range map[string]*googlegrpc.UnaryServerInfo{
		"EntityManage (write)": entityManageInfo(),
		"EntitySearch (read)":  {FullMethod: cyodapb.CloudEventsService_EntitySearch_FullMethodName},
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := token.NewSigner(make32(t))
			f, gate, claims := liveRouteFence(t, "tx-1")
			tok, _ := s.Issue(claims)
			f.Advance(claims.Callout, 2)
			ic := newTxRouteInterceptor(s, fakeRouteRegistry{}, "local", mustJoiner(t, s, fakeJoinTM{}, f, gate), 9090, true)
			ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("tx-token", tok))
			resp, err := ic.unary()(ctx, &cepb.CloudEvent{Id: "req-late"}, info, func(context.Context, any) (any, error) {
				t.Fatal("handler must not run for a cnode that was replaced")
				return nil, nil
			})
			if err != nil {
				t.Fatalf("expected envelope response, got gRPC err: %v", err)
			}
			ce, ok := resp.(*cepb.CloudEvent)
			if !ok {
				t.Fatalf("expected *cepb.CloudEvent, got %T", resp)
			}
			assertRefusalEnvelope(t, ce, "req-late", "CALLOUT_SUPERSEDED")
		})
	}
}

// Late callback after the callout ended, on both server-streaming RPCs.
func TestTxRouteInterceptor_SupersededAfterCalloutEnded_Stream(t *testing.T) {
	for name, method := range map[string]string{
		"EntityManageCollection": cyodapb.CloudEventsService_EntityManageCollection_FullMethodName,
		"EntitySearchCollection": cyodapb.CloudEventsService_EntitySearchCollection_FullMethodName,
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := token.NewSigner(make32(t))
			f, gate := gatedFence()
			_, end := f.Begin(context.Background(), "req-tx-1", "tx-1", nil)
			f.Advance("req-tx-1", 1)
			end()
			tok, _ := s.Issue(token.Claims{NodeID: "local", TxRef: "tx-1", ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-tx-1", Major: 1})
			ic := newTxRouteInterceptor(s, fakeRouteRegistry{}, "local", mustJoiner(t, s, fakeJoinTM{}, f, gate), 9090, true)
			ss := &fakeServerStream{ctx: metadata.NewIncomingContext(context.Background(), metadata.Pairs("tx-token", tok)),
				request: &cepb.CloudEvent{Id: "req-late"}}
			err := ic.stream()(nil, ss, &googlegrpc.StreamServerInfo{FullMethod: method}, func(any, googlegrpc.ServerStream) error {
				t.Fatal("handler must not run once the callout has ended")
				return nil
			})
			if err != nil {
				t.Fatalf("expected an envelope on the stream, got gRPC err: %v", err)
			}
			assertStreamEnvelopeCode(t, ss, "req-late", "CALLOUT_SUPERSEDED")
		})
	}
}

// A non-routed unary method (EntityModelManage) passes through untouched (no
// token processing), even with a bad token present.
func TestTxRouteInterceptor_NonEntityManagePassThrough(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	ic := newTxRouteInterceptor(s, fakeRouteRegistry{}, "local", noCalloutJoiner(t, s, fakeJoinTM{}), 9090, true)
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
	info := &googlegrpc.UnaryServerInfo{FullMethod: cyodapb.CloudEventsService_EntityModelManage_FullMethodName}
	resp, err := ic.unary()(ctx, nil, info, handler)
	if err != nil || !called || resp != "ok" {
		t.Fatalf("expected passthrough, called=%v resp=%v err=%v", called, resp, err)
	}
}

// --- stream -----------------------------------------------------------------

// A valid self-node token joins the tx onto the stream context.
func TestTxRouteInterceptor_StreamLocalJoin(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	f, gate, claims := liveRouteFence(t, "tx-7")
	tok, _ := s.Issue(claims)
	ic := newTxRouteInterceptor(s, fakeRouteRegistry{}, "local", mustJoiner(t, s, fakeJoinTM{}, f, gate), 9090, true)
	baseCtx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("tx-token", tok))
	ss := &fakeServerStream{ctx: baseCtx, request: &cepb.CloudEvent{Id: "req-7"}}

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

// A non-routed stream method (StartStreaming) with a bad token must pass through
// untouched — no routing, no envelope, handler invoked directly.
func TestTxRouteInterceptor_StreamNonEntityManagePassThrough(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	ic := newTxRouteInterceptor(s, fakeRouteRegistry{}, "local", noCalloutJoiner(t, s, fakeJoinTM{}), 9090, true)
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
	info := &googlegrpc.StreamServerInfo{FullMethod: cyodapb.CloudEventsService_StartStreaming_FullMethodName}
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
	tok, _ := s.Issue(token.Claims{NodeID: "node-B", TxRef: "tx-11", ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-tx-11", Major: 1})
	reg := fakeRouteRegistry{nodes: map[string]contract.NodeInfo{
		"node-B": {NodeID: "node-B", Addr: "http://node-b:8080", Alive: true},
	}}
	ic := newTxRouteInterceptor(s, reg, "node-A", noCalloutJoiner(t, s, fakeJoinTM{}), 9090, true)

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
	tok, _ := s.Issue(token.Claims{NodeID: "node-B", TxRef: "tx-99", ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-tx-99", Major: 1})
	reg := fakeRouteRegistry{nodes: map[string]contract.NodeInfo{
		"node-B": {NodeID: "node-B", Addr: "http://node-b:8080", Alive: true},
	}}
	ic := newTxRouteInterceptor(s, reg, "node-A", noCalloutJoiner(t, s, fakeJoinTM{}), 9090, true)
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
	tok, _ := s.Issue(token.Claims{NodeID: "node-B", TxRef: "tx-88", ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-tx-88", Major: 1})
	reg := fakeRouteRegistry{nodes: map[string]contract.NodeInfo{
		"node-B": {NodeID: "node-B", Addr: "http://node-b:8080", Alive: true},
	}}
	ic := newTxRouteInterceptor(s, reg, "node-A", noCalloutJoiner(t, s, fakeJoinTM{}), 9090, true)
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

// --- the transaction's lock on this door -------------------------------------

// joinedRouteInterceptor returns an interceptor whose join layer has the callout
// of txID in progress at major 1, a probe that reports whether the
// transaction's lock is free, and the pass that names the callout.
func joinedRouteInterceptor(t *testing.T, txID string) (*txRouteInterceptor, func() bool, string) {
	t.Helper()
	s, err := token.NewSigner(make32(t))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	f, gate, claims := liveRouteFence(t, txID)
	tok, err := s.Issue(claims)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	lockFree := func() bool {
		free := make(chan struct{})
		go func() { gate.Acquire(txID)(); close(free) }()
		select {
		case <-free:
			return true
		case <-time.After(50 * time.Millisecond):
			return false
		}
	}
	ic := newTxRouteInterceptor(s, fakeRouteRegistry{}, "local", mustJoiner(t, s, fakeJoinTM{}, f, gate), 9090, true)
	return ic, lockFree, tok
}

// gRPC server-streaming: the handler's sends are held until the lock is
// released, and the request message is received before the lock is taken.
func TestTxRouteInterceptor_StreamSendsAreHeldUntilTheLockIsReleased(t *testing.T) {
	ic, lockFree, tok := joinedRouteInterceptor(t, "tx-1")

	ss := newFakeServerStream(metadata.NewIncomingContext(context.Background(), metadata.Pairs("tx-token", tok)))
	ss.request = &cepb.CloudEvent{Id: "req-1"}
	ss.onRecv = func() {
		if !lockFree() {
			t.Error("the request message was received under the transaction's lock")
		}
	}
	ss.onSend = func() {
		if !lockFree() {
			t.Error("a response frame was sent under the transaction's lock")
		}
	}
	err := ic.stream()(nil, ss, entitySearchCollectionInfo(),
		func(_ any, stream googlegrpc.ServerStream) error {
			var req cepb.CloudEvent
			if err := stream.RecvMsg(&req); err != nil || req.Id != "req-1" {
				t.Errorf("handler RecvMsg = %v, id %q; want the request replayed", err, req.Id)
			}
			if lockFree() {
				t.Error("the handler ran without the transaction's lock")
			}
			for _, id := range []string{"f-1", "f-2"} {
				if err := stream.SendMsg(&cepb.CloudEvent{Id: id}); err != nil {
					return err
				}
			}
			if len(ss.sent) != 0 {
				t.Error("a frame reached the client before the handler returned")
			}
			return nil
		})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(ss.sent) != 2 || ss.sent[0].Id != "f-1" || ss.sent[1].Id != "f-2" {
		t.Fatalf("sent %+v; want f-1, f-2, in order, after the handler returned", ss.sent)
	}
}

// A joined chunked collection that fails at chunk n still delivers the answers
// of chunks 1…n-1, as an unheld stream does, and the handler's error is the
// stream's outcome.
func TestTxRouteInterceptor_HeldStreamSendsWhatItHeldThenReturnsTheHandlerError(t *testing.T) {
	ic, _, tok := joinedRouteInterceptor(t, "tx-1")
	ss := newFakeServerStream(metadata.NewIncomingContext(context.Background(), metadata.Pairs("tx-token", tok)))
	ss.request = &cepb.CloudEvent{Id: "req-1"}

	chunkFailed := errors.New("chunk 3 failed")
	err := ic.stream()(nil, ss, entityManageCollectionInfo(),
		func(_ any, stream googlegrpc.ServerStream) error {
			for _, id := range []string{"chunk-1", "chunk-2"} {
				if err := stream.SendMsg(&cepb.CloudEvent{Id: id}); err != nil {
					return err
				}
			}
			return chunkFailed
		})
	if !errors.Is(err, chunkFailed) {
		t.Fatalf("stream err = %v; want the handler's error", err)
	}
	if len(ss.sent) != 2 || ss.sent[0].Id != "chunk-1" || ss.sent[1].Id != "chunk-2" {
		t.Fatalf("sent %+v; want the answers of the chunks that succeeded", ss.sent)
	}
}

// gRPC unary: the handler runs under the transaction's lock, which is free once
// the interceptor has returned. The message is already complete, so nothing is
// read ahead of the lock.
func TestTxRouteInterceptor_UnaryHandlerRunsUnderTheLock(t *testing.T) {
	ic, lockFree, tok := joinedRouteInterceptor(t, "tx-1")
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("tx-token", tok))

	resp, err := ic.unary()(ctx, &cepb.CloudEvent{Id: "req-1"}, entityManageInfo(),
		func(context.Context, any) (any, error) {
			if lockFree() {
				t.Error("the handler ran without the transaction's lock")
			}
			return "ok", nil
		})
	if err != nil || resp != "ok" {
		t.Fatalf("unary = %v, %v; want ok, nil", resp, err)
	}
	if !lockFree() {
		t.Fatal("the transaction's lock is still held after the interceptor returned")
	}
}
