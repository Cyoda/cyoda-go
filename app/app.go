package app

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/cmd/cyoda/help"
	internalapi "github.com/cyoda-platform/cyoda-go/internal/api"
	"github.com/cyoda-platform/cyoda-go/internal/api/middleware"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/internal/cluster"
	clusterdispatch "github.com/cyoda-platform/cyoda-go/internal/cluster/dispatch"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/lifecycle"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/modelcache"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/proxy"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/registry"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/domain/account"
	"github.com/cyoda-platform/cyoda-go/internal/domain/audit"
	"github.com/cyoda-platform/cyoda-go/internal/domain/entity"
	"github.com/cyoda-platform/cyoda-go/internal/domain/messaging"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
	mockiam "github.com/cyoda-platform/cyoda-go/internal/iam/mock"
	"github.com/cyoda-platform/cyoda-go/internal/observability"
	"github.com/cyoda-platform/cyoda-go/internal/skeleton"
)

type App struct {
	config             Config
	storeFactory       spi.StoreFactory
	transactionManager spi.TransactionManager
	authService        contract.AuthenticationService
	authzService       contract.AuthorizationService
	workflowEngine     *workflow.Engine
	searchService      *search.SearchService
	auditService       contract.AuditService
	clusterService     contract.ClusterService
	memberRegistry     *internalgrpc.MemberRegistry
	grpcServer         *internalgrpc.Server
	handler            http.Handler
	tokenSigner        *token.Signer
	nodeRegistry       contract.NodeRegistry
	txLifecycle        *lifecycle.Manager
	stopReaper         chan struct{}
	stopSearchReaper   chan struct{}
	grpcStopOnce       sync.Once
}

