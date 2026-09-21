package e2e_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"flag"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/app"
	"github.com/cyoda-platform/cyoda-go/internal/e2e/openapivalidator"
	"github.com/cyoda-platform/cyoda-go/internal/observability"
	"github.com/cyoda-platform/cyoda-go/internal/testing/localproc"
	"github.com/cyoda-platform/cyoda-go/internal/testpg"

	// Register stock storage plugins so spi.GetPlugin("postgres") resolves.
	_ "github.com/cyoda-platform/cyoda-go/plugins/memory"
	_ "github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

var (
	serverURL       string                            // base URL of the test server (e.g., "http://127.0.0.1:12345")
	dbPool          *pgxpool.Pool                     // direct DB access for verification queries
	procSvc         *localproc.LocalProcessingService // in-process processor/criteria for workflow tests
	allOperationIds []string
	markedOps       = map[string]string{} // operationId → x-cyoda-status value
	testApp         *app.App              // exposed for test-mode store seeding (e.g. cross-tenant M2M client bootstrap)
	e2eSignKey      *rsa.PrivateKey       // the stack's JWT signing key, for tests that mint bespoke claims
	e2eIssuer       string                // the stack's JWT issuer, for the same
)

// readCyodaStatus returns the x-cyoda-status marker on an operation, or "".
func readCyodaStatus(op *openapi3.Operation) string {
	if op == nil || op.Extensions == nil {
		return ""
	}
	if v, ok := op.Extensions["x-cyoda-status"]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func TestMain(m *testing.M) {
	// flag.Parse must be called before testing.Short() is valid.
	flag.Parse()
	if testing.Short() {
		os.Exit(0) // skip E2E in short mode
	}

	ctx := context.Background()

	// Start PostgreSQL container
	opts := append([]testcontainers.ContainerCustomizer{
		tcpostgres.WithDatabase("minicyoda_test"),
		tcpostgres.WithUsername("testuser"),
		tcpostgres.WithPassword("testpass"),
	}, testpg.HardenedOptions()...)
	pgContainer, err := tcpostgres.Run(ctx, "postgres:17-alpine", opts...)
	if err != nil {
		log.Fatalf("failed to start postgres container: %v", err)
	}
	defer func() {
		testpg.DumpDiagnosticsIfDied(ctx, pgContainer)
		_ = pgContainer.Terminate(ctx)
	}()

	connStr, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		log.Fatalf("failed to get connection string: %v", err)
	}

	// Create a direct pool for verification queries
	dbPool, err = pgxpool.New(ctx, connStr)
	if err != nil {
		log.Fatalf("failed to create verification pool: %v", err)
	}
	defer dbPool.Close()

	// Generate JWT signing key
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		log.Fatalf("failed to generate RSA key: %v", err)
	}
	keyBytes, err := x509.MarshalPKCS8PrivateKey(rsaKey)
	if err != nil {
		log.Fatalf("failed to marshal key: %v", err)
	}
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes}))

	// Configure postgres plugin via env vars — the plugin reads its own
	// config from CYODA_POSTGRES_* through the getenv function passed to
	// NewFactory. Unset after app.New captures them.
	os.Setenv("CYODA_POSTGRES_URL", connStr)
	os.Setenv("CYODA_POSTGRES_MAX_CONNS", "5")
	os.Setenv("CYODA_POSTGRES_MIN_CONNS", "1")
	os.Setenv("CYODA_POSTGRES_MAX_CONN_IDLE_TIME", "1m")
	os.Setenv("CYODA_POSTGRES_AUTO_MIGRATE", "true")
	defer func() {
		os.Unsetenv("CYODA_POSTGRES_URL")
		os.Unsetenv("CYODA_POSTGRES_MAX_CONNS")
		os.Unsetenv("CYODA_POSTGRES_MIN_CONNS")
		os.Unsetenv("CYODA_POSTGRES_MAX_CONN_IDLE_TIME")
		os.Unsetenv("CYODA_POSTGRES_AUTO_MIGRATE")
	}()

	cfg := app.DefaultConfig()
	cfg.ContextPath = "/api"
	cfg.StorageBackend = "postgres"
	// Override only the JWT auth fields; preserve IAM feature defaults
	// (KeypairDefaultValidityDays, TrustedKeyMax*, etc.) from DefaultConfig.
	cfg.IAM.Mode = "jwt"
	cfg.IAM.JWTSigningKey = keyPEM
	cfg.IAM.JWTIssuer = "cyoda-test"
	cfg.IAM.JWTExpiry = 3600
	e2eSignKey = rsaKey
	e2eIssuer = cfg.IAM.JWTIssuer
	cfg.Bootstrap = app.BootstrapConfig{
		ClientID:     "testclient",
		ClientSecret: "testsecret",
		TenantID:     "test-tenant",
		UserID:       "test-admin",
		Roles:        "ROLE_ADMIN,ROLE_M2M",
	}
	// Enable trusted-key feature for E2E coverage. The KV store backing the
	// trusted-key store is wired in app.New, so this must be set before that call.
	cfg.IAM.TrustedKeyRegistrationEnabled = true
	cfg.IAM.M2MAdminRoleEnabled = true

	// The package-global testApp shares this Postgres with every per-test
	// harness. With the reclaim sweep on the heartbeat interval it would
	// otherwise claim released/stale RUNNING jobs from other tests' Apps and
	// make "which node completed the job" nondeterministic — the async
	// orphan/crash/shutdown-release tests each stand up their own App and
	// assert which node re-executes a job. Quiesce it: a 1h heartbeat interval
	// and a 4h stale bound (staleAfter == the enforced 4x floor, so
	// Config.Validate still accepts it) mean its only reclaim sweep is the
	// startup one, which runs once at TestMain before any test synthesises a
	// job. Plain config, no test hook.
	cfg.SearchJobHeartbeatInterval = time.Hour
	cfg.SearchJobStaleAfter = 4 * time.Hour

	// The package-global testApp's scheduler would scan the SAME
	// scheduled_tasks rows every per-test harness's own Postgres-backed App
	// writes into (ScanDue is deliberately cross-tenant and node-blind, and
	// no per-test harness gossips with this one, so each independently
	// believes itself the sole coordinator). Whichever scanner sees a due row
	// first throttles every other scanner — this one included — from
	// retrying it for its own RedispatchBackoff, so a private harness's
	// tighter settings cannot help once this scheduler has already won that
	// race and failed the fire (its ExternalProcessingService, procSvc, has
	// no callback registered for a private harness's processor names). Only
	// one scheduler may scan this database: this one does not. Tests that
	// need a scheduled fire against testApp start their own bespoke
	// scheduler.Service (see startTestScheduler in scheduled_transition_test.go).
	// Plain config, no test hook.
	cfg.Scheduler.Enabled = false

	// In-process processor/criteria service for workflow E2E tests.
	procSvc = localproc.New()
	cfg.ExternalProcessing = procSvc

	// Create an unstarted server to discover the port BEFORE constructing the app.
	// The app constructs the JWKS validator URL using cfg.HTTPPort — it must match
	// the actual server port for JWT validation to work.
	srv := httptest.NewUnstartedServer(nil)
	srv.Start()
	serverURL = srv.URL
	defer srv.Close()

	// Extract the port from the httptest server URL and set it in the config
	// so the JWKS validator URL points to the right place.
	srvPort := srv.Listener.Addr().(*net.TCPAddr).Port
	cfg.HTTPPort = srvPort

	// Same order as cmd/cyoda/main.go: the metrics pipeline exists before the
	// storage plugin registers its instruments.
	otelShutdown, err := observability.Init(ctx, "cyoda-e2e", "e2e", false)
	if err != nil {
		log.Fatalf("observability init: %v", err)
	}
	defer otelShutdown(ctx)

	testApp = app.New(cfg)

	// Build the conformance validator from the embedded spec. Wraps the
	// production handler; failures collected end-to-end and reported by
	// TestOpenAPIConformanceReport (zzz_openapi_conformance_test.go).
	swagger, err := api.GetSwagger()
	if err != nil {
		log.Fatalf("get swagger: %v", err)
	}
	// Replace declared server URLs with a single relative-base entry that
	// reflects the test server's mount point. The test server hosts the app
	// under cfg.ContextPath ("/api"); the spec's paths are relative to the
	// server URL. Without this, the kin-openapi router matches /entity/{id}
	// from the spec against the test server's /api/entity/{id} requests and
	// reports every operation as "no spec route matches".
	swagger.Servers = openapi3.Servers{{URL: cfg.ContextPath}}
	validator, err := openapivalidator.NewValidator(swagger)
	if err != nil {
		log.Fatalf("build validator: %v", err)
	}
	srv.Config.Handler = openapivalidator.NewMiddleware(validator)(testApp.Handler())

	// Capture the full operationId set so the conformance test can compute
	// the uncovered list at end-of-suite. Every published operation is
	// included; ops that aren't live must carry an x-cyoda-status marker
	// (enforced by TestOpenAPIConformanceReport).
	for _, item := range swagger.Paths.Map() {
		for _, op := range item.Operations() {
			if op.OperationID == "" {
				continue
			}
			if s := readCyodaStatus(op); s != "" {
				markedOps[op.OperationID] = s
			}
			allOperationIds = append(allOperationIds, op.OperationID)
		}
	}

	os.Exit(m.Run())
}

func TestHealth(t *testing.T) {
	req, err := e2eNewRequest(t, "GET", serverURL+"/api/health", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("health check failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}
