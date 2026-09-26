// Multi-node fixture: launches one Postgres testcontainer plus N
// cyoda-go subprocesses sharing it, with cluster bootstrap envs and
// one shared compute-test-client. Lives in e2e/parity/postgres (the
// test fixture package), NOT plugins/postgres (the storage backend
// submodule, which has its own go.mod and cannot import e2e/parity).
package postgres

import (
	"context"
	"fmt"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/client"
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
	nodeLogs []*fixtureutil.SyncBuffer
	// killNode SIGKILLs node i (see fixtureutil.ClusterLaunchResult.KillNode).
	// Exposed via the KillNode method, off the shared MultiNodeFixture
	// interface — a crash test type-asserts for it.
	killNode func(i int)
	// signalNode and awaitNodeExit back SignalNode and AwaitNodeExit (see
	// fixtureutil.ClusterLaunchResult); off the shared interface, like killNode.
	signalNode    func(i int, sig syscall.Signal) error
	awaitNodeExit func(i int, within time.Duration) error
	// containerID is the PostgreSQL container, for PauseDatabase.
	containerID string
	// connStr is the shared Postgres connection string every node uses. Exposed
	// via the ConnString method so a crash test can open its own read-only pgx
	// handle and assert on persisted job state (e.g. claim epoch), which is
	// invisible at the HTTP data plane. Off the shared interface for the same
	// reason as KillNode/NodeLogs.
	connStr string
	// computeBin and grpcEndpoints back the optional ComputeClientCapable
	// capability: a further compute client attached to a chosen pnode.
	computeBin    string
	grpcEndpoints []string
}

var _ multinode.ComputeClientCapable = (*pgMultiNode)(nil)

