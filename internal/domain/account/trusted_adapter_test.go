package account_test

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
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

func rsaJWK(t *testing.T, kid string) map[string]interface{} {
	t.Helper()
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	n := base64.RawURLEncoding.EncodeToString(priv.PublicKey.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(priv.PublicKey.E)).Bytes())
	return map[string]interface{}{"kty": "RSA", "kid": kid, "n": n, "e": e}
}

func enabledHandler(t *testing.T) *account.Handler {
	t.Helper()
	feats := auth.DefaultIAMFeatures()
	feats.TrustedKeyRegistrationEnabled = true
	return account.New(nil, nil, auth.NewInMemoryKeyStore(), auth.NewInMemoryTrustedKeyStore(), feats)
}

func TestRegisterTrustedKey_Happy(t *testing.T) {
	h := enabledHandler(t)
	body, _ := json.Marshal(genapi.RegisterTrustedKeyRequestDto{KeyId: "k1", Jwk: rsaJWK(t, "k1"), Audience: "human"})
	req := adminReq(t, "POST", "/oauth/keys/trusted", body)
	w := httptest.NewRecorder()
	h.RegisterTrustedKey(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp genapi.TrustedKeyResponseDto
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.KeyId != "k1" || resp.LegalEntityId != "t1" || resp.Jwk["kty"] != "RSA" {
		t.Errorf("resp: %+v", resp)
	}
}

func TestRegisterTrustedKey_FlagDisabled_404(t *testing.T) {
	feats := auth.DefaultIAMFeatures()
	h := account.New(nil, nil, auth.NewInMemoryKeyStore(), auth.NewInMemoryTrustedKeyStore(), feats)
	body, _ := json.Marshal(genapi.RegisterTrustedKeyRequestDto{KeyId: "k1", Jwk: rsaJWK(t, "k1"), Audience: "human"})
	w := httptest.NewRecorder()
	h.RegisterTrustedKey(w, adminReq(t, "POST", "/", body))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d", w.Code)
	}
	commontest.ExpectErrorCode(t, w.Result(), "FEATURE_DISABLED")
}

func TestRegisterTrustedKey_KidKeyIdMismatch_400(t *testing.T) {
	h := enabledHandler(t)
	jwk := rsaJWK(t, "evil")
	body, _ := json.Marshal(genapi.RegisterTrustedKeyRequestDto{KeyId: "good", Jwk: jwk, Audience: "human"})
	w := httptest.NewRecorder()
	h.RegisterTrustedKey(w, adminReq(t, "POST", "/", body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", w.Code)
	}
}

func TestRegisterTrustedKey_NonRSA_400_UnsupportedKeyType(t *testing.T) {
	h := enabledHandler(t)
	body, _ := json.Marshal(genapi.RegisterTrustedKeyRequestDto{KeyId: "k1", Jwk: map[string]interface{}{"kty": "EC", "kid": "k1", "crv": "P-256", "x": "abc", "y": "def"}, Audience: "human"})
	w := httptest.NewRecorder()
	h.RegisterTrustedKey(w, adminReq(t, "POST", "/", body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", w.Code)
	}
	commontest.ExpectErrorCode(t, w.Result(), "UNSUPPORTED_KEY_TYPE")
}

func TestRegisterTrustedKey_CrossTenantCollision_409(t *testing.T) {
	ts := auth.NewInMemoryTrustedKeyStore()
	pre := &auth.TrustedKey{KID: "shared", TenantID: spi.TenantID("tenant-a"), PublicKey: mkRSAPub(t), Audience: "human", Active: true, ValidFrom: time.Now()}
	_ = ts.Register(pre, auth.RotateOptions{})
	feats := auth.DefaultIAMFeatures()
	feats.TrustedKeyRegistrationEnabled = true
	h := account.New(nil, nil, auth.NewInMemoryKeyStore(), ts, feats)
	uc := &spi.UserContext{UserID: "u", UserName: "u", Tenant: spi.Tenant{ID: "tenant-b"}, Roles: []string{"ROLE_ADMIN"}}
	body, _ := json.Marshal(genapi.RegisterTrustedKeyRequestDto{KeyId: "shared", Jwk: rsaJWK(t, "shared"), Audience: "human"})
	req := httptest.NewRequest("POST", "/", bytes.NewReader(body)).WithContext(spi.WithUserContext(httptest.NewRequest("POST", "/", nil).Context(), uc))
	w := httptest.NewRecorder()
	h.RegisterTrustedKey(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("status=%d", w.Code)
	}
	commontest.ExpectErrorCode(t, w.Result(), "KEY_OWNED_BY_DIFFERENT_TENANT")
}
