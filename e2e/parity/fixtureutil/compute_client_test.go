package fixtureutil_test

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/fixtureutil"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

// TestProvisionComputeClient_IsAStoredClientOfTheTenant: the credentials
// ProvisionComputeClient returns belong to a stored M2M client of the given
// tenant — the token endpoint issues them a client-credentials token (it
// carries cgen) naming that tenant and ROLE_M2M, which StartStreaming
// requires. ComputeCredentials provisions one client per tenant and hands the
// same one to every compute client of that tenant.
func TestProvisionComputeClient_IsAStoredClientOfTheTenant(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess launch under -short")
	}
	cyodaBin, err := fixtureutil.BuildCyodaBinary()
	if err != nil {
		t.Fatalf("BuildCyodaBinary: %v", err)
	}
	ks, err := fixtureutil.GenerateJWTKeySet()
	if err != nil {
		t.Fatalf("GenerateJWTKeySet: %v", err)
	}
	node, err := fixtureutil.LaunchCyodaNode(cyodaBin, ks, []string{"CYODA_STORAGE_BACKEND=memory"}, 0)
	if err != nil {
		t.Fatalf("LaunchCyodaNode: %v", err)
	}
	t.Cleanup(node.Kill)

	id, secret := fixtureutil.ProvisionComputeClient(t, node.BaseURL, "tenant-xyz", ks)
	tok, status, err := client.FetchClientCredentialsToken(t.Context(), node.BaseURL, "tenant-xyz", id, secret)
	if err != nil || status != http.StatusOK {
		t.Fatalf("client_credentials grant with the provisioned client: status %d, %v", status, err)
	}
	parsed, err := auth.Parse(tok)
	if err != nil {
		t.Fatalf("parse token: %v", err)
	}
	if got := parsed.Claims["caas_org_id"]; got != "tenant-xyz" {
		t.Errorf("caas_org_id = %v; want tenant-xyz", got)
	}
	if got := parsed.Claims["caas_user_id"]; got != id {
		t.Errorf("caas_user_id = %v; want the client id", got)
	}
	if _, ok := parsed.Claims["cgen"]; !ok {
		t.Error("token carries no cgen; it is not a stored client's client-credentials token")
	}
	scopes, _ := parsed.Claims["scopes"].([]any)
	if !slices.Contains(scopes, any("ROLE_M2M")) {
		t.Errorf("scopes = %v; want ROLE_M2M among them", scopes)
	}

	creds := fixtureutil.NewComputeCredentials(ks)
	id1, _ := creds.For(t, node.BaseURL, "tenant-abc")
	id2, _ := creds.For(t, node.BaseURL, "tenant-abc")
	other, _ := creds.For(t, node.BaseURL, "tenant-def")
	if id1 != id2 {
		t.Errorf("two compute clients of one tenant got clients %q and %q; want one shared client", id1, id2)
	}
	if other == id1 {
		t.Error("two tenants share one client")
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

	var cc parity.ComputeClient = fixtureutil.StartComputeClientForFixture(t, fixtureutil.NewComputeCredentials(ks), result.ComputeBin,
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
		ComputeBin: bin, GRPCEndpoint: "127.0.0.1:1", HTTPBase: "http://127.0.0.1:1",
		TenantID: "t", ClientID: "x", ClientSecret: "y", Behaviour: "explode",
	})
	if err == nil {
		t.Fatal("StartComputeClient accepted an unknown behaviour")
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("took %s to report a client that exits at once", time.Since(start))
	}
}
