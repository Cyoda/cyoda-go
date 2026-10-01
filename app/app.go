package app

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime/debug"
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
	"github.com/cyoda-platform/cyoda-go/internal/callout"
	"github.com/cyoda-platform/cyoda-go/internal/cluster"
	clusterdispatch "github.com/cyoda-platform/cyoda-go/internal/cluster/dispatch"
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
	"github.com/cyoda-platform/cyoda-go/internal/domain/scheduledtask"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/internal/domain/txjoin"
	"github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
	"github.com/cyoda-platform/cyoda-go/internal/fence"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
	"github.com/cyoda-platform/cyoda-go/internal/httpmw"
	mockiam "github.com/cyoda-platform/cyoda-go/internal/iam/mock"
	"github.com/cyoda-platform/cyoda-go/internal/observability"
	"github.com/cyoda-platform/cyoda-go/internal/scheduler"
	"github.com/cyoda-platform/cyoda-go/internal/skeleton"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
)

type App struct {
	config             Config
	storeFactory       spi.StoreFactory
	transactionManager spi.TransactionManager
	authService        contract.AuthenticationService
	authzService       contract.AuthorizationService
	authSvc            *auth.AuthService // non-nil only in JWT IAM mode; nil in mock IAM mode
	workflowEngine     *workflow.Engine
	txGate             *txgate.Registry // per-tx application gate serialising joined callbacks and the owner's commit
	fence              *fence.Fence     // one arbiter per process; both callback doors and every callout judge a callback by it
	joiner             *txjoin.Joiner   // the join layer both callback doors run a request carrying a pass through
	searchService      *search.SearchService
	auditService       contract.AuditService
	clusterService     contract.ClusterService
	memberRegistry     *internalgrpc.MemberRegistry
	grpcServer         *internalgrpc.Server
	handler            http.Handler
	tokenSigner        *token.Signer
	selfNodeID         string
	nodeRegistry       contract.NodeRegistry
	scheduler          *scheduler.Service
	// stopSearchReapers stops the async-search sweeps and waits for them
	// (startSearchReapers). stopSearchReapersOnce makes it idempotent — both
	// Shutdown and Close call it.
	stopSearchReapers     func()
	stopSearchReapersOnce sync.Once
	// searchPool is the bounded worker pool async-search submissions run
	// on, sized from cfg.SearchAsync. Shutdown drains it (bounded by
	// searchDrainBudget) before aborting whatever async jobs are still
	// registered on this node.
	searchPool   *search.WorkerPool
	grpcStopOnce sync.Once
	// healthFlag starts true and is latched false by the first recovered
	// panic at any of the four sites that run engine or store work: the HTTP
	// recovery middleware, the gRPC recovery interceptors, the async-search
	// goroutine and the scheduler's goroutines (its claim loop, heartbeat,
	// watchdog and runs). Notification-callback recoveries (member-registry
	// onChange, auth key-store reconcile broadcast) deliberately do not.
	// Nothing resets it: a node that has panicked has state nothing has
	// verified. Read by RegisterHealthRoutes (GET /health) and by
	// ReadinessCheck (/readyz).
	healthFlag *atomic.Bool
}

