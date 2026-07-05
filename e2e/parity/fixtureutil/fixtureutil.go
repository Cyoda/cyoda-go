// Package fixtureutil provides shared helpers for parity test backend
// fixtures: port picking, RSA key generation, JWT minting, binary building,
// subprocess lifecycle, and readiness probes.
package fixtureutil

import (
	"bufio"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/parity"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

// --- Binary building (sync.Once cached) ---

var (
	cyodaBuildOnce    sync.Once
	cyodaBinaryPath   string
	cyodaBuildErr     error
	computeBuildOnce  sync.Once
	computeBinaryPath string
	computeBuildErr   error
)

// BuildCyodaBinary builds the cyoda binary once per process and
// returns the path. Thread-safe via sync.Once.
func BuildCyodaBinary() (string, error) {
	moduleRoot := FindModuleRoot()
	cyodaBuildOnce.Do(func() {
		cyodaBinaryPath, cyodaBuildErr = buildBinary(moduleRoot, "./cmd/cyoda", "cyoda")
	})
	if cyodaBuildErr != nil {
		return "", fmt.Errorf("failed to build cyoda: %w", cyodaBuildErr)
	}
	return cyodaBinaryPath, nil
}

// BuildComputeBinary builds the compute-test-client binary once per
// process and returns the path. Thread-safe via sync.Once.
func BuildComputeBinary() (string, error) {
	moduleRoot := FindModuleRoot()
	computeBuildOnce.Do(func() {
		computeBinaryPath, computeBuildErr = buildBinary(moduleRoot, "./cmd/compute-test-client", "compute-test-client")
	})
	if computeBuildErr != nil {
		return "", fmt.Errorf("failed to build compute-test-client: %w", computeBuildErr)
	}
	return computeBinaryPath, nil
}

func buildBinary(moduleRoot, pkg, name string) (string, error) {
	tmpDir, err := os.MkdirTemp("", "parity-build-*")
	if err != nil {
		return "", err
	}
	outPath := filepath.Join(tmpDir, name)
	cmd := exec.Command("go", "build", "-o", outPath, pkg)
	cmd.Dir = moduleRoot
	cmd.Env = os.Environ()
	// Use the in-tree go.work when present so pre-release cross-module
	// development (e.g. feature branches that depend on an unpublished
	// sibling-module change) resolves against the local working copy.
	// Only force GOWORK=off when moduleRoot has no go.work — which is
	// the case for out-of-tree callers (cyoda-go-cassandra's e2e suite)
	// that resolve moduleRoot to cyoda-go's copy in the Go module cache.
	if _, statErr := os.Stat(filepath.Join(moduleRoot, "go.work")); os.IsNotExist(statErr) {
		cmd.Env = append(cmd.Env, "GOWORK=off")
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("go build %s: %w", pkg, err)
	}
	return outPath, nil
}

// --- RSA / JWT helpers ---

// JWTKeySet holds an RSA keypair, its KID, and issuer for JWT minting.
type JWTKeySet struct {
	Key    *rsa.PrivateKey
	Kid    string
	Issuer string
	KeyPEM string // PEM-encoded private key for passing to cyoda env
}

// GenerateJWTKeySet creates a fresh RSA key, derives the KID the same
// way cyoda-go does (SHA256 of DER public key, first 16 bytes hex),
// and returns the complete set.
func GenerateJWTKeySet() (*JWTKeySet, error) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("failed to generate RSA key: %w", err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&rsaKey.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal public key for KID: %w", err)
	}
	kidHash := sha256.Sum256(pubDER)
	kid := hex.EncodeToString(kidHash[:16])

	keyBytes, err := x509.MarshalPKCS8PrivateKey(rsaKey)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal key: %w", err)
	}
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes}))

	return &JWTKeySet{
		Key:    rsaKey,
		Kid:    kid,
		Issuer: "cyoda-test",
		KeyPEM: keyPEM,
	}, nil
}

// MintNonAdminTenantJWT creates a fresh tenant JWT with no ROLE_ADMIN scope.
// Use this to test endpoints that require ROLE_ADMIN — the request should
// be rejected with 403 FORBIDDEN. The returned tenant has the same shape as
// MintTenantJWT but carries only ROLE_M2M so that the request authenticates
// successfully while failing the admin authorization gate.
func MintNonAdminTenantJWT(t *testing.T, ks *JWTKeySet) parity.Tenant {
	t.Helper()

	tenantID := uuid.NewString()
	now := time.Now()

	claims := map[string]any{
		"sub":          "test-nonadmin-" + tenantID[:8],
		"iss":          ks.Issuer,
		"caas_user_id": "test-nonadmin-" + tenantID[:8],
		"caas_org_id":  tenantID,
		"scopes":       []string{"ROLE_M2M"},
		"caas_tier":    "unlimited",
		"exp":          now.Add(1 * time.Hour).Unix(),
		"iat":          now.Unix(),
		"jti":          uuid.NewString(),
	}

	token, err := auth.Sign(claims, ks.Key, ks.Kid)
	if err != nil {
		t.Fatalf("failed to mint non-admin tenant JWT: %v", err)
	}

	return parity.Tenant{
		ID:    tenantID,
		Token: token,
	}
}

