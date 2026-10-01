package parity

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// RunKeyPairNoAudience: the operator reads the current signing key without an
// audience parameter, and the JWKS publishes it.
func RunKeyPairNoAudience(t *testing.T, fixture BackendFixture) {
	op := client.NewClient(fixture.BaseURL(), fixture.PlatformOperator(t).Token)
	status, body, err := op.CurrentKeyPairRaw(t)
	if err != nil || status != http.StatusOK {
		t.Fatalf("current: %d %s %v", status, body, err)
	}
	var cur struct {
		KeyID string `json:"keyId"`
	}
	if err := json.Unmarshal(body, &cur); err != nil || cur.KeyID == "" {
		t.Fatalf("current body %s: %v", body, err)
	}
	kids, err := op.JWKSKIDs(t)
	if err != nil {
		t.Fatalf("jwks: %v", err)
	}
	if !kids[cur.KeyID] {
		t.Fatalf("JWKS %v lacks current key %s", kids, cur.KeyID)
	}
}