func New(cfg Config) *App {
	// Invariants this function's own wiring depends on (worker-pool sizing,
	// heartbeat/stale-after cadence, IAM mode admitting only "mock" or
	// "jwt"). Checked here rather than only in the binary so an in-process
	// embedder gets them too — see Config.Validate.
	if err := cfg.Validate(); err != nil {
		slog.Error("startup failure", "phase", "config-validation", "error", err.Error())
		os.Exit(1)
	}

	// Metrics-auth coupled predicate: if CYODA_METRICS_REQUIRE_AUTH=true
	// the bearer must be set. Refuse to start rather than silently drop
	// auth for operators who thought they'd enabled it.
	if err := validateMetricsAuth(&cfg); err != nil {
		slog.Error("invalid metrics auth configuration", "pkg", "app", "err", err)
		os.Exit(1)
	}

	a := &App{config: cfg}

	// Created before anything that can latch it: the async-search goroutine
	// is wired below, well ahead of the HTTP mux and the gRPC server.
	a.healthFlag = &atomic.Bool{}
	a.healthFlag.Store(true)

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
	// Transaction routing token signer. Always present: in cluster mode it uses
	// the configured HMAC secret so tokens verify across nodes; in single-node
	// mode it uses a per-process ephemeral secret (tokens only round-trip
	// through this process, and a tx never outlives the process).
	a.selfNodeID = "local"
	secret := cfg.Cluster.HMACSecret
	if cfg.Cluster.Enabled {
		validateClusterConfig(cfg.Cluster)
		a.selfNodeID = cfg.Cluster.NodeID
		gossipReg = mustNewGossip(cfg.Cluster)
	} else {
		secret = make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			slog.Error("failed to generate ephemeral token secret", "pkg", "cluster", "err", err)
			os.Exit(1)
		}
	}
	var signerErr error
	a.tokenSigner, signerErr = token.NewSigner(secret)
	if signerErr != nil {
		slog.Error("failed to create token signer", "pkg", "cluster", "err", signerErr)
		os.Exit(1)
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
	// the extension log; SQLite/Memory use it to apply in-place.
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
		a.transactionManager = observability.NewTracingTransactionManager(a.transactionManager, observability.Meter())
	}

	// Auth service: JWT or mock mode
	var authSvc *auth.AuthService

	// The platform-wide admin endpoints accept only a platform operator. Mock
	// mode has one fixed principal, so there the operator check is the admin
	// check. Config.Validate (run at the top of New, and unconditionally on
	// every app.New caller) admits only "mock" and "jwt" — no other value
	// reaches the branch below.
	operatorGuard := auth.OperatorGuard{}
	if cfg.IAM.Mode == "mock" {
		operatorGuard = auth.MockOperatorGuard()
	}

	if cfg.IAM.Mode == "jwt" {
		if cfg.IAM.JWTSigningKey == "" {
			slog.Error("startup failure",
				"phase", "jwt-signing-key",
				"error", "CYODA_JWT_SIGNING_KEY is required when IAM mode is jwt")
			os.Exit(1)
		}
		// The SYSTEM-tenant KV store holds the cluster's auth state: signing
		// key pairs and trusted keys.
		systemCtx := spi.WithUserContext(context.Background(), &spi.UserContext{
			UserID:   "system",
			UserName: "System",
			Kind:     spi.PrincipalSystem,
			Tenant:   spi.Tenant{ID: spi.SystemTenantID, Name: "System"},
		})
		kvStore, err := a.storeFactory.KeyValueStore(systemCtx)
		if err != nil {
			slog.Error("startup failure",
				"phase", "kv-store-system",
				"error", err.Error())
			os.Exit(1)
		}
		// D7 invariant — broadcaster MUST be non-nil when cluster mode is
		// enabled. Checked here (before the auth service is constructed) so
		// the key stores are never constructed with a missing broadcaster in
		// cluster mode.
		if cfg.Cluster.Enabled && gossipReg == nil {
			slog.Error("startup failure", "phase", "auth-broadcaster-missing")
			os.Exit(1)
		}

		trustedMetrics, err := auth.NewOTelReconcileMetrics(observability.Meter(), "auth.trustedkeys")
		if err != nil {
			slog.Error("startup failure", "phase", "auth-reconcile-metrics-init", "error", err.Error())
			os.Exit(1)
		}
		signingMetrics, err := auth.NewOTelReconcileMetrics(observability.Meter(), "auth.signingkeys")
		if err != nil {
			slog.Error("startup failure", "phase", "auth-reconcile-metrics-init", "error", err.Error())
			os.Exit(1)
		}
		var authBroadcaster spi.ClusterBroadcaster
		if gossipReg != nil {
			// Typed-nil guard: only assign when non-nil (same rationale as
			// cacheBroadcaster above).
			authBroadcaster = gossipReg
		}
		authSvc, err = auth.NewAuthService(systemCtx, auth.AuthConfig{
			SigningKeyPEM:     cfg.IAM.JWTSigningKey,
			Issuer:            cfg.IAM.JWTIssuer,
			Audience:          cfg.IAM.JWTAudience,
			ExpirySeconds:     cfg.IAM.JWTExpiry,
			IAMFeatures:       cfg.IAM.AuthIAMFeatures(),
			KV:                kvStore,
			Broadcaster:       authBroadcaster,
			ReconcileInterval: cfg.IAM.AuthCacheReconcileInterval,
			TrustedKeyMetrics: trustedMetrics,
			SigningKeyMetrics: signingMetrics,
		})
		if err != nil {
			slog.Error("startup failure",
				"phase", "auth-service",
				"error", err.Error())
			os.Exit(1)
		}
		// Periodic KV re-read of both key stores; systemCtx is
		// process-lifetime, so the loops run until exit.
		authSvc.Start(systemCtx)
		// The built-in IAM holds a copy of the cluster's signing keys on every
		// node, so the validator reads public keys directly from that copy. No
		// loopback JWKS fetch, no HTTP client, no attack surface on that path.
		jwksValidator := auth.NewValidatorFromSource(auth.NewLocalKeySource(authSvc.KeyStore()), authSvc.Issuer())
		if cfg.IAM.JWTAudience != "" {
			jwksValidator.SetExpectedAudience(cfg.IAM.JWTAudience)
		}
		a.authService = auth.NewDelegatingAuthenticator(jwksValidator)
		a.authSvc = authSvc
	} else {
		defaultUser := &spi.UserContext{
			UserID:   cfg.IAM.MockUserID,
			UserName: cfg.IAM.MockUserName,
			Kind:     spi.PrincipalKind(cfg.IAM.MockKind),
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
	// One lock registry and one fence per process: both callback doors, the
	// entity service and every callout judge a callback by the same state.
	a.txGate = txgate.New()
	a.fence = fence.New(a.txGate)
	localDispatcher := internalgrpc.NewProcessorDispatcher(a.memberRegistry, internalgrpc.NewRoundRobinSelector(a.memberRegistry), a.tokenSigner, cfg.Callout.ResponseTimeout, cfg.Callout.ResponseTimeoutMax, cfg.Callout.PassAllowance)
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
	// single-node and multi-node alike: subscribing to the descriptor
	// cache rather than to the broadcaster directly is what makes
	// single-node deployments, where the broadcaster is nil, receive
	// invalidations at all. Per-(tenant, ref) bucketed otter caches
	// isolate cross-tenant eviction.
	pathValidationCache := search.NewPathValidationCache()
	cachingStoreFactory.SubscribeLocal(pathValidationCache.InvalidateRef)
	// Bounded async-search worker pool, sized from config (validated by
	// cfg.Validate at the top of New).
	a.searchPool = search.NewWorkerPool(cfg.SearchAsync.Workers, cfg.SearchAsync.QueueLen)
	a.searchService = search.
		NewSearchService(a.storeFactory, common.NewDefaultUUIDGenerator(), searchStore).
		WithPathValidationCache(pathValidationCache).
		WithMaxSortKeys(a.config.SearchMaxSortKeys).
		WithHealthFlag(a.healthFlag).
		WithAsyncPool(a.searchPool).
		WithAsyncMaxPerTenant(cfg.SearchAsync.MaxPerTenant).
		WithHeartbeat(cfg.SearchJobHeartbeatInterval)

	a.stopSearchReapers = startSearchReapers(&cfg, a.searchService, searchStore, a.healthFlag)

	a.auditService = skeleton.NewAuditService()
	a.clusterService = internalgrpc.NewClusterService(a.memberRegistry)

	// Cluster components
	if cfg.Cluster.Enabled {
		// gossipReg was created above (before plugin.NewFactory) so the plugin
		// could subscribe to broadcast topics. Join the cluster now; subscribers
		// are already registered, so no messages are dropped.
		// Use startupCtx so the gossip join honors the configured
		// gossip-registration deadline (CYODA_STARTUP_TIMEOUT) instead of a
		// hard-coded 2-minute one.
		if err := gossipReg.Register(startupCtx, cfg.Cluster.NodeID, cfg.Cluster.NodeAddr); err != nil {
			slog.Error("failed to register with gossip cluster", "pkg", "cluster", "err", err)
			os.Exit(1)
		}
		a.nodeRegistry = gossipReg

		slog.Info("cluster mode enabled", "pkg", "cluster", "nodeID", cfg.Cluster.NodeID, "gossipAddr", cfg.Cluster.GossipAddr)
	} else {
		a.nodeRegistry = registry.NewLocal("local", fmt.Sprintf("localhost:%d", cfg.HTTPPort))
	}

	// Wire external processing dispatcher
	var extProc contract.ExternalProcessingService
	// Peer-auth for inter-node dispatch. AES-256-GCM + HKDF-derived key over
	// the shared cluster secret. 30-second timestamp skew window. This node's
	// id is what every inbound envelope must have been sealed for. Constructed
	// once and shared by forwarder and handler so rotation is atomic.
	var peerAuth clusterdispatch.PeerAuth
	if cfg.Cluster.Enabled {
		auth, err := clusterdispatch.NewAEADPeerAuth(cfg.Cluster.HMACSecret, a.selfNodeID, 30*time.Second)
		if err != nil {
			slog.Error("failed to construct dispatch peer auth", "pkg", "cluster", "err", err)
			os.Exit(1)
		}
		peerAuth = auth
	}
	if cfg.ExternalProcessing != nil {
		extProc = cfg.ExternalProcessing
	} else {
		// The owner's loop, in both modes. On a single node it has no peer
		// router: peers stays a nil interface (a nil *PeerRouter would not be).
		var peers callout.PeerRouter
		if cfg.Cluster.Enabled {
			forwarder := clusterdispatch.NewHTTPForwarder(peerAuth, cfg.Cluster.DispatchConnectTimeout)
			if cfg.Cluster.DispatchAllowLoopback {
				// Test-only: multi-node E2E fixtures run every node on 127.0.0.1.
				// Never set in production (SSRF guard stays active by default).
				forwarder = forwarder.AllowLoopbackForTesting()
			}
			peerRouter, err := clusterdispatch.NewPeerRouter(a.nodeRegistry, a.selfNodeID,
				clusterdispatch.NewRandomSelector(), forwarder, observability.Meter())
			if err != nil {
				slog.Error("failed to construct the peer router", "pkg", "cluster", "err", err)
				os.Exit(1)
			}
			peers = peerRouter
		}
		extProc = callout.New(localDispatcher, a.memberRegistry, peers, a.fence, common.NewDefaultUUIDGenerator(), callout.Config{
			SelfNodeID:        a.selfNodeID,
			FixedNumRetries:   cfg.Callout.FixedNumRetries,
			Patience:          cfg.Cluster.DispatchWaitTimeout,
			HandoverAllowance: cfg.Callout.HandoverAllowance,
		})
	}
	if cfg.OTelEnabled {
		extProc = observability.NewTracingExternalProcessingService(extProc, observability.Meter())
	}
	// schedClock is the pnode clock, shared by the engine's arm and fire math
	// and the scheduler, so both agree on "now".
	schedClock := scheduler.NewRealClock()
	a.workflowEngine = workflow.NewEngine(a.storeFactory, common.NewDefaultUUIDGenerator(), a.transactionManager,
		workflow.WithExternalProcessing(extProc),
		workflow.WithMaxStateVisits(cfg.MaxStateVisits),
		workflow.WithScheduledClock(schedClock.Now))

	// Wire MemberRegistry onChange to gossip tag updates
	if cfg.Cluster.Enabled {
		a.memberRegistry.SetOnChange(func(tags map[string][]string) error {
			gossipReg, ok := a.nodeRegistry.(*registry.Gossip)
			if !ok {
				return nil
			}
			if err := gossipReg.UpdateTags(tags); err != nil {
				return fmt.Errorf("update gossip tags: %w", err)
			}
			return nil
		})
	}

	// The scheduler: this node claims due scheduled tasks and runs them
	// itself. A disabled scheduler starts nothing; Shutdown drains it either way.
	a.scheduler = scheduler.New(scheduler.Config(cfg.Scheduler), scheduler.Deps{
		Store:              a.storeFactory,
		TxManager:          a.transactionManager,
		Firer:              a.workflowEngine,
		Clock:              schedClock,
		HealthFlag:         a.healthFlag,
		Meter:              observability.Meter(),
		CalloutDeadlineMax: schedulerCalloutDeadlineMax(cfg),
	})
	if err := a.scheduler.Start(context.Background()); err != nil {
		slog.Error("startup failure", "phase", "scheduler-start", "error", err.Error())
		os.Exit(1)
	}

	// The join layer: every request that carries a pass runs through it, on
	// either door — joined, checked under the transaction's lock, and holding
	// that lock for the length of the handler.
	joiner, err := txjoin.NewJoiner(a.tokenSigner, a.transactionManager, a.fence, a.txGate,
		cfg.Callout.JoinedResponseMaxBytes, cfg.Callout.JoinedMaxWaiters, observability.Meter())
	if err != nil {
		slog.Error("startup failure", "phase", "joiner-metrics-init", "error", err.Error())
		os.Exit(1)
	}
	a.joiner = joiner

	// Domain handlers
	entityHandler := entity.New(a.storeFactory, a.transactionManager, common.NewDefaultUUIDGenerator(), a.workflowEngine, a.txGate)
	modelHandler := model.New(a.storeFactory)
	server := internalapi.NewServer()
	server.Entity = entityHandler
	server.Model = modelHandler
	server.Workflow = workflow.New(a.storeFactory, a.workflowEngine, a.config.Callout.ResponseTimeoutMax)
	server.Search = search.NewHandler(a.searchService).WithMaxSortKeys(a.config.SearchMaxSortKeys)
	server.Audit = audit.New(a.storeFactory)
	server.ScheduledTasks = scheduledtask.NewHandler(a.storeFactory)
	server.Messaging = messaging.New(a.storeFactory, common.NewDefaultUUIDGenerator())
	var accountKeyStore auth.KeyStore
	var accountTrustedKeyStore auth.TrustedKeyStore
	var accountM2MStore auth.M2MClientStore
	if authSvc != nil {
		accountKeyStore = authSvc.KeyStore()
		accountTrustedKeyStore = authSvc.TrustedKeyStore()
		accountM2MStore = authSvc.M2MClientStore()
	}
	accountHandler := account.New(a.authService, a.authzService, accountKeyStore, accountTrustedKeyStore, accountM2MStore, cfg.IAM.AuthIAMFeatures(), operatorGuard)
	server.Account = accountHandler

	// Build HTTP handler
	mux := http.NewServeMux()

	// Infrastructure routes (no auth, receives health flag)
	internalapi.RegisterHealthRoutes(mux, a.healthFlag)

	// Auth service route registration is split into two strict groups so
	// nothing administrative leaks into the public surface:
	//
	//   PUBLIC (no auth): /.well-known/jwks.json, POST /oauth/token.
	//     These are the JWKS discovery + token-exchange endpoints and must
	//     be reachable by unauthenticated callers by protocol.
	//
	//   ADMIN (authMW + ROLE_ADMIN): served via the chi router (account
	//     handler). The /account/m2m* legacy mux entries were retired
	//     when /clients chi adapters landed; see m2m_adapter.go.

	// Public auth endpoints (no auth middleware).
	if authSvc != nil {
		mux.Handle("/.well-known/", authSvc.Handler())
		mux.Handle("POST /oauth/token", authSvc.Handler())
	}

	// Admin routes (auth middleware required).
	authMW := middleware.Auth(a.authService)

	adminHandlers := internalapi.NewAdminHandlers(operatorGuard)
	mux.Handle("GET /admin/log-level", authMW(http.HandlerFunc(adminHandlers.GetLogLevel)))
	mux.Handle("POST /admin/log-level", authMW(http.HandlerFunc(adminHandlers.SetLogLevel)))
	mux.Handle("GET /admin/trace-sampler", authMW(http.HandlerFunc(adminHandlers.GetTraceSampler)))
	mux.Handle("POST /admin/trace-sampler", authMW(http.HandlerFunc(adminHandlers.SetTraceSampler)))

	// Entity transition routes (with auth, outside generated API mux).
	// TxJoin is nested inside authMW so UserContext is available for tenant checks.
	txJoinMW := httpmw.TxJoin(a.joiner)
	mux.Handle("GET /entity/{entityId}/transitions", authMW(txJoinMW(http.HandlerFunc(entityHandler.HandleGetTransitions))))
	mux.Handle("GET /platform-api/entity/fetch/transitions", authMW(txJoinMW(http.HandlerFunc(entityHandler.HandleFetchTransitions))))

	// Grouped-stats route (POST /entity/stats/{name}/{ver}/query). Wired here (not via openapi.yaml) so
	// the closure can capture a.storeFactory directly — the handler needs
	// the EntityStore and a validated ModelRef for the calling tenant.
	// The resolver returns ok=false when the model is not registered (after
	// one bounded cache refresh — closing the multi-node stale-cache race);
	// the handler maps that to 404 MODEL_NOT_FOUND. Genuine store errors
	// (non-ErrNotFound from Get or RefreshAndGet) surface as Internal(500)
	// and are propagated to the 500-with-ticket path.
	storeFactory := a.storeFactory
	groupedStatsResolver := func(r *http.Request, entityName, modelVersion string) (spi.EntityStore, spi.ModelRef, map[string]schema.FieldDescriptor, spi.ModelStore, bool, error) {
		ctx := r.Context()
		modelStore, err := storeFactory.ModelStore(ctx)
		if err != nil {
			return nil, spi.ModelRef{}, nil, nil, false, err
		}
		ref := spi.ModelRef{EntityName: entityName, ModelVersion: modelVersion}
		if appErr := common.EnsureModelRegistered(ctx, modelStore, ref); appErr != nil {
			if appErr.Status == http.StatusNotFound {
				// Not registered after one bounded refresh → handler emits 404 MODEL_NOT_FOUND.
				return nil, ref, nil, nil, false, nil
			}
			// Genuine store error → propagate to 500-with-ticket path.
			return nil, ref, nil, nil, false, appErr
		}
		entityStore, err := storeFactory.EntityStore(ctx)
		if err != nil {
			return nil, ref, nil, nil, false, err
		}
		// Load the model's declared field types so grouped-stats comparison is
		// type-directed (temporal data fields compare temporally), consistent
		// with the search path. A genuine store/schema-load error fails closed
		// (correctness-over-availability): the schema is a required input for
		// correct typing, so surface it to the 500-with-ticket path rather than
		// silently under-match with untyped leaves. The no-schema-registered
		// case is (nil, nil) — fields stays nil and data leaves degrade to
		// non-type-directed comparison.
		fields, err := search.LoadFieldsMap(ctx, modelStore, ref)
		if err != nil {
			return nil, ref, nil, nil, false, err
		}
		return entityStore, ref, fields, modelStore, true, nil
	}
	groupedStatsHandler := entity.NewGroupedStatsHandler(groupedStatsResolver, cfg.StatsGroupMax)
	mux.Handle("POST /entity/stats/{entityName}/{modelVersion}/query", authMW(txJoinMW(groupedStatsHandler)))

	// Generated API routes (with auth) — uses chi to avoid ServeMux
	// wildcard-conflict panics in overlapping /model/… paths. Recovery is
	// applied once, below, to the fully assembled handler.
	apiHandler := genapi.HandlerWithOptions(server, genapi.StdHTTPServerOptions{
		BaseRouter:       internalapi.NewChiMux(),
		ErrorHandlerFunc: internalapi.BindingErrorHandler,
	})
	if cfg.OTelEnabled {
		apiHandler = otelhttp.NewMiddleware("cyoda")(apiHandler)
	}
	mux.Handle("/", middleware.Auth(a.authService)(txJoinMW(apiHandler)))

	// Context path — wrap all routes under configurable prefix
	contextPath := strings.TrimRight(cfg.ContextPath, "/")
	if contextPath != "" {
		outerMux := http.NewServeMux()
		outerMux.Handle(contextPath+"/", http.StripPrefix(contextPath, mux))
		// Discovery routes at root (no auth, no context path)
		internalapi.RegisterDiscoveryRoutes(outerMux, contextPath)
		// Help routes — unauthenticated, public content embedded in the binary
		// (OSS base merged with any plugin-registered overlays).
		internalapi.RegisterHelpRoutes(outerMux, help.BuildTree(), contextPath, cfg.Version)
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
		// (OSS base merged with any plugin-registered overlays).
		internalapi.RegisterHelpRoutes(mux, help.BuildTree(), "", cfg.Version)
		// Internal dispatch routes (AEAD-authenticated)
		if cfg.Cluster.Enabled {
			dispatchHandler := clusterdispatch.NewDispatchHandler(localDispatcher, peerAuth)
			dispatchHandler.Register(mux)
		}
		a.handler = mux
	}

	// Cluster routing sits directly over the mux: a request carrying a
	// transaction token for another node is forwarded before auth runs here
	// (auth is applied on the owning node).
	if cfg.Cluster.Enabled {
		a.handler = proxy.HTTPRouting(a.tokenSigner, a.nodeRegistry, cfg.Cluster.NodeID, cfg.Cluster.ProxyTimeout, cfg.Cluster.DispatchAllowLoopback, contextPath)(a.handler)
	}

	// CORS sits outside cluster routing so preflights short-circuit at the
	// receiving node and never get proxied, and outside outerMux so /help,
	// discovery, and the API surface share one policy. See
	// docs/superpowers/specs/2026-05-01-issue-196-cors-design.md.
	corsPolicy := middleware.NewCORSPolicy(cfg.CORS.Enabled, cfg.CORS.Wildcard, cfg.CORS.AllowedOrigins)
	a.handler = middleware.CORS(corsPolicy)(a.handler)

	// Recovery is the outermost layer, so nothing — CORS, cluster routing,
	// or any route added later — sits outside panic containment. CORS writes
	// its headers before calling the next handler, so a recovered 500 keeps
	// them. Recovery re-raises http.ErrAbortHandler, which is how the reverse
	// proxy reports a client hang-up, so proxied disconnects stay silent.
	a.handler = middleware.Recovery(a.healthFlag)(a.handler)

	// gRPC server — uses inner handler (without context path prefix)
	a.grpcServer = internalgrpc.NewServer(a.authService, a.memberRegistry, a.transactionManager, entityHandler, modelHandler, a.searchService, a.tokenSigner, a.joiner, a.nodeRegistry, a.selfNodeID, cfg.OTelEnabled, cfg.GRPC.Port, cfg.Cluster.DispatchAllowLoopback, a.healthFlag, internalgrpc.KeepAliveConfig{Interval: time.Duration(cfg.GRPC.KeepAliveInterval) * time.Second, Timeout: time.Duration(cfg.GRPC.KeepAliveTimeout) * time.Second})

	return a
}

// latchOnPanic recovers a panic beneath a reaper sweep and latches healthFlag
// false, exactly as the async-search executor's own recovery does: a reaper
// goroutine has no HTTP handler above it, so an unrecovered panic takes the
// process down, and a node that has panicked has state nothing has verified.
// Recovering also keeps the ticker alive, so one bad tick does not silently
// end reaping for the rest of the process's life. Deferred at the top of each
// tick; site names the sweep in the log. No plugin currently returns the
// shapes that would panic (a nil element from ClaimStale, say), so this is
// hardening rather than a fix for a live defect.
func latchOnPanic(healthFlag *atomic.Bool, site string) {
	if rec := recover(); rec != nil {
		slog.Error("panic recovered in "+site, "pkg", "search",
			"err", fmt.Errorf("panic: %v", rec), "stack", string(debug.Stack()))
		if healthFlag != nil {
			healthFlag.Store(false)
		}
	}
}

// startSearchReapers starts the async-search sweeps and returns the func that
// stops them and waits for them to exit. Each sweep has a goroutine of its
// own, so neither waits on the other:
//   - the snapshot-TTL reap, on SearchReapInterval. Its DELETE shares the
//     store's main pool with entity transactions and may wait on it.
//   - the stale-job claim, on the finer SearchJobHeartbeatInterval, plus one
//     sweep at startup. It never waits on the main pool; the writes it owes
//     jobs it will not run go to the service's second pass
//     (search.SearchService.ReclaimStaleJobs).
//
// Both run under one context, which stop cancels: a statement still waiting
// for a connection gives up, and the second pass sends nothing more. stop then
// waits for both goroutines and for the second pass.
func startSearchReapers(cfg *Config, svc *search.SearchService, store spi.AsyncSearchStore, healthFlag *atomic.Bool) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() {
		ticker := time.NewTicker(cfg.SearchReapInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				reapExpiredSnapshotsTick(ctx, store, cfg.SearchSnapshotTTL, healthFlag)
			case <-ctx.Done():
				return
			}
		}
	})
	wg.Go(func() {
		// Startup sweep: a restarted node reclaims its own released jobs and
		// any already-stale jobs the moment it can execute, not after the
		// first interval.
		reclaimStaleTick(ctx, svc, cfg.SearchJobStaleAfter, cfg.SearchJobMaxAttempts, healthFlag)
		ticker := time.NewTicker(cfg.SearchJobHeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				reclaimStaleTick(ctx, svc, cfg.SearchJobStaleAfter, cfg.SearchJobMaxAttempts, healthFlag)
			case <-ctx.Done():
				return
			}
		}
	})
	return func() {
		cancel()
		wg.Wait()
		svc.WaitReclaimSecondPass()
	}
}