// MintTenantJWT creates a fresh tenant JWT for use in parity tests.
func MintTenantJWT(t *testing.T, ks *JWTKeySet) parity.Tenant {
	t.Helper()

	tenantID := uuid.NewString()
	now := time.Now()

	claims := map[string]any{
		"sub":          "test-user-" + tenantID[:8],
		"iss":          ks.Issuer,
		"caas_user_id": "test-user-" + tenantID[:8],
		"caas_org_id":  tenantID,
		"scopes":       []string{"ROLE_ADMIN"},
		"caas_tier":    "unlimited",
		"exp":          now.Add(1 * time.Hour).Unix(),
		"iat":          now.Unix(),
		"jti":          uuid.NewString(),
	}

	token, err := auth.Sign(claims, ks.Key, ks.Kid)
	if err != nil {
		t.Fatalf("failed to mint tenant JWT: %v", err)
	}

	return parity.Tenant{
		ID:    tenantID,
		Token: token,
	}
}

// ComputeTenantID is the tenant under which the compute-test-client
// registers via its M2M JWT. Processor/criteria dispatch is tenant-scoped,
// so tests exercising gRPC dispatch must use this tenant for entity
// creation. Exported so fixtures can reference it without duplicating
// the string.
const ComputeTenantID = "system-tenant"

// MintM2MJWT creates an M2M JWT for the compute-test-client.
func MintM2MJWT(ks *JWTKeySet) (string, error) {
	now := time.Now()
	claims := map[string]any{
		"sub":          "compute-test",
		"iss":          ks.Issuer,
		"caas_user_id": "compute-admin",
		"caas_org_id":  ComputeTenantID,
		"scopes":       []string{"ROLE_ADMIN", "ROLE_M2M"},
		"caas_tier":    "unlimited",
		"exp":          now.Add(2 * time.Hour).Unix(),
		"iat":          now.Unix(),
		"jti":          uuid.NewString(),
	}
	return auth.Sign(claims, ks.Key, ks.Kid)
}

// MintComputeTenantJWT creates a regular (non-M2M) JWT whose tenant matches
// the compute-test-client's tenant. Tests that exercise gRPC processor/criteria
// dispatch use this instead of MintTenantJWT so the MemberRegistry finds
// the compute-test-client member.
func MintComputeTenantJWT(t *testing.T, ks *JWTKeySet) parity.Tenant {
	t.Helper()

	now := time.Now()
	claims := map[string]any{
		"sub":          "test-user-compute",
		"iss":          ks.Issuer,
		"caas_user_id": "test-user-compute",
		"caas_org_id":  ComputeTenantID,
		"scopes":       []string{"ROLE_ADMIN"},
		"caas_tier":    "unlimited",
		"exp":          now.Add(1 * time.Hour).Unix(),
		"iat":          now.Unix(),
		"jti":          uuid.NewString(),
	}

	token, err := auth.Sign(claims, ks.Key, ks.Kid)
	if err != nil {
		t.Fatalf("failed to mint compute tenant JWT: %v", err)
	}

	return parity.Tenant{
		ID:    ComputeTenantID,
		Token: token,
	}
}

// --- Port picking ---

// FreePort returns an available ephemeral TCP port on 127.0.0.1.
func FreePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port, nil
}

// --- Module root ---

// FindModuleRoot walks up from the caller's source file to find go.mod.
// Panics if no go.mod is found walking up to the filesystem root —
// silently falling back to cwd would hide the misconfiguration and
// surface as an opaque "no packages found" build error later.
func FindModuleRoot() string {
	_, thisFile, _, _ := runtime.Caller(0)
	dir := filepath.Dir(thisFile)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			panic(fmt.Sprintf("FindModuleRoot: no go.mod found walking up from %s", thisFile))
		}
		dir = parent
	}
}

// --- Subprocess lifecycle ---

// KillProcessGroup kills the process group of the given command and reaps it
// via cmd.Wait(). Use this on paths where the caller owns the single allowed
// Wait() for the process (single-node + compute launch).
func KillProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err == nil {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	} else {
		_ = cmd.Process.Kill()
	}
	_ = cmd.Wait()
}

