package parity

import (
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

	refused := func(t *testing.T, c *client.Client, who string) {
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
}
