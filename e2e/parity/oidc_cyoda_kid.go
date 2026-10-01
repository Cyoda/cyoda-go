package parity

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

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
