// Package fixtureutil provides shared helpers for parity test backend
// fixtures: port picking, RSA key generation, JWT minting, binary building,
// subprocess lifecycle, and readiness probes.
package fixtureutil

import (
	"bufio"
	"context"
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

	token, err := auth.Sign(context.Background(), claims, auth.NewRSASigner(ks.Key), ks.Kid)
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

	token, err := auth.Sign(context.Background(), claims, auth.NewRSASigner(ks.Key), ks.Kid)
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

// MintM2MJWT creates the M2M JWT of the fixture's own compute-test-client.
func MintM2MJWT(ks *JWTKeySet) (string, error) { return MintM2MJWTForTenant(ks, ComputeTenantID) }

// MintM2MJWTForTenant creates an M2M JWT under which a compute-test-client
// joins as a compute node of tenantID.
func MintM2MJWTForTenant(ks *JWTKeySet, tenantID string) (string, error) {
	now := time.Now()
	claims := map[string]any{
		"sub":          "compute-test",
		"iss":          ks.Issuer,
		"caas_user_id": "compute-admin",
		"caas_org_id":  tenantID,
		"scopes":       []string{"ROLE_ADMIN", "ROLE_M2M"},
		"caas_tier":    "unlimited",
		"exp":          now.Add(2 * time.Hour).Unix(),
		"iat":          now.Unix(),
		"jti":          uuid.NewString(),
	}
	return auth.Sign(context.Background(), claims, auth.NewRSASigner(ks.Key), ks.Kid)
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

	token, err := auth.Sign(context.Background(), claims, auth.NewRSASigner(ks.Key), ks.Kid)
	if err != nil {
		t.Fatalf("failed to mint compute tenant JWT: %v", err)
	}

	return parity.Tenant{
		ID:    ComputeTenantID,
		Token: token,
	}
}

// MintComputeUserJWT creates a USER-kind (OBO-shaped) JWT scoped to the
// compute-test-client's tenant. It carries the user_roles claim key so the
// validator assigns Kind=user (even with an empty role set), and caas_user_id
// == userID so attribution records that principal. Tenant is ComputeTenantID so
// processor/criteria dispatch still finds the registered gRPC member — the
// combination used by cross-node attribution scenarios where a HUMAN origin
// (user) drives a cascade whose joined writes execute as the member's service
// identity.
func MintComputeUserJWT(t *testing.T, ks *JWTKeySet, userID string, roles ...string) parity.Tenant {
	t.Helper()
	if roles == nil {
		roles = []string{}
	}

	now := time.Now()
	claims := map[string]any{
		"sub":          userID,
		"iss":          ks.Issuer,
		"caas_user_id": userID,
		"caas_org_id":  ComputeTenantID,
		"user_roles":   roles, // KEY present → Kind=user (even when empty)
		"caas_tier":    "unlimited",
		"exp":          now.Add(1 * time.Hour).Unix(),
		"iat":          now.Unix(),
		"jti":          uuid.NewString(),
	}

	token, err := auth.Sign(context.Background(), claims, auth.NewRSASigner(ks.Key), ks.Kid)
	if err != nil {
		t.Fatalf("failed to mint compute user JWT: %v", err)
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
	// ComputeBin is the built compute-test-client, so a fixture can start
	// further clients for a scenario (fixtureutil.StartComputeClientForFixture).
	ComputeBin string
}

// LaunchOpts configures optional behavior for LaunchCyodaAndCompute.
type LaunchOpts struct {
	// ReadinessTimeout overrides the default health-check timeout for
	// cyoda-go. Defaults to defaultCyodaReadinessTimeout if zero.
	ReadinessTimeout time.Duration
	// NodeEnv, when set, returns extra environment for cluster node i. It is
	// appended after every other variable, so it overrides them (os/exec uses
	// the last value of a duplicated key). A scenario uses it to turn one
	// node's scheduler off, or to isolate one node's gossip.
	NodeEnv func(i int) []string
}

// NodeProc is one running cyoda-go process launched by LaunchCyodaNode.
type NodeProc struct {
	BaseURL      string
	GRPCEndpoint string
	// Logs is the process's combined output, also tee'd to os.Stderr. Never
	// assert on token or secret material read from it (Gate 3).
	Logs     *SyncBuffer
	cmd      *exec.Cmd
	exitedCh chan struct{}
	killOnce sync.Once
}

// Kill SIGKILLs the process group and reaps it through the monitor's exit
// signal (never a second cmd.Wait()). Calling it again is harmless.
func (p *NodeProc) Kill() {
	p.killOnce.Do(func() {
		killProcessGroupNoWait(p.cmd)
		<-p.exitedCh
	})
}

// LaunchCyodaNode starts one cyoda-go process with extraEnv on fresh ports and
// waits until it is healthy. It starts no compute client. A scenario that
// restarts a node on the same storage calls it twice with the same env.
// readiness 0 means the default readiness timeout.
//
// The whole launch is retried with fresh ports to self-heal a transient
// FreePort() TOCTOU collision (see clusterLaunchAttempts), and the health
// probe races the child's exit (nodeOutcome), so a bind-collision death fails
// fast instead of stalling the full readiness timeout.
func LaunchCyodaNode(cyodaBin string, ks *JWTKeySet, extraEnv []string, readiness time.Duration) (*NodeProc, error) {
	if readiness == 0 {
		readiness = defaultCyodaReadinessTimeout
	}
	var proc *NodeProc
	err := retryLaunch(clusterLaunchAttempts, func() error {
		hPort, e := FreePort()
		if e != nil {
			return fmt.Errorf("failed to get HTTP port: %w", e)
		}
		gPort, e := FreePort()
		if e != nil {
			return fmt.Errorf("failed to get gRPC port: %w", e)
		}
		// The admin port is picked too: the default (9091) is fixed, so parity
		// packages running in parallel would collide on one host.
		aPort, e := FreePort()
		if e != nil {
			return fmt.Errorf("failed to get admin port: %w", e)
		}
		cmd := exec.Command(cyodaBin)
		cmd.WaitDelay = 3 * time.Second
		cmd.Env = append(CyodaEnv(hPort, gPort, ks), extraEnv...)
		cmd.Env = append(cmd.Env, fmt.Sprintf("CYODA_ADMIN_PORT=%d", aPort))
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		// Output goes to the test runner's stderr (diagnostics for 5xx and
		// startup failures) and into Logs, so a test can read it as data.
		logs := &SyncBuffer{}
		cmd.Stdout = io.MultiWriter(os.Stderr, logs)
		cmd.Stderr = io.MultiWriter(os.Stderr, logs)
		if e := cmd.Start(); e != nil {
			return fmt.Errorf("failed to start cyoda-go: %w", e)
		}
		// The single owner of this process's Wait(). exitErr is read only
		// after exitedCh's close is observed, which happens-after the store.
		exitedCh := make(chan struct{})
		var exitErr error
		go func() {
			exitErr = cmd.Wait()
			close(exitedCh)
		}()
		url := fmt.Sprintf("http://127.0.0.1:%d", hPort)
		healthDoneCh := make(chan error, 1)
		go func() { healthDoneCh <- WaitForHTTPHealth(url+"/api/health", readiness) }()
		if e := nodeOutcome(0, healthDoneCh, exitedCh, func() error { return exitErr }); e != nil {
			killProcessGroupNoWait(cmd)
			<-exitedCh
			return e
		}
		proc = &NodeProc{BaseURL: url, GRPCEndpoint: fmt.Sprintf("127.0.0.1:%d", gPort), Logs: logs, cmd: cmd, exitedCh: exitedCh}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("cyoda launch failed: %w", err)
	}
	slog.Info("cyoda-go is ready", "pkg", "fixtureutil", "baseURL", proc.BaseURL)
	return proc, nil
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
	var opt LaunchOpts
	if len(opts) > 0 {
		opt = opts[0]
	}
	node, err := LaunchCyodaNode(cyodaBin, ks, extraEnv, opt.ReadinessTimeout)
	if err != nil {
		return nil, nil, err
	}
	cleanup := node.Kill

	// Mint M2M JWT for compute client.
	m2mToken, err := MintM2MJWT(ks)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("failed to mint M2M JWT: %w", err)
	}

	// Callbacks target the same single node that dispatched them.
	compute, err := StartComputeClient(ComputeClientOpts{
		ComputeBin: computeBin, GRPCEndpoint: node.GRPCEndpoint, HTTPBase: node.BaseURL, Token: m2mToken,
	})
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	cleanup = func() {
		// The client owns its own Wait; cyoda is reaped by its monitor
		// goroutine, so it is torn down kill-only + exit-signal wait.
		compute.Stop()
		node.Kill()
	}
	slog.Info("compute-test-client is ready", "pkg", "fixtureutil", "controlURL", compute.ControlURL())

	return &LaunchResult{
		BaseURL:      node.BaseURL,
		GRPCEndpoint: node.GRPCEndpoint,
		CyodaCmd:     node.cmd,
		ComputeCmd:   compute.Cmd(),
		ComputeBin:   computeBin,
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
	// GRPCEndpoints is the per-node gRPC endpoint list, same order as BaseURLs.
	GRPCEndpoints []string
	// ComputeBin is the built compute-test-client (see LaunchResult.ComputeBin).
	ComputeBin string
	// CyodaCmds holds one *exec.Cmd per node, in the same order as
	// BaseURLs. Exposed mainly for diagnostics; cleanup handles
	// process termination.
	CyodaCmds []*exec.Cmd
	// ComputeCmd is the single compute-test-client subprocess.
	ComputeCmd *exec.Cmd
	// NodeLogs holds one live capture of each node's combined stdout+stderr,
	// in the same order as BaseURLs. Each node's output is tee'd to os.Stderr
	// (unchanged diagnostics) AND into its SyncBuffer here, so a test can read
	// a node's logs as data — e.g. the scheduler incarnation the node
	// announced at start (IncarnationFromLog). Never assert on token/secret
	// material read from here (Gate 3).
	NodeLogs []*SyncBuffer
	// KillNode SIGKILLs node i's process group and reaps it by waiting on that
	// node's monitor exit signal (the same kill-no-wait + exit-signal reap the
	// teardown path uses — never a second cmd.Wait()). It exists so a crash
	// test can take a single node down mid-operation and assert a survivor
	// completes the orphaned work. Out-of-range i is a no-op. Killing a node is
	// permanent for the life of the fixture: the node is not restarted, and the
	// returned cleanup still tears down whatever remains safely (killing an
	// already-dead process group is harmless).
	KillNode func(i int)
	// SignalNode sends sig to node i's process group without waiting: SIGTERM
	// for a graceful shutdown, SIGSTOP / SIGCONT to freeze and resume it.
	SignalNode func(i int, sig syscall.Signal) error
	// AwaitNodeExit waits up to within for node i's process to exit, reaping
	// it through the monitor's exit signal. It returns an error if the
	// process is still running. within 0 checks without waiting.
	AwaitNodeExit func(i int, within time.Duration) error
}

// SyncBuffer is a goroutine-safe in-memory log sink. os/exec copies a
// subprocess's stdout and stderr on separate goroutines, so the sink both
// receive concurrently; every access takes the mutex. It is unbounded — the
// cluster fixture is short-lived (one test binary) so the accumulated volume
// is bounded by the run itself.
type SyncBuffer struct {
	mu  sync.Mutex
	buf []byte
}

// Write appends p to the buffer. Always returns len(p), nil (io.Writer never
// short-writes to memory), so a tee'd os.Stderr write is never starved.
func (b *SyncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	return len(p), nil
}

// String returns a snapshot copy of everything captured so far.
func (b *SyncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
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
// here. Symmetric to LaunchCyodaAndComputeWithBinaries.
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
	// nodeLogBufs holds the per-node combined-output captures (index i ==
	// node-i), published alongside nodes on the successful launch attempt.
	var nodeLogBufs []*SyncBuffer

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
		attemptLogBufs := make([]*SyncBuffer, n)

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
			if opt.NodeEnv != nil {
				env = append(env, opt.NodeEnv(i)...)
			}
			cmd.Env = env
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			// Tee each node's output to os.Stderr (unchanged diagnostics) AND
			// into a per-node capture buffer so tests can read node logs as
			// data (see ClusterLaunchResult.NodeLogs).
			logBuf := &SyncBuffer{}
			attemptLogBufs[i] = logBuf
			cmd.Stdout = io.MultiWriter(os.Stderr, logBuf)
			cmd.Stderr = io.MultiWriter(os.Stderr, logBuf)
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
		nodeLogBufs = attemptLogBufs
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
	// killNode SIGKILLs one node and reaps it via its monitor's exit signal —
	// the same discipline killNodes uses (kill-no-wait + <-exitedCh), never a
	// second cmd.Wait(). Published as ClusterLaunchResult.KillNode.
	killNode := func(i int) {
		if i < 0 || i >= len(nodes) {
			return
		}
		nd := nodes[i]
		if nd == nil || nd.cmd == nil {
			return
		}
		killProcessGroupNoWait(nd.cmd)
		if nd.exitedCh != nil {
			<-nd.exitedCh // reap: block until the monitor's Wait() returns
		}
	}
	signalNode := func(i int, sig syscall.Signal) error {
		if i < 0 || i >= len(nodes) || nodes[i] == nil || nodes[i].cmd == nil || nodes[i].cmd.Process == nil {
			return fmt.Errorf("signal node %d: no such node", i)
		}
		pgid, err := syscall.Getpgid(nodes[i].cmd.Process.Pid)
		if err != nil {
			return fmt.Errorf("signal node %d: %w", i, err)
		}
		if err := syscall.Kill(-pgid, sig); err != nil {
			return fmt.Errorf("signal node %d with %v: %w", i, sig, err)
		}
		return nil
	}
	awaitNodeExit := func(i int, within time.Duration) error {
		if i < 0 || i >= len(nodes) || nodes[i] == nil || nodes[i].exitedCh == nil {
			return fmt.Errorf("await node %d: no such node", i)
		}
		// An exit already observed wins over a zero wait: a select over two
		// ready channels would pick one at random.
		select {
		case <-nodes[i].exitedCh:
			return nil
		default:
		}
		timer := time.NewTimer(within)
		defer timer.Stop()
		select {
		case <-nodes[i].exitedCh:
			return nil
		case <-timer.C:
			return fmt.Errorf("node %d is still running %s after the wait began", i, within)
		}
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

	// The fixture's own client attaches to node 0, and its callbacks target
	// node 0; cross-node callback forwarding is covered by scenarios.
	grpcEndpoints := make([]string, n)
	for i := 0; i < n; i++ {
		grpcEndpoints[i] = fmt.Sprintf("127.0.0.1:%d", grpcPorts[i])
	}
	compute, err := StartComputeClient(ComputeClientOpts{
		ComputeBin: computeBin, GRPCEndpoint: grpcEndpoints[0],
		HTTPBase: fmt.Sprintf("http://127.0.0.1:%d", httpPorts[0]), Token: m2mToken,
		ReadyTimeout: defaultCyodaReadinessTimeout,
	})
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	cleanup = func() {
		compute.Stop()
		killNodes(nodes)
	}
	slog.Info("compute-test-client (cluster) is ready", "pkg", "fixtureutil", "controlURL", compute.ControlURL(), "nodes", n)

	baseURLs := make([]string, n)
	for i := 0; i < n; i++ {
		baseURLs[i] = fmt.Sprintf("http://127.0.0.1:%d", httpPorts[i])
	}

	return &ClusterLaunchResult{
		BaseURLs:      baseURLs,
		GRPCEndpoint:  grpcEndpoints[0],
		GRPCEndpoints: grpcEndpoints,
		ComputeBin:    computeBin,
		CyodaCmds:     cyodaCmds,
		ComputeCmd:    compute.Cmd(),
		NodeLogs:      nodeLogBufs,
		KillNode:      killNode,
		SignalNode:    signalNode,
		AwaitNodeExit: awaitNodeExit,
	}, cleanup, nil
}
