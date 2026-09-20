package fixtureutil_test

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/fixtureutil"
)

// TestMintM2MJWTForTenant: the token names the given tenant and carries
// ROLE_M2M, which StartStreaming requires.
func TestMintM2MJWTForTenant(t *testing.T) {
	ks, err := fixtureutil.GenerateJWTKeySet()
	if err != nil {
		t.Fatalf("GenerateJWTKeySet: %v", err)
	}
	tok, err := fixtureutil.MintM2MJWTForTenant(ks, "tenant-xyz")
	if err != nil {
		t.Fatalf("MintM2MJWTForTenant: %v", err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts; want 3", len(parts))
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	var claims struct {
		Org    string   `json:"caas_org_id"`
		Scopes []string `json:"scopes"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	if claims.Org != "tenant-xyz" {
		t.Errorf("caas_org_id = %q; want tenant-xyz", claims.Org)
	}
	hasM2M := false
	for _, s := range claims.Scopes {
		hasM2M = hasM2M || s == "ROLE_M2M"
	}
	if !hasM2M {
		t.Errorf("scopes = %v; want ROLE_M2M among them", claims.Scopes)
	}
}

// TestStartComputeClient_JoinsRecordsAndStops: an extra client started beside
// the fixture's own joins the server (it holds a member id), serves its
// control surface, and is gone after Stop.
func TestStartComputeClient_JoinsRecordsAndStops(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess launch under -short")
	}
	ks, err := fixtureutil.GenerateJWTKeySet()
	if err != nil {
		t.Fatalf("GenerateJWTKeySet: %v", err)
	}
	result, cleanup, err := fixtureutil.LaunchCyodaAndCompute(ks, []string{"CYODA_STORAGE_BACKEND=memory"})
	if err != nil {
		t.Fatalf("LaunchCyodaAndCompute: %v", err)
	}
	t.Cleanup(cleanup)
	if result.ComputeBin == "" {
		t.Fatal("LaunchResult.ComputeBin is empty; a fixture needs it to start further clients")
	}

	var cc parity.ComputeClient = fixtureutil.StartComputeClientForFixture(t, ks, result.ComputeBin,
		result.GRPCEndpoint, result.BaseURL, parity.ComputeClientSpec{
			TenantID:  "h8-extra-tenant",
			Tags:      []string{"h8-extra"},
			Behaviour: parity.ComputeBehaviourStall,
		})
	if cc.MemberID() == "" {
		t.Error("the extra client reports no member id; it did not complete its join")
	}
	if got := cc.Received(t); len(got) != 0 {
		t.Errorf("a fresh client has received %d callouts; want 0", len(got))
	}

	proc := cc.(*fixtureutil.ComputeClientProc)
	cc.Stop()
	cc.Stop() // idempotent
	hc := &http.Client{Timeout: 2 * time.Second}
	if resp, err := hc.Get(proc.ControlURL() + "/healthz"); err == nil {
		resp.Body.Close()
		t.Error("the client's control endpoint still answers after Stop")
	}
}

// TestStartComputeClient_RejectsUnknownBehaviour: the client refuses to start,
// and the launcher reports it instead of waiting out the readiness timeout.
func TestStartComputeClient_RejectsUnknownBehaviour(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess launch under -short")
	}
	bin, err := fixtureutil.BuildComputeBinary()
	if err != nil {
		t.Fatalf("BuildComputeBinary: %v", err)
	}
	start := time.Now()
	_, err = fixtureutil.StartComputeClient(fixtureutil.ComputeClientOpts{
		ComputeBin: bin, GRPCEndpoint: "127.0.0.1:1", Token: "x", Behaviour: "explode",
	})
	if err == nil {
		t.Fatal("StartComputeClient accepted an unknown behaviour")
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("took %s to report a client that exits at once", time.Since(start))
	}
}
