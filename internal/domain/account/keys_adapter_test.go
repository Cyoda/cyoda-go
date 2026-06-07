package account_test

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
	"github.com/cyoda-platform/cyoda-go/internal/domain/account"
)

func adminUC() *spi.UserContext {
	return &spi.UserContext{
		UserID: "u", UserName: "u",
		Tenant: spi.Tenant{ID: "t1", Name: "t1"},
		Roles:  []string{"ROLE_ADMIN"},
	}
}

func adminReq(t *testing.T, method, path string, body []byte) *http.Request {
	t.Helper()
	var br *bytes.Reader
	if body != nil {
		br = bytes.NewReader(body)
	}
	var req *http.Request
	if br == nil {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, br)
	}
	return req.WithContext(spi.WithUserContext(req.Context(), adminUC()))
}

func resultResp(w *httptest.ResponseRecorder) *http.Response { return w.Result() }

func newHandler(t *testing.T) (*account.Handler, auth.KeyStore, auth.TrustedKeyStore) {
	t.Helper()
	ks := auth.NewInMemoryKeyStore()
	ts := auth.NewInMemoryTrustedKeyStore()
	h := account.New(nil, nil, ks, ts, auth.DefaultIAMFeatures())
	return h, ks, ts
}

func TestIssueJwtKeyPair_Happy(t *testing.T) {
	h, _, _ := newHandler(t)
	body, _ := json.Marshal(genapi.IssueJwtKeyPairRequestDto{Algorithm: "RS256", Audience: "client"})
	req := adminReq(t, "POST", "/oauth/keys/keypair", body)
	w := httptest.NewRecorder()
	h.IssueJwtKeyPair(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp genapi.JwtKeyPairResponseDto
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(resp.Algorithm) != "RS256" || resp.KeyId == "" {
		t.Errorf("resp: %+v", resp)
	}
	if _, err := base64.StdEncoding.DecodeString(resp.PublicKey); err != nil {
		t.Errorf("publicKey not base64-DER: %v", err)
	}
	if resp.ValidFrom.After(time.Now().Add(2 * time.Second)) {
		t.Error("validFrom in future")
	}
}

func TestIssueJwtKeyPair_RejectsNonRS256(t *testing.T) {
	h, _, _ := newHandler(t)
	body, _ := json.Marshal(genapi.IssueJwtKeyPairRequestDto{Algorithm: "ES256", Audience: "client"})
	req := adminReq(t, "POST", "/oauth/keys/keypair", body)
	w := httptest.NewRecorder()
	h.IssueJwtKeyPair(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", w.Code)
	}
	commontest.ExpectErrorCode(t, resultResp(w), "UNSUPPORTED_ALGORITHM")
	if ct := w.Header().Get("Content-Type"); !contains(ct, "application/problem+json") {
		t.Errorf("Content-Type=%q want application/problem+json", ct)
	}
}

func TestIssueJwtKeyPair_RejectsBadAudience(t *testing.T) {
	h, _, _ := newHandler(t)
	body := []byte(`{"algorithm":"RS256","audience":"robot"}`)
	req := adminReq(t, "POST", "/oauth/keys/keypair", body)
	w := httptest.NewRecorder()
	h.IssueJwtKeyPair(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", w.Code)
	}
}

func TestIssueJwtKeyPair_401_NoAuth(t *testing.T) {
	h, _, _ := newHandler(t)
	body, _ := json.Marshal(genapi.IssueJwtKeyPairRequestDto{Algorithm: "RS256", Audience: "client"})
	req := httptest.NewRequest("POST", "/oauth/keys/keypair", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.IssueJwtKeyPair(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status=%d want 401", w.Code)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func mkRSAKeyPair(t *testing.T, audience string) *auth.KeyPair {
	t.Helper()
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	return &auth.KeyPair{KID: "k", Audience: audience, Algorithm: "RS256", PublicKey: &priv.PublicKey, PrivateKey: priv, Active: true, ValidFrom: time.Now()}
}

func mkRSAPub(t *testing.T) *rsa.PublicKey {
	t.Helper()
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	return &priv.PublicKey
}

func TestGetCurrentJwtKeyPair_Happy(t *testing.T) {
	h, ks, _ := newHandler(t)
	_ = ks.Save(mkRSAKeyPair(t, "client"), auth.RotateOptions{})
	req := adminReq(t, "GET", "/oauth/keys/keypair/current?audience=client", nil)
	w := httptest.NewRecorder()
	h.GetCurrentJwtKeyPair(w, req, genapi.GetCurrentJwtKeyPairParams{Audience: "client"})
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestGetCurrentJwtKeyPair_404_NoKeyForAudience(t *testing.T) {
	h, _, _ := newHandler(t)
	req := adminReq(t, "GET", "/oauth/keys/keypair/current?audience=human", nil)
	w := httptest.NewRecorder()
	h.GetCurrentJwtKeyPair(w, req, genapi.GetCurrentJwtKeyPairParams{Audience: "human"})
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d", w.Code)
	}
	commontest.ExpectErrorCode(t, w.Result(), "KEYPAIR_NOT_FOUND")
}

func TestDeleteJwtKeyPair(t *testing.T) {
	h, ks, _ := newHandler(t)
	kp := mkRSAKeyPair(t, "client")
	_ = ks.Save(kp, auth.RotateOptions{})
	w := httptest.NewRecorder()
	h.DeleteJwtKeyPair(w, adminReq(t, "DELETE", "/", nil), "k")
	if w.Code != http.StatusNoContent {
		t.Fatalf("status=%d", w.Code)
	}
	if _, err := ks.Get("k"); err == nil {
		t.Error("expected deleted")
	}
}

func TestDeleteJwtKeyPair_404(t *testing.T) {
	h, _, _ := newHandler(t)
	w := httptest.NewRecorder()
	h.DeleteJwtKeyPair(w, adminReq(t, "DELETE", "/", nil), "missing")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d", w.Code)
	}
	commontest.ExpectErrorCode(t, w.Result(), "KEYPAIR_NOT_FOUND")
}

func TestInvalidateJwtKeyPair_GraceDefaultZero(t *testing.T) {
	h, ks, _ := newHandler(t)
	now := time.Now()
	kp := mkRSAKeyPair(t, "client")
	_ = ks.Save(kp, auth.RotateOptions{})
	w := httptest.NewRecorder()
	h.InvalidateJwtKeyPair(w, adminReq(t, "POST", "/", []byte(`{}`)), "k")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	got, _ := ks.Get("k")
	if got.ValidTo == nil || !got.ValidTo.Before(now.Add(2*time.Second)) {
		t.Errorf("expected ValidTo near now (grace=0); got %v", got.ValidTo)
	}
}

func TestInvalidateJwtKeyPair_NegativeGraceRejected(t *testing.T) {
	h, ks, _ := newHandler(t)
	_ = ks.Save(mkRSAKeyPair(t, "client"), auth.RotateOptions{})
	w := httptest.NewRecorder()
	h.InvalidateJwtKeyPair(w, adminReq(t, "POST", "/", []byte(`{"gracePeriodSec":-5}`)), "k")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", w.Code)
	}
}

func TestReactivateJwtKeyPair_RequiresFreshValidTo(t *testing.T) {
	h, ks, _ := newHandler(t)
	past := time.Now().Add(-1 * time.Hour)
	priv := &auth.KeyPair{KID: "k", Audience: "client", Algorithm: "RS256", PublicKey: mkRSAPub(t), Active: false, ValidFrom: past, ValidTo: &past}
	_ = ks.Save(priv, auth.RotateOptions{})

	w := httptest.NewRecorder()
	h.ReactivateJwtKeyPair(w, adminReq(t, "POST", "/", []byte(`{}`)), "k")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing validTo: status=%d", w.Code)
	}

	body, _ := json.Marshal(genapi.ReactivateKeyRequestDto{ValidTo: past})
	w = httptest.NewRecorder()
	h.ReactivateJwtKeyPair(w, adminReq(t, "POST", "/", body), "k")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("past validTo: status=%d", w.Code)
	}

	future := time.Now().Add(24 * time.Hour)
	body, _ = json.Marshal(genapi.ReactivateKeyRequestDto{ValidTo: future})
	w = httptest.NewRecorder()
	h.ReactivateJwtKeyPair(w, adminReq(t, "POST", "/", body), "k")
	if w.Code != http.StatusOK {
		t.Fatalf("fresh validTo: status=%d body=%s", w.Code, w.Body.String())
	}
}