func New(cfg Config) *App {
	// Validate and normalise bootstrap config before any auth wiring.
	validatedCfg, err := validateBootstrapConfig(&cfg)
	if err != nil {
		slog.Error("invalid bootstrap configuration", "pkg", "app", "err", err)
		os.Exit(1)
	}
	cfg = *validatedCfg

	// Metrics-auth coupled predicate: if CYODA_METRICS_REQUIRE_AUTH=true
	// the bearer must be set. Refuse to start rather than silently drop
	// auth for operators who thought they'd enabled it.
	if err := validateMetricsAuth(&cfg); err != nil {
		slog.Error("invalid metrics auth configuration", "pkg", "app", "err", err)
		os.Exit(1)
	}

	a := &App{config: cfg}

	common.SetErrorResponseMode(cfg.ErrorResponseMode)

	// cfg.StorageBackend is populated at config-construction time from the
	// CYODA_STORAGE_BACKEND env var with "memory" as the default.
	plugin, ok := spi.GetPlugin(cfg.StorageBackend)
	if !ok {
		slog.Error("unknown storage backend",
			"backend", cfg.StorageBackend,
			"available", spi.RegisteredPlugins())
		os.Exit(1)
	}

	slog.Info("storage backend selected",
		"backend", plugin.Name(),
		"available", spi.RegisteredPlugins())

	// Cluster infrastructure the plugin factory may need (e.g. the cassandra
	// plugin uses the broadcaster for clock gossip) is created up-front when
	// cluster mode is on; the same instance is then bound as the app's node
	// registry later in this function. In single-node mode gossipReg stays
	// nil and plugins receive no broadcaster.
	var gossipReg *registry.Gossip
	if cfg.Cluster.Enabled {
		validateClusterConfig(cfg.Cluster)
		var signerErr error
		a.tokenSigner, signerErr = token.NewSigner(cfg.Cluster.HMACSecret)
		if signerErr != nil {
			slog.Error("failed to create token signer", "pkg", "cluster", "err", signerErr)
			os.Exit(1)
		}
		gossipReg = mustNewGossip(cfg.Cluster)
	}

	var factoryOpts []spi.FactoryOption
	if gossipReg != nil {
		factoryOpts = append(factoryOpts, spi.WithClusterBroadcaster(gossipReg))
	}

	// startupCtx carries a deadline so unreachable infrastructure fails fast
	// instead of hanging in pgxpool or gocql.
	startupCtx, cancel := context.WithTimeout(context.Background(), cfg.StartupTimeout)
	defer cancel()

	factory, err := plugin.NewFactory(startupCtx, os.Getenv, factoryOpts...)
	if err != nil {
		slog.Error("startup failure",
			"phase", "create-storage-factory",
			"backend", plugin.Name(),
			"error", err.Error())
		os.Exit(1)
	}

	// Wire the schema.Apply replay function into the plugin factory so
	// ExtendSchema can fold deltas on read. Postgres uses this to fold
	// the extension log; SQLite/Memory use it to apply in-place. The
	// interface uses the raw function signature (not any plugin-local
	// named ApplyFunc type) so a single type-assertion satisfies all
	// plugins uniformly.
	type applyFuncSetter interface {
		SetApplyFunc(fn func(base []byte, delta spi.SchemaDelta) ([]byte, error))
	}
	if setter, ok := factory.(applyFuncSetter); ok {
		setter.SetApplyFunc(makeSchemaApply())
	}

	// Wrap the factory in the caching decorator. ModelStore(ctx) now
	// returns a per-request adapter that reads through one shared
	// cache (tenant-scoped via ctx). In cluster mode the decorator
	// publishes "model.invalidate" on gossipReg so peer nodes stay in
	// sync; in single-node mode no broadcaster is installed (passing a
	// typed-nil *registry.Gossip as an interface would still evaluate
	// != nil and trigger a nil-deref, so we pass an untyped nil).
	var cacheBroadcaster spi.ClusterBroadcaster
	if gossipReg != nil {
		cacheBroadcaster = gossipReg
	}
	cachingStoreFactory := modelcache.NewCachingStoreFactory(
		factory,
		cacheBroadcaster,
		nil, // wall clock
		cfg.ModelCacheLease,
	)
	a.storeFactory = cachingStoreFactory

	// Startable plugins (cassandra, etc.) must complete Start BEFORE the
	// factory can serve TransactionManager: the initial takeover / shard-
	// rebalance / clock-cache warmup that Start drives is a precondition
	// for tx begin. Plugins with no background lifecycle (memory,
	// postgres) don't implement Startable, so this is a no-op for them.
	if s, ok := factory.(spi.Startable); ok {
		if err := s.Start(startupCtx); err != nil {
			slog.Error("startup failure",
				"phase", "start-storage-factory",
				"backend", plugin.Name(),
				"error", err.Error())
			os.Exit(1)
		}
		slog.Info("storage plugin started", "pkg", "app", "backend", plugin.Name())
	}

	txMgr, err := factory.TransactionManager(startupCtx)
	if err != nil {
		slog.Error("startup failure",
			"phase", "transaction-manager",
			"backend", plugin.Name(),
			"error", err.Error())
		os.Exit(1)
	}
	a.transactionManager = txMgr

	// Decorator wrap order (innermost → outermost, per D13 of the spec):
	//   plugin TM → metrics → tracing → logging → domain-service consumers
	// Today only tracing is wired; add future decorators between tracing and
	// the plugin TM in the order named here.
	if cfg.OTelEnabled {
		a.transactionManager = observability.NewTracingTransactionManager(a.transactionManager)
	}

	// Auth service: JWT or mock mode
	var authSvc *auth.AuthService
	if cfg.IAM.Mode == "jwt" {
		if cfg.IAM.JWTSigningKey == "" {
			slog.Error("startup failure",
				"phase", "jwt-signing-key",
				"error", "CYODA_JWT_SIGNING_KEY is required when IAM mode is jwt")
			os.Exit(1)
		}
		// Create a KV-backed trusted key store for persistence across restarts.
		systemCtx := spi.WithUserContext(context.Background(), &spi.UserContext{
			UserID:   "system",
			UserName: "System",
			Tenant:   spi.Tenant{ID: spi.SystemTenantID, Name: "System"},
		})
		kvStore, err := a.storeFactory.KeyValueStore(systemCtx)
		if err != nil {
			slog.Error("startup failure",
				"phase", "kv-store-trusted-keys",
				"error", err.Error())
			os.Exit(1)
		}
		trustedKeyStore, err := auth.NewKVTrustedKeyStore(systemCtx, kvStore)
		if err != nil {
			slog.Error("startup failure",
				"phase", "kv-trusted-store-bootstrap",
				"error", err.Error())
			os.Exit(1)
		}
		authSvc, err = auth.NewAuthService(auth.AuthConfig{
			SigningKeyPEM:   cfg.IAM.JWTSigningKey,
			Issuer:          cfg.IAM.JWTIssuer,
			ExpirySeconds:   cfg.IAM.JWTExpiry,
			TrustedKeyStore: trustedKeyStore,
			IAMFeatures:     cfg.IAM.AuthIAMFeatures(),
		})
		if err != nil {
			slog.Error("startup failure",
				"phase", "auth-service",
				"error", err.Error())
			os.Exit(1)
		}
		// The built-in IAM holds its signing keys in-process, so the validator
		// reads public keys directly from the local key store. No loopback JWKS
		// fetch, no HTTP client, no attack surface on that path.
		validator := auth.NewValidatorFromSource(auth.NewLocalKeySource(authSvc.KeyStore()), authSvc.Issuer())
		if cfg.IAM.JWTAudience != "" {
			validator.SetExpectedAudience(cfg.IAM.JWTAudience)
		}
		a.authService = auth.NewDelegatingAuthenticator(validator)

		// Bootstrap M2M client if configured.
		// validateBootstrapConfig (called above) guarantees that in jwt mode,
		// ClientID and ClientSecret are coupled: both set or neither set.
		if cfg.Bootstrap.ClientID != "" {
			roles := strings.Split(cfg.Bootstrap.Roles, ",")
			for i := range roles {
				roles[i] = strings.TrimSpace(roles[i])
			}
			if err := authSvc.M2MClientStore().CreateWithSecret(
				cfg.Bootstrap.ClientID,
				cfg.Bootstrap.TenantID,
				cfg.Bootstrap.UserID,
				cfg.Bootstrap.ClientSecret,
				roles,
			); err != nil {
				slog.Error("startup failure",
					"phase", "bootstrap-m2m-client",
					"clientId", cfg.Bootstrap.ClientID,
					"error", err.Error())
				os.Exit(1)
			}
			slog.Info("bootstrap M2M client registered",
				"pkg", "app",
				"clientId", cfg.Bootstrap.ClientID,
				"tenantId", cfg.Bootstrap.TenantID,
				"roles", roles,
			)
		}
	} else {
		defaultUser := &spi.UserContext{
			UserID:   cfg.IAM.MockUserID,
			UserName: cfg.IAM.MockUserName,
			Tenant: spi.Tenant{
				ID:   spi.TenantID(cfg.IAM.MockTenantID),
				Name: cfg.IAM.MockTenantName,
			},
			Roles: cfg.IAM.MockRoles,
		}
		a.authService = mockiam.NewAuthenticationService(defaultUser)
	}
	a.authzService = mockiam.NewAuthorizationService()

	a.memberRegistry = internalgrpc.NewMemberRegistry()
	localDispatcher := internalgrpc.NewProcessorDispatcher(a.memberRegistry, common.NewDefaultUUIDGenerator())
	searchStore, err := a.storeFactory.AsyncSearchStore(context.Background())
	if err != nil {
		slog.Error("startup failure",
			"phase", "async-search-store",
			"error", err.Error())
		os.Exit(1)
	}
	// Negative cache for pre-execution field-path validation. Wired
	// to the descriptor cache via SubscribeLocal: every model
	// invalidation (local mutation OR gossip-received event) drops
	// the corresponding negative-cache bucket. This works on
	// single-node and multi-node alike (issue #174 — pre-fix the
	// cache subscribed to the broadcaster directly, so single-node
	// deployments where the broadcaster is nil never received any
	// invalidations). Per-(tenant, ref) bucketed otter caches isolate
	// cross-tenant eviction (issue #175).
	pathValidationCache := search.NewPathValidationCache()
	cachingStoreFactory.SubscribeLocal(pathValidationCache.InvalidateRef)
	a.searchService = search.
		NewSearchService(a.storeFactory, common.NewDefaultUUIDGenerator(), searchStore).
		WithPathValidationCache(pathValidationCache)

	// Search snapshot TTL reaper (uses stopSearchReaper for graceful shutdown)
	a.stopSearchReaper = make(chan struct{})
	go func() {
		ticker := time.NewTicker(cfg.SearchReapInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				reaped, err := searchStore.ReapExpired(context.Background(), cfg.SearchSnapshotTTL)
				if err != nil {
					slog.Error("search snapshot reaper error", "pkg", "search", "err", err)
				} else if reaped > 0 {
					slog.Info("reaped expired search snapshots", "pkg", "search", "count", reaped)
				}
			case <-a.stopSearchReaper:
				return
			}
		}
	}()

	a.auditService = skeleton.NewAuditService()
	a.clusterService = internalgrpc.NewClusterService(a.memberRegistry)

	// Cluster components
	a.txLifecycle = lifecycle.NewManager(cfg.Cluster.OutcomeTTL)
	// Wire the TM so the TTL reaper can roll back the underlying transaction
	// when a cluster-level timeout fires; otherwise the plugin's physical
	// handle is orphaned until the database's own idle timeout catches it.
	a.txLifecycle.SetTransactionManager(a.transactionManager)
	if cfg.Cluster.Enabled {
		// gossipReg was created above (before plugin.NewFactory) so the plugin
		// could subscribe to broadcast topics. Join the cluster now; subscribers
		// are already registered, so no messages are dropped.
		// Use startupCtx so the gossip join honors CYODA_STARTUP_TIMEOUT
		// (issue #9) instead of the legacy hard-coded 2-minute deadline.
		if err := gossipReg.Register(startupCtx, cfg.Cluster.NodeID, cfg.Cluster.NodeAddr); err != nil {
			slog.Error("failed to register with gossip cluster", "pkg", "cluster", "err", err)
			os.Exit(1)
		}
		a.nodeRegistry = gossipReg

		slog.Info("cluster mode enabled", "pkg", "cluster", "nodeID", cfg.Cluster.NodeID, "gossipAddr", cfg.Cluster.GossipAddr)

		// Start TTL reaper goroutine with shutdown support
		a.stopReaper = make(chan struct{})
		go func() {
			ticker := time.NewTicker(cfg.Cluster.TxReapInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					reaped, err := a.txLifecycle.ReapExpired(context.Background())
					if err != nil {
						slog.Error("tx reaper error", "pkg", "cluster", "err", err)
					} else if reaped > 0 {
						slog.Info("reaped expired transactions", "pkg", "cluster", "count", reaped)
					}
				case <-a.stopReaper:
					return
				}
			}
		}()
	} else {
		a.nodeRegistry = registry.NewLocal("local", fmt.Sprintf("localhost:%d", cfg.HTTPPort))
	}

	// Wire external processing dispatcher
	var extProc contract.ExternalProcessingService
	// Peer-auth for inter-node dispatch. AES-256-GCM + HKDF-derived key over
	// the shared cluster secret. 30-second timestamp skew window. Constructed
	// once and shared by forwarder and handler so rotation is atomic.
	var peerAuth clusterdispatch.PeerAuth
	if cfg.Cluster.Enabled {
		auth, err := clusterdispatch.NewAEADPeerAuth(cfg.Cluster.HMACSecret, 30*time.Second)
		if err != nil {
			slog.Error("failed to construct dispatch peer auth", "pkg", "cluster", "err", err)
			os.Exit(1)
		}
		peerAuth = auth
	}
	if cfg.ExternalProcessing != nil {
		extProc = cfg.ExternalProcessing
	} else if cfg.Cluster.Enabled {
		extProc = clusterdispatch.NewClusterDispatcher(
			localDispatcher,
			a.nodeRegistry,
			cfg.Cluster.NodeID,
			clusterdispatch.NewRandomSelector(),
			clusterdispatch.NewHTTPForwarder(peerAuth, cfg.Cluster.DispatchForwardTimeout),
			cfg.Cluster.DispatchWaitTimeout,
		)
	} else {
		extProc = localDispatcher
	}
	if cfg.OTelEnabled {
		extProc = observability.NewTracingExternalProcessingService(extProc)
	}
	a.workflowEngine = workflow.NewEngine(a.storeFactory, common.NewDefaultUUIDGenerator(), a.transactionManager,
		workflow.WithExternalProcessing(extProc),
		workflow.WithMaxStateVisits(cfg.MaxStateVisits))

	// Wire MemberRegistry onChange to gossip tag updates
	if cfg.Cluster.Enabled {
		a.memberRegistry.SetOnChange(func(tags map[string][]string) {
			if gossipReg, ok := a.nodeRegistry.(*registry.Gossip); ok {
				if err := gossipReg.UpdateTags(tags); err != nil {
					slog.Error("failed to update gossip tags", "pkg", "cluster", "err", err)
				}
			}
		})
	}

	// Domain handlers
	entityHandler := entity.New(a.storeFactory, a.transactionManager, common.NewDefaultUUIDGenerator(), a.workflowEngine)
	modelHandler := model.New(a.storeFactory)
	server := internalapi.NewServer()
	server.Entity = entityHandler
	server.Model = modelHandler
	server.Workflow = workflow.New(a.storeFactory, a.workflowEngine)
	server.Search = search.NewHandlerWithModel(a.searchService, a.storeFactory)
	server.Audit = audit.New(a.storeFactory)
	server.Messaging = messaging.New(a.storeFactory, common.NewDefaultUUIDGenerator())
	var accountKeyStore auth.KeyStore
	var accountTrustedKeyStore auth.TrustedKeyStore
	if authSvc != nil {
		accountKeyStore = authSvc.KeyStore()
		accountTrustedKeyStore = authSvc.TrustedKeyStore()
	}
	server.Account = account.New(a.authService, a.authzService, accountKeyStore, accountTrustedKeyStore, cfg.IAM.AuthIAMFeatures())

	// Build HTTP handler
	mux := http.NewServeMux()

	healthFlag := &atomic.Bool{}
	healthFlag.Store(true)

	// Infrastructure routes (no auth, receives health flag)
	internalapi.RegisterHealthRoutes(mux, healthFlag)

	// Auth service route registration is split into two strict groups so
	// nothing administrative leaks into the public surface (#34 item 1):
	//
	//   PUBLIC (no auth): /.well-known/jwks.json, POST /oauth/token.
	//     These are the OAuth2/OIDC discovery + token-exchange endpoints
	//     and must be reachable by unauthenticated callers by protocol.
	//
	//   ADMIN (authMW + ROLE_ADMIN): /account/m2m, /account/m2m/*.
	//     Two-layer enforcement: middleware.Auth populates UserContext (or
	//     rejects with 401), then the handlers in internal/auth/ call the
	//     requireAdmin guard which enforces ROLE_ADMIN (or rejects with 403).
	//     Both layers are required — authMW alone would let any
	//     authenticated caller manage M2M clients.

	// Public auth endpoints (no auth middleware).
	if authSvc != nil {
		mux.Handle("/.well-known/", authSvc.Handler())
		mux.Handle("POST /oauth/token", authSvc.Handler())
	}

	// Admin routes (auth middleware required).
	authMW := middleware.Auth(a.authService)

	// Admin auth endpoints: key management, M2M clients, trusted keys.
	// The handler-side requireAdmin guard enforces ROLE_ADMIN; authMW here
	// guarantees the UserContext is populated so the guard has something
	// to check.
	if authSvc != nil {
		mux.Handle("/account/m2m/", authMW(authSvc.AdminHandler()))
		mux.Handle("/account/m2m", authMW(authSvc.AdminHandler()))
	}
	mux.Handle("GET /admin/log-level", authMW(http.HandlerFunc(internalapi.HandleGetLogLevel)))
	mux.Handle("POST /admin/log-level", authMW(http.HandlerFunc(internalapi.HandleSetLogLevel)))
	mux.Handle("GET /admin/trace-sampler", authMW(http.HandlerFunc(internalapi.HandleGetTraceSampler)))
	mux.Handle("POST /admin/trace-sampler", authMW(http.HandlerFunc(internalapi.HandleSetTraceSampler)))

	// Entity transition routes (with auth, outside generated API mux)
	mux.Handle("GET /entity/{entityId}/transitions", authMW(http.HandlerFunc(entityHandler.HandleGetTransitions)))
	mux.Handle("GET /platform-api/entity/fetch/transitions", authMW(http.HandlerFunc(entityHandler.HandleFetchTransitions)))

	// Generated API routes (with recovery + auth) — uses chi to avoid ServeMux
	// wildcard-conflict panics in overlapping /model/… paths.
	apiHandler := genapi.HandlerFromMux(server, internalapi.NewChiMux())
	if cfg.OTelEnabled {
		apiHandler = otelhttp.NewMiddleware("cyoda")(apiHandler)
	}
	mux.Handle("/", middleware.Recovery(healthFlag)(
		middleware.Auth(a.authService)(apiHandler),
	))

	// Context path — wrap all routes under configurable prefix
	contextPath := strings.TrimRight(cfg.ContextPath, "/")
	if contextPath != "" {
		outerMux := http.NewServeMux()
		outerMux.Handle(contextPath+"/", http.StripPrefix(contextPath, mux))
		// Discovery routes at root (no auth, no context path)
		internalapi.RegisterDiscoveryRoutes(outerMux, contextPath)
		// Help routes — unauthenticated, public content embedded in the binary
		internalapi.RegisterHelpRoutes(outerMux, help.DefaultTree, contextPath, cfg.Version)
		// Internal dispatch routes at root (AEAD-authenticated, not under context path)
		if cfg.Cluster.Enabled {
			dispatchHandler := clusterdispatch.NewDispatchHandler(localDispatcher, peerAuth)
			dispatchHandler.Register(outerMux)
		}
		a.handler = outerMux
	} else {
		// No context path — discovery routes on the main mux
		internalapi.RegisterDiscoveryRoutes(mux, "")
		// Help routes — unauthenticated, public content embedded in the binary
		internalapi.RegisterHelpRoutes(mux, help.DefaultTree, "", cfg.Version)
		// Internal dispatch routes (AEAD-authenticated)
		if cfg.Cluster.Enabled {
			dispatchHandler := clusterdispatch.NewDispatchHandler(localDispatcher, peerAuth)
			dispatchHandler.Register(mux)
		}
		a.handler = mux
	}

	// Cluster routing middleware — outermost layer, before auth and recovery.
	// The proxy forwards the original request including auth headers to the
	// target node, where auth is applied locally.
	if cfg.Cluster.Enabled {
		a.handler = proxy.HTTPRouting(a.tokenSigner, a.nodeRegistry, cfg.Cluster.NodeID, cfg.Cluster.ProxyTimeout)(a.handler)
	}

	// CORS middleware — outermost wrapper. Sits outside cluster-routing
	// so preflights short-circuit at the receiving node and never get
	// proxied. Sits outside outerMux so /help, discovery, and the API
	// surface are all covered by a single CORS policy. See spec
	// docs/superpowers/specs/2026-05-01-issue-196-cors-design.md.
	corsPolicy := middleware.NewCORSPolicy(cfg.CORS.Enabled, cfg.CORS.Wildcard, cfg.CORS.AllowedOrigins)
	a.handler = middleware.CORS(corsPolicy)(a.handler)

	// gRPC server — uses inner handler (without context path prefix)
	a.grpcServer = internalgrpc.NewServer(a.authService, a.memberRegistry, a.transactionManager, entityHandler, modelHandler, a.searchService, cfg.OTelEnabled)

	return a
}

