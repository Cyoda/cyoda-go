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
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	cyodapb "github.com/cyoda-platform/cyoda-go/api/grpc/cyoda"
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

// silentOwner returns the address of an owner that accepts connections and
// never speaks: no transport to it becomes ready, so no call header is ever
// sent to it.
func silentOwner(t *testing.T) string {
	t.Helper()
	silent, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = silent.Close() })
	go func() {
		for {
			c, err := silent.Accept()
			if err != nil {
				return
			}
			defer c.Close() // hold the connection open, say nothing
		}
	}()
	return silent.Addr().String()
}

// Over a real connection: an owner whose port refuses connections, and one
// that accepts the connection and never speaks, both answer the retryable 503
// TRANSACTION_NODE_UNAVAILABLE within the proxy timeout: no header of the call
// reached either.
func TestTxRouteInterceptor_RealNetworkOwnerUnreachable(t *testing.T) {
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddr := closed.Addr().String()
	_ = closed.Close()

	for name, addr := range map[string]string{"refused": closedAddr, "silent": silentOwner(t)} {
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
			if r := envelopeRetryable(t, resp, "req-net"); r == nil || !*r {
				t.Fatalf("Retryable = %v, want true", r)
			}
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

// Over a real connection, a streamed forward to a refused port, and one to an
// owner that never speaks, answer the retryable 503
// TRANSACTION_NODE_UNAVAILABLE on the stream. The silent owner holds the call
// until the proxy timeout fires with no header sent.
func TestTxRouteInterceptor_RealNetworkStreamOwnerUnreachable(t *testing.T) {
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddr := closed.Addr().String()
	_ = closed.Close()

	for name, addr := range map[string]string{"refused": closedAddr, "silent": silentOwner(t)} {
		t.Run(name, func(t *testing.T) {
			const proxyTimeout = 300 * time.Millisecond
			ic, ctx := realForwardInterceptor(t, addr, proxyTimeout)
			ss := &fakeServerStream{ctx: ctx, recv: []*cepb.CloudEvent{{Id: "req-net-stream"}}}
			start := time.Now()
			if err := ic.stream()(nil, ss, entityManageCollectionInfo(), mustNotRunStream(t)); err != nil {
				t.Fatalf("expected an envelope on the stream, got err: %v", err)
			}
			elapsed := time.Since(start)
			if elapsed > 5*time.Second {
				t.Fatalf("the wait was not bounded: %v", elapsed)
			}
			// The silent owner's answer is the timer's: the forward returned
			// only once the proxy timeout had cancelled it.
			if name == "silent" && elapsed < proxyTimeout {
				t.Fatalf("answered after %v, before the proxy timeout %v fired", elapsed, proxyTimeout)
			}
			if len(ss.sent) != 1 {
				t.Fatalf("sent %d frames, want one error envelope", len(ss.sent))
			}
			if r := envelopeRetryable(t, ss.sent[0], "req-net-stream"); r == nil || !*r {
				t.Fatalf("Retryable = %v, want true", r)
			}
		})
	}
}

// --- retryable only with proof that nothing was sent -------------------------

// envelopeRetryable asserts ce is a TRANSACTION_NODE_UNAVAILABLE envelope for
// reqID and returns its Error.Retryable as sent.
func envelopeRetryable(t *testing.T, ce any, reqID string) *bool {
	t.Helper()
	assertEnvelopeCode(t, ce, reqID, "TRANSACTION_NODE_UNAVAILABLE")
	return decodeTxResp(t, ce.(*cepb.CloudEvent)).Error.Retryable
}

// The registry says the owner is not live: nothing is sent, so a retry cannot
// repeat anything. Both routed doors.
func TestTxRoute_OwnerNotLive_Retryable(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	tok, _ := s.Issue(token.Claims{NodeID: "node-B", TxRef: "tx-dead", ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-tx-dead", Major: 1})
	reg := fakeRouteRegistry{nodes: map[string]contract.NodeInfo{
		"node-B": {NodeID: "node-B", Addr: "http://node-b:8080", Alive: false},
	}}
	ic := newTxRouteInterceptor(s, reg, "node-A", noCalloutJoiner(t, s, fakeJoinTM{}), 9090, true, testProxyTimeout)
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("tx-token", tok))

	resp, err := ic.unary()(ctx, &cepb.CloudEvent{Id: "req-dead"}, entityManageInfo(), mustNotRunUnary(t))
	if err != nil {
		t.Fatalf("expected envelope response, got gRPC err: %v", err)
	}
	if r := envelopeRetryable(t, resp, "req-dead"); r == nil || !*r {
		t.Fatalf("unary: Retryable = %v, want true", r)
	}

	ss := &fakeServerStream{ctx: ctx, recv: []*cepb.CloudEvent{{Id: "req-dead-stream"}}}
	if err := ic.stream()(nil, ss, entityManageCollectionInfo(), mustNotRunStream(t)); err != nil {
		t.Fatalf("expected an envelope on the stream, got err: %v", err)
	}
	if len(ss.sent) != 1 {
		t.Fatalf("sent %d frames, want one error envelope", len(ss.sent))
	}
	// A refusal before the request message is read carries no request id.
	if r := envelopeRetryable(t, ss.sent[0], ""); r == nil || !*r {
		t.Fatalf("stream: Retryable = %v, want true", r)
	}
}

// The pool's peer address guard refuses the owner's address before any
// connection is set up: nothing was sent. Both routed doors, through the
// production forward.
func TestTxRoute_ForbiddenPeerAddress_Retryable(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	tok, _ := s.Issue(token.Claims{NodeID: "node-B", TxRef: "tx-guard", ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-tx-guard", Major: 1})
	reg := fakeRouteRegistry{nodes: map[string]contract.NodeInfo{
		"node-B": {NodeID: "node-B", Addr: "http://127.0.0.1:8080", GRPCAddr: "127.0.0.1:19090", Alive: true},
	}}
	// allowLoopback=false: the production posture, under which the guard
	// refuses the loopback address.
	ic := newTxRouteInterceptor(s, reg, "node-A", noCalloutJoiner(t, s, fakeJoinTM{}), 9090, false, testProxyTimeout)
	t.Cleanup(ic.pool.Close)
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("tx-token", tok))

	resp, err := ic.unary()(ctx, &cepb.CloudEvent{Id: "req-guard"}, entityManageInfo(), mustNotRunUnary(t))
	if err != nil {
		t.Fatalf("expected envelope response, got gRPC err: %v", err)
	}
	if r := envelopeRetryable(t, resp, "req-guard"); r == nil || !*r {
		t.Fatalf("unary: Retryable = %v, want true", r)
	}

	ss := &fakeServerStream{ctx: ctx, recv: []*cepb.CloudEvent{{Id: "req-guard-stream"}}}
	if err := ic.stream()(nil, ss, entityManageCollectionInfo(), mustNotRunStream(t)); err != nil {
		t.Fatalf("expected an envelope on the stream, got err: %v", err)
	}
	if len(ss.sent) != 1 {
		t.Fatalf("sent %d frames, want one error envelope", len(ss.sent))
	}
	if r := envelopeRetryable(t, ss.sent[0], "req-guard-stream"); r == nil || !*r {
		t.Fatalf("stream: Retryable = %v, want true", r)
	}
}

// A forward error that carries no proxy.NotSentProof — here a DeadlineExceeded
// from the call — proves nothing about whether the owner received the request,
// and the owner may have run it: the answer carries no retryable flag.
func TestTxRoute_ForwardErrorWithoutProof_NotRetryable(t *testing.T) {
	ic, ctx := forwardingInterceptor(t, testProxyTimeout)
	ic.forwardUnary = func(context.Context, *proxy.ClientPool, string, *cepb.CloudEvent) (*cepb.CloudEvent, error) {
		return nil, status.Error(codes.DeadlineExceeded, "context deadline exceeded")
	}
	resp, err := ic.unary()(ctx, &cepb.CloudEvent{Id: "req-deadline"}, entityManageInfo(), mustNotRunUnary(t))
	if err != nil {
		t.Fatalf("expected envelope response, got gRPC err: %v", err)
	}
	if r := envelopeRetryable(t, resp, "req-deadline"); r != nil {
		t.Fatalf("Retryable = %v, want nil", *r)
	}
}

// The owner's port refuses connections: grpc-go never opens a stream to it, so
// no header of the request reached a transport. Nothing was sent: retryable.
// Both routed doors, over a real connection.
func TestTxRoute_DialRefused_Retryable(t *testing.T) {
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := closed.Addr().String()
	_ = closed.Close()

	ic, ctx := realForwardInterceptor(t, addr, 2*time.Second)
	resp, err := ic.unary()(ctx, &cepb.CloudEvent{Id: "req-refused"}, entityManageInfo(), mustNotRunUnary(t))
	if err != nil {
		t.Fatalf("expected envelope response, got gRPC err: %v", err)
	}
	if r := envelopeRetryable(t, resp, "req-refused"); r == nil || !*r {
		t.Fatalf("unary: Retryable = %v, want true", r)
	}

	ss := &fakeServerStream{ctx: ctx, recv: []*cepb.CloudEvent{{Id: "req-refused-stream"}}}
	if err := ic.stream()(nil, ss, entityManageCollectionInfo(), mustNotRunStream(t)); err != nil {
		t.Fatalf("expected an envelope on the stream, got err: %v", err)
	}
	if len(ss.sent) != 1 {
		t.Fatalf("sent %d frames, want one error envelope", len(ss.sent))
	}
	if r := envelopeRetryable(t, ss.sent[0], "req-refused-stream"); r == nil || !*r {
		t.Fatalf("stream: Retryable = %v, want true", r)
	}
}

// stallingOwner is a real owner that receives each call and never answers it.
type stallingOwner struct {
	cyodapb.UnimplementedCloudEventsServiceServer
	received chan string
}

func (o *stallingOwner) EntityManage(ctx context.Context, ce *cepb.CloudEvent) (*cepb.CloudEvent, error) {
	o.received <- ce.Id
	<-ctx.Done()
	return nil, ctx.Err()
}

func (o *stallingOwner) EntityManageCollection(ce *cepb.CloudEvent, ss googlegrpc.ServerStreamingServer[cepb.CloudEvent]) error {
	o.received <- ce.Id
	<-ss.Context().Done()
	return ss.Context().Err()
}

// awaitReceived fails the test unless the owner received the call named want
// within a bound.
func (o *stallingOwner) awaitReceived(t *testing.T, want string) {
	t.Helper()
	select {
	case got := <-o.received:
		if got != want {
			t.Fatalf("owner received %q, want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("owner did not receive %q", want)
	}
}

// awaitReady fails the test unless pool's connection to addr is ready within a
// bound.
func awaitReady(t *testing.T, pool *proxy.ClientPool, addr string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := pool.Get(ctx, addr)
	if err != nil {
		t.Fatalf("pool.Get(%s): %v", addr, err)
	}
	conn.Connect()
	for state := conn.GetState(); state != connectivity.Ready; state = conn.GetState() {
		if !conn.WaitForStateChange(ctx, state) {
			t.Fatalf("connection to %s not ready: %s", addr, state)
		}
	}
}

// The owner received the call and did not answer within the proxy timeout: it
// may have run the request, so a retry could repeat a write it kept. Not
// retryable. Both routed doors, over a real connection.
func TestTxRoute_OwnerTimesOutAfterSend_NotRetryable(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	owner := &stallingOwner{received: make(chan string, 2)}
	srv := googlegrpc.NewServer()
	cyodapb.RegisterCloudEventsServiceServer(srv, owner)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	ic, ctx := realForwardInterceptor(t, lis.Addr().String(), 300*time.Millisecond)
	// The connection is ready before the timed call, so the proxy timeout
	// bounds only the owner's silence, never a slow transport set-up.
	awaitReady(t, ic.pool, lis.Addr().String())
	resp, err := ic.unary()(ctx, &cepb.CloudEvent{Id: "req-stalled"}, entityManageInfo(), mustNotRunUnary(t))
	if err != nil {
		t.Fatalf("expected envelope response, got gRPC err: %v", err)
	}
	owner.awaitReceived(t, "req-stalled")
	if r := envelopeRetryable(t, resp, "req-stalled"); r != nil {
		t.Fatalf("unary: Retryable = %v, want nil", *r)
	}

	ss := &fakeServerStream{ctx: ctx, recv: []*cepb.CloudEvent{{Id: "req-stalled-stream"}}}
	if err := ic.stream()(nil, ss, entityManageCollectionInfo(), mustNotRunStream(t)); err != nil {
		t.Fatalf("expected an envelope on the stream, got err: %v", err)
	}
	owner.awaitReceived(t, "req-stalled-stream")
	if len(ss.sent) != 1 {
		t.Fatalf("sent %d frames, want one error envelope", len(ss.sent))
	}
	if r := envelopeRetryable(t, ss.sent[0], "req-stalled-stream"); r != nil {
		t.Fatalf("stream: Retryable = %v, want nil", *r)
	}
}
