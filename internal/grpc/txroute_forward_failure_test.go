package grpc

import (
	"context"
	"errors"
	"io"
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
	ic, ctx := forwardingInterceptor(t, shortProxyTimeout)
	ic.forwardStream = func(ctx context.Context, _ *proxy.ClientPool, _ string, _ *cepb.CloudEvent) (googlegrpc.ServerStreamingClient[cepb.CloudEvent], error) {
		return &blockingClientStream{ctx: ctx,
			frames: []*cepb.CloudEvent{{Id: "f1"}, {Id: "f2"}},
			delays: []time.Duration{0, 4 * shortProxyTimeout}}, nil
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
