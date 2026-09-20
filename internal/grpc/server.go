package grpc

import (
	"net"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	cyodapb "github.com/cyoda-platform/cyoda-go/api/grpc/cyoda"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/domain/entity"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/internal/domain/txjoin"
)

// CloudEventsServiceImpl implements the Cyoda CloudEventsService gRPC service.
type CloudEventsServiceImpl struct {
	cyodapb.UnimplementedCloudEventsServiceServer
	registry          *MemberRegistry
	authSvc           contract.AuthenticationService
	txMgr             spi.TransactionManager
	entityHandler     *entity.Handler
	modelHandler      *model.Handler
	searchService     *search.SearchService
	keepAliveInterval time.Duration
	keepAliveTimeout  time.Duration
}

// Server wraps the gRPC server.
type Server struct {
	grpcServer *googlegrpc.Server
	service    *CloudEventsServiceImpl
}

// KeepAliveConfig governs both keep-alive layers on the member stream: the
// application-level CloudEvent ping and eviction (Interval between pings,
// Timeout of inbound silence or write stall before eviction) and grpc-go's
// transport keepalive (an HTTP/2 PING after Interval of idleness, the
// connection closed if unacknowledged within Timeout), which catches a peer
// whose TCP is alive but whose process is gone.
type KeepAliveConfig struct {
	Interval time.Duration
	Timeout  time.Duration
}

// NewServer creates a new gRPC server with auth interceptors and the
// CloudEventsService registered. When otelEnabled is true, OTel tracing
// is added via a stats handler before the auth interceptors.
// localGRPCPort is this node's gRPC listen port; it is used as the fallback
// when deriving a peer's gRPC address from its HTTP address (advertise-or-derive).
// allowLoopback must match cfg.Cluster.DispatchAllowLoopback; it gates the
// peer-address SSRF guard on the gRPC forward path (symmetric with the
// dispatch forwarder and HTTP proxy).
// j is the join layer: the tx-route interceptor hands it every request that
// carries a pass, and it joins the transaction, refuses a pass that no longer
// names the callout that compute node holds, and holds the transaction's lock
// for the length of the handler.
// healthFlag is the same flag the HTTP Recovery middleware stores into — a
// panic recovered on either door marks the node unhealthy. May be nil in
// tests that don't care about health-flag observation.
func NewServer(
	authSvc contract.AuthenticationService,
	registry *MemberRegistry,
	txMgr spi.TransactionManager,
	entityHandler *entity.Handler,
	modelHandler *model.Handler,
	searchService *search.SearchService,
	tokenSigner *token.Signer,
	j *txjoin.Joiner,
	nodeRegistry contract.NodeRegistry,
	selfNodeID string,
	otelEnabled bool,
	localGRPCPort int,
	allowLoopback bool,
	healthFlag *atomic.Bool,
	keepAlive KeepAliveConfig,
) *Server {
	var opts []googlegrpc.ServerOption
	if otelEnabled {
		opts = append(opts, googlegrpc.StatsHandler(otelgrpc.NewServerHandler()))
	}
	// Recovery runs first so it also covers a panic inside auth or tx-routing.
	// Auth runs second so the tx-route interceptor sees the authenticated
	// UserContext (JoinFromToken's tenant check depends on it); tx-route runs
	// third, joining the referenced transaction or forwarding to its owner.
	txRoute := newTxRouteInterceptor(tokenSigner, nodeRegistry, selfNodeID, j, localGRPCPort, allowLoopback)
	opts = append(opts,
		googlegrpc.ChainUnaryInterceptor(
			UnaryRecoveryInterceptor(healthFlag),
			UnaryAuthInterceptor(authSvc),
			txRoute.unary(),
		),
		googlegrpc.ChainStreamInterceptor(
			StreamRecoveryInterceptor(healthFlag),
			StreamAuthInterceptor(authSvc),
			txRoute.stream(),
		),
	)
	// Transport keepalive. MaxConnectionIdle/Age stay infinite: an age
	// would GOAWAY healthy compute nodes on a timer and fail their in-flight
	// dispatches. The enforcement policy is deliberately permissive —
	// grpc-go's default (MinTime 5m) would GOAWAY an external compute node
	// that pings more often than every five minutes.
	// The MinTime/PermitWithoutStream constants below are reviewed, not
	// covered by a unit test: discriminating a too-strict enforcement
	// policy from a correct one needs ~20s of real-network idle time (three
	// GOAWAY strikes at grpc-go's own ping cadence), which is too slow for
	// this package's test budget.
	opts = append(opts,
		googlegrpc.KeepaliveParams(keepalive.ServerParameters{Time: keepAlive.Interval, Timeout: keepAlive.Timeout}),
		googlegrpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 5 * time.Second, PermitWithoutStream: true}),
	)
	grpcServer := googlegrpc.NewServer(opts...)
	svc := &CloudEventsServiceImpl{
		registry:          registry,
		authSvc:           authSvc,
		txMgr:             txMgr,
		entityHandler:     entityHandler,
		modelHandler:      modelHandler,
		searchService:     searchService,
		keepAliveInterval: keepAlive.Interval,
		keepAliveTimeout:  keepAlive.Timeout,
	}
	cyodapb.RegisterCloudEventsServiceServer(grpcServer, svc)
	return &Server{grpcServer: grpcServer, service: svc}
}

// Serve starts the gRPC server on the given listener.
func (s *Server) Serve(lis net.Listener) error {
	return s.grpcServer.Serve(lis)
}

// GracefulStop gracefully stops the gRPC server.
func (s *Server) GracefulStop() {
	s.grpcServer.GracefulStop()
}

// GRPCServer returns the underlying grpc.Server for testing.
func (s *Server) GRPCServer() *googlegrpc.Server {
	return s.grpcServer
}

// KeepAlive returns the keep-alive configuration the server was constructed
// with, for tests that assert config wiring reaches the server.
func (s *Server) KeepAlive() KeepAliveConfig {
	return KeepAliveConfig{Interval: s.service.keepAliveInterval, Timeout: s.service.keepAliveTimeout}
}
