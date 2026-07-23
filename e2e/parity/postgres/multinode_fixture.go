// Multi-node fixture: launches one Postgres testcontainer plus N
// cyoda-go subprocesses sharing it, with cluster bootstrap envs and
// one shared compute-test-client. Lives in e2e/parity/postgres (the
// test fixture package), NOT plugins/postgres (the storage backend
// submodule, which has its own go.mod and cannot import e2e/parity).
package postgres

import (
	"context"
	"fmt"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/cyoda-platform/cyoda-go/e2e/parity"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/fixtureutil"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/multinode"
	"github.com/cyoda-platform/cyoda-go/internal/testpg"
)

// pgMultiNode implements multinode.MultiNodeFixture for the postgres backend.
type pgMultiNode struct {
	baseURLs []string
	keySet   *fixtureutil.JWTKeySet
}

// BaseURLs implements multinode.MultiNodeFixture.
func (f *pgMultiNode) BaseURLs() []string {
	out := make([]string, len(f.baseURLs))
	copy(out, f.baseURLs)
	return out
}

// NodeCount implements multinode.MultiNodeFixture.
func (f *pgMultiNode) NodeCount() int { return len(f.baseURLs) }

// NewTenant implements multinode.MultiNodeFixture — mints a fresh JWT
// with a unique tenant for each test. Valid against every node in the
// cluster (all nodes share the same JWT signing key).
func (f *pgMultiNode) NewTenant(t *testing.T) parity.Tenant {
	t.Helper()
	return fixtureutil.MintTenantJWT(t, f.keySet)
}

// ComputeTenant implements multinode.MultiNodeFixture — mints a JWT
// scoped to the compute-test-client's tenant so processor/criteria
// dispatch can find the registered gRPC member.
func (f *pgMultiNode) ComputeTenant(t *testing.T) parity.Tenant {
	t.Helper()
	return fixtureutil.MintComputeTenantJWT(t, f.keySet)
}

// ComputeUser mints a USER-kind JWT (caas_user_id == userID) scoped to the
// compute-test-client's tenant — a human origin whose cascades still dispatch
// to the registered gRPC member. Used by cross-node attribution scenarios that
// need a user-kind causal origin distinct from the member's service identity.
// Not part of the MultiNodeFixture interface (attribution is postgres-first);
// the postgres multinode attribution tests type-assert for it.
func (f *pgMultiNode) ComputeUser(t *testing.T, userID string, roles ...string) parity.Tenant {
	t.Helper()
	return fixtureutil.MintComputeUserJWT(t, f.keySet, userID, roles...)
}

// MustSetupMultiNode boots a Postgres testcontainer plus n cyoda-go
// subprocesses sharing it (with cluster bootstrap) and returns a
// MultiNodeFixture plus a cleanup function. Caller MUST defer cleanup
// immediately. Fails the test on any setup error, ensuring partial
// state is torn down before fataling.
func MustSetupMultiNode(t *testing.T, n int) (multinode.MultiNodeFixture, func()) {
	t.Helper()
	ctx := context.Background()

	// 1. Start PostgreSQL container.
	opts := append([]testcontainers.ContainerCustomizer{
		tcpostgres.WithDatabase("cyoda_parity_multinode"),
		tcpostgres.WithUsername("testuser"),
		tcpostgres.WithPassword("testpass"),
	}, testpg.HardenedOptions()...)
	pgContainer, err := tcpostgres.Run(ctx, "postgres:17-alpine", opts...)
	if err != nil {
		t.Fatalf("failed to start postgres container: %v", err)
	}
	containerCleanup := func() {
		testpg.DumpDiagnosticsIfDied(ctx, pgContainer)
		_ = pgContainer.Terminate(ctx)
	}

	connStr, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		containerCleanup()
		t.Fatalf("failed to get postgres connection string: %v", err)
	}

	// 2. Generate JWT key set shared by every node.
	ks, err := fixtureutil.GenerateJWTKeySet()
	if err != nil {
		containerCleanup()
		t.Fatalf("failed to generate JWT key set: %v", err)
	}

	// 3. Launch n cyoda-go subprocesses + one compute-test-client.
	//    Auto-migrate handling (leader-only) is in the fixtureutil helper.
	result, processCleanup, err := fixtureutil.LaunchCyodaClusterAndCompute(ks, n, []string{
		"CYODA_STORAGE_BACKEND=postgres",
		fmt.Sprintf("CYODA_POSTGRES_URL=%s", connStr),
		"CYODA_POSTGRES_AUTO_MIGRATE=true",
	})
	if err != nil {
		containerCleanup()
		t.Fatalf("failed to launch cyoda-go cluster: %v", err)
	}

	cleanup := func() {
		processCleanup()
		containerCleanup()
	}

	return &pgMultiNode{
		baseURLs: result.BaseURLs,
		keySet:   ks,
	}, cleanup
}
