package e2e_test

import (
	"net/http"
	"testing"
)

// gatedOp is a single IAM-gated endpoint identified by HTTP method and
// server-relative path (with the /api context prefix).
type gatedOp struct {
	method string
	path   string
	opID   string // operationId — used as t.Run label and in failure messages
}

// all21GatedOps lists every op gated behind the IAM subsystem.
// Path-parameter placeholders:
//   - string keyId / clientId → any alphanumeric segment; gate fires before
//     format checks in the handler.
//   - {id} for OIDC providers → valid UUID required (parsed by generated code
//     before reaching the handler stub).
//   - getCurrentJwtKeyPair → ?audience=human required (generated code enforces
//     the required query param before calling the handler).
var all21GatedOps = []gatedOp{
	// 4 M2M ops (requireM2MStore → 501)
	{http.MethodGet, "/api/clients", "listTechnicalUsers"},
	{http.MethodPost, "/api/clients", "createTechnicalUser"},
	{http.MethodDelete, "/api/clients/testclient", "deleteTechnicalUser"},
	{http.MethodPut, "/api/clients/testclient/secret", "resetTechnicalUserSecret"},
	// 5 keypair ops (requireKeyStore → 501)
	{http.MethodPost, "/api/oauth/keys/keypair", "issueJwtKeyPair"},
	{http.MethodGet, "/api/oauth/keys/keypair/current?audience=human", "getCurrentJwtKeyPair"},
	{http.MethodDelete, "/api/oauth/keys/keypair/anykeyid", "deleteJwtKeyPair"},
	{http.MethodPost, "/api/oauth/keys/keypair/anykeyid/invalidate", "invalidateJwtKeyPair"},
	{http.MethodPost, "/api/oauth/keys/keypair/anykeyid/reactivate", "reactivateJwtKeyPair"},
	// 5 trusted-key ops (feature ON → requireTrustedKeyStore → 501)
	{http.MethodGet, "/api/oauth/keys/trusted", "listTrustedKeys"},
	{http.MethodPost, "/api/oauth/keys/trusted", "registerTrustedKey"},
	{http.MethodDelete, "/api/oauth/keys/trusted/anykeyid", "deleteTrustedKey"},
	{http.MethodPost, "/api/oauth/keys/trusted/anykeyid/invalidate", "invalidateTrustedKey"},
	{http.MethodPost, "/api/oauth/keys/trusted/anykeyid/reactivate", "reactivateTrustedKey"},
	// 7 OIDC ops (oidc adapter nil → stub → 501; UUID required for {id})
	{http.MethodGet, "/api/oauth/oidc/providers", "listOidcProviders"},
	{http.MethodPost, "/api/oauth/oidc/providers", "registerOidcProvider"},
	{http.MethodPost, "/api/oauth/oidc/providers/reload", "reloadOidcProviders"},
	{http.MethodDelete, "/api/oauth/oidc/providers/00000000-0000-0000-0000-000000000000", "deleteOidcProvider"},
	{http.MethodPatch, "/api/oauth/oidc/providers/00000000-0000-0000-0000-000000000000", "updateOidcProvider"},
	{http.MethodPost, "/api/oauth/oidc/providers/00000000-0000-0000-0000-000000000000/invalidate", "invalidateOidcProvider"},
	{http.MethodPost, "/api/oauth/oidc/providers/00000000-0000-0000-0000-000000000000/reactivate", "reactivateOidcProvider"},
}

// trustedOps is the subset of all21GatedOps that are gated by the
// CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED feature flag.
var trustedOps = []gatedOp{
	{http.MethodGet, "/api/oauth/keys/trusted", "listTrustedKeys"},
	{http.MethodPost, "/api/oauth/keys/trusted", "registerTrustedKey"},
	{http.MethodDelete, "/api/oauth/keys/trusted/anykeyid", "deleteTrustedKey"},
	{http.MethodPost, "/api/oauth/keys/trusted/anykeyid/invalidate", "invalidateTrustedKey"},
	{http.MethodPost, "/api/oauth/keys/trusted/anykeyid/reactivate", "reactivateTrustedKey"},
}

// TestGated_MockIAM_All21Return501 verifies that every IAM-gated operation
// returns 501 NOT_IMPLEMENTED when running in mock IAM mode (IAM subsystem not
// active). The trusted-key feature gate is enabled so the trusted ops reach the
// nil-store gate (not the feature gate) and also return 501.
func TestGated_MockIAM_All21Return501(t *testing.T) {
	base, closeFn := newMockIAMServer(t)
	defer closeFn()

	for _, op := range all21GatedOps {
		op := op
		t.Run(op.opID, func(t *testing.T) {
			req, err := http.NewRequest(op.method, base+op.path, nil)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("do: %v", err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusNotImplemented {
				t.Errorf("%s %s: got %d, want 501 NOT_IMPLEMENTED", op.method, op.path, resp.StatusCode)
			}
		})
	}
}

// TestGated_Trusted_FeatureOff_Return404 verifies that the 5 trusted-key ops
// return 404 FEATURE_DISABLED when the trusted-key feature gate is disabled
// (CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED=false, the default).
// The feature gate fires before the nil-store gate, so the response is 404,
// not 501.
func TestGated_Trusted_FeatureOff_Return404(t *testing.T) {
	base, closeFn := newFeatureOffServer(t)
	defer closeFn()

	for _, op := range trustedOps {
		op := op
		t.Run(op.opID, func(t *testing.T) {
			req, err := http.NewRequest(op.method, base+op.path, nil)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("do: %v", err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("%s %s: got %d, want 404 FEATURE_DISABLED", op.method, op.path, resp.StatusCode)
			}
		})
	}
}
