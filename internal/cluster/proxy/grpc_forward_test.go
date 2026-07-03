package proxy_test

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

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

	c1, err := pool.Get(addr)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	c2, err := pool.Get(addr)
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

	_, err := pool.Get("127.0.0.1:9090")
	if err == nil {
		t.Fatal("Get(loopback) succeeded with allowLoopback=false, expected error")
	}
	if !errors.Is(err, peeraddr.ErrForbiddenPeerAddress) {
		t.Fatalf("expected ErrForbiddenPeerAddress, got %v", err)
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

	conn, err := pool.Get(addr)
	if err != nil {
		t.Fatalf("Get(loopback) with allowLoopback=true: %v", err)
	}
	if conn == nil {
		t.Fatal("expected non-nil conn")
	}
}
