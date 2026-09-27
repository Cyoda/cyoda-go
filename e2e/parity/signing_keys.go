package parity

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// RunSigningKeyPairLifecycle drives one key pair through issue, current,
// JWKS, invalidate, reactivate and delete on every backend. It uses audience
// "human" — nothing in the parity suite signs with it — and never
// invalidateCurrent, so the shared server's own signing keys are untouched.
// No other registered scenario issues a "human" key pair (grepped for
// "keypair"/"KeyPair" across e2e/parity before adding this one), so the
// current-key assertions below compare the returned keyId directly rather
// than needing to tolerate another test's key also being current.
func RunSigningKeyPairLifecycle(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	code, body, err := c.IssueKeyPairRaw(t, map[string]any{"algorithm": "RS256", "audience": "human"})
	if err != nil || code != http.StatusOK {
		t.Fatalf("issue: %d %s %v", code, body, err)
	}
	var kp struct {
		KeyId  string `json:"keyId"`
		Active bool   `json:"active"`
	}
	_ = json.Unmarshal(body, &kp)
	c.DeleteKeyPairOnCleanup(t, kp.KeyId)

	if code, body, _ := c.CurrentKeyPairRaw(t, "human"); code != http.StatusOK || !jsonHasKID(body, kp.KeyId) {
		t.Fatalf("current: %d %s", code, body)
	}
	if kids, err := c.JWKSKIDs(t); err != nil || !kids[kp.KeyId] {
		t.Fatalf("JWKS missing the issued key: %v", err)
	}
	if code, body, _ := c.InvalidateKeyPairRaw(t, kp.KeyId); code != http.StatusOK {
		t.Fatalf("invalidate: %d %s", code, body)
	}
	if code, _, _ := c.CurrentKeyPairRaw(t, "human"); code != http.StatusNotFound {
		t.Fatalf("current after invalidate: %d, want 404", code)
	}
	if code, body, _ := c.ReactivateKeyPairRaw(t, kp.KeyId, time.Now().Add(time.Hour)); code != http.StatusOK || !jsonHasKID(body, kp.KeyId) {
		t.Fatalf("reactivate: %d %s", code, body)
	}
	if code, body, _ := c.DeleteKeyPairRaw(t, kp.KeyId); code != http.StatusOK {
		t.Fatalf("delete: %d %s", code, body)
	}
	if code, body, _ := c.DeleteKeyPairRaw(t, kp.KeyId); code != http.StatusNotFound || !containsErrorCode(body, "KEYPAIR_NOT_FOUND") {
		t.Fatalf("second delete: %d %s, want 404 KEYPAIR_NOT_FOUND", code, body)
	}
}

func jsonHasKID(body []byte, kid string) bool {
	var v struct {
		KeyId string `json:"keyId"`
	}
	return json.Unmarshal(body, &v) == nil && v.KeyId == kid
}
