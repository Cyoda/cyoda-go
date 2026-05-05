package multinode

import (
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity"
)

// MultiNodeFixture is the cluster-capable counterpart to
// parity.BackendFixture. Implementations launch N cyoda-go
// subprocesses sharing the same backing storage and expose one
// HTTP base URL per node. Tenants are minted once and used across
// all nodes (the cluster shares state, including auth).
type MultiNodeFixture interface {
	// BaseURLs returns one HTTP base URL per node, in stable order.
	// Length equals NodeCount(). Each URL has no trailing slash.
	BaseURLs() []string

	// NodeCount returns the number of nodes in the cluster.
	NodeCount() int

	// NewTenant mints a fresh tenant for the test. The returned JWT
	// is valid against every node in the cluster.
	NewTenant(t *testing.T) parity.Tenant

	// ComputeTenant returns a Tenant whose ID matches the
	// compute-test-client's tenant. Tests that exercise gRPC
	// processor/criteria dispatch against the cluster-shared compute
	// member must use this — MemberRegistry lookup is tenant-scoped
	// and only the compute-test-client tenant has a registered member.
	// Mirrors parity.BackendFixture.ComputeTenant for the cluster case.
	ComputeTenant(t *testing.T) parity.Tenant
}
