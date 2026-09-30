package parity

import (
	"net/http"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// RunPlatformOperatorGate: the platform-wide admin endpoints refuse a tenant
// admin with 403 FORBIDDEN — also when the tenant admin aims at the bootstrap
// key the fixture signs with, and when ROLE_ADMIN comes from the tenant's own
// OIDC provider with a PLATFORM tenant claim — and admit the platform
// operator. Operator success on each key-pair call is
// RunSigningKeyPairLifecycle.
func RunPlatformOperatorGate(t *testing.T, fixture BackendFixture) {
	base := fixture.BaseURL()
	tenant := fixture.NewTenant(t)
	tc := client.NewClient(base, tenant.Token)
	op := client.NewClient(base, fixture.PlatformOperator(t).Token)

	// The fixture's tokens are signed with the bootstrap key: aim at it.
	bootKID := client.TokenKID(tenant.Token)
	if bootKID == "" {
		t.Fatal("the fixture's tenant token carries no kid")
	}

	refused := func(t *testing.T, c *client.Client, who string) {
		t.Helper()
		calls := []struct {
			name string
			do   func() (int, []byte, error)
		}{
			{"issue", func() (int, []byte, error) {
				return c.IssueKeyPairRaw(t, map[string]any{"algorithm": "RS256", "audience": "human"})
			}},
			{"current", func() (int, []byte, error) { return c.CurrentKeyPairRaw(t, "human") }},
			{"invalidate", func() (int, []byte, error) { return c.InvalidateKeyPairRaw(t, bootKID) }},
			{"reactivate", func() (int, []byte, error) { return c.ReactivateKeyPairRaw(t, bootKID, time.Now().Add(time.Hour)) }},
			{"delete", func() (int, []byte, error) { return c.DeleteKeyPairRaw(t, bootKID) }},
			{"reload", func() (int, []byte, error) { return c.ReloadOidcProvidersRaw(t) }},
		}
		for _, call := range calls {
			code, body, err := call.do()
			if err != nil {
				t.Fatalf("%s %s: transport: %v", who, call.name, err)
			}
			if code != http.StatusForbidden || !containsErrorCode(body, "FORBIDDEN") {
				t.Fatalf("%s %s: %d %s, want 403 FORBIDDEN", who, call.name, code, body)
			}
		}
	}

	refused(t, tc, "tenant admin")

	// The bootstrap key survived: the tenant token still authenticates.
	if code, body, err := tc.ProbeAuthRaw(t); err != nil || code != http.StatusOK {
		t.Fatalf("bootstrap-signed token after the refused calls: %d %s %v", code, body, err)
	}

	if code, body, err := op.ReloadOidcProvidersRaw(t); err != nil || code != http.StatusOK {
		t.Fatalf("reload as operator: %d %s %v", code, body, err)
	}

	// ROLE_ADMIN from the tenant's own OIDC provider, with a PLATFORM claim.
	idp := NewParityFixtureIdP(t)
	p, err := tc.RegisterOidcProvider(t, map[string]any{"wellKnownConfigUri": idp.WellKnownURI()})
	if err != nil {
		t.Fatalf("RegisterOidcProvider: %v", err)
	}
	oidcTok := idp.MintJWTWithRolesClaim(t, idp.DefaultKid, tenant.ID, map[string]any{
		"roles":       []string{"ROLE_ADMIN"},
		"caas_org_id": "PLATFORM",
	})
	oc := client.NewClient(base, oidcTok)
	if code, body, err := oc.ProbeAuthRaw(t); err != nil || code != http.StatusOK {
		t.Fatalf("control: the OIDC token authenticates: %d %s %v", code, body, err)
	}
	// Control: its ROLE_ADMIN counts in its own tenant (a no-op PATCH).
	if _, err := oc.UpdateOidcProvider(t, p.ID, map[string]any{}); err != nil {
		t.Fatalf("control: the OIDC admin updates its own provider: %v", err)
	}
	refused(t, oc, "OIDC tenant admin")
}
