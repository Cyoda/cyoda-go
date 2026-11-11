package proxy

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/resolver"

	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	cyodapb "github.com/cyoda-platform/cyoda-go/api/grpc/cyoda"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/peeraddr"
)

// ClientPool holds one lazily-dialled *grpc.ClientConn per peer address.
//
// Intra-cluster gRPC dials use insecure transport credentials, matching the
// cluster's HTTP peer posture (the dispatch forwarder POSTs over plain HTTP):
// peers are trusted at the transport layer, and every forwarded call still
// carries the caller's JWT (re-authenticated by the owner) plus the HMAC
// tx-token. If the cluster later adopts mTLS for peer traffic, swap the creds
// here and in the HTTP forwarder together.
//
// Every dial is gated by the same peer-address SSRF guard used by the dispatch
// forwarder and the HTTP reverse-proxy path — see peeraddr.Validate.
type ClientPool struct {
	mu            sync.Mutex
	conns         map[string]*grpc.ClientConn
	allowLoopback bool
}

// NewClientPool returns an empty pool. allowLoopback must match
// cfg.Cluster.DispatchAllowLoopback; it gates whether 127.x/::1 targets are
// permitted. Set true only in test fixtures — in production leave it false so
// the SSRF guard rejects loopback dials.
func NewClientPool(allowLoopback bool) *ClientPool {
	return &ClientPool{conns: make(map[string]*grpc.ClientConn), allowLoopback: allowLoopback}
}

// NotSentProof marks a forward error that proves nothing was sent to the
// owner: the pool refused or failed to set up the connection, before the call
// was made. Its absence proves nothing: a failure of the call itself may follow
// a send, because grpc-go connects lazily, inside the call.
//
// It changes nothing a reader of the error sees: Error is the wrapped error's
// text, and Unwrap exposes it to errors.Is and errors.As.
type NotSentProof struct{ Err error }

func (p *NotSentProof) Error() string { return p.Err.Error() }

func (p *NotSentProof) Unwrap() error { return p.Err }

// ProvesNotSent reports whether err carries a NotSentProof anywhere on its
// chain.
func ProvesNotSent(err error) bool {
	var proof *NotSentProof
	return errors.As(err, &proof)
}

// Get returns a cached connection for addr, dialling one on first use.
// The peer address is validated via the SSRF guard before any dial is
// attempted; forbidden addresses (loopback when disallowed, link-local,
// unspecified, multicast) are rejected with peeraddr.ErrForbiddenPeerAddress.
// ctx bounds the name lookup that guard makes, so a forwarded call whose caller
// has gone does not wait on a resolver, and peeraddr's own bound for a lookup
// that precedes a dial is applied on top of it.
//
// Every error Get returns carries NotSentProof: no call has been made.
func (p *ClientPool) Get(ctx context.Context, addr string) (*grpc.ClientConn, error) {
	conn, err := p.get(ctx, addr)
	if err != nil {
		return nil, &NotSentProof{Err: err}
	}
	return conn, nil
}

func (p *ClientPool) get(ctx context.Context, addr string) (*grpc.ClientConn, error) {
	target, err := dialTarget(addr)
	if err != nil {
		return nil, err
	}
	lookupCtx, cancel := context.WithTimeout(ctx, peeraddr.LookupTimeout)
	defer cancel()
	if err := peeraddr.Validate(lookupCtx, addr, p.allowLoopback); err != nil {
		return nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if conn, ok := p.conns[addr]; ok {
		return conn, nil
	}
	// Never a proxy: grpc-go's default dialer reads the environment's proxy
	// settings, and through one the call goes to an address the guard above
	// never validated.
	//
	// No receive limit of the hop's own: the caller gets what the owner
	// answers, as it would calling the owner directly. The owner bounds a
	// joined answer (CYODA_CALLOUT_JOINED_RESPONSE_MAX_BYTES), as it does on
	// the HTTP proxy, which adds no limit either; grpc-go's 4 MiB client
	// default would refuse an answer the owner is allowed to send.
	conn, err := grpc.NewClient(target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithNoProxy(),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(math.MaxInt32)))
	if err != nil {
		return nil, err
	}
	p.conns[addr] = conn
	return conn, nil
}

// dialTarget returns the grpc-go dial target for addr, or refuses addr with
// peeraddr.ErrForbiddenPeerAddress. addr is the peer's gRPC endpoint, a bare
// "host:port": derived by resolveGRPCAddr in internal/grpc, or the peer's
// advertised CYODA_GRPC_NODE_ADDR as gossiped. Any other form is refused.
// grpc-go parses its target as a URL: it takes a scheme and authority from
// it, percent-decodes the endpoint and cuts it at ? and #. A target in any
// such form could dial a host other than the one peeraddr.Validate checks on
// the raw string, so the target names the dns resolver itself and addr is
// accepted only if grpc-go's own parse gives it back unchanged.
func dialTarget(addr string) (string, error) {
	target := "dns:///" + addr
	refuse := fmt.Errorf("%w: %q is not a host:port", peeraddr.ErrForbiddenPeerAddress, addr)
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return "", refuse
	}
	u, err := url.Parse(target)
	if err != nil || u.Host != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", refuse
	}
	if (resolver.Target{URL: *u}).Endpoint() != addr {
		return "", refuse
	}
	return target, nil
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

// ForwardEntityManage dials the owning node and replays a unary EntityManage
// call, propagating the inbound metadata (auth + tx-token) onto the outgoing
// call. Connections are cached per addr. The token is never logged.
func ForwardEntityManage(ctx context.Context, pool *ClientPool, addr string, ce *cepb.CloudEvent) (*cepb.CloudEvent, error) {
	conn, err := pool.Get(ctx, addr)
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
	conn, err := pool.Get(ctx, addr)
	if err != nil {
		return nil, err
	}
	client := cyodapb.NewCloudEventsServiceClient(conn)
	return client.EntityManageCollection(withForwardedMetadata(ctx), ce)
}

// ForwardEntitySearch dials the owning node and replays a unary EntitySearch
// call, propagating the inbound metadata (auth + tx-token) so the owner joins
// the referenced transaction and the read observes its uncommitted writes.
// Connections are cached per addr. The token is never logged.
func ForwardEntitySearch(ctx context.Context, pool *ClientPool, addr string, ce *cepb.CloudEvent) (*cepb.CloudEvent, error) {
	conn, err := pool.Get(ctx, addr)
	if err != nil {
		return nil, err
	}
	client := cyodapb.NewCloudEventsServiceClient(conn)
	return client.EntitySearch(withForwardedMetadata(ctx), ce)
}

// ForwardEntitySearchCollection dials the owning node and re-issues the
// server-streaming EntitySearchCollection call, returning the client stream so
// the caller can copy frames back to the inbound stream.
func ForwardEntitySearchCollection(ctx context.Context, pool *ClientPool, addr string, ce *cepb.CloudEvent) (grpc.ServerStreamingClient[cepb.CloudEvent], error) {
	conn, err := pool.Get(ctx, addr)
	if err != nil {
		return nil, err
	}
	client := cyodapb.NewCloudEventsServiceClient(conn)
	return client.EntitySearchCollection(withForwardedMetadata(ctx), ce)
}

// withForwardedMetadata copies inbound metadata (auth + tx-token) onto the
// outgoing context so the owner node re-authenticates and re-routes correctly.
func withForwardedMetadata(ctx context.Context) context.Context {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		return metadata.NewOutgoingContext(ctx, md)
	}
	return ctx
}