// reapExpiredSnapshotsTick deletes terminal jobs past the snapshot TTL. Runs
// on SearchReapInterval. Panic-latches health like the other engine-work sites.
func reapExpiredSnapshotsTick(ctx context.Context, store spi.AsyncSearchStore, snapshotTTL time.Duration, healthFlag *atomic.Bool) {
	defer latchOnPanic(healthFlag, "search snapshot reaper")
	reaped, err := store.ReapExpired(ctx, snapshotTTL)
	if err != nil {
		if ctx.Err() != nil {
			return // the sweeps are stopping
		}
		slog.Error("search snapshot reaper error", "pkg", "search", "err", err)
	} else if reaped > 0 {
		slog.Info("reaped expired search snapshots", "pkg", "search", "count", reaped)
	}
}

// reclaimStaleTick claims stale/released RUNNING jobs and re-executes them on
// this node; the second pass fails those past the attempt cap. Runs on the
// heartbeat interval — a finer cadence than the snapshot reap — plus once at
// startup.
func reclaimStaleTick(ctx context.Context, svc *search.SearchService, staleAfter time.Duration, maxAttempts int, healthFlag *atomic.Bool) {
	defer latchOnPanic(healthFlag, "search stale-job reaper")
	reenqueued, err := svc.ReclaimStaleJobs(ctx, staleAfter, maxAttempts)
	if err != nil {
		if ctx.Err() != nil {
			return // the sweeps are stopping
		}
		slog.Error("stale search job reclaim error", "pkg", "search", "err", err)
		return
	}
	if reenqueued > 0 {
		slog.Info("re-enqueued stale async search jobs", "pkg", "search", "count", reenqueued)
	}
}

