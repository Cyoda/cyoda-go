package e2e_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// operatorEndpoints are the platform-wide endpoints behind the operator
// guard. kid is well formed and never exists: every refusal here happens
// before a lookup.
func operatorEndpoints() []struct{ name, method, path, body string } {
	const kid = "0123456789abcdef0123456789abcdef"
	validTo := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	return []struct{ name, method, path, body string }{
		{"issue", http.MethodPost, "/oauth/keys/keypair", `{"algorithm":"RS256"}`},
		{"current", http.MethodGet, "/oauth/keys/keypair/current", ""},
		{"invalidate", http.MethodPost, "/oauth/keys/keypair/" + kid + "/invalidate", ""},
		{"reactivate", http.MethodPost, "/oauth/keys/keypair/" + kid + "/reactivate", `{"validTo":"` + validTo + `"}`},
		{"delete", http.MethodDelete, "/oauth/keys/keypair/" + kid, ""},
		{"get log-level", http.MethodGet, "/admin/log-level", ""},
		{"set log-level", http.MethodPost, "/admin/log-level", `{"level":"info"}`},
		{"get trace-sampler", http.MethodGet, "/admin/trace-sampler", ""},
		{"set trace-sampler", http.MethodPost, "/admin/trace-sampler", `{"sampler":"always"}`},
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

// TestPlatformOperator_OBOToken_403: an on-behalf-of token is refused on every
// operator route, with the refusal that names on-behalf-of tokens.
func TestPlatformOperator_OBOToken_403(t *testing.T) {
	obo := oboToken(t, "mallory")
	for _, ep := range operatorEndpoints() {
		t.Run(ep.name, func(t *testing.T) {
			assertOBORefusedAdmin(t, requestAs(t, obo, ep.method, ep.path, bodyBytes(ep.body)))
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
	resp := requestAs(t, tok, http.MethodPost, "/oauth/keys/keypair", []byte(`{"algorithm":"RS256"}`))
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

// TestPlatformOperator_TraceSamplerRoundTrip: the operator reads the sampler
// and writes the same configuration back.
func TestPlatformOperator_TraceSamplerRoundTrip(t *testing.T) {
	resp := operatorRequest(t, http.MethodGet, "/admin/trace-sampler", nil)
	cfg := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET trace-sampler as operator: %d %s", resp.StatusCode, cfg)
	}
	resp = operatorRequest(t, http.MethodPost, "/admin/trace-sampler", []byte(cfg))
	if body := readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("POST trace-sampler as operator: %d %s", resp.StatusCode, body)
	}
}

// TestAdminTraceSampler_UnknownSampler asserts the wired route answers 400
// for a sampler value outside the accepted set, that the body does not echo
// the submitted value back to the caller, and that a subsequent GET shows
// the sampler was left unchanged. Mirrors TestAdminLogLevel_UnknownLevel for
// the log-level endpoint.
func TestAdminTraceSampler_UnknownSampler(t *testing.T) {
	getResp := operatorRequest(t, http.MethodGet, "/admin/trace-sampler", nil)
	original := readBody(t, getResp)
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET trace-sampler as operator: %d %s", getResp.StatusCode, original)
	}

	postResp := operatorRequest(t, http.MethodPost, "/admin/trace-sampler", []byte(`{"sampler":"bogus"}`))
	defer postResp.Body.Close()
	body, err := io.ReadAll(postResp.Body)
	if err != nil {
		t.Fatalf("read POST body: %v", err)
	}
	if postResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST unknown sampler: status=%d, want 400; body=%s", postResp.StatusCode, body)
	}
	if ct := postResp.Header.Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("content-type: got %q, want application/problem+json", ct)
	}
	var pd struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(body, &pd); err != nil {
		t.Fatalf("unmarshal ProblemDetail: %v; body=%s", err, body)
	}
	if got := fmt.Sprintf("%v", pd.Properties["errorCode"]); got != "BAD_REQUEST" {
		t.Fatalf("errorCode: got %q, want BAD_REQUEST; body=%s", got, body)
	}
	if strings.Contains(string(body), "bogus") {
		t.Fatalf("400 body echoes the submitted value: %s", body)
	}

	getResp2 := operatorRequest(t, http.MethodGet, "/admin/trace-sampler", nil)
	now := readBody(t, getResp2)
	if getResp2.StatusCode != http.StatusOK {
		t.Fatalf("GET trace-sampler after refused POST: %d %s", getResp2.StatusCode, now)
	}
	if now != original {
		t.Errorf("GET after a refused unknown sampler: %s, want unchanged %s", now, original)
	}
}
