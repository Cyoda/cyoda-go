package fixtureutil

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity"
)

// ComputeClientOpts says how to start one compute-test-client.
type ComputeClientOpts struct {
	ComputeBin   string // absolute path of the built compute-test-client
	GRPCEndpoint string // host:port of the cyoda node the client attaches to
	HTTPBase     string // HTTP base its callbacks go to
	Token        string // M2M bearer for the tenant it joins under; never logged
	Tags         []string
	Behaviour    string
	// ReadyTimeout bounds the wait for the client to report ready (it does so
	// once it holds the greet). Zero means 30s.
	ReadyTimeout time.Duration
}

// ComputeClientProc is a running compute-test-client. It implements
// parity.ComputeClient.
type ComputeClientProc struct {
	cmd        *exec.Cmd
	controlURL string
	memberID   string
	stopOnce   sync.Once
}

var _ parity.ComputeClient = (*ComputeClientProc)(nil)

// StartComputeClient starts one compute-test-client, waits until it has joined
// the server, and returns a handle on it. The fixture's own client and every
// further one a scenario asks for are started here.
func StartComputeClient(opts ComputeClientOpts) (*ComputeClientProc, error) {
	ready := opts.ReadyTimeout
	if ready == 0 {
		ready = 30 * time.Second
	}
	cmd := exec.Command(opts.ComputeBin)
	cmd.WaitDelay = 3 * time.Second
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("CYODA_COMPUTE_GRPC_ENDPOINT=%s", opts.GRPCEndpoint),
		fmt.Sprintf("CYODA_COMPUTE_TOKEN=%s", opts.Token),
		fmt.Sprintf("CYODA_COMPUTE_HTTP_BASE=%s", opts.HTTPBase),
		fmt.Sprintf("CYODA_TEST_COMPUTE_TAGS=%s", strings.Join(opts.Tags, ",")),
		fmt.Sprintf("CYODA_TEST_COMPUTE_BEHAVIOUR=%s", opts.Behaviour),
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create compute stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start compute-test-client: %w", err)
	}
	p := &ComputeClientProc{cmd: cmd}

	// ParseHealthAddr returns at once when stdout closes without the line,
	// which is what a client that refuses to start looks like.
	healthAddr, err := ParseHealthAddr(stdout, defaultComputeHealthAddrTimeout)
	if err != nil {
		p.Stop()
		return nil, fmt.Errorf("failed to parse HEALTH_ADDR from compute-test-client: %w", err)
	}
	go func() { _, _ = io.Copy(io.Discard, stdout) }()
	p.controlURL = "http://" + healthAddr

	if err := WaitForHTTPHealth(p.controlURL+"/healthz", ready); err != nil {
		p.Stop()
		return nil, fmt.Errorf("compute-test-client health probe failed: %w", err)
	}
	doc, err := p.fetch(http.MethodGet, "/record")
	if err != nil {
		p.Stop()
		return nil, fmt.Errorf("failed to read compute-test-client record: %w", err)
	}
	p.memberID = doc.MemberID
	return p, nil
}

// computeRecordDoc mirrors the client's /record document.
type computeRecordDoc struct {
	MemberID string                   `json:"memberId"`
	Received []parity.ReceivedCallout `json:"received"`
}

func (p *ComputeClientProc) fetch(method, path string) (computeRecordDoc, error) {
	req, err := http.NewRequest(method, p.controlURL+path, nil)
	if err != nil {
		return computeRecordDoc{}, fmt.Errorf("failed to build %s %s: %w", method, path, err)
	}
	// /release waits for the callbacks it triggers; each is bounded at 15s.
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return computeRecordDoc{}, fmt.Errorf("failed to call %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return computeRecordDoc{}, fmt.Errorf("failed to read %s %s: %w", method, path, err)
	}
	if resp.StatusCode != http.StatusOK {
		return computeRecordDoc{}, fmt.Errorf("%s %s answered %d: %s", method, path, resp.StatusCode, raw)
	}
	var doc computeRecordDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return computeRecordDoc{}, fmt.Errorf("failed to decode %s %s: %w", method, path, err)
	}
	return doc, nil
}

// MemberID implements parity.ComputeClient.
func (p *ComputeClientProc) MemberID() string { return p.memberID }

// Received implements parity.ComputeClient.
func (p *ComputeClientProc) Received(t *testing.T) []parity.ReceivedCallout {
	t.Helper()
	doc, err := p.fetch(http.MethodGet, "/record")
	if err != nil {
		t.Fatalf("compute client record: %v", err)
	}
	return doc.Received
}

// Release implements parity.ComputeClient.
func (p *ComputeClientProc) Release(t *testing.T) []parity.ReceivedCallout {
	t.Helper()
	doc, err := p.fetch(http.MethodPost, "/release")
	if err != nil {
		t.Fatalf("compute client release: %v", err)
	}
	return doc.Received
}

// Stop implements parity.ComputeClient. The process has no monitor goroutine,
// so KillProcessGroup — which owns the one Wait — is the right teardown, once.
func (p *ComputeClientProc) Stop() {
	p.stopOnce.Do(func() { KillProcessGroup(p.cmd) })
}

// Cmd returns the client's process handle, for diagnostics.
func (p *ComputeClientProc) Cmd() *exec.Cmd { return p.cmd }

// ControlURL returns the base URL of the client's local control endpoint.
func (p *ComputeClientProc) ControlURL() string { return p.controlURL }

// StartComputeClientForFixture is what a fixture's StartComputeClient calls:
// it checks the spec, mints an M2M bearer for the spec's tenant from the
// fixture's key set, and starts the client. It fails the test on any error.
func StartComputeClientForFixture(t *testing.T, ks *JWTKeySet, computeBin, grpcEndpoint, httpBase string, spec parity.ComputeClientSpec) parity.ComputeClient {
	t.Helper()
	if spec.TenantID == "" || len(spec.Tags) == 0 {
		t.Fatalf("ComputeClientSpec needs a TenantID and at least one tag: %+v", spec)
	}
	token, err := MintM2MJWTForTenant(ks, spec.TenantID)
	if err != nil {
		t.Fatalf("failed to mint M2M JWT for the compute client: %v", err)
	}
	p, err := StartComputeClient(ComputeClientOpts{
		ComputeBin: computeBin, GRPCEndpoint: grpcEndpoint, HTTPBase: httpBase,
		Token: token, Tags: spec.Tags, Behaviour: spec.Behaviour,
	})
	if err != nil {
		t.Fatalf("failed to start compute client: %v", err)
	}
	return p
}