func (a *App) Handler() http.Handler { return a.handler }

// ReadinessCheck returns nil when the instance is ready to serve external
// traffic. Called synchronously by the /readyz admin endpoint on every
// probe — keep it cheap; both conditions below are a pointer test and an
// atomic load, and neither performs I/O.
//
// Two conditions fail it independently, and the returned reasons differ so
// the admin handler's server-side log tells an operator which fired:
//
//   - Storage is not initialized. A defensive guard rather than a live
//     window: cmd/cyoda builds the admin listener from an App that New()
//     has already returned, so a probe never observes it. It costs nothing
//     and keeps the check honest for any other caller.
//   - A panic was recovered on some door. The node's state is then
//     unverified, so it must stop receiving traffic — fail closed. Nothing
//     resets the flag.
//
// What failing readiness achieves is bounded: Kubernetes drops the pod from
// the client-facing Service, so new client connections stop. Peers resolve
// each other through the gossip registry rather than the Service, so
// forwarded work keeps arriving, and /livez is deliberately unaffected so a
// deterministic panic does not become a restart loop. Replacing a drained
// node is an operator action.
func (a *App) ReadinessCheck() error {
	if a.storeFactory == nil {
		return fmt.Errorf("storage not initialized")
	}
	// nil only for an App not built by New(); New() always sets the flag.
	if a.healthFlag != nil && !a.healthFlag.Load() {
		return fmt.Errorf("node unhealthy: a panic was recovered and this node's state is unverified")
	}
	return nil
}

