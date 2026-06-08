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
	if w.Code != http.StatusOK {
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

func TestListTrustedKeys_TenantScoped(t *testing.T) {
	ts := auth.NewInMemoryTrustedKeyStore()
	mine := &auth.TrustedKey{KID: "mine", TenantID: spi.TenantID("t1"), PublicKey: mkRSAPub(t), Audience: "human", Active: true, ValidFrom: time.Now(), JWK: map[string]any{"kty": "RSA", "kid": "mine"}}
	theirs := &auth.TrustedKey{KID: "theirs", TenantID: spi.TenantID("other"), PublicKey: mkRSAPub(t), Audience: "human", Active: true, ValidFrom: time.Now(), JWK: map[string]any{"kty": "RSA", "kid": "theirs"}}
	_ = ts.Register(mine, auth.RotateOptions{})
	_ = ts.Register(theirs, auth.RotateOptions{})
	feats := auth.DefaultIAMFeatures()
	feats.TrustedKeyRegistrationEnabled = true
	h := account.New(nil, nil, auth.NewInMemoryKeyStore(), ts, feats)
	w := httptest.NewRecorder()
	h.ListTrustedKeys(w, adminReq(t, "GET", "/oauth/keys/trusted", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	var resp []genapi.TrustedKeyResponseDto
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp) != 1 || resp[0].KeyId != "mine" {
		t.Fatalf("expected only 'mine', got %+v", resp)
	}
}

func TestDeleteTrustedKey_CrossTenant_404(t *testing.T) {
	ts := auth.NewInMemoryTrustedKeyStore()
	tk := &auth.TrustedKey{KID: "k", TenantID: spi.TenantID("other"), PublicKey: mkRSAPub(t), Audience: "human", Active: true, ValidFrom: time.Now()}
	_ = ts.Register(tk, auth.RotateOptions{})
	feats := auth.DefaultIAMFeatures()
	feats.TrustedKeyRegistrationEnabled = true
	h := account.New(nil, nil, auth.NewInMemoryKeyStore(), ts, feats)
	w := httptest.NewRecorder()
	h.DeleteTrustedKey(w, adminReq(t, "DELETE", "/", nil), "k")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d", w.Code)
	}
}

func TestInvalidateTrustedKey_Grace(t *testing.T) {
	ts := auth.NewInMemoryTrustedKeyStore()
	tk := &auth.TrustedKey{KID: "k", TenantID: spi.TenantID("t1"), PublicKey: mkRSAPub(t), Audience: "human", Active: true, ValidFrom: time.Now(), JWK: map[string]any{"kty": "RSA", "kid": "k"}}
	_ = ts.Register(tk, auth.RotateOptions{})
	feats := auth.DefaultIAMFeatures()
	feats.TrustedKeyRegistrationEnabled = true
	h := account.New(nil, nil, auth.NewInMemoryKeyStore(), ts, feats)
	body, _ := json.Marshal(genapi.InvalidateKeyRequestDto{GracePeriodSec: ptrInt64(60)})
	w := httptest.NewRecorder()
	h.InvalidateTrustedKey(w, adminReq(t, "POST", "/", body), "k")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	got, _ := ts.Get(spi.TenantID("t1"), "k")
	if got.Active || got.ValidTo == nil {
		t.Errorf("expected invalidated; got %+v", got)
	}
}

func TestReactivateTrustedKey_RequiresValidTo(t *testing.T) {
	ts := auth.NewInMemoryTrustedKeyStore()
	past := time.Now().Add(-1 * time.Hour)
	tk := &auth.TrustedKey{KID: "k", TenantID: spi.TenantID("t1"), PublicKey: mkRSAPub(t), Audience: "human", Active: false, ValidFrom: past, ValidTo: &past, JWK: map[string]any{"kty": "RSA", "kid": "k"}}
	_ = ts.Register(tk, auth.RotateOptions{})
	feats := auth.DefaultIAMFeatures()
	feats.TrustedKeyRegistrationEnabled = true
	h := account.New(nil, nil, auth.NewInMemoryKeyStore(), ts, feats)
	body, _ := json.Marshal(genapi.ReactivateKeyRequestDto{ValidTo: time.Now().Add(24 * time.Hour)})
	w := httptest.NewRecorder()
	h.ReactivateTrustedKey(w, adminReq(t, "POST", "/", body), "k")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var reactivateResp genapi.TrustedKeyResponseDto
	if err := json.Unmarshal(w.Body.Bytes(), &reactivateResp); err != nil {
		t.Fatalf("reactivate response decode: %v", err)
	}
	if reactivateResp.KeyId == "" {
		t.Error("reactivate response missing keyId")
	}
}

func ptrInt64(v int64) *int64 { return &v }

func TestRegisterTrustedKey_ResponseIncludesActiveTrue(t *testing.T) {
	h := enabledHandler(t)
	body, _ := json.Marshal(genapi.RegisterTrustedKeyRequestDto{KeyId: "k1", Jwk: rsaJWK(t, "k1"), Audience: "human"})
	w := httptest.NewRecorder()
	h.RegisterTrustedKey(w, adminReq(t, "POST", "/", body))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp genapi.TrustedKeyResponseDto
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Active {
		t.Error("newly registered trusted key should have active=true")
	}
}

func TestListTrustedKeys_InvalidatedKeyHasActiveFalse(t *testing.T) {
	ts := auth.NewInMemoryTrustedKeyStore()
	tk := &auth.TrustedKey{
		KID: "k", TenantID: spi.TenantID("t1"), PublicKey: mkRSAPub(t),
		Audience: "human", Active: true, ValidFrom: time.Now(),
		JWK: map[string]any{"kty": "RSA", "kid": "k"},
	}
	_ = ts.Register(tk, auth.RotateOptions{})
	_ = ts.Invalidate(spi.TenantID("t1"), "k", 0)
	feats := auth.DefaultIAMFeatures()
	feats.TrustedKeyRegistrationEnabled = true
	h := account.New(nil, nil, auth.NewInMemoryKeyStore(), ts, feats)
	w := httptest.NewRecorder()
	h.ListTrustedKeys(w, adminReq(t, "GET", "/oauth/keys/trusted", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp []genapi.TrustedKeyResponseDto
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp) != 1 {
		t.Fatalf("expected 1 key, got %d", len(resp))
	}
	if resp[0].Active {
		t.Error("invalidated trusted key should have active=false")
	}
}

func TestReactivateTrustedKey_ResponseIncludesActiveTrue(t *testing.T) {
	ts := auth.NewInMemoryTrustedKeyStore()
	past := time.Now().Add(-1 * time.Hour)
	tk := &auth.TrustedKey{
		KID: "k", TenantID: spi.TenantID("t1"), PublicKey: mkRSAPub(t),
		Audience: "human", Active: false, ValidFrom: past, ValidTo: &past,
		JWK: map[string]any{"kty": "RSA", "kid": "k"},
	}
	_ = ts.Register(tk, auth.RotateOptions{})
	feats := auth.DefaultIAMFeatures()
	feats.TrustedKeyRegistrationEnabled = true
	h := account.New(nil, nil, auth.NewInMemoryKeyStore(), ts, feats)
	body, _ := json.Marshal(genapi.ReactivateKeyRequestDto{ValidTo: time.Now().Add(24 * time.Hour)})
	w := httptest.NewRecorder()
	h.ReactivateTrustedKey(w, adminReq(t, "POST", "/", body), "k")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp genapi.TrustedKeyResponseDto
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Active {
		t.Error("reactivated trusted key response should have active=true")
	}
}

// Regression-lock: nil trustedKeyStore (mock IAM mode wiring) must return 501
// NOT_IMPLEMENTED, never panic. The feature flag is enabled to bypass
// FEATURE_DISABLED and reach the nil-store guard.
func TestTrustedAdapter_NilStoreInMockMode_Returns501(t *testing.T) {
	feats := auth.DefaultIAMFeatures()
	feats.TrustedKeyRegistrationEnabled = true // bypass FEATURE_DISABLED to reach nil-store guard
	h := account.New(nil, nil, nil, nil, feats)
	req := adminReq(t, "GET", "/oauth/keys/trusted", nil)
	w := httptest.NewRecorder()
	h.ListTrustedKeys(w, req)
	if w.Code != http.StatusNotImplemented {
		t.Errorf("want 501 NOT_IMPLEMENTED; got %d", w.Code)
	}
	commontest.ExpectErrorCode(t, w.Result(), "NOT_IMPLEMENTED")
}

// Regression-lock test: cyoda-go honors the request `audience` and round-trips
// it on the response. Cloud always coerces to "human".
// Spec §3.2 #4.
func TestRegression_TrustedAudienceRoundTrip(t *testing.T) {
	h := enabledHandler(t)
	body, _ := json.Marshal(genapi.RegisterTrustedKeyRequestDto{KeyId: "k1", Jwk: rsaJWK(t, "k1"), Audience: "client"})
	w := httptest.NewRecorder()
	h.RegisterTrustedKey(w, adminReq(t, "POST", "/", body))
	var resp genapi.TrustedKeyResponseDto
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if string(resp.Audience) != "client" {
		t.Errorf("expected audience='client' (cloud coerces to 'human'); got %q", resp.Audience)
	}
}

// Regression-lock test: same-tenant re-register with same keyId is a silent
// upsert (200, no error). Cloud does atomic delete-and-replace transactionally;
// cyoda-go preserves the existing silent-upsert behaviour in KVTrustedKeyStore.Register.
// Spec §3.2 #8.
func TestRegression_SameTenantSilentUpsert(t *testing.T) {
	h := enabledHandler(t)
	for i := 0; i < 2; i++ {
		body, _ := json.Marshal(genapi.RegisterTrustedKeyRequestDto{KeyId: "k", Jwk: rsaJWK(t, "k"), Audience: "human"})
		w := httptest.NewRecorder()
		h.RegisterTrustedKey(w, adminReq(t, "POST", "/", body))
		if w.Code != http.StatusOK {
			t.Errorf("iteration %d: status=%d", i, w.Code)
		}
	}
}