// killProcessGroupNoWait sends SIGKILL to the command's process group (or the
// process itself if the pgid lookup fails) WITHOUT calling cmd.Wait().
//
// The cluster launch path spawns exactly one "monitor" goroutine per node that
// owns that node's single cmd.Wait() call; calling Wait() a second time here
// would be a concurrent double-Wait — a data race that the -race build would
// (correctly) flag. Callers on that path kill via this helper and then reap the
// process by waiting on the monitor's exit signal instead of calling Wait().
func killProcessGroupNoWait(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err == nil {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	} else {
		_ = cmd.Process.Kill()
	}
}

// retryLaunch runs fn up to attempts times, returning nil on the first success.
// If every attempt fails it returns the error from the final attempt. attempts
// < 1 is treated as a single attempt. Each failed-then-retried attempt is
// logged so a transient port collision self-healing across fresh-port retries
// is visible in test output.
func retryLaunch(attempts int, fn func() error) error {
	if attempts < 1 {
		attempts = 1
	}
	var err error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err = fn(); err == nil {
			return nil
		}
		if attempt < attempts {
			slog.Info("cluster launch attempt failed; retrying with fresh ports",
				"pkg", "fixtureutil", "attempt", attempt, "maxAttempts", attempts, "err", err)
		}
	}
	return err
}

// nodeOutcome decides a single cluster node's readiness by racing its health
// probe result against the node process exiting.
//
//   - healthDoneCh delivers the health probe's result (nil == healthy,
//     non-nil == unhealthy or timed out). It is surfaced verbatim.
//   - exitedCh is closed by the node's monitor goroutine once cmd.Wait()
//     returns. A close observed before a health result means the process died
//     before it could become healthy — always a failure, so we fail fast
//     instead of blocking on a health probe that can never succeed.
//   - exitErrFn supplies the cmd.Wait() error for the exit branch. It is read
//     only after exitedCh's close is observed, which happens-after the monitor
//     stored the error — so the read is race-free without extra locking. May be
//     nil.
func nodeOutcome(nodeIdx int, healthDoneCh <-chan error, exitedCh <-chan struct{}, exitErrFn func() error) error {
	select {
	case herr := <-healthDoneCh:
		return herr
	case <-exitedCh:
		if exitErrFn != nil {
			if we := exitErrFn(); we != nil {
				return fmt.Errorf("cyoda node %d exited before becoming healthy: %w", nodeIdx, we)
			}
		}
		return fmt.Errorf("cyoda node %d exited before becoming healthy", nodeIdx)
	}
}

// --- Readiness probes ---

// WaitForHTTPHealth polls the given URL until it returns 200 OK or the
// timeout elapses.
func WaitForHTTPHealth(url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("health check %s did not return 200 within %v", url, timeout)
}

// ParseHealthAddr reads from r until it finds a line starting with
// "HEALTH_ADDR=" and returns the address, or times out.
func ParseHealthAddr(r io.Reader, timeout time.Duration) (string, error) {
	type result struct {
		addr string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "HEALTH_ADDR=") {
				ch <- result{addr: strings.TrimPrefix(line, "HEALTH_ADDR=")}
				return
			}
		}
		if err := scanner.Err(); err != nil {
			ch <- result{err: fmt.Errorf("scanner error: %w", err)}
		} else {
			ch <- result{err: fmt.Errorf("stdout closed without HEALTH_ADDR line")}
		}
	}()

	select {
	case res := <-ch:
		return res.addr, res.err
	case <-time.After(timeout):
		return "", fmt.Errorf("timed out waiting for HEALTH_ADDR after %v", timeout)
	}
}

// --- Cyoda + Compute launch ---

// CyodaEnv returns the base environment variables needed by every
// cyoda-go fixture. Callers append backend-specific vars (e.g.
// CYODA_POSTGRES_URL for postgres).
//
// OIDC network-level overrides are set here for test isolation:
//   - CYODA_OIDC_REQUIRE_HTTPS=false — parity tests register providers
//     with http:// URIs (fake hostnames) so no external TLS is needed.
//   - CYODA_OIDC_ALLOW_PRIVATE_NETWORKS=true — skips DNS-based SSRF
//     checks so tests can use arbitrary hostnames without network I/O.
func CyodaEnv(httpPort, grpcPort int, ks *JWTKeySet) []string {
	return append(os.Environ(),
		fmt.Sprintf("CYODA_HTTP_PORT=%d", httpPort),
		fmt.Sprintf("CYODA_GRPC_PORT=%d", grpcPort),
		"CYODA_CONTEXT_PATH=/api",
		"CYODA_IAM_MODE=jwt",
		fmt.Sprintf("CYODA_JWT_SIGNING_KEY=%s", ks.KeyPEM),
		fmt.Sprintf("CYODA_JWT_ISSUER=%s", ks.Issuer),
		"CYODA_LOG_LEVEL=info",
		"CYODA_BOOTSTRAP_CLIENT_ID=compute-test",
		"CYODA_BOOTSTRAP_CLIENT_SECRET=compute-secret",
		"CYODA_BOOTSTRAP_TENANT_ID=system-tenant",
		"CYODA_BOOTSTRAP_USER_ID=compute-admin",
		"CYODA_BOOTSTRAP_ROLES=ROLE_ADMIN,ROLE_M2M",
		// OIDC test overrides — allow http:// and skip SSRF DNS checks.
		"CYODA_OIDC_REQUIRE_HTTPS=false",
		"CYODA_OIDC_ALLOW_PRIVATE_NETWORKS=true",
	)
}