// StartComputeClient implements multinode.ComputeClientCapable.
func (f *pgMultiNode) StartComputeClient(t *testing.T, node int, spec parity.ComputeClientSpec) parity.ComputeClient {
	t.Helper()
	if node < 0 || node >= len(f.baseURLs) || node >= len(f.grpcEndpoints) {
		t.Fatalf("StartComputeClient: pnode %d out of range (cluster has %d)", node, len(f.baseURLs))
	}
	return fixtureutil.StartComputeClientForFixture(t, f.keySet, f.computeBin, f.grpcEndpoints[node], f.baseURLs[node], spec)
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

// NodeLogs returns node idx's captured combined stdout+stderr as a string
// snapshot. Its callers are Incarnation, which reads the scheduler's start
// line from it, and TestMultiNodeFixture_ControlSurface
// (multinode_control_test.go), which proves a node with the scheduler off
// announces no incarnation. Returns "" for an out-of-range index. Not part of
// the MultiNodeFixture interface.
func (f *pgMultiNode) NodeLogs(idx int) string {
	if idx < 0 || idx >= len(f.nodeLogs) || f.nodeLogs[idx] == nil {
		return ""
	}
	return f.nodeLogs[idx].String()
}

// KillNode SIGKILLs node i's process group and reaps it. Part of the optional
// crash-testing capability — NOT on the shared MultiNodeFixture interface (a
// crash test type-asserts for it, like ComputeUser), since the crash
// scenario is postgres-first and the shared scenario registry must not gain a
// kill. Killing is permanent for the fixture's life; the node is not restarted.
func (f *pgMultiNode) KillNode(i int) {
	if f.killNode == nil {
		return
	}
	f.killNode(i)
}

// SignalNode sends sig to node i (SIGTERM, SIGSTOP, SIGCONT). Off the shared
// interface, like KillNode: a scheduler scenario type-asserts for it.
func (f *pgMultiNode) SignalNode(i int, sig syscall.Signal) error { return f.signalNode(i, sig) }

// AwaitNodeExit waits up to within for node i to exit.
func (f *pgMultiNode) AwaitNodeExit(i int, within time.Duration) error {
	return f.awaitNodeExit(i, within)
}

// PauseDatabase freezes the PostgreSQL container: every pnode's statements
// hang until UnpauseDatabase, as in a network partition from the database.
func (f *pgMultiNode) PauseDatabase(t *testing.T) {
	t.Helper()
	dc, err := testcontainers.NewDockerClientWithOpts(context.Background())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer dc.Close()
	if _, err := dc.ContainerPause(context.Background(), f.containerID, client.ContainerPauseOptions{}); err != nil {
		t.Fatalf("pause the database container: %v", err)
	}
}

// UnpauseDatabase resumes the PostgreSQL container.
func (f *pgMultiNode) UnpauseDatabase(t *testing.T) {
	t.Helper()
	dc, err := testcontainers.NewDockerClientWithOpts(context.Background())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer dc.Close()
	if _, err := dc.ContainerUnpause(context.Background(), f.containerID, client.ContainerUnpauseOptions{}); err != nil {
		t.Fatalf("unpause the database container: %v", err)
	}
}

// Incarnation returns the scheduler incarnation node i announced at start,
// polling its captured log for up to 10s.
func (f *pgMultiNode) Incarnation(t *testing.T, i int) uuid.UUID {
	t.Helper()
	id, err := fixtureutil.AwaitIncarnation(func() string { return f.NodeLogs(i) }, 10*time.Second)
	if err != nil {
		t.Fatalf("node %d: %v", i, err)
	}
	return id
}

// ConnString returns the shared Postgres connection string every node uses, so
// a crash test can open its own read-only pgx handle and assert on persisted
// job state. Not part of the MultiNodeFixture interface — a crash test
// type-asserts for it. Never log the returned value (it carries credentials).
func (f *pgMultiNode) ConnString() string { return f.connStr }

// MustSetupMultiNode boots a Postgres testcontainer plus n cyoda-go
// subprocesses sharing it (with cluster bootstrap) and returns a
// MultiNodeFixture plus a cleanup function. Caller MUST defer cleanup
// immediately. Fails the test on any setup error, ensuring partial
// state is torn down before fataling.
//
// It is the default-cadence variant: it delegates to
// MustSetupMultiNodeWithEnv with only the standard postgres backend env, so
// the shared multinode scenarios keep the server's default search-job
// heartbeat/stale/reap cadences.
func MustSetupMultiNode(t *testing.T, n int) (multinode.MultiNodeFixture, func()) {
	t.Helper()
	return MustSetupMultiNodeWithEnv(t, n, nil)
}

// MustSetupMultiNodeWithEnv is MustSetupMultiNode with extra per-node
// environment appended after the standard postgres backend env. A crash test
// uses it to shorten the search-job heartbeat/stale/reap cadences so an
// orphaned job is reclaimed by a survivor within the test's budget. extraEnv
// entries are "KEY=value" strings; nil means the default env only.
func MustSetupMultiNodeWithEnv(t *testing.T, n int, extraEnv []string) (multinode.MultiNodeFixture, func()) {
	t.Helper()
	return MustSetupMultiNodeWithOpts(t, n, extraEnv, fixtureutil.LaunchOpts{})
}

// MustSetupMultiNodeWithOpts is MustSetupMultiNodeWithEnv with launch options
// for the cluster — e.g. LaunchOpts.NodeEnv, environment for one node only.
func MustSetupMultiNodeWithOpts(t *testing.T, n int, extraEnv []string, launch fixtureutil.LaunchOpts) (multinode.MultiNodeFixture, func()) {
	t.Helper()
	ctx := context.Background()

	// 1. Start PostgreSQL container.
	opts := append([]testcontainers.ContainerCustomizer{
		tcpostgres.WithDatabase("cyoda_parity_multinode"),
		tcpostgres.WithUsername("testuser"),
		tcpostgres.WithPassword("testpass"),
	}, testpg.HardenedOptions()...)
	// Up to six pnodes, each with a main pool (25), a scheduler pool (10) and
	// a heartbeat connection (1), plus the test's own reader: above the
	// default of 100.
	opts = append(opts, testcontainers.WithCmdArgs("-c", "max_connections=400"))
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
	//    extraEnv comes last so a caller can override any of the above,
	//    including the tuned cluster patience.
	launchEnv := append([]string{
		"CYODA_STORAGE_BACKEND=postgres",
		fmt.Sprintf("CYODA_POSTGRES_URL=%s", connStr),
		"CYODA_POSTGRES_AUTO_MIGRATE=true",
	}, fixtureutil.TunedClusterEnv()...)
	launchEnv = append(launchEnv, extraEnv...)
	result, processCleanup, err := fixtureutil.LaunchCyodaClusterAndCompute(ks, n, launchEnv, launch)
	if err != nil {
		containerCleanup()
		t.Fatalf("failed to launch cyoda-go cluster: %v", err)
	}

	cleanup := func() {
		processCleanup()
		containerCleanup()
	}

	return &pgMultiNode{
		baseURLs:      result.BaseURLs,
		keySet:        ks,
		nodeLogs:      result.NodeLogs,
		killNode:      result.KillNode,
		signalNode:    result.SignalNode,
		awaitNodeExit: result.AwaitNodeExit,
		containerID:   pgContainer.GetContainerID(),
		connStr:       connStr,
		computeBin:    result.ComputeBin,
		grpcEndpoints: result.GRPCEndpoints,
	}, cleanup
}