// DrainScheduler runs the scheduler's shutdown steps 1-5. The binary calls it
// on a signal before the servers drain, so runs still in progress keep their
// compute-node streams and callback routes. Shutdown and Close drain it again;
// every call after the first does nothing.
func (a *App) DrainScheduler(ctx context.Context) {
	if a.scheduler != nil {
		a.scheduler.Drain(ctx)
	}
}

// schedulerCalloutDeadlineMax is the longest one callout can take: every try
// at the largest answer limit, the patience and the hand-over allowance.
func schedulerCalloutDeadlineMax(c Config) time.Duration {
	return time.Duration(1+c.Callout.FixedNumRetries)*c.Callout.ResponseTimeoutMax +
		c.Cluster.DispatchWaitTimeout + c.Callout.HandoverAllowance
}

func (a *App) StoreFactory() spi.StoreFactory             { return a.storeFactory }
func (a *App) TransactionManager() spi.TransactionManager { return a.transactionManager }
func (a *App) AuthenticationService() contract.AuthenticationService {
	return a.authService
}

// AuthService returns the underlying *auth.AuthService when JWT IAM mode is
// active, or nil when running in mock IAM mode. Exposed for tests that
// inspect the auth stores.
func (a *App) AuthService() *auth.AuthService { return a.authSvc }
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
func (a *App) Fence() *fence.Fence                          { return a.fence }
func (a *App) NodeRegistry() contract.NodeRegistry          { return a.nodeRegistry }