// LaunchResult holds the state from launching cyoda + compute.
type LaunchResult struct {
	BaseURL      string
	GRPCEndpoint string
	CyodaCmd     *exec.Cmd
	ComputeCmd   *exec.Cmd
}

// LaunchOpts configures optional behavior for LaunchCyodaAndCompute.
type LaunchOpts struct {
	// ReadinessTimeout overrides the default health-check timeout for
	// cyoda-go. Defaults to 30s if zero.
	ReadinessTimeout time.Duration
}

// LaunchCyodaAndCompute builds the stock cyoda-go binary and the
// compute-test-client from this module, starts both, waits for
// readiness, and returns the fixture. Use this from in-tree parity
// tests. For out-of-tree consumers (e.g. cyoda-go-cassandra's full
// binary) that need to inject their own pre-built cyoda binary, use
// LaunchCyodaAndComputeWithBinaries.
//
// extraEnv is appended to the cyoda environment (for backend-specific vars).
func LaunchCyodaAndCompute(ks *JWTKeySet, extraEnv []string, opts ...LaunchOpts) (*LaunchResult, func(), error) {
	cyodaBin, err := BuildCyodaBinary()
	if err != nil {
		return nil, nil, err
	}
	computeBin, err := BuildComputeBinary()
	if err != nil {
		return nil, nil, err
	}
	return LaunchCyodaAndComputeWithBinaries(cyodaBin, computeBin, ks, extraEnv, opts...)
}

// LaunchCyodaAndComputeWithBinaries is the binary-path-explicit variant
// of LaunchCyodaAndCompute. Callers that need to inject their own
// cyoda binary — typically a downstream binary that blank-imports
// additional plugins (cassandra, etc.) — build it separately and pass
// the path here.
//
// cyodaBin and computeBin must be absolute paths to already-built
// executables. The env for cyoda is assembled via CyodaEnv plus
// extraEnv; the env for compute-test-client carries the gRPC
// endpoint and an M2M token minted from ks.
func LaunchCyodaAndComputeWithBinaries(cyodaBin, computeBin string, ks *JWTKeySet, extraEnv []string, opts ...LaunchOpts) (*LaunchResult, func(), error) {
	httpPort, err := FreePort()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get HTTP port: %w", err)
	}
	grpcPort, err := FreePort()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get gRPC port: %w", err)
	}
	// Admin port must be picked too — the default (9091) is fixed, so
	// parity packages running in parallel (memory, postgres, sqlite, …)
	// collide on a single host and one subprocess logs "bind: address
	// already in use" while the others succeed. Isolating the admin port
	// per fixture mirrors HTTP/gRPC isolation.
	adminPort, err := FreePort()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get admin port: %w", err)
	}

	var opt LaunchOpts
	if len(opts) > 0 {
		opt = opts[0]
	}
	cyodaReadinessTimeout := opt.ReadinessTimeout
	if cyodaReadinessTimeout == 0 {
		cyodaReadinessTimeout = defaultCyodaReadinessTimeout
	}

	// Launch cyoda-go. Subprocess stdout/stderr flow to the test runner's
	// stderr so go test -v surfaces the binary's log output — critical
	// for diagnosing failures (5xx responses, startup panics, etc.).
	// Without this, failures report only the HTTP error code with no
	// server-side context.
	cyodaCmd := exec.Command(cyodaBin)
	cyodaCmd.WaitDelay = 3 * time.Second
	cyodaCmd.Env = append(CyodaEnv(httpPort, grpcPort, ks), extraEnv...)
	cyodaCmd.Env = append(cyodaCmd.Env, fmt.Sprintf("CYODA_ADMIN_PORT=%d", adminPort))
	cyodaCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cyodaCmd.Stdout = os.Stderr
	cyodaCmd.Stderr = os.Stderr

	if err := cyodaCmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("failed to start cyoda-go: %w", err)
	}
	cleanup := func() {
		KillProcessGroup(cyodaCmd)
	}

	// Wait for cyoda health.
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", httpPort)
	if err := WaitForHTTPHealth(baseURL+"/api/health", cyodaReadinessTimeout); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("cyoda health probe failed: %w", err)
	}
	slog.Info("cyoda-go is ready", "pkg", "fixtureutil", "baseURL", baseURL)

	// Mint M2M JWT for compute client.
	m2mToken, err := MintM2MJWT(ks)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("failed to mint M2M JWT: %w", err)
	}

	// Launch compute-test-client.
	grpcEndpoint := fmt.Sprintf("127.0.0.1:%d", grpcPort)
	computeCmd := exec.Command(computeBin)
	computeCmd.WaitDelay = 3 * time.Second
	computeCmd.Env = append(os.Environ(),
		fmt.Sprintf("CYODA_COMPUTE_GRPC_ENDPOINT=%s", grpcEndpoint),
		fmt.Sprintf("CYODA_COMPUTE_TOKEN=%s", m2mToken),
		// HTTP base for feature #287 callback-join processors (callbacks target
		// the same single node that dispatched them).
		fmt.Sprintf("CYODA_COMPUTE_HTTP_BASE=%s", baseURL),
	)
	computeCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	computeStdout, err := computeCmd.StdoutPipe()
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("failed to create compute stdout pipe: %w", err)
	}
	if err := computeCmd.Start(); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("failed to start compute-test-client: %w", err)
	}
	cleanup = func() {
		KillProcessGroup(computeCmd)
		KillProcessGroup(cyodaCmd)
	}

	// Parse HEALTH_ADDR from stdout.
	healthAddr, err := ParseHealthAddr(computeStdout, 15*time.Second)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("failed to parse HEALTH_ADDR from compute-test-client: %w", err)
	}
	go func() { _, _ = io.Copy(io.Discard, computeStdout) }()

	// Wait for compute-test-client health.
	computeHealthURL := fmt.Sprintf("http://%s/healthz", healthAddr)
	if err := WaitForHTTPHealth(computeHealthURL, 30*time.Second); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("compute-test-client health probe failed: %w", err)
	}
	slog.Info("compute-test-client is ready", "pkg", "fixtureutil", "healthAddr", healthAddr)

	return &LaunchResult{
		BaseURL:      baseURL,
		GRPCEndpoint: grpcEndpoint,
		CyodaCmd:     cyodaCmd,
		ComputeCmd:   computeCmd,
	}, cleanup, nil
}

