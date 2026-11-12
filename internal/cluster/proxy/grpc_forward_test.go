package proxy_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	cyodapb "github.com/cyoda-platform/cyoda-go/api/grpc/cyoda"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/peeraddr"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/proxy"
)

// echoServer captures inbound metadata and echoes the CloudEvent back.
type echoServer struct {
	cyodapb.UnimplementedCloudEventsServiceServer
	gotMD metadata.MD
}

func (e *echoServer) EntityManage(ctx context.Context, ce *cepb.CloudEvent) (*cepb.CloudEvent, error) {
	e.gotMD, _ = metadata.FromIncomingContext(ctx)
	return ce, nil
}

func startEchoServer(t *testing.T) (addr string, srv *echoServer, stop func()) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	gs := grpc.NewServer()
	srv = &echoServer{}
	cyodapb.RegisterCloudEventsServiceServer(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	return lis.Addr().String(), srv, func() { gs.Stop() }
}

func TestForwardEntityManage_PropagatesMetadata(t *testing.T) {
	addr, srv, stop := startEchoServer(t)
	defer stop()

	pool := proxy.NewClientPool(true) // allowLoopback=true: test server binds on 127.0.0.1
	defer pool.Close()

	md := metadata.Pairs("tx-token", "tok-abc", "authorization", "Bearer xyz")
	ctx, cancel := context.WithTimeout(metadata.NewIncomingContext(context.Background(), md), 5*time.Second)
	defer cancel()

	resp, err := proxy.ForwardEntityManage(ctx, pool, addr, &cepb.CloudEvent{Id: "ce-1"})
	if err != nil {
		t.Fatalf("ForwardEntityManage: %v", err)
	}
	if resp.Id != "ce-1" {
		t.Fatalf("expected echoed id ce-1, got %q", resp.Id)
	}
	if got := srv.gotMD.Get("tx-token"); len(got) != 1 || got[0] != "tok-abc" {
		t.Fatalf("tx-token not propagated: %v", got)
	}
	if got := srv.gotMD.Get("authorization"); len(got) != 1 || got[0] != "Bearer xyz" {
		t.Fatalf("authorization not propagated: %v", got)
	}
}

func TestClientPool_ReusesConnections(t *testing.T) {
	addr, _, stop := startEchoServer(t)
	defer stop()

	pool := proxy.NewClientPool(true) // allowLoopback=true: test server binds on 127.0.0.1
	defer pool.Close()

	c1, err := pool.Get(context.Background(), addr)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	c2, err := pool.Get(context.Background(), addr)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if c1 != c2 {
		t.Fatal("expected the pool to reuse the same *grpc.ClientConn for an addr")
	}
}

// TestClientPool_SSRFGuard_RejectsLoopback verifies that ClientPool.Get
// returns peeraddr.ErrForbiddenPeerAddress when allowLoopback=false and a
// loopback target is requested, closing the SSRF pivot on the gRPC forward
// path symmetric with the HTTP proxy and dispatch forwarder.
func TestClientPool_SSRFGuard_RejectsLoopback(t *testing.T) {
	pool := proxy.NewClientPool(false) // production posture — loopback forbidden
	defer pool.Close()

	_, err := pool.Get(context.Background(), "127.0.0.1:9090")
	if err == nil {
		t.Fatal("Get(loopback) succeeded with allowLoopback=false, expected error")
	}
	if !errors.Is(err, peeraddr.ErrForbiddenPeerAddress) {
		t.Fatalf("expected ErrForbiddenPeerAddress, got %v", err)
	}
}

