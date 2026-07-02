package proxy

import (
	"context"
	"strings"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	cyodapb "github.com/cyoda-platform/cyoda-go/api/grpc/cyoda"
)

// ClientPool holds one lazily-dialled *grpc.ClientConn per peer address.
//
// Intra-cluster gRPC dials use insecure transport credentials, matching the
// cluster's HTTP peer posture (the dispatch forwarder POSTs over plain HTTP):
// peers are trusted at the transport layer, and every forwarded call still
// carries the caller's JWT (re-authenticated by the owner) plus the HMAC
// tx-token. If the cluster later adopts mTLS for peer traffic, swap the creds
// here and in the HTTP forwarder together.
type ClientPool struct {
	mu    sync.Mutex
	conns map[string]*grpc.ClientConn
}

// NewClientPool returns an empty pool.
func NewClientPool() *ClientPool {
	return &ClientPool{conns: make(map[string]*grpc.ClientConn)}
}

// Get returns a cached connection for addr, dialling one on first use.
func (p *ClientPool) Get(addr string) (*grpc.ClientConn, error) {
	target := grpcTarget(addr)

	p.mu.Lock()
	defer p.mu.Unlock()
	if conn, ok := p.conns[target]; ok {
		return conn, nil
	}
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	p.conns[target] = conn
	return conn, nil
}

// Close tears down every pooled connection.
func (p *ClientPool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for addr, conn := range p.conns {
		_ = conn.Close()
		delete(p.conns, addr)
	}
}

// grpcTarget normalises a registry address into a gRPC dial target. Registry
// addresses carry an HTTP scheme (e.g. "http://host:8080"); grpc.NewClient
// wants a bare "host:port" (a leading scheme is parsed as a resolver name and
// fails). The scheme is stripped here.
//
// NOTE: the stripped host:port still points at the peer's HTTP port. The node
// registry advertises only the HTTP NodeAddr, not a distinct gRPC endpoint, so
// cross-node gRPC forwarding requires the registry to advertise a gRPC address
// (or a derivable convention) before it reaches a real peer's gRPC server.
func grpcTarget(addr string) string {
	if i := strings.Index(addr, "://"); i >= 0 {
		return addr[i+3:]
	}
	return addr
}

// ForwardEntityManage dials the owning node and replays a unary EntityManage
// call, propagating the inbound metadata (auth + tx-token) onto the outgoing
// call. Connections are cached per addr. The token is never logged.
func ForwardEntityManage(ctx context.Context, pool *ClientPool, addr string, ce *cepb.CloudEvent) (*cepb.CloudEvent, error) {
	conn, err := pool.Get(addr)
	if err != nil {
		return nil, err
	}
	client := cyodapb.NewCloudEventsServiceClient(conn)
	return client.EntityManage(withForwardedMetadata(ctx), ce)
}

// ForwardEntityManageCollection dials the owning node and re-issues the
// server-streaming EntityManageCollection call, returning the client stream so
// the caller can copy frames back to the inbound stream.
func ForwardEntityManageCollection(ctx context.Context, pool *ClientPool, addr string, ce *cepb.CloudEvent) (grpc.ServerStreamingClient[cepb.CloudEvent], error) {
	conn, err := pool.Get(addr)
	if err != nil {
		return nil, err
	}
	client := cyodapb.NewCloudEventsServiceClient(conn)
	return client.EntityManageCollection(withForwardedMetadata(ctx), ce)
}

// withForwardedMetadata copies inbound metadata (auth + tx-token) onto the
// outgoing context so the owner node re-authenticates and re-routes correctly.
func withForwardedMetadata(ctx context.Context) context.Context {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		return metadata.NewOutgoingContext(ctx, md)
	}
	return ctx
}