// --- Multi-node cluster launch ---

// ClusterLaunchResult holds the state from launching N cyoda-go
// subprocesses sharing one backing storage, plus one shared
// compute-test-client connected to node 0.
type ClusterLaunchResult struct {
	// BaseURLs is the per-node HTTP base URL list, in stable order
	// (index i corresponds to node-{i}).
	BaseURLs []string
	// GRPCEndpoint is node 0's gRPC endpoint; the compute-test-client
	// connects here.
	GRPCEndpoint string
	// CyodaCmds holds one *exec.Cmd per node, in the same order as
	// BaseURLs. Exposed mainly for diagnostics; cleanup handles
	// process termination.
	CyodaCmds []*exec.Cmd
	// ComputeCmd is the single compute-test-client subprocess.
	ComputeCmd *exec.Cmd
}

// defaultCyodaReadinessTimeout is the default time to wait for each
// cyoda-go node to pass its /api/health check. Sized to accommodate
// race-detector instrumentation overhead (2-10x slower) on CI runners
// under load: the prior 30s value flaked on the multinode launch path
// when node 2 missed its probe window. Single-node runs in the same
// suite settle in well under 30s; the headroom is for race-instrumented
// runs of TestMultiNode and friends.
const defaultCyodaReadinessTimeout = 120 * time.Second

// defaultComputeHealthAddrTimeout is the default time to wait for the
// compute-test-client to print its HEALTH_ADDR line on stdout.
const defaultComputeHealthAddrTimeout = 15 * time.Second

// gossipSettleDelay is a brief pause after all nodes are healthy to
// allow gossip membership views to fully converge before the
// compute-test-client and test driver start hitting the cluster.
const gossipSettleDelay = 1 * time.Second

// clusterLaunchAttempts bounds how many times the node-launch phase (allocate
// n×4 ports → start n nodes → wait all healthy) is retried with freshly
// allocated ports. FreePort() has an unavoidable TOCTOU window — it closes the
// probe listener and returns the number, and the cyoda child binds it only
// later, so a concurrently-running test process can steal the port in between
// and the node dies with "bind: address already in use". A collision is
// transient; retrying with a fresh set of ports makes a run-ending failure
// astronomically unlikely (a fresh independent collision would have to recur
// on every attempt).
const clusterLaunchAttempts = 3

