// Package postgres provides a BackendFixture that runs cyoda-go with
// the PostgreSQL storage backend (via testcontainers-go) and a
// compute-test-client subprocess connected via gRPC.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/cyoda-platform/cyoda-go/e2e/parity"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/fixtureutil"
	"github.com/cyoda-platform/cyoda-go/internal/testpg"
)

// postgresFixture implements parity.BackendFixture for the postgres backend.
type postgresFixture struct {
	baseURL      string
	grpcEndpoint string
	keySet       *fixtureutil.JWTKeySet
	// computeCreds holds the M2M client of each tenant a further compute
	// client joins under.
	computeCreds *fixtureutil.ComputeCredentials
	computeBin   string
	// logs is the cyoda node's captured output (parity.SchedulerWaitFixture).
	logs *fixtureutil.SyncBuffer
	// container and connStr are the shared database server, from which a
	// fresh stack gets a database of its own (NewFreshStack).
	container *tcpostgres.PostgresContainer
	connStr   string
}

var _ parity.ComputeClientFixture = (*postgresFixture)(nil)

var _ parity.SchedulerWaitFixture = (*postgresFixture)(nil)

// AwaitScheduler implements parity.SchedulerWaitFixture.
func (f *postgresFixture) AwaitScheduler(t *testing.T, slack time.Duration, what string, check func() (bool, string)) {
	t.Helper()
	fixtureutil.AwaitNodeScheduler(t, slack, what, check, f.logs)
}

// BaseURL implements parity.BackendFixture.
func (f *postgresFixture) BaseURL() string { return f.baseURL }

// GRPCEndpoint implements parity.BackendFixture.
func (f *postgresFixture) GRPCEndpoint() string { return f.grpcEndpoint }

// NewTenant implements parity.BackendFixture — mints a fresh JWT with
// a unique tenant for each test.
func (f *postgresFixture) NewTenant(t *testing.T) parity.Tenant {
	t.Helper()
	return fixtureutil.MintTenantJWT(t, f.keySet)
}

// ComputeTenant implements parity.BackendFixture.
func (f *postgresFixture) ComputeTenant(t *testing.T) parity.Tenant {
	t.Helper()
	return fixtureutil.MintComputeTenantJWT(t, f.keySet)
}

// PlatformOperator implements parity.BackendFixture.
func (f *postgresFixture) PlatformOperator(t *testing.T) parity.Tenant {
	t.Helper()
	return fixtureutil.MintPlatformOperatorJWT(t, f.keySet)
}

// StartComputeClient implements parity.ComputeClientFixture.
func (f *postgresFixture) StartComputeClient(t *testing.T, spec parity.ComputeClientSpec) parity.ComputeClient {
	t.Helper()
	return fixtureutil.StartComputeClientForFixture(t, f.computeCreds, f.computeBin, f.grpcEndpoint, f.baseURL, spec)
}

var _ parity.FreshStackFixture = (*postgresFixture)(nil)

// freshDBSeq numbers the databases fresh stacks create.
var freshDBSeq atomic.Int64

// NewFreshStack implements parity.FreshStackFixture: a stack on a database of
// its own on the fixture's server, dropped when the test ends.
func (f *postgresFixture) NewFreshStack(t *testing.T, env map[string]string) parity.FreshStack {
	t.Helper()
	ctx := context.Background()
	name := fmt.Sprintf("cyoda_fresh_%d_%d", time.Now().UnixNano(), freshDBSeq.Add(1)) // realtime: a unique name
	psql := func(sql string) {
		t.Helper()
		code, out, err := f.container.Exec(ctx, []string{"psql", "-U", "testuser", "-d", "cyoda_parity_test", "-v", "ON_ERROR_STOP=1", "-c", sql})
		if err != nil || code != 0 {
			var msg []byte
			if out != nil {
				msg, _ = io.ReadAll(out)
			}
			t.Fatalf("psql %q: exit %d, %v: %s", sql, code, err, msg)
		}
	}
	psql("CREATE DATABASE " + name)
	// Registered before the node starts, so it runs after the node stops.
	t.Cleanup(func() { psql("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)") })

	u, err := url.Parse(f.connStr)
	if err != nil {
		// errors.Unwrap: *url.Error renders the URL, which carries the password.
		t.Fatalf("parse connection string: %v", errors.Unwrap(err))
	}
	u.Path = "/" + name
	return fixtureutil.LaunchFreshStack(t, f.keySet, []string{
		"CYODA_STORAGE_BACKEND=postgres",
		"CYODA_POSTGRES_URL=" + u.String(),
		"CYODA_POSTGRES_AUTO_MIGRATE=true",
	}, env)
}

// setup boots a Postgres testcontainer, builds binaries, launches
// subprocesses, and waits for readiness. It returns a teardown function
// that kills subprocesses and terminates the container.
func setup() (*postgresFixture, func(), error) {
	ctx := context.Background()

	// 1. Start PostgreSQL container.
	opts := append([]testcontainers.ContainerCustomizer{
		tcpostgres.WithDatabase("cyoda_parity_test"),
		tcpostgres.WithUsername("testuser"),
		tcpostgres.WithPassword("testpass"),
	}, testpg.HardenedOptions()...)
	pgContainer, err := tcpostgres.Run(ctx, "postgres:17-alpine", opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to start postgres container: %w", err)
	}

	containerCleanup := func() {
		testpg.DumpDiagnosticsIfDied(ctx, pgContainer)
		_ = pgContainer.Terminate(ctx)
	}

	connStr, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		containerCleanup()
		return nil, nil, fmt.Errorf("failed to get connection string: %w", err)
	}

	// 2. Generate JWT key set.
	ks, err := fixtureutil.GenerateJWTKeySet()
	if err != nil {
		containerCleanup()
		return nil, nil, err
	}

	// 3. Launch cyoda-go + compute-test-client with postgres backend.
	result, processCleanup, err := fixtureutil.LaunchCyodaAndCompute(ks, append([]string{
		"CYODA_STORAGE_BACKEND=postgres",
		fmt.Sprintf("CYODA_POSTGRES_URL=%s", connStr),
		"CYODA_POSTGRES_AUTO_MIGRATE=true",
	}, fixtureutil.TunedServerEnv()...))
	if err != nil {
		containerCleanup()
		return nil, nil, err
	}

	cleanup := func() {
		processCleanup()
		containerCleanup()
	}

	fix := &postgresFixture{
		baseURL:      result.BaseURL,
		grpcEndpoint: result.GRPCEndpoint,
		keySet:       ks,
		computeCreds: fixtureutil.NewComputeCredentials(ks),
		computeBin:   result.ComputeBin,
		logs:         result.Logs,
		container:    pgContainer,
		connStr:      connStr,
	}

	return fix, cleanup, nil
}