func (a *App) Handler() http.Handler { return a.handler }

// ReadinessCheck returns nil when the instance is ready to serve external
// traffic. Called synchronously by the /readyz admin endpoint on every
// probe — keep it cheap. By the time New() returns, the plugin factory
// has successfully opened connections and applied migrations (per the
// existing startup sequence), so a non-nil storeFactory is a sufficient
// readiness signal until the SPI gains a dedicated Ping method.
func (a *App) ReadinessCheck() error {
	if a.storeFactory == nil {
		return fmt.Errorf("storage not initialized")
	}
	return nil
}

func (a *App) StoreFactory() spi.StoreFactory             { return a.storeFactory }
func (a *App) TransactionManager() spi.TransactionManager { return a.transactionManager }
func (a *App) AuthenticationService() contract.AuthenticationService {
	return a.authService
}
func (a *App) AuthorizationService() contract.AuthorizationService {
	return a.authzService
}
func (a *App) WorkflowEngine() *workflow.Engine             { return a.workflowEngine }
func (a *App) SearchService() *search.SearchService         { return a.searchService }
func (a *App) AuditService() contract.AuditService          { return a.auditService }
func (a *App) ClusterService() contract.ClusterService      { return a.clusterService }
func (a *App) GRPCServer() *internalgrpc.Server             { return a.grpcServer }
func (a *App) MemberRegistry() *internalgrpc.MemberRegistry { return a.memberRegistry }
func (a *App) TokenSigner() *token.Signer                   { return a.tokenSigner }
func (a *App) NodeRegistry() contract.NodeRegistry          { return a.nodeRegistry }
func (a *App) TxLifecycle() *lifecycle.Manager              { return a.txLifecycle }