// LaunchCyodaClusterAndCompute builds the stock cyoda + compute
// binaries and launches n cyoda-go subprocesses sharing the supplied
// backing storage (carried in extraEnv). Use this from in-tree parity
// tests. For out-of-tree consumers (e.g. cyoda-go-cassandra's full
// binary) that need to inject their own pre-built cyoda binary, use
// LaunchCyodaClusterAndComputeWithBinaries.
//
// Cluster bootstrap envs (CYODA_CLUSTER_ENABLED, CYODA_NODE_ID,
// CYODA_NODE_ADDR, CYODA_GOSSIP_ADDR, CYODA_SEED_NODES,
// CYODA_HMAC_SECRET) are added per node by this function — callers
// MUST NOT supply them. extraEnv is for backend wiring only (e.g.
// CYODA_STORAGE_BACKEND=postgres, CYODA_POSTGRES_URL=...).
//
// Allocates n × 4 free ports (HTTP, gRPC, gossip, admin) for per-node
// isolation.
//
// The compute-test-client connects to node 0's gRPC. The returned
// cleanup function kills all subprocesses; the caller is responsible
// for any external resource (e.g. the postgres testcontainer).
func LaunchCyodaClusterAndCompute(ks *JWTKeySet, n int, extraEnv []string, opts ...LaunchOpts) (*ClusterLaunchResult, func(), error) {
	cyodaBin, err := BuildCyodaBinary()
	if err != nil {
		return nil, nil, err
	}
	computeBin, err := BuildComputeBinary()
	if err != nil {
		return nil, nil, err
	}
	return LaunchCyodaClusterAndComputeWithBinaries(cyodaBin, computeBin, ks, n, extraEnv, opts...)
}

