// Package postgres provides a BackendFixture that runs cyoda-go with
// the PostgreSQL storage backend (via testcontainers-go) and a
// compute-test-client subprocess connected via gRPC.
package postgres

import (
	"context"
	"fmt"
	"testing"

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

// NewNonAdminTenant implements parity.NonAdminTenantFixture — mints a
// fresh JWT without ROLE_ADMIN for authz-negative parity scenarios.
func (f *postgresFixture) NewNonAdminTenant(t *testing.T) parity.Tenant {
	t.Helper()
	return fixtureutil.MintNonAdminTenantJWT(t, f.keySet)
}

// IsTxBoundAuditStore implements parity.TxBoundAuditFixture. The
// postgres backend writes audit events into the same SQL transaction as
// the entity writes, so a rolled-back entity-update transaction also
// discards its paired STATE_MACHINE_START + TRANSITION_ABORTED events.
// Audit-shape parity scenarios (issue #228) branch on this property.
func (f *postgresFixture) IsTxBoundAuditStore() bool { return true }

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
	result, processCleanup, err := fixtureutil.LaunchCyodaAndCompute(ks, []string{
		"CYODA_STORAGE_BACKEND=postgres",
		fmt.Sprintf("CYODA_POSTGRES_URL=%s", connStr),
		"CYODA_POSTGRES_AUTO_MIGRATE=true",
		// Tuned down from the 1s production default so the
		// scheduledtransition parity scenarios (e2e/parity/scheduledtransition)
		// observe fires within a small, bounded poll window instead of
		// needing multi-second timeouts. Harmless to every other parity
		// scenario — an empty ScanDue is a cheap no-op query.
		"CYODA_SCHEDULER_SCAN_INTERVAL=50ms",
	})
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
	}

	return fix, cleanup, nil
}
