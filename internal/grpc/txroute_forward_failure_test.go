package grpc

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/proxy"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// shortProxyTimeout is the CYODA_PROXY_TIMEOUT of the tests that reach it.
const shortProxyTimeout = 50 * time.Millisecond

// gapProxyTimeout is the CYODA_PROXY_TIMEOUT of a test whose first frame must
// arrive within it: wide enough that a stalled runner does not miss it.
const gapProxyTimeout = 250 * time.Millisecond

// forwardingInterceptor builds an interceptor on node-A holding a pass for a
// transaction owned by node-B, which the registry lists alive.
func forwardingInterceptor(t *testing.T, proxyTimeout time.Duration) (*txRouteInterceptor, context.Context) {
	t.Helper()
	s, _ := token.NewSigner(make32(t))
	tok, _ := s.Issue(token.Claims{NodeID: "node-B", TxRef: "tx-fwd", ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-tx-fwd", Major: 1})
	reg := fakeRouteRegistry{nodes: map[string]contract.NodeInfo{
		"node-B": {NodeID: "node-B", Addr: "http://node-b:8080", Alive: true},
	}}
	ic := newTxRouteInterceptor(s, reg, "node-A", noCalloutJoiner(t, s, fakeJoinTM{}), 9090, true, proxyTimeout)
	return ic, metadata.NewIncomingContext(context.Background(), metadata.Pairs("tx-token", tok))
}

func mustNotRunUnary(t *testing.T) googlegrpc.UnaryHandler {
	return func(context.Context, any) (any, error) {
		t.Fatal("handler must not run for a call forwarded to its owner")
		return nil, nil
	}
}

func mustNotRunStream(t *testing.T) googlegrpc.StreamHandler {
	return func(any, googlegrpc.ServerStream) error {
		t.Fatal("handler must not run for a call forwarded to its owner")
		return nil
	}
}

// An owner the registry lists alive but that cannot be reached answers 503
// TRANSACTION_NODE_UNAVAILABLE on gRPC, as the HTTP proxy does — not a
// ticketed SERVER_ERROR.
func TestTxRouteInterceptor_UnreachableOwnerUnavailableEnvelope(t *testing.T) {
	for name, fwdErr := range map[string]error{
		"dial failure":        errors.New("dial tcp 10.0.0.2:9090: connect: connection refused"),
		"transport failure":   status.Error(codes.Unavailable, "connection error: desc = \"transport: Error while dialing\""),
		"deadline at the hop": status.Error(codes.DeadlineExceeded, "context deadline exceeded"),
	} {
		t.Run(name, func(t *testing.T) {
			ic, ctx := forwardingInterceptor(t, testProxyTimeout)
			ic.forwardUnary = func(context.Context, *proxy.ClientPool, string, *cepb.CloudEvent) (*cepb.CloudEvent, error) {
				return nil, fwdErr
			}
			resp, err := ic.unary()(ctx, &cepb.CloudEvent{Id: "req-unreach"}, entityManageInfo(), mustNotRunUnary(t))
			if err != nil {
				t.Fatalf("expected envelope response, got gRPC err: %v", err)
			}
			assertEnvelopeCode(t, resp, "req-unreach", "TRANSACTION_NODE_UNAVAILABLE")
		})
	}
}

// A status the owner itself answered with reaches the caller unchanged, as if
// the caller had called the owner directly.
func TestTxRouteInterceptor_OwnerStatusPassesThrough(t *testing.T) {
	ic, ctx := forwardingInterceptor(t, testProxyTimeout)
	ownerErr := status.Error(codes.PermissionDenied, "role required")
	ic.forwardUnary = func(context.Context, *proxy.ClientPool, string, *cepb.CloudEvent) (*cepb.CloudEvent, error) {
		return nil, ownerErr
	}
	_, err := ic.unary()(ctx, &cepb.CloudEvent{Id: "req-owner"}, entityManageInfo(), mustNotRunUnary(t))
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("err = %v, want the owner's PermissionDenied status", err)
	}
}

// An owner that does not answer within CYODA_PROXY_TIMEOUT answers 503
// TRANSACTION_NODE_UNAVAILABLE, and the forwarded call is cancelled.
func TestTxRouteInterceptor_OwnerSilentPastProxyTimeoutUnavailable(t *testing.T) {
	ic, ctx := forwardingInterceptor(t, shortProxyTimeout)
	ic.forwardUnary = func(ctx context.Context, _ *proxy.ClientPool, _ string, _ *cepb.CloudEvent) (*cepb.CloudEvent, error) {
		select {
		case <-ctx.Done():
			return nil, status.FromContextError(ctx.Err()).Err()
		case <-time.After(5 * time.Second):
			return &cepb.CloudEvent{Id: "too-late"}, nil
		}
	}
	start := time.Now()
	resp, err := ic.unary()(ctx, &cepb.CloudEvent{Id: "req-silent"}, entityManageInfo(), mustNotRunUnary(t))
	if err != nil {
		t.Fatalf("expected envelope response, got gRPC err: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("the wait was not bounded by the proxy timeout: %v", elapsed)
	}
	assertEnvelopeCode(t, resp, "req-silent", "TRANSACTION_NODE_UNAVAILABLE")
}

// blockingClientStream yields its frames, each after its delay, and ends a
// wait early when its context ends.
type blockingClientStream struct {
	googlegrpc.ClientStream
	ctx    context.Context
	frames []*cepb.CloudEvent
	delays []time.Duration
	idx    int
}

func (c *blockingClientStream) Recv() (*cepb.CloudEvent, error) {
	if c.idx >= len(c.frames) {
		return nil, io.EOF
	}
	select {
	case <-c.ctx.Done():
		return nil, status.FromContextError(c.ctx.Err()).Err()
	case <-time.After(c.delays[c.idx]):
	}
	f := c.frames[c.idx]
	c.idx++
	return f, nil
}

// A streamed call whose owner sends no first message within
// CYODA_PROXY_TIMEOUT answers 503 TRANSACTION_NODE_UNAVAILABLE on the stream.
func TestTxRouteInterceptor_StreamOwnerSilentPastProxyTimeoutUnavailable(t *testing.T) {
	ic, ctx := forwardingInterceptor(t, shortProxyTimeout)
	ic.forwardStream = func(ctx context.Context, _ *proxy.ClientPool, _ string, _ *cepb.CloudEvent) (googlegrpc.ServerStreamingClient[cepb.CloudEvent], error) {
		return &blockingClientStream{ctx: ctx, frames: []*cepb.CloudEvent{{Id: "late"}}, delays: []time.Duration{5 * time.Second}}, nil
	}
	ss := &fakeServerStream{ctx: ctx, recv: []*cepb.CloudEvent{{Id: "req-stream-silent"}}}
	start := time.Now()
	if err := ic.stream()(nil, ss, entityManageCollectionInfo(), mustNotRunStream(t)); err != nil {
		t.Fatalf("expected an envelope on the stream, got err: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("the wait was not bounded by the proxy timeout: %v", elapsed)
	}
	if len(ss.sent) != 1 {
		t.Fatalf("sent %d frames, want one error envelope", len(ss.sent))
	}
	assertEnvelopeCode(t, ss.sent[0], "req-stream-silent", "TRANSACTION_NODE_UNAVAILABLE")
}

// The proxy timeout bounds only the wait for the first message: a stream whose
// owner started answering in time is copied to its end, however long it runs.
func TestTxRouteInterceptor_StreamPastFirstMessageNotCut(t *testing.T) {
	ic, ctx := forwardingInterceptor(t, gapProxyTimeout)
	ic.forwardStream = func(ctx context.Context, _ *proxy.ClientPool, _ string, _ *cepb.CloudEvent) (googlegrpc.ServerStreamingClient[cepb.CloudEvent], error) {
		return &blockingClientStream{ctx: ctx,
			frames: []*cepb.CloudEvent{{Id: "f1"}, {Id: "f2"}},
			delays: []time.Duration{0, 3 * gapProxyTimeout}}, nil
	}
	ss := &fakeServerStream{ctx: ctx, recv: []*cepb.CloudEvent{{Id: "req-stream-long"}}}
	if err := ic.stream()(nil, ss, entityManageCollectionInfo(), mustNotRunStream(t)); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(ss.sent) != 2 || ss.sent[0].Id != "f1" || ss.sent[1].Id != "f2" {
		t.Fatalf("expected frames f1,f2 copied whole; got %+v", ss.sent)
	}
}

// A stream that cannot be opened to an unreachable owner answers 503
// TRANSACTION_NODE_UNAVAILABLE on the stream.
func TestTxRouteInterceptor_StreamUnreachableOwnerUnavailable(t *testing.T) {
	ic, ctx := forwardingInterceptor(t, testProxyTimeout)
	ic.forwardStream = func(context.Context, *proxy.ClientPool, string, *cepb.CloudEvent) (googlegrpc.ServerStreamingClient[cepb.CloudEvent], error) {
		return nil, errors.New("dial tcp 10.0.0.2:9090: connect: connection refused")
	}
	ss := &fakeServerStream{ctx: ctx, recv: []*cepb.CloudEvent{{Id: "req-stream-unreach"}}}
	if err := ic.stream()(nil, ss, entityManageCollectionInfo(), mustNotRunStream(t)); err != nil {
		t.Fatalf("expected an envelope on the stream, got err: %v", err)
	}
	if len(ss.sent) != 1 {
		t.Fatalf("sent %d frames, want one error envelope", len(ss.sent))
	}
	assertEnvelopeCode(t, ss.sent[0], "req-stream-unreach", "TRANSACTION_NODE_UNAVAILABLE")
}

// A status that the forwarding hop's own gRPC client may have built — an
// answer past the receive limit, a stream reset — is not passed off as the
// owner's: the caller gets the ticketed SERVER_ERROR envelope and the cause is
// logged on this node.
func TestTxRouteInterceptor_HopStatusIsNotPassedThrough(t *testing.T) {
	for name, fwdErr := range map[string]error{
		"receive limit": status.Error(codes.ResourceExhausted, "grpc: received message larger than max (5242880 vs. 4194304)"),
		"stream reset":  status.Error(codes.Internal, "stream terminated by RST_STREAM with error code: PROTOCOL_ERROR"),
	} {
		t.Run(name, func(t *testing.T) {
			ic, ctx := forwardingInterceptor(t, testProxyTimeout)
			ic.forwardUnary = func(context.Context, *proxy.ClientPool, string, *cepb.CloudEvent) (*cepb.CloudEvent, error) {
				return nil, fwdErr
			}
			resp, err := ic.unary()(ctx, &cepb.CloudEvent{Id: "req-hop"}, entityManageInfo(), mustNotRunUnary(t))
			if err != nil {
				t.Fatalf("expected envelope response, got gRPC err: %v", err)
			}
			assertEnvelopeCode(t, resp, "req-hop", "SERVER_ERROR")
		})
	}
}

// The owner's own refusals reach the caller unchanged, as they would calling
// the owner directly.
func TestTxRouteInterceptor_OwnerRefusalsPassThrough(t *testing.T) {
	for _, c := range []codes.Code{codes.InvalidArgument, codes.Unauthenticated, codes.PermissionDenied, codes.NotFound} {
		t.Run(c.String(), func(t *testing.T) {
			ic, ctx := forwardingInterceptor(t, testProxyTimeout)
			ic.forwardUnary = func(context.Context, *proxy.ClientPool, string, *cepb.CloudEvent) (*cepb.CloudEvent, error) {
				return nil, status.Error(c, "refused by the owner")
			}
			_, err := ic.unary()(ctx, &cepb.CloudEvent{Id: "req-refused"}, entityManageInfo(), mustNotRunUnary(t))
			if status.Code(err) != c {
				t.Fatalf("err = %v, want the owner's %s", err, c)
			}
		})
	}
}

// failingClientStream fails its first Recv with err.
type failingClientStream struct {
	googlegrpc.ClientStream
	err error
}

func (c *failingClientStream) Recv() (*cepb.CloudEvent, error) { return nil, c.err }

// On a stream, a hop status before the first message gets the ticketed
// SERVER_ERROR envelope, and an owner refusal reaches the caller unchanged.
func TestTxRouteInterceptor_StreamFirstMessageFailureClassified(t *testing.T) {
	for name, c := range map[string]struct {
		err      error
		wantCode string // envelope code; "" means the status passes through
	}{
		"hop status":    {status.Error(codes.ResourceExhausted, "grpc: received message larger than max"), "SERVER_ERROR"},
		"owner refusal": {status.Error(codes.PermissionDenied, "this operation requires ROLE_M2M"), ""},
		"unreachable":   {status.Error(codes.Unavailable, "connection reset"), "TRANSACTION_NODE_UNAVAILABLE"},
	} {
		t.Run(name, func(t *testing.T) {
			ic, ctx := forwardingInterceptor(t, testProxyTimeout)
			ic.forwardStream = func(context.Context, *proxy.ClientPool, string, *cepb.CloudEvent) (googlegrpc.ServerStreamingClient[cepb.CloudEvent], error) {
				return &failingClientStream{err: c.err}, nil
			}
			ss := &fakeServerStream{ctx: ctx, recv: []*cepb.CloudEvent{{Id: "req-stream-fail"}}}
			err := ic.stream()(nil, ss, entityManageCollectionInfo(), mustNotRunStream(t))
			if c.wantCode == "" {
				if status.Code(err) != status.Code(c.err) || len(ss.sent) != 0 {
					t.Fatalf("err = %v, sent %d; want the owner's status unchanged", err, len(ss.sent))
				}
				return
			}
			if err != nil || len(ss.sent) != 1 {
				t.Fatalf("err = %v, sent %d; want one envelope", err, len(ss.sent))
			}
			assertEnvelopeCode(t, ss.sent[0], "req-stream-fail", c.wantCode)
		})
	}
}

// A caller that goes away while its call is forwarded is not an outage of the
// owner: no ERROR is logged and no 503 envelope is built for nobody; the call
// ends with the caller's own cancellation.
func TestTxRouteInterceptor_CallerGoneDuringForwardIsNotAnOutage(t *testing.T) {
	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	ic, baseCtx := forwardingInterceptor(t, testProxyTimeout)
	ctx, cancel := context.WithCancel(baseCtx)
	ic.forwardUnary = func(fctx context.Context, _ *proxy.ClientPool, _ string, _ *cepb.CloudEvent) (*cepb.CloudEvent, error) {
		cancel()
		<-fctx.Done()
		return nil, status.FromContextError(fctx.Err()).Err()
	}
	_, err := ic.unary()(ctx, &cepb.CloudEvent{Id: "req-gone"}, entityManageInfo(), mustNotRunUnary(t))
	if status.Code(err) != codes.Canceled {
		t.Fatalf("err = %v, want the caller's cancellation", err)
	}
	if strings.Contains(logBuf.String(), "level=ERROR") {
		t.Fatalf("a departed caller was logged as an error:\n%s", logBuf.String())
	}
}

// realForwardInterceptor is forwardingInterceptor with the production forward
// and a peer that advertises grpcAddr.
func realForwardInterceptor(t *testing.T, grpcAddr string, proxyTimeout time.Duration) (*txRouteInterceptor, context.Context) {
	t.Helper()
	s, _ := token.NewSigner(make32(t))
	tok, _ := s.Issue(token.Claims{NodeID: "node-B", TxRef: "tx-net", ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-tx-net", Major: 1})
	reg := fakeRouteRegistry{nodes: map[string]contract.NodeInfo{
		"node-B": {NodeID: "node-B", Addr: "http://127.0.0.1:8080", GRPCAddr: grpcAddr, Alive: true},
	}}
	ic := newTxRouteInterceptor(s, reg, "node-A", noCalloutJoiner(t, s, fakeJoinTM{}), 9090, true, proxyTimeout)
	t.Cleanup(ic.pool.Close)
	return ic, metadata.NewIncomingContext(context.Background(), metadata.Pairs("tx-token", tok))
}

// Over a real connection: an owner whose port refuses connections, and one
// that accepts the connection and never speaks, both answer 503
// TRANSACTION_NODE_UNAVAILABLE within the proxy timeout.
func TestTxRouteInterceptor_RealNetworkOwnerUnreachable(t *testing.T) {
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddr := closed.Addr().String()
	_ = closed.Close()

	silent, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	go func() {
		for {
			c, err := silent.Accept()
			if err != nil {
				return
			}
			defer c.Close() // hold the connection open, say nothing
		}
	}()

	for name, addr := range map[string]string{"refused": closedAddr, "silent": silent.Addr().String()} {
		t.Run(name, func(t *testing.T) {
			ic, ctx := realForwardInterceptor(t, addr, 300*time.Millisecond)
			start := time.Now()
			resp, err := ic.unary()(ctx, &cepb.CloudEvent{Id: "req-net"}, entityManageInfo(), mustNotRunUnary(t))
			if err != nil {
				t.Fatalf("expected envelope response, got gRPC err: %v", err)
			}
			if elapsed := time.Since(start); elapsed > 5*time.Second {
				t.Fatalf("the wait was not bounded: %v", elapsed)
			}
			assertEnvelopeCode(t, resp, "req-net", "TRANSACTION_NODE_UNAVAILABLE")
		})
	}
}

// framesThenFail yields its frames, then fails with err.
type framesThenFail struct {
	googlegrpc.ClientStream
	frames []*cepb.CloudEvent
	err    error
	idx    int
}

func (c *framesThenFail) Recv() (*cepb.CloudEvent, error) {
	if c.idx < len(c.frames) {
		f := c.frames[c.idx]
		c.idx++
		return f, nil
	}
	return nil, c.err
}

// A forwarded stream that breaks after its first frame ends with a generic
// status: the hop's transport text names cluster-internal addresses and is
// logged here, never sent. The owner's own refusal still passes through.
func TestTxRouteInterceptor_StreamBreakAfterFirstFrameIsGeneric(t *testing.T) {
	const leak = "10.0.0.2:9090"
	for name, c := range map[string]struct {
		err      error
		wantCode codes.Code
		wantText string
	}{
		"transport break": {status.Error(codes.Unavailable, "connection error: desc = \"error reading from server: read tcp 10.0.0.1:41000->"+leak+": read: connection reset by peer\""), codes.Unavailable, "transaction node is unreachable"},
		"hop status":      {status.Error(codes.Internal, "stream terminated by RST_STREAM with error code: INTERNAL_ERROR"), codes.Internal, "transaction node answer failed"},
		"owner refusal":   {status.Error(codes.InvalidArgument, "invalid payload: x"), codes.InvalidArgument, "invalid payload: x"},
	} {
		t.Run(name, func(t *testing.T) {
			ic, ctx := forwardingInterceptor(t, testProxyTimeout)
			ic.forwardStream = func(context.Context, *proxy.ClientPool, string, *cepb.CloudEvent) (googlegrpc.ServerStreamingClient[cepb.CloudEvent], error) {
				return &framesThenFail{frames: []*cepb.CloudEvent{{Id: "f1"}}, err: c.err}, nil
			}
			ss := &fakeServerStream{ctx: ctx, recv: []*cepb.CloudEvent{{Id: "req-break"}}}
			err := ic.stream()(nil, ss, entityManageCollectionInfo(), mustNotRunStream(t))
			if len(ss.sent) != 1 || ss.sent[0].Id != "f1" {
				t.Fatalf("sent %+v, want the first frame copied", ss.sent)
			}
			st, _ := status.FromError(err)
			if st.Code() != c.wantCode || st.Message() != c.wantText {
				t.Fatalf("err = %v, want %s %q", err, c.wantCode, c.wantText)
			}
			if strings.Contains(st.Message(), leak) {
				t.Fatalf("the caller was sent an internal address: %q", st.Message())
			}
		})
	}
}

// A caller that goes away during a streamed forward — before the first message,
// or after it — ends with its own cancellation and no ERROR.
func TestTxRouteInterceptor_StreamCallerGoneIsNotAnOutage(t *testing.T) {
	for name, frames := range map[string][]*cepb.CloudEvent{"before the first message": nil, "after it": {{Id: "f1"}}} {
		t.Run(name, func(t *testing.T) {
			var logBuf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
			t.Cleanup(func() { slog.SetDefault(prev) })

			ic, baseCtx := forwardingInterceptor(t, testProxyTimeout)
			ctx, cancel := context.WithCancel(baseCtx)
			ic.forwardStream = func(fctx context.Context, _ *proxy.ClientPool, _ string, _ *cepb.CloudEvent) (googlegrpc.ServerStreamingClient[cepb.CloudEvent], error) {
				cancel()
				<-fctx.Done()
				return &framesThenFail{frames: frames, err: status.FromContextError(fctx.Err()).Err()}, nil
			}
			ss := &fakeServerStream{ctx: ctx, recv: []*cepb.CloudEvent{{Id: "req-stream-gone"}}}
			err := ic.stream()(nil, ss, entityManageCollectionInfo(), mustNotRunStream(t))
			if status.Code(err) != codes.Canceled {
				t.Fatalf("err = %v, want the caller's cancellation", err)
			}
			for _, f := range ss.sent {
				if f.Id != "f1" {
					t.Fatalf("an envelope was built for a departed caller: %+v", f)
				}
			}
			if strings.Contains(logBuf.String(), "level=ERROR") {
				t.Fatalf("a departed caller was logged as an error:\n%s", logBuf.String())
			}
		})
	}
}

// Over a real connection, a streamed forward to a refused port answers 503
// TRANSACTION_NODE_UNAVAILABLE on the stream.
func TestTxRouteInterceptor_RealNetworkStreamOwnerUnreachable(t *testing.T) {
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := closed.Addr().String()
	_ = closed.Close()

	ic, ctx := realForwardInterceptor(t, addr, 300*time.Millisecond)
	ss := &fakeServerStream{ctx: ctx, recv: []*cepb.CloudEvent{{Id: "req-net-stream"}}}
	if err := ic.stream()(nil, ss, entityManageCollectionInfo(), mustNotRunStream(t)); err != nil {
		t.Fatalf("expected an envelope on the stream, got err: %v", err)
	}
	if len(ss.sent) != 1 {
		t.Fatalf("sent %d frames, want one error envelope", len(ss.sent))
	}
	assertEnvelopeCode(t, ss.sent[0], "req-net-stream", "TRANSACTION_NODE_UNAVAILABLE")
}
