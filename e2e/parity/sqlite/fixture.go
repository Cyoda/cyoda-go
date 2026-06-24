// Package sqlite provides a BackendFixture that runs cyoda-go with
// the SQLite storage backend and a compute-test-client subprocess
// connected via gRPC.
//
// Unlike the postgres fixture, no container is needed — SQLite is
// embedded in the cyoda-go binary. A temporary directory holds the
// database file for test isolation.
package sqlite

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/fixtureutil"
)

// sqliteFixture implements parity.BackendFixture for the sqlite backend.
type sqliteFixture struct {
	baseURL      string
	grpcEndpoint string
	keySet       *fixtureutil.JWTKeySet
}

// BaseURL implements parity.BackendFixture.
func (f *sqliteFixture) BaseURL() string { return f.baseURL }

// GRPCEndpoint implements parity.BackendFixture.
func (f *sqliteFixture) GRPCEndpoint() string { return f.grpcEndpoint }

// NewTenant implements parity.BackendFixture — mints a fresh JWT with
// a unique tenant for each test.
func (f *sqliteFixture) NewTenant(t *testing.T) parity.Tenant {
	t.Helper()
	return fixtureutil.MintTenantJWT(t, f.keySet)
}

// ComputeTenant implements parity.BackendFixture.
func (f *sqliteFixture) ComputeTenant(t *testing.T) parity.Tenant {
	t.Helper()
	return fixtureutil.MintComputeTenantJWT(t, f.keySet)
}

// NewNonAdminTenant implements parity.NonAdminTenantFixture — mints a
// fresh JWT without ROLE_ADMIN for authz-negative parity scenarios.
func (f *sqliteFixture) NewNonAdminTenant(t *testing.T) parity.Tenant {
	t.Helper()
	return fixtureutil.MintNonAdminTenantJWT(t, f.keySet)
}

// IsTxBoundAuditStore implements parity.TxBoundAuditFixture. The sqlite
// backend writes audit events outside the entity-write transaction (via
// the same in-process bus as the memory backend), so a rolled-back
// entity write still leaves its paired STATE_MACHINE_START +
// TRANSITION_ABORTED events durable.
func (f *sqliteFixture) IsTxBoundAuditStore() bool { return false }

// setup creates a temp directory for the SQLite database, builds
// binaries, launches subprocesses, and waits for readiness. It returns
// a teardown function that kills subprocesses and removes the temp dir.
func setup() (*sqliteFixture, func(), error) {
	// 1. Create temp directory for SQLite database.
	tmpDir, err := os.MkdirTemp("", "cyoda-parity-sqlite-*")
	if err != nil {
		return nil, nil, err
	}
	dbPath := filepath.Join(tmpDir, "test.db")

	tmpCleanup := func() {
		_ = os.RemoveAll(tmpDir)
	}

	// 2. Generate JWT key set.
	ks, err := fixtureutil.GenerateJWTKeySet()
	if err != nil {
		tmpCleanup()
		return nil, nil, err
	}

	// 3. Launch cyoda-go + compute-test-client with sqlite backend.
	result, processCleanup, err := fixtureutil.LaunchCyodaAndCompute(ks, []string{
		"CYODA_STORAGE_BACKEND=sqlite",
		"CYODA_SQLITE_PATH=" + dbPath,
		"CYODA_SQLITE_AUTO_MIGRATE=true",
	})
	if err != nil {
		tmpCleanup()
		return nil, nil, err
	}

	cleanup := func() {
		processCleanup()
		tmpCleanup()
	}

	fix := &sqliteFixture{
		baseURL:      result.BaseURL,
		grpcEndpoint: result.GRPCEndpoint,
		keySet:       ks,
	}

	return fix, cleanup, nil
}