// gRPCGracefulStopBudget is the upper bound on graceful drain at shutdown.
// Matched to the HTTP server's drain deadline in cmd/cyoda/main.go so a
// caller can predict total stop time as ~max(http, grpc) drain budgets.
const gRPCGracefulStopBudget = 10 * time.Second

// Close performs graceful shutdown of all backend resources.
//
// Close is the single teardown path for storeFactory and the gRPC server;
// Shutdown only releases background goroutines and cluster registration.
// Order: storage first, then gRPC. The gRPC server can block waiting on
// in-flight streams, so we want pools released before that blocks.
//
// gRPC is stopped via GracefulStop bounded by gRPCGracefulStopBudget; if
// the budget elapses without graceful completion (a stuck stream, a
// non-cooperative client) we fall back to a hard Stop and emit a slog.Warn
// so operators can see the budget was hit (#68 item 19).
func (a *App) Close() error {
	slog.Info("shutting down")
	var err error
	if a.storeFactory != nil {
		err = a.storeFactory.Close()
	}
	a.StopGRPC()
	return err
}

// StopGRPC drains the gRPC server with a deadline-bounded graceful-stop
// (gRPCGracefulStopBudget). The drain runs at most once across the
// lifetime of the App via sync.Once — runServers' watcher invokes this
// when rootCtx cancels, and Close() calls it again as a belt-and-braces
// teardown. Without the once, a stuck stream could burn up to 2× the
// budget across the runServers + Close layers.
func (a *App) StopGRPC() {
	a.grpcStopOnce.Do(func() {
		if a.grpcServer == nil {
			return
		}
		done := make(chan struct{})
		go func() {
			a.grpcServer.GracefulStop()
			close(done)
		}()
		select {
		case <-done:
			// Graceful drain completed within budget.
		case <-time.After(gRPCGracefulStopBudget):
			slog.Warn("gRPC graceful stop deadline exceeded; forcing",
				"phase", "shutdown",
				"budget", gRPCGracefulStopBudget.String())
			a.grpcServer.GRPCServer().Stop()
		}
	})
}

