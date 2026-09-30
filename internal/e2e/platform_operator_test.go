package e2e_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// operatorEndpoints are the platform-wide endpoints behind the operator
// guard. kid is well formed and never exists: every refusal here happens
// before a lookup. The GET needs ?audience= (the generated wrapper answers
// 400 for a missing one before the handler runs).
func operatorEndpoints() []struct{ name, method, path, body string } {
	const kid = "0123456789abcdef0123456789abcdef"
	validTo := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	return []struct{ name, method, path, body string }{
		{"issue", http.MethodPost, "/oauth/keys/keypair", `{"algorithm":"RS256","audience":"human"}`},
		{"current", http.MethodGet, "/oauth/keys/keypair/current?audience=human", ""},
		{"invalidate", http.MethodPost, "/oauth/keys/keypair/" + kid + "/invalidate", ""},
		{"reactivate", http.MethodPost, "/oauth/keys/keypair/" + kid + "/reactivate", `{"validTo":"` + validTo + `"}`},
		{"delete", http.MethodDelete, "/oauth/keys/keypair/" + kid, ""},
		{"reload", http.MethodPost, "/oauth/oidc/providers/reload", ""},
	}
}

func bodyBytes(s string) []byte {
	if s == "" {
		return nil
	}
	return []byte(s)
}

func TestPlatformOperator_NoToken_401(t *testing.T) {
	for _, ep := range operatorEndpoints() {
		t.Run(ep.name, func(t *testing.T) {
			assertProblemJSON(t, unauthRequest(t, ep.method, "/api"+ep.path, ""), http.StatusUnauthorized, "UNAUTHORIZED")
		})
	}
}

func TestPlatformOperator_TenantAdmin_403(t *testing.T) {
	for _, ep := range operatorEndpoints() {
		t.Run(ep.name, func(t *testing.T) {
			resp := requestAs(t, suiteToken(t), ep.method, ep.path, bodyBytes(ep.body))
			assertProblemJSON(t, resp, http.StatusForbidden, "FORBIDDEN")
		})
	}
}

func TestPlatformOperator_PlatformWithoutAdmin_403(t *testing.T) {
	tok, err := platformTokenRaw("ROLE_M2M")
	if err != nil {
		t.Fatal(err)
	}
	for _, ep := range operatorEndpoints() {
		t.Run(ep.name, func(t *testing.T) {
			assertProblemJSON(t, requestAs(t, tok, ep.method, ep.path, bodyBytes(ep.body)), http.StatusForbidden, "FORBIDDEN")
		})
	}
}

// TestPlatformOperator_AdminM2MClientInPlatform: an admin M2M client created
// in PLATFORM is a platform operator — the route an operator keeps once the
// bootstrap key is revoked.
func TestPlatformOperator_AdminM2MClientInPlatform(t *testing.T) {
	id, secret := createM2MClient(t, "PLATFORM", "platform-seed", true)
	tok := getToken(t, id, secret)
	resp := requestAs(t, tok, http.MethodPost, "/oauth/keys/keypair", []byte(`{"algorithm":"RS256","audience":"human"}`))
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("issue as a PLATFORM admin M2M client: %d %s", resp.StatusCode, body)
	}
	var kp struct {
		KeyID string `json:"keyId"`
	}
	if err := json.Unmarshal([]byte(body), &kp); err != nil || kp.KeyID == "" {
		t.Fatalf("no keyId: %s", body)
	}
	t.Cleanup(func() { operatorRequest(t, http.MethodDelete, "/oauth/keys/keypair/"+kp.KeyID, nil).Body.Close() })
}
