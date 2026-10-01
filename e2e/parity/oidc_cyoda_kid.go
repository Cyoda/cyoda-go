package parity

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// RunOidcKidSharedByTwoIssuers: two tenants' IdPs, of different issuers,
// publish a key under the same kid. Each issuer's token resolves to its own
// tenant's provider, in either order: once one issuer's token has been
// resolved, the other's is not refused for an issuer mismatch.
func RunOidcKidSharedByTwoIssuers(t *testing.T, fixture BackendFixture) {
	base := fixture.BaseURL()
	tenantA, tenantB := fixture.NewTenant(t), fixture.NewTenant(t)
	idpA, idpB := NewParityFixtureIdP(t), NewParityFixtureIdP(t)
	kid := idpA.DefaultKid
	idpB.AddSharedKidEntry(t, kid) // B publishes its own key under A's kid

	pa, err := client.NewClient(base, tenantA.Token).RegisterOidcProvider(t, map[string]any{"wellKnownConfigUri": idpA.WellKnownURI()})
	if err != nil {
		t.Fatalf("RegisterOidcProvider A: %v", err)
	}
	pb, err := client.NewClient(base, tenantB.Token).RegisterOidcProvider(t, map[string]any{"wellKnownConfigUri": idpB.WellKnownURI()})
	if err != nil {
		t.Fatalf("RegisterOidcProvider B: %v", err)
	}

	// The provider list a token gets is its own tenant's: the token acts in
	// that tenant, through that tenant's provider.
	actsIn := func(t *testing.T, who, token string, providerID string) {
		t.Helper()
		code, body, err := client.NewClient(base, token).ProbeAuthRaw(t)
		if err != nil || code != http.StatusOK {
			t.Fatalf("%s token under the shared kid: %d %s %v, want 200", who, code, body, err)
		}
		if !strings.Contains(string(body), providerID) {
			t.Fatalf("%s token does not act in its own tenant: %s", who, body)
		}
	}
	actsIn(t, "issuer A", idpA.MintTenantJWT(t, kid, tenantA.ID), pa.ID.String())
	actsIn(t, "issuer B", idpB.MintTenantJWT(t, kid, tenantB.ID), pb.ID.String())
	actsIn(t, "issuer A again", idpA.MintTenantJWT(t, kid, tenantA.ID), pa.ID.String())
}

// RunOidcCannotClaimCyodaKID: a tenant's OIDC provider that publishes a key
// under the kid of a cyoda-go key pair cannot have tokens accepted under that
// kid — neither while the key pair is published ahead of its window nor
// after it was invalidated. The key pairs are `human` audience, which
// nothing signs with, so the shared server's signers are untouched.
func RunOidcCannotClaimCyodaKID(t *testing.T, fixture BackendFixture) {
	base := fixture.BaseURL()
	op := client.NewClient(base, fixture.PlatformOperator(t).Token)

	issue := func(t *testing.T, validFrom time.Time) string {
		t.Helper()
		code, body, err := op.IssueKeyPairRaw(t, map[string]any{
			"algorithm": "RS256", "audience": "human",
			"validFrom": validFrom.UTC().Format(time.RFC3339),
		})
		if err != nil || code != http.StatusOK {
			t.Fatalf("issue: %d %s %v", code, body, err)
		}
		var kp struct {
			KeyId string `json:"keyId"`
		}
		if err := json.Unmarshal(body, &kp); err != nil || kp.KeyId == "" {
			t.Fatalf("issue: no keyId in %s: %v", body, err)
		}
		op.DeleteKeyPairOnCleanup(t, kp.KeyId)
		return kp.KeyId
	}

	ahead := issue(t, time.Now().Add(365*24*time.Hour))
	if kids, err := op.JWKSKIDs(t); err != nil || !kids[ahead] {
		t.Fatalf("control: JWKS publishes the key pair ahead of its window: %v", err)
	}
	ended := issue(t, time.Now())
	if code, body, err := op.InvalidateKeyPairWithGraceRaw(t, ended, 0); err != nil || code != http.StatusOK {
		t.Fatalf("invalidate: %d %s %v", code, body, err)
	}

	tenant := fixture.NewTenant(t)
	idp := NewParityFixtureIdP(t)
	idp.AddSharedKidEntry(t, ahead)
	idp.AddSharedKidEntry(t, ended)
	if _, err := client.NewClient(base, tenant.Token).RegisterOidcProvider(t,
		map[string]any{"wellKnownConfigUri": idp.WellKnownURI()}); err != nil {
		t.Fatalf("RegisterOidcProvider: %v", err)
	}

	ctl := client.NewClient(base, idp.MintTenantJWT(t, idp.DefaultKid, tenant.ID))
	if code, body, err := ctl.ProbeAuthRaw(t); err != nil || code != http.StatusOK {
		t.Fatalf("control: a token under the provider's own kid authenticates: %d %s %v", code, body, err)
	}
	for name, kid := range map[string]string{"ahead of its window": ahead, "invalidated": ended} {
		c := client.NewClient(base, idp.MintTenantJWT(t, kid, tenant.ID))
		if code, body, err := c.ProbeAuthRaw(t); err != nil || code != http.StatusUnauthorized {
			t.Fatalf("token under the kid of a cyoda-go key pair %s: %d %s %v, want 401", name, code, body, err)
		}
	}
}