// TestClientPool_SSRFGuard_RefusesSchemedTargets pins that the guard and the
// dialer read the same host. grpc-go reads "scheme://authority/endpoint" with
// its own resolver rules: passthrough dials the endpoint whatever the
// authority, and dns asks the authority to resolve the endpoint. The guard
// would check only the authority, so a target in either form must be refused
// before any dial, and nothing may reach the loopback listener behind it.
func TestClientPool_SSRFGuard_RefusesSchemedTargets(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer lis.Close()
	accepted := make(chan struct{}, 1)
	go func() {
		if c, err := lis.Accept(); err == nil {
			accepted <- struct{}{}
			_ = c.Close()
		}
	}()
	loop := lis.Addr().String()

	pool := proxy.NewClientPool(false) // production posture — loopback forbidden
	defer pool.Close()

	for _, target := range []string{
		"passthrough://8.8.8.8/" + loop,
		"dns://8.8.8.8/" + loop,
		"passthrough:///" + loop,
		"passthrough:" + loop,
	} {
		conn, err := pool.Get(context.Background(), target)
		if err == nil {
			conn.Connect()
			t.Errorf("Get(%q) succeeded; want it refused", target)
			continue
		}
		if !errors.Is(err, peeraddr.ErrForbiddenPeerAddress) {
			t.Errorf("Get(%q) error = %v; want ErrForbiddenPeerAddress", target, err)
		}
	}
	select {
	case <-accepted:
		t.Fatal("a refused target still reached the loopback listener")
	case <-time.After(300 * time.Millisecond):
	}
}

// TestClientPool_SSRFGuard_AllowsLoopback verifies that allowLoopback=true
// permits loopback targets (so test fixtures that bind cluster nodes on
// 127.0.0.1 can dial through the pool without special casing).
func TestClientPool_SSRFGuard_AllowsLoopback(t *testing.T) {
	addr, _, stop := startEchoServer(t)
	defer stop()

	pool := proxy.NewClientPool(true) // test-fixture posture
	defer pool.Close()

	conn, err := pool.Get(context.Background(), addr)
	if err != nil {
		t.Fatalf("Get(loopback) with allowLoopback=true: %v", err)
	}
	if conn == nil {
		t.Fatal("expected non-nil conn")
	}
}

// largeAnswerServer answers EntitySearch with a CloudEvent of size bytes.
type largeAnswerServer struct {
	cyodapb.UnimplementedCloudEventsServiceServer
	size int
}

func (s *largeAnswerServer) EntitySearch(_ context.Context, ce *cepb.CloudEvent) (*cepb.CloudEvent, error) {
	return &cepb.CloudEvent{Id: ce.Id, Data: &cepb.CloudEvent_BinaryData{BinaryData: make([]byte, s.size)}}, nil
}

// A forwarded call receives what the owner answers, whatever its size, as the
// caller would calling the owner directly: the owner bounds a joined answer
// (CYODA_CALLOUT_JOINED_RESPONSE_MAX_BYTES, 10 MiB by default), and the hop
// adds no lower limit of its own — grpc-go's 4 MiB client default would.
func TestForwardEntitySearch_AnswerPastClientDefaultLimit(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	gs := grpc.NewServer()
	cyodapb.RegisterCloudEventsServiceServer(gs, &largeAnswerServer{size: 6 << 20})
	go func() { _ = gs.Serve(lis) }()
	defer gs.Stop()

	pool := proxy.NewClientPool(true) // allowLoopback=true: test server binds on 127.0.0.1
	defer pool.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := proxy.ForwardEntitySearch(ctx, pool, lis.Addr().String(), &cepb.CloudEvent{Id: "big"})
	if err != nil {
		t.Fatalf("ForwardEntitySearch: %v", err)
	}
	if got := len(resp.GetBinaryData()); got != 6<<20 {
		t.Fatalf("answer = %d bytes, want %d", got, 6<<20)
	}
}

// NotSentProof changes nothing a reader of the error sees: a status it wraps
// reads back unchanged, code and message, and an error without a status still
// has none.
func TestNotSentProof_ChangesNothingAStatusReaderSees(t *testing.T) {
	inner := status.Error(codes.Unavailable, "connection error")
	st, ok := status.FromError(&proxy.NotSentProof{Err: inner})
	if !ok || st.Code() != codes.Unavailable || st.Message() != "connection error" {
		t.Fatalf("FromError = %v, %v; want Unavailable %q, true", st, ok, "connection error")
	}
	if _, ok := status.FromError(&proxy.NotSentProof{Err: errors.New("guard refused")}); ok {
		t.Fatal("an error without a status reads as one when wrapped")
	}
	if !proxy.ProvesNotSent(fmt.Errorf("forward: %w", &proxy.NotSentProof{Err: inner})) {
		t.Fatal("the proof is not found through a wrap")
	}
}