// LaunchCyodaClusterAndComputeWithBinaries is the binary-path-explicit
// variant of LaunchCyodaClusterAndCompute. Out-of-tree consumers
// maintaining their own backend plugin (e.g. cyoda-go-cassandra) build
// a cmd/cyoda-go binary that blank-imports their plugin, then drive the
// shared parity scenario suite against that binary by passing its path
// here. Issue #157 — symmetric to LaunchCyodaAndComputeWithBinaries.
//
// cyodaBin and computeBin must be absolute paths to already-built
// executables. All cluster-bootstrap logic (port allocation, gossip
// seed CSV, HMAC derivation, health probing, compute-client wiring to
// node 0) is plugin-agnostic and lives here.
func LaunchCyodaClusterAndComputeWithBinaries(cyodaBin, computeBin string, ks *JWTKeySet, n int, extraEnv []string, opts ...LaunchOpts) (*ClusterLaunchResult, func(), error) {
	if n < 1 {
		return nil, nil, fmt.Errorf("LaunchCyodaClusterAndComputeWithBinaries: n must be >= 1, got %d", n)
	}

	var err error
	var opt LaunchOpts
	if len(opts) > 0 {
		opt = opts[0]
	}
	cyodaReadinessTimeout := opt.ReadinessTimeout
	if cyodaReadinessTimeout == 0 {
		cyodaReadinessTimeout = defaultCyodaReadinessTimeout
	}

	// Per-node process handle plus the monitor's single-owner exit signal.
	// Exactly one monitor goroutine per node calls cmd.Wait() and, when it
	// returns, stores the result in exitErr and closes exitedCh. Reading
	// exitErr only after observing exitedCh's close is race-free (the close
	// happens-after the store). No other code calls Wait() on that cmd — the
	// teardown paths kill via killProcessGroupNoWait and reap by waiting on
	// exitedCh, which avoids a concurrent double-Wait data race.
	type clusterNode struct {
		cmd      *exec.Cmd
		exitErr  error
		exitedCh chan struct{}
	}

	// killNodes SIGKILLs each launched node and reaps it by waiting on its
	// monitor's exit signal. Safe to call repeatedly and on nil entries.
	killNodes := func(nodes []*clusterNode) {
		for _, nd := range nodes {
			if nd == nil || nd.cmd == nil {
				continue
			}
			killProcessGroupNoWait(nd.cmd)
			if nd.exitedCh != nil {
				<-nd.exitedCh // reap: block until the monitor's Wait() returns
			}
		}
	}

	// nodes holds the successfully-launched, healthy cluster nodes once the
	// retry loop below succeeds. It stays populated for the returned cleanup.
	var nodes []*clusterNode
	var httpPorts, grpcPorts []int

	// Retry the whole node-launch phase (fresh ports each time) to self-heal
	// a transient FreePort() TOCTOU port collision — see clusterLaunchAttempts.
	launchErr := retryLaunch(clusterLaunchAttempts, func() error {
		// Allocate n ports for each of HTTP, gRPC, gossip, admin — fresh on
		// every attempt so a collided port is not reused.
		hPorts := make([]int, n)
		gPorts := make([]int, n)
		gossipPorts := make([]int, n)
		adminPorts := make([]int, n)
		for i := 0; i < n; i++ {
			var e error
			if hPorts[i], e = FreePort(); e != nil {
				return fmt.Errorf("failed to get HTTP port for node %d: %w", i, e)
			}
			if gPorts[i], e = FreePort(); e != nil {
				return fmt.Errorf("failed to get gRPC port for node %d: %w", i, e)
			}
			if gossipPorts[i], e = FreePort(); e != nil {
				return fmt.Errorf("failed to get gossip port for node %d: %w", i, e)
			}
			if adminPorts[i], e = FreePort(); e != nil {
				return fmt.Errorf("failed to get admin port for node %d: %w", i, e)
			}
		}

		// Build the seed-nodes CSV: host:port for every node.
		seedAddrs := make([]string, n)
		for i := 0; i < n; i++ {
			seedAddrs[i] = fmt.Sprintf("127.0.0.1:%d", gossipPorts[i])
		}
		seedNodes := strings.Join(seedAddrs, ",")

		// HMAC secret shared across the cluster. Random per attempt so
		// concurrent test packages cannot accidentally talk to each other.
		hmacBytes := make([]byte, 32)
		if _, e := rand.Read(hmacBytes); e != nil {
			return fmt.Errorf("failed to generate HMAC secret: %w", e)
		}
		hmacSecret := hex.EncodeToString(hmacBytes)

		attemptNodes := make([]*clusterNode, n)

		// Concurrent start, then concurrent health-wait. Cluster registration
		// blocks until at least one seed is reachable; if we started node 0 in
		// isolation it would deadlock waiting on nodes 1..n-1 that haven't been
		// launched yet. Migration concurrency is safe — golang-migrate uses a
		// database-level lock on the schema_migrations table.
		for i := 0; i < n; i++ {
			cmd := exec.Command(cyodaBin)
			cmd.WaitDelay = 3 * time.Second
			env := append(CyodaEnv(hPorts[i], gPorts[i], ks), extraEnv...)
			env = append(env,
				fmt.Sprintf("CYODA_ADMIN_PORT=%d", adminPorts[i]),
				"CYODA_CLUSTER_ENABLED=true",
				fmt.Sprintf("CYODA_NODE_ID=node-%d", i),
				fmt.Sprintf("CYODA_NODE_ADDR=http://127.0.0.1:%d", hPorts[i]),
				fmt.Sprintf("CYODA_GOSSIP_ADDR=127.0.0.1:%d", gossipPorts[i]),
				fmt.Sprintf("CYODA_SEED_NODES=%s", seedNodes),
				fmt.Sprintf("CYODA_HMAC_SECRET=%s", hmacSecret),
				// Advertise each node's real gRPC endpoint so cross-node EntityManage
				// forwarding (tx-token callbacks landing on a non-owner node) resolves
				// the owner's gRPC port directly instead of deriving it from the
				// forwarding node's own port — the fixture assigns a distinct gRPC port
				// per node, so the uniform-deployment derive fallback would misroute.
				fmt.Sprintf("CYODA_GRPC_NODE_ADDR=127.0.0.1:%d", gPorts[i]),
				// Test-only: every node runs on 127.0.0.1, so the dispatch HTTP
				// forwarder must be allowed to target loopback peers to exercise
				// forwarded processor/criteria dispatch (A→B) between nodes.
				"CYODA_DISPATCH_ALLOW_LOOPBACK_FOR_TESTING=true",
			)
			cmd.Env = env
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			cmd.Stdout = os.Stderr
			cmd.Stderr = os.Stderr
			if e := cmd.Start(); e != nil {
				killNodes(attemptNodes)
				return fmt.Errorf("failed to start cyoda-go node %d: %w", i, e)
			}
			nd := &clusterNode{cmd: cmd, exitedCh: make(chan struct{})}
			attemptNodes[i] = nd
			// The single owner of this process's cmd.Wait(). Publishes the
			// result via exitErr + close(exitedCh) so both the health race and
			// teardown can observe the exit without a second Wait() call.
			go func(nd *clusterNode) {
				nd.exitErr = nd.cmd.Wait()
				close(nd.exitedCh)
			}(nd)
		}

		// Concurrent health probe — every node must come ready within the
		// readiness timeout OR its process must be seen to exit. A node that
		// dies from a failed bind fails fast instead of hanging the full
		// timeout. First failure is reported; all nodes torn down on failure.
		healthErrCh := make(chan error, n)
		for i := 0; i < n; i++ {
			go func(idx int) {
				nd := attemptNodes[idx]
				baseURL := fmt.Sprintf("http://127.0.0.1:%d", hPorts[idx])
				healthDoneCh := make(chan error, 1)
				go func() {
					healthDoneCh <- WaitForHTTPHealth(baseURL+"/api/health", cyodaReadinessTimeout)
				}()
				if e := nodeOutcome(idx, healthDoneCh, nd.exitedCh, func() error { return nd.exitErr }); e != nil {
					healthErrCh <- e
					return
				}
				slog.Info("cyoda-go cluster node ready", "pkg", "fixtureutil", "node", idx, "baseURL", baseURL)
				healthErrCh <- nil
			}(i)
		}
		var firstHealthErr error
		for i := 0; i < n; i++ {
			if e := <-healthErrCh; e != nil && firstHealthErr == nil {
				firstHealthErr = e
			}
		}
		if firstHealthErr != nil {
			killNodes(attemptNodes)
			return firstHealthErr
		}

		// Attempt succeeded — publish the healthy cluster for use below and in
		// the returned cleanup.
		nodes = attemptNodes
		httpPorts = hPorts
		grpcPorts = gPorts
		return nil
	})
	if launchErr != nil {
		return nil, nil, launchErr
	}

	// Cmd slice for the returned cluster info; teardown uses killNodes(nodes), not this.
	cyodaCmds := make([]*exec.Cmd, n)
	for i, nd := range nodes {
		cyodaCmds[i] = nd.cmd
	}
	// cleanup for the node phase; compute wiring below replaces it with a
	// variant that also tears down the compute-test-client.
	cleanup := func() {
		killNodes(nodes)
	}

	// Brief settle for gossip convergence so the seed-nodes list is
	// fully populated before the compute-test-client (and any test
	// driver) starts hitting the cluster. Default StabilityWindow is
	// 2s server-side; a short conservative pause here is still useful
	// for the membership view to propagate after the last node joins.
	time.Sleep(gossipSettleDelay)

	// Mint M2M JWT for the compute client.
	m2mToken, err := MintM2MJWT(ks)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("failed to mint M2M JWT: %w", err)
	}

	// Compute-test-client points at node 0's gRPC.
	grpcEndpoint := fmt.Sprintf("127.0.0.1:%d", grpcPorts[0])
	computeCmd := exec.Command(computeBin)
	computeCmd.WaitDelay = 3 * time.Second
	computeCmd.Env = append(os.Environ(),
		fmt.Sprintf("CYODA_COMPUTE_GRPC_ENDPOINT=%s", grpcEndpoint),
		fmt.Sprintf("CYODA_COMPUTE_TOKEN=%s", m2mToken),
		// HTTP base for feature #287 callback-join processors. Callbacks target
		// node 0 (where the compute client connects and dispatch originates);
		// cross-node callback forwarding is covered separately, not here.
		fmt.Sprintf("CYODA_COMPUTE_HTTP_BASE=http://127.0.0.1:%d", httpPorts[0]),
	)
	computeCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	computeStdout, err := computeCmd.StdoutPipe()
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("failed to create compute stdout pipe: %w", err)
	}
	if err := computeCmd.Start(); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("failed to start compute-test-client: %w", err)
	}
	cleanup = func() {
		// computeCmd has no monitor goroutine, so KillProcessGroup (which owns
		// its Wait) is correct. The cyoda nodes are reaped by their monitor
		// goroutines, so they must be torn down kill-only + exit-signal wait —
		// never KillProcessGroup, which would double-Wait.
		KillProcessGroup(computeCmd)
		killNodes(nodes)
	}

	healthAddr, err := ParseHealthAddr(computeStdout, defaultComputeHealthAddrTimeout)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("failed to parse HEALTH_ADDR from compute-test-client: %w", err)
	}
	go func() { _, _ = io.Copy(io.Discard, computeStdout) }()

	computeHealthURL := fmt.Sprintf("http://%s/healthz", healthAddr)
	if err := WaitForHTTPHealth(computeHealthURL, defaultCyodaReadinessTimeout); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("compute-test-client health probe failed: %w", err)
	}
	slog.Info("compute-test-client (cluster) is ready", "pkg", "fixtureutil", "healthAddr", healthAddr, "nodes", n)

	baseURLs := make([]string, n)
	for i := 0; i < n; i++ {
		baseURLs[i] = fmt.Sprintf("http://127.0.0.1:%d", httpPorts[i])
	}

	return &ClusterLaunchResult{
		BaseURLs:     baseURLs,
		GRPCEndpoint: grpcEndpoint,
		CyodaCmds:    cyodaCmds,
		ComputeCmd:   computeCmd,
	}, cleanup, nil
}
