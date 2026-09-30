package parity

import "testing"

// BackendFixture is the only contract between parity scenarios and a
// concrete backend implementation. Per-backend wrappers (memory, postgres,
// and any out-of-tree storage plugin) implement this and pass it into
// AllTests().
//
// The interface is intentionally minimal:
//   - There is no Backend() accessor — scenarios cannot ask which backend
//     they are running against, because the contract is that they pass
//     identically on all of them.
//   - There is no compute-client handle on this interface — the fixture's own
//     compute-test-client is a separate subprocess reached via gRPC. A fixture
//     that can start FURTHER compute clients for a scenario advertises it
//     through the optional ComputeClientFixture (compute_client.go).
//   - There is no storage handle — verification is API-only.
type BackendFixture interface {
	// BaseURL returns the cyoda HTTP base URL with scheme, host, port, and any
	// context path prefix. The returned value has NO trailing slash, so callers
	// construct paths as baseURL + "/api/...".
	//   e.g. "http://127.0.0.1:54321"  or  "http://127.0.0.1:54321/cyoda"
	BaseURL() string

	// GRPCEndpoint returns the cyoda gRPC host:port for tests that drive
	// the gRPC CloudEvents API directly. Most tests use HTTP via BaseURL().
	GRPCEndpoint() string

	// NewTenant mints a fresh tenant for the test, returning its ID and a
	// signed JWT. Each test uses a fresh tenant so test order does not
	// matter and tests can in principle be run in parallel.
	//
	// Implementations MUST call t.Helper() at the top of their NewTenant
	// implementation, and MUST t.Fatal on provisioning failure (so the
	// caller does not need to error-check). The returned Tenant is fresh
	// per call — implementations must not return a cached value.
	NewTenant(t *testing.T) Tenant

	// ComputeTenant returns a Tenant whose ID matches the compute-test-client's
	// registered tenant. Processor and criteria dispatch is tenant-scoped:
	// the gRPC MemberRegistry only routes requests to members registered
	// under the same tenant. The compute-test-client connects with a fixed
	// tenant (from its M2M JWT), so tests that exercise processor/criteria
	// dispatch must create entities under this tenant.
	//
	// Tests that do NOT need processor/criteria dispatch should use
	// NewTenant for full tenant isolation.
	ComputeTenant(t *testing.T) Tenant

	// PlatformOperator returns a platform-operator token: ROLE_ADMIN in the
	// PLATFORM tenant. The platform-wide admin endpoints (signing key pairs,
	// OIDC reload) accept only this principal. PLATFORM is one shared tenant,
	// unlike NewTenant: use it only on those endpoints, never for tenant data.
	// Implementations MUST call t.Helper() and t.Fatal on failure.
	PlatformOperator(t *testing.T) Tenant
}

// NonAdminTenantFixture is an OPTIONAL capability interface that
// BackendFixture implementations may satisfy to support minting
// non-admin JWTs for authorization-negative parity scenarios.
//
// Scenarios that test 403 FORBIDDEN responses on admin-only endpoints
// call NewNonAdminTenant to get a token that authenticates but does not
// carry ROLE_ADMIN. Backends that cannot mint arbitrary JWTs (e.g. an
// out-of-tree backend that delegates auth to an external IdP) may leave
// this interface unimplemented — the OIDC authz-negative scenarios will
// skip automatically via t.Skip.
//
// All in-tree backends (memory, sqlite, postgres) implement this
// interface because they mint JWTs from a locally held RSA keypair.
type NonAdminTenantFixture interface {
	// NewNonAdminTenant mints a fresh tenant JWT that carries no
	// ROLE_ADMIN scope. The tenant ID is unique per call (same
	// isolation contract as NewTenant). Implementations MUST call
	// t.Helper() and t.Fatal on provisioning failure.
	NewNonAdminTenant(t *testing.T) Tenant
}

// NonAdminTenantOrSkip resolves the fixture's non-admin tenant
// capability. If the fixture does not implement NonAdminTenantFixture,
// the test is skipped. Otherwise it returns the non-admin tenant.
// Call this at the top of every authz-negative scenario.
func NonAdminTenantOrSkip(t *testing.T, fixture BackendFixture) Tenant {
	t.Helper()
	na, ok := fixture.(NonAdminTenantFixture)
	if !ok {
		t.Skip("fixture does not support non-admin JWT minting; skipping authz-negative test")
	}
	return na.NewNonAdminTenant(t)
}

// Tenant identifies a fresh tenant scope for a single test, plus the JWT
// the test uses to authenticate API calls within that scope.
type Tenant struct {
	// ID is the tenant id the token carries in the "caas_org_id" claim: a
	// fresh UUID for NewTenant, "system-tenant" for ComputeTenant, or
	// "PLATFORM" for PlatformOperator. Kept as string (not uuid.UUID) so the
	// parity package
	// does not pull github.com/google/uuid into its import graph beyond what
	// the generated OpenAPI client already requires. Cyoda's generated API
	// types use the string form for tenant IDs on the wire, so the parity
	// types match the wire shape.
	ID string

	// Token is the signed JWT used in the Authorization header. Never log
	// it, never include it in test-failure messages, never serialize it to
	// disk. Tests use it only as the "Bearer ..." value of an Authorization
	// header. (CLAUDE.md security gate: credentials must not be logged at
	// any level.)
	Token string
}
