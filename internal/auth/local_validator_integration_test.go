package auth_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

// TestIntegration_JWTMode_LocalKeySource_NoHTTPFetch proves the hardened
// default wiring: validator built via NewValidatorFromSource + LocalKeySource
// validates tokens minted by the same authSvc without making any HTTP call,
// even if a JWKS endpoint is also exposed. That is the invariant: there is
// no loopback fetch to MITM because there is no fetch at all.
func TestIntegration_JWTMode_LocalKeySource_NoHTTPFetch(t *testing.T) {
	svc := newTestAuthService(t, auth.AuthConfig{
		SigningKeyPEM: generateTestPEM(t),
		Issuer:        "cyoda",
		ExpirySeconds: 3600,
	})

	secret, err := svc.M2MClientStore().Create(
		systemCtx(), "tenant-1", "CLIENT1", "CLIENT1", []string{"ROLE_USER"}, false,
	)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Serve only the token endpoint. Crucially, no JWKS endpoint is exposed —
	// if the validator tried to fetch one, this test would fail.
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		svc.Handler().ServeHTTP(w, r)
	}))
	defer tokenSrv.Close()

	// Mint a token via the normal token endpoint. Client credentials are
	// conveyed via HTTP Basic, matching the existing integration pattern.
	req, _ := http.NewRequest("POST", tokenSrv.URL+"/oauth/token",
		strings.NewReader("grant_type=client_credentials"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Basic "+
		base64.StdEncoding.EncodeToString([]byte("CLIENT1:"+secret)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("token request failed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("token endpoint returned %d: %s", resp.StatusCode, body)
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		t.Fatalf("decode token: %v", err)
	}
	if tokenResp.AccessToken == "" {
		t.Fatal("empty access token")
	}

	// Build the validator the same way app.go does: LocalKeySource wrapping
	// the in-process KeyStore. No JWKS URL, no http.Client.
	validator := auth.NewValidatorFromSource(auth.NewLocalKeySource(svc.KeyStore()), svc.Issuer())

	uc, _, err := validator.Validate(tokenResp.AccessToken)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if uc == nil || uc.UserID != "CLIENT1" {
		t.Fatalf("unexpected user context: %+v", uc)
	}
}

// TestIntegration_TokenStopsVerifyingWhenItsKeyPairWindowEnds: through the
// validator the server wires, a token signed by a key pair verifies while the
// key pair is inside its window and is rejected once the window has ended,
// even though the key pair is still marked active.
func TestIntegration_TokenStopsVerifyingWhenItsKeyPairWindowEnds(t *testing.T) {
	svc := newTestAuthService(t, auth.AuthConfig{
		SigningKeyPEM: generateTestPEM(t),
		Issuer:        "cyoda",
		ExpirySeconds: 3600,
	})
	ctx := systemCtx()
	now := time.Now()
	from := now.Add(-2 * time.Hour)
	issued, err := svc.KeyStore().Issue(ctx, auth.IssueRequest{ValidFrom: from, ValidTo: now.Add(time.Hour)})
	if err != nil {
		t.Fatalf("issue key pair: %v", err)
	}
	kp, signer, err := svc.KeyStore().Signer()
	if err != nil || kp.KID != issued.KID {
		t.Fatalf("signer = %v, %v; want the issued key pair %s", kp, err, issued.KID)
	}
	tok, err := auth.Sign(context.Background(), map[string]any{
		"iss": "cyoda", "sub": "user-1", "caas_user_id": "user-1", "caas_org_id": "tenant-1",
		"iat": float64(now.Unix()), "exp": float64(now.Add(time.Hour).Unix()),
	}, signer, kp.KID)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	validator := auth.NewValidatorFromSource(auth.NewLocalKeySource(svc.KeyStore()), svc.Issuer())

	if _, _, err := validator.Validate(tok); err != nil {
		t.Fatalf("token rejected while its key pair is in its window: %v", err)
	}
	// End the window; the key pair stays active.
	if _, err := svc.KeyStore().Reactivate(ctx, kp.KID, from, now.Add(-time.Minute)); err != nil {
		t.Fatalf("end the window: %v", err)
	}
	if _, _, err := validator.Validate(tok); err == nil {
		t.Fatal("token accepted after its key pair's window ended")
	}
}