// Shutdown performs graceful cleanup of background goroutines and cluster
// resources. The storeFactory is intentionally NOT closed here — Close()
// is the single teardown path for that, so callers invoking Shutdown()
// followed by Close() (the runServers sequence) close the factory
// exactly once.
func (a *App) Shutdown() {
	if a.stopSearchReaper != nil {
		close(a.stopSearchReaper)
	}
	if a.stopReaper != nil {
		close(a.stopReaper)
	}
	if a.nodeRegistry != nil && a.config.Cluster.Enabled {
		if err := a.nodeRegistry.Deregister(context.Background(), a.config.Cluster.NodeID); err != nil {
			slog.Warn("failed to deregister from cluster", "pkg", "cluster", "err", err)
		}
	}
}

// validateBootstrapConfig enforces bootstrap-secret policy:
//   - jwt mode: CYODA_BOOTSTRAP_CLIENT_SECRET is required (fatal startup error
//     if unset); the Helm chart always provides it via a Kubernetes Secret, so
//     auto-generation is never needed in a deployment context.
//   - mock mode: the secret is irrelevant; zero it to prevent accidental use.
//
// Returns a new Config with the policy applied, or an error the caller must
// surface as a fatal startup failure.
func validateBootstrapConfig(cfg *Config) (*Config, error) {
	out := *cfg
	if out.IAM.Mode != "jwt" {
		// Mock (or any non-jwt) mode: bootstrap is irrelevant. Zero the secret defensively so
		// downstream code can't accidentally use it.
		out.Bootstrap.ClientSecret = ""
		return &out, nil
	}
	idSet := out.Bootstrap.ClientID != ""
	secretSet := out.Bootstrap.ClientSecret != ""
	switch {
	case !idSet && !secretSet:
		// No bootstrap M2M client configured. System starts without one;
		// operator authenticates via JWKS / external signing keys.
		return &out, nil
	case idSet && secretSet:
		// Bootstrap M2M client configured. Creation happens in New().
		return &out, nil
	case idSet && !secretSet:
		return nil, fmt.Errorf(
			"CYODA_BOOTSTRAP_CLIENT_SECRET is required when CYODA_BOOTSTRAP_CLIENT_ID is set in jwt mode")
	default: // !idSet && secretSet
		return nil, fmt.Errorf(
			"CYODA_BOOTSTRAP_CLIENT_ID is required when CYODA_BOOTSTRAP_CLIENT_SECRET is set in jwt mode (secret would otherwise be unused)")
	}
}