// Regression-lock test: SUPER_USER must NOT be admitted. cyoda-go enforces
// ROLE_ADMIN only; cloud accepts ADMIN ∨ SUPER_USER.
// Spec §3.2 #2.
func TestRegression_RoleGate_RoleAdminOnly(t *testing.T) {
	h, _, _ := newHandler(t)
	uc := &spi.UserContext{UserID: "u", UserName: "u", Tenant: spi.Tenant{ID: "t1"}, Roles: []string{"SUPER_USER"}}
	req := httptest.NewRequest("GET", "/", nil).WithContext(spi.WithUserContext(httptest.NewRequest("GET", "/", nil).Context(), uc))
	w := httptest.NewRecorder()
	h.GetCurrentJwtKeyPair(w, req, genapi.GetCurrentJwtKeyPairParams{Audience: "client"})
	if w.Code != http.StatusForbidden {
		t.Errorf("SUPER_USER should not be admitted; want 403, got %d", w.Code)
	}
}

// Regression-lock test: validTo < validFrom must be rejected with 400.
// Cloud accepts and silently produces invalid keys.
// Spec §3.2 #6.
func TestRegression_StrictValidation_ValidToBeforeValidFrom(t *testing.T) {
	h, _, _ := newHandler(t)
	from := time.Now().Add(2 * time.Hour)
	to := time.Now().Add(1 * time.Hour)
	body, _ := json.Marshal(genapi.IssueJwtKeyPairRequestDto{Algorithm: "RS256", Audience: "client", ValidFrom: &from, ValidTo: &to})
	w := httptest.NewRecorder()
	h.IssueJwtKeyPair(w, adminReq(t, "POST", "/", body))
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for validTo<validFrom; got %d", w.Code)
	}
}

// §3.2 #3 Cross-tenant lifecycle 404 (not 403) — already covered by
// TestDeleteTrustedKey_CrossTenant_404 in trusted_adapter_test.go.
// §3.2 #5 Reactivate fresh validTo — covered by TestReactivateJwtKeyPair_RequiresFreshValidTo.
// §3.2 #6 negative gracePeriodSec — covered by TestInvalidateJwtKeyPair_NegativeGraceRejected.
// §3.2 #7 gracePeriodSec default 0 — covered by TestInvalidateJwtKeyPair_GraceDefaultZero.
// §3.2 #10 Non-RS256 rejected — covered by TestIssueJwtKeyPair_RejectsNonRS256 and
//
//	the table in internal/auth/keypair_signing_test.go.
//
// §3.2 #1 JWKS-retained — covered by TestE2E_GracePeriodRoundTrip.
