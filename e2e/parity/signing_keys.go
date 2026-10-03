package parity

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// RunSigningKeyPairLifecycle drives one key pair through issue, current,
// JWKS, invalidate, reactivate and delete on every backend. It never sets
// invalidateCurrent, so the shared server's own signing keys are untouched
// by this scenario — issuing without invalidating leaves every prior signer
// alone, and this scenario deletes the key it created on cleanup.
func RunSigningKeyPairLifecycle(t *testing.T, fixture BackendFixture) {
	c := client.NewClient(fixture.BaseURL(), fixture.PlatformOperator(t).Token)

	code, body, err := c.IssueKeyPairRaw(t, map[string]any{"algorithm": "RS256"})
	if err != nil || code != http.StatusOK {
		t.Fatalf("issue: %d %s %v", code, body, err)
	}
	var kp struct {
		KeyId  string `json:"keyId"`
		Active bool   `json:"active"`
	}
	_ = json.Unmarshal(body, &kp)
	c.DeleteKeyPairOnCleanup(t, kp.KeyId)

	if code, body, _ := c.CurrentKeyPairRaw(t); code != http.StatusOK || !jsonHasKID(body, kp.KeyId) {
		t.Fatalf("current: %d %s", code, body)
	}
	if kids, err := c.JWKSKIDs(t); err != nil || !kids[kp.KeyId] {
		t.Fatalf("JWKS missing the issued key: %v", err)
	}
	if code, body, _ := c.InvalidateKeyPairRaw(t, kp.KeyId); code != http.StatusOK {
		t.Fatalf("invalidate: %d %s", code, body)
	}
	// Invalidating this key never leaves no signer at all: the bootstrap key,
	// or whichever key pair was signing before this scenario issued its own,
	// takes over. Only the deleted key itself must no longer be current.
	if code, body, _ := c.CurrentKeyPairRaw(t); code != http.StatusOK || jsonHasKID(body, kp.KeyId) {
		t.Fatalf("current after invalidate: %d %s, want 200 and a different key", code, body)
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
