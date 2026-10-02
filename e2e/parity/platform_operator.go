package parity

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// RunPlatformOperatorGate: the platform-wide admin endpoints refuse a tenant
// admin with 403 FORBIDDEN — also when the tenant admin aims at the bootstrap
// key the fixture signs with — and admit the platform operator. Operator
// success on each key-pair call is RunSigningKeyPairLifecycle.
func RunPlatformOperatorGate(t *testing.T, fixture BackendFixture) {
	base := fixture.BaseURL()
	tenant := fixture.NewTenant(t)
	tc := client.NewClient(base, tenant.Token)

	// The fixture's tokens are signed with the bootstrap key: aim at it.
	bootKID := client.TokenKID(tenant.Token)
	if bootKID == "" {
		t.Fatal("the fixture's tenant token carries no kid")
	}

	// refused checks every operator route answers c with 403 FORBIDDEN, and
	// with detail in the body when detail is not empty.
	refused := func(t *testing.T, c *client.Client, who, detail string) {
		t.Helper()
		calls := []struct {
			name string
			do   func() (int, []byte, error)
		}{
			{"issue", func() (int, []byte, error) {
				return c.IssueKeyPairRaw(t, map[string]any{"algorithm": "RS256"})
			}},
			{"current", func() (int, []byte, error) { return c.CurrentKeyPairRaw(t) }},
			{"invalidate", func() (int, []byte, error) { return c.InvalidateKeyPairRaw(t, bootKID) }},
			{"reactivate", func() (int, []byte, error) { return c.ReactivateKeyPairRaw(t, bootKID, time.Now().Add(time.Hour)) }},
			{"delete", func() (int, []byte, error) { return c.DeleteKeyPairRaw(t, bootKID) }},
		}
		for _, call := range calls {
			code, body, err := call.do()
			if err != nil {
				t.Fatalf("%s %s: transport: %v", who, call.name, err)
			}
			if code != http.StatusForbidden || !containsErrorCode(body, "FORBIDDEN") || !bytes.Contains(body, []byte(detail)) {
				t.Fatalf("%s %s: %d %s, want 403 FORBIDDEN %s", who, call.name, code, body, detail)
			}
		}
	}

	refused(t, tc, "tenant admin", "")

	// An on-behalf-of token of a tenant is refused on every operator route, and
	// an assertion that claims PLATFORM and ROLE_ADMIN is refused at the exchange.
	obo := client.NewClient(base, OBOToken(t, fixture, tenant, "mallory"))
	refused(t, obo, "an on-behalf-of token", oboRefusal)
	status, body, err := exchangeAsserting(t, fixture, tenant, map[string]any{
		"caas_org_id": "PLATFORM", "user_roles": []string{"ROLE_ADMIN"}})
	if err != nil || status != http.StatusForbidden || !bytes.Contains(body, []byte(`"access_denied"`)) {
		if status == http.StatusOK {
			body = []byte("(token withheld)")
		}
		t.Fatalf("exchange claiming PLATFORM: %d %s %v, want 403 access_denied", status, body, err)
	}

	// The bootstrap key survived: the tenant token still authenticates.
	if code, body, err := tc.ProbeAuthRaw(t); err != nil || code != http.StatusOK {
		t.Fatalf("bootstrap-signed token after the refused calls: %d %s %v", code, body, err)
	}
}

// oboRefusal is the detail of the 403 an on-behalf-of token gets on an
// administration route.
const oboRefusal = "on-behalf-of tokens cannot administer"

// RunOBOTokenCannotAdminister: an on-behalf-of token of a tenant is refused
// with 403 FORBIDDEN on the tenant's client and trusted-key routes, and the
// client it aimed at is left as it was.
func RunOBOTokenCannotAdminister(t *testing.T, fixture BackendFixture) {
	base := fixture.BaseURL()
	tenant := fixture.NewTenant(t)
	admin := client.NewClient(base, tenant.Token)
	code, body, err := admin.CreateClientRaw(t, false, false)
	if err != nil || code != http.StatusOK {
		t.Fatalf("create client: %d %v", code, err)
	}
	var cred struct {
		ID string `json:"client_id"`
	}
	if err := json.Unmarshal(body, &cred); err != nil || cred.ID == "" {
		t.Fatalf("create client: no client_id in the response (%v)", err)
	}

	obo := client.NewClient(base, OBOToken(t, fixture, tenant, "mallory"))
	const kid = "parity-obo-refused"
	calls := []struct {
		name string
		do   func() (int, []byte, error)
	}{
		{"list clients", func() (int, []byte, error) { return obo.ListClientsRaw(t) }},
		{"create client", func() (int, []byte, error) { return obo.CreateClientRaw(t, false, false) }},
		{"delete client", func() (int, []byte, error) { return obo.DeleteClientRaw(t, cred.ID) }},
		{"reset secret", func() (int, []byte, error) { return obo.ResetClientSecretRaw(t, cred.ID) }},
		{"list trusted keys", func() (int, []byte, error) { return obo.ListTrustedKeysRaw(t) }},
		{"register trusted key", func() (int, []byte, error) {
			return obo.RegisterTrustedKeyRaw(t, map[string]any{"keyId": kid})
		}},
		{"invalidate trusted key", func() (int, []byte, error) { return obo.InvalidateTrustedKeyRaw(t, kid) }},
		{"delete trusted key", func() (int, []byte, error) { return obo.DeleteTrustedKeyRaw(t, kid) }},
	}
	for _, call := range calls {
		code, body, err := call.do()
		if err != nil {
			t.Fatalf("%s: transport: %v", call.name, err)
		}
		if code != http.StatusForbidden || !containsErrorCode(body, "FORBIDDEN") || !bytes.Contains(body, []byte(oboRefusal)) {
			if code == http.StatusOK {
				body = []byte("(credentials withheld)")
			}
			t.Fatalf("on-behalf-of token %s: %d %s, want 403 FORBIDDEN %s", call.name, code, body, oboRefusal)
		}
	}

	// The refused delete and reset left the client in place.
	code, body, err = admin.ListClientsRaw(t)
	if err != nil || code != http.StatusOK || !bytes.Contains(body, []byte(cred.ID)) {
		t.Fatalf("list after the refused calls: %d %v, want the client %s listed", code, err, cred.ID)
	}
}