// validateMetricsAuth enforces the coupled predicate on metrics-endpoint
// authentication. CYODA_METRICS_REQUIRE_AUTH=true together with an empty
// CYODA_METRICS_BEARER is an operator misconfiguration — they asked for
// auth but did not provide a credential, and silently leaving /metrics
// open in that case is strictly worse than refusing to start (they would
// ship a shared-cluster deployment thinking scrape was authenticated).
// In all other cases the token itself drives the behaviour: non-empty
// token enables auth on /metrics, empty token leaves it unauthenticated
// (the desktop/docker default).
func validateMetricsAuth(cfg *Config) error {
	if cfg.Admin.MetricsRequireAuth && cfg.Admin.MetricsBearerToken == "" {
		return fmt.Errorf(
			"CYODA_METRICS_BEARER (or _FILE) is required when CYODA_METRICS_REQUIRE_AUTH=true")
	}
	return nil
}

// validateClusterConfig fails fast on missing/invalid cluster settings.
// Called before any cluster infrastructure is constructed so the failure
// surfaces at startup instead of during traffic.
func validateClusterConfig(c cluster.Config) {
	if c.NodeID == "" {
		slog.Error("CYODA_NODE_ID is required when cluster mode is enabled", "pkg", "cluster")
		os.Exit(1)
	}
	if len(c.HMACSecret) == 0 {
		slog.Error("CYODA_HMAC_SECRET is required when cluster mode is enabled", "pkg", "cluster")
		os.Exit(1)
	}
	if !strings.HasPrefix(c.NodeAddr, "http://") && !strings.HasPrefix(c.NodeAddr, "https://") {
		slog.Error("CYODA_NODE_ADDR must include scheme (http:// or https://)", "pkg", "cluster", "addr", c.NodeAddr)
		os.Exit(1)
	}
}

