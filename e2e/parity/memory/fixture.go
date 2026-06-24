// Package memory provides a BackendFixture that runs cyoda-go with
// the in-memory storage backend and a compute-test-client subprocess
// connected via gRPC.
package memory

import (
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/fixtureutil"
)

// memoryFixture implements parity.BackendFixture for the memory backend.
type memoryFixture struct {
	baseURL      string
	grpcEndpoint string
	keySet       *fixtureutil.JWTKeySet
}

// BaseURL implements parity.BackendFixture.
func (f *memoryFixture) BaseURL() string { return f.baseURL }

// GRPCEndpoint implements parity.BackendFixture.
func (f *memoryFixture) GRPCEndpoint() string { return f.grpcEndpoint }

// NewTenant implements parity.BackendFixture — mints a fresh JWT with
// a unique tenant for each test.
func (f *memoryFixture) NewTenant(t *testing.T) parity.Tenant {
	t.Helper()
	return fixtureutil.MintTenantJWT(t, f.keySet)
}

// ComputeTenant implements parity.BackendFixture.
func (f *memoryFixture) ComputeTenant(t *testing.T) parity.Tenant {
	t.Helper()
	return fixtureutil.MintComputeTenantJWT(t, f.keySet)
}

// NewNonAdminTenant implements parity.NonAdminTenantFixture — mints a
// fresh JWT without ROLE_ADMIN for authz-negative parity scenarios.
func (f *memoryFixture) NewNonAdminTenant(t *testing.T) parity.Tenant {
	t.Helper()
	return fixtureutil.MintNonAdminTenantJWT(t, f.keySet)
}

// IsTxBoundAuditStore implements parity.TxBoundAuditFixture. The memory
// backend writes audit events via the in-process bus before any
// entity-update rollback runs, so a rolled-back entity write still
// leaves its paired STATE_MACHINE_START + TRANSITION_ABORTED events
// durable in the audit log.
func (f *memoryFixture) IsTxBoundAuditStore() bool { return false }

// setup builds binaries, launches subprocesses, and waits for readiness.
// It returns a teardown function that kills the subprocesses.
func setup() (*memoryFixture, func(), error) {
	ks, err := fixtureutil.GenerateJWTKeySet()
	if err != nil {
		return nil, nil, err
	}

	result, cleanup, err := fixtureutil.LaunchCyodaAndCompute(ks, []string{
		"CYODA_STORAGE_BACKEND=memory",
	})
	if err != nil {
		return nil, nil, err
	}

	fix := &memoryFixture{
		baseURL:      result.BaseURL,
		grpcEndpoint: result.GRPCEndpoint,
		keySet:       ks,
	}

	return fix, cleanup, nil
}

// MustSetup is a test helper that boots the memory fixture and returns
// it along with a cleanup func. It exists for external callers
// (currently only e2e/externalapi/driver/remote_smoke_test.go) that
// need access to BaseURL + a tenant without going through the full
// AllTests loop.
//
// Fails the test on setup error. Callers MUST `defer cleanup()` on the
// line immediately following MustSetup — a panic before the defer
// registers will leave the cyoda-go + compute-test-client subprocesses
// running.
//
// Symmetric helpers do not yet exist on the sqlite/postgres fixtures
// because tranche-1's only consumer is a memory-backed remote-mode
// smoke. If a future test needs a sqlite/postgres equivalent, add the
// helper there with the same signature.
func MustSetup(t *testing.T) (parity.BackendFixture, func()) {
	t.Helper()
	fix, cleanup, err := setup()
	if err != nil {
		t.Fatalf("memory fixture setup: %v", err)
	}
	return fix, cleanup
}