// gRPCGracefulStopBudget is the upper bound on graceful drain at shutdown.
// Matched to shutdownDrainBudget, the HTTP and admin drain deadline in
// cmd/cyoda/run.go, so a caller can predict total stop time as
// ~max(http, admin, grpc) drain budgets.
const gRPCGracefulStopBudget = 10 * time.Second

// searchDrainBudget bounds how long Shutdown waits for in-flight async
// search jobs to finish naturally before releasing whatever is still
// registered for reclaim. Jobs run their own context (not the pool's — see
// search.WithAsyncPool's doc comment), so pool.Drain's own ctx cancellation
// does not itself abort them; this budget is what actually bounds the wait.
const searchDrainBudget = 5 * time.Second

// stopSearchReaperLoop stops the async-search sweeps and waits for them to
// exit. Idempotent: safe to call from both Shutdown and Close (sync.Once runs
// the stop once, and a second caller waits for it to finish).
func (a *App) stopSearchReaperLoop() {
	if a.stopSearchReapers == nil {
		return
	}
	a.stopSearchReapersOnce.Do(a.stopSearchReapers)
}

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
// so operators can see the budget was hit.
func (a *App) Close() error {
	slog.Info("shutting down")
	// Drain the scheduler before anything is torn down, so a Close without
	// Shutdown does not leave it claiming and heartbeating against a store
	// that is closing. After Shutdown this does nothing.
	a.DrainScheduler(context.Background())
	// Stop the reaper first so a node whose store is closing does not keep
	// sweeping on the claim ticker against a store being torn down.
	a.stopSearchReaperLoop()
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
	// A server failed, or the binary did not drain the scheduler first: the
	// same steps run here, after the servers stopped, with no outside
	// deadline. After a DrainScheduler this does nothing.
	if a.scheduler != nil {
		a.scheduler.Stop()
	}
	a.stopSearchReaperLoop()
	if a.searchPool != nil {
		drainCtx, cancel := context.WithTimeout(context.Background(), searchDrainBudget)
		a.searchPool.Drain(drainCtx)
		cancel()
	}
	if a.searchService != nil {
		// Jobs still registered after the drain budget are released for
		// reclaim (a peer, or this node on restart, re-runs them), not failed.
		if n := a.searchService.ReleaseRegisteredJobs(context.Background()); n > 0 {
			slog.Info("released in-flight async search jobs for reclaim at shutdown", "pkg", "search", "count", n)
		}
	}
	if a.nodeRegistry != nil && a.config.Cluster.Enabled {
		if err := a.nodeRegistry.Deregister(context.Background(), a.config.Cluster.NodeID); err != nil {
			slog.Warn("failed to deregister from cluster", "pkg", "cluster", "err", err)
		}
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

// applyFuncSetter is the wiring surface Run soft-asserts on each plugin
// factory. It uses the raw function signature (not any plugin-local named
// ApplyFunc type) so a single type-assertion satisfies all plugins
// uniformly. The assertion is soft: a factory that stops implementing
// this exact signature would be skipped silently and every fold-on-read
// of pending deltas would fail — applyfunc_wiring_test.go pins the
// in-tree factories to this same declaration at compile time.
type applyFuncSetter interface {
	SetApplyFunc(fn func(base []byte, delta spi.SchemaDelta) ([]byte, error))
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
		NodeID:           c.NodeID,
		NodeAddr:         c.NodeAddr,
		GRPCNodeAddr:     c.GRPCNodeAddr,
		BindAddr:         gossipHost,
		BindPort:         gossipPort,
		Seeds:            c.SeedNodes,
		StabilityWindow:  c.StabilityWindow,
		SecretKey:        c.HMACSecret,
		ListScanInterval: registry.ScanIntervalFor(c.DispatchWaitTimeout),
		Meter:            observability.Meter(),
	})
	if err != nil {
		slog.Error("failed to create gossip registry", "pkg", "cluster", "err", err)
		os.Exit(1)
	}
	return g
}