// makeSchemaApply returns the schema-apply replay function the plugin
// factories use to fold extension-log deltas on read. Defined here so
// the plugin packages don't depend on internal/domain/model/schema.
func makeSchemaApply() func(base []byte, delta spi.SchemaDelta) ([]byte, error) {
	return func(base []byte, delta spi.SchemaDelta) ([]byte, error) {
		node, err := schema.Unmarshal(base)
		if err != nil {
			return nil, fmt.Errorf("apply: unmarshal base: %w", err)
		}
		extended, err := schema.Apply(node, delta)
		if err != nil {
			return nil, err
		}
		return schema.Marshal(extended)
	}
}

// mustNewGossip parses the gossip address, creates the memberlist-backed
// registry, and exits on any failure. Returns the registry so the caller
// can both (a) pass it to plugin.NewFactory as a broadcaster and (b) use
// it as the app's node registry after Register.
func mustNewGossip(c cluster.Config) *registry.Gossip {
	gossipHost, gossipPortStr, err := net.SplitHostPort(c.GossipAddr)
	if err != nil {
		slog.Error("invalid CYODA_GOSSIP_ADDR", "pkg", "cluster", "addr", c.GossipAddr, "err", err)
		os.Exit(1)
	}
	gossipPort, err := strconv.Atoi(gossipPortStr)
	if err != nil {
		slog.Error("invalid gossip port", "pkg", "cluster", "port", gossipPortStr, "err", err)
		os.Exit(1)
	}
	g, err := registry.NewGossip(registry.GossipConfig{
		NodeID:          c.NodeID,
		NodeAddr:        c.NodeAddr,
		BindAddr:        gossipHost,
		BindPort:        gossipPort,
		Seeds:           c.SeedNodes,
		StabilityWindow: c.StabilityWindow,
		SecretKey:       c.HMACSecret,
	})
	if err != nil {
		slog.Error("failed to create gossip registry", "pkg", "cluster", "err", err)
		os.Exit(1)
	}
	return g
}
