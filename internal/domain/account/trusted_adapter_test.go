package account_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
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
	return account.New(newTestKeyStore(t), newTestTrustedStore(t), nil, feats, auth.OperatorGuard{})
}

func TestRegisterTrustedKey_Happy(t *testing.T) {
	h := enabledHandler(t)
	body, _ := json.Marshal(genapi.RegisterTrustedKeyRequestDto{KeyId: "k1", Jwk: rsaJWK(t, "k1")})
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
	h := account.New(newTestKeyStore(t), newTestTrustedStore(t), nil, feats, auth.OperatorGuard{})
	body, _ := json.Marshal(genapi.RegisterTrustedKeyRequestDto{KeyId: "k1", Jwk: rsaJWK(t, "k1")})
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
	body, _ := json.Marshal(genapi.RegisterTrustedKeyRequestDto{KeyId: "good", Jwk: jwk})
	w := httptest.NewRecorder()
	h.RegisterTrustedKey(w, adminReq(t, "POST", "/", body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", w.Code)
	}
}

func TestRegisterTrustedKey_NonRSA_400_UnsupportedKeyType(t *testing.T) {
	h := enabledHandler(t)
	body, _ := json.Marshal(genapi.RegisterTrustedKeyRequestDto{KeyId: "k1", Jwk: map[string]interface{}{"kty": "EC", "kid": "k1", "crv": "P-256", "x": "abc", "y": "def"}})
	w := httptest.NewRecorder()
	h.RegisterTrustedKey(w, adminReq(t, "POST", "/", body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", w.Code)
	}
	commontest.ExpectErrorCode(t, w.Result(), "UNSUPPORTED_KEY_TYPE")
}

// Key ids are unique per tenant: registering a kid another tenant already
// holds succeeds, and leaves the other tenant's key untouched.
func TestRegisterTrustedKey_SameKidInTwoTenants(t *testing.T) {
	ts := newTestTrustedStore(t)
	theirs := &auth.TrustedKey{KID: "shared", TenantID: spi.TenantID("tenant-a"), PublicKey: mkRSAPub(t), Active: true, ValidFrom: time.Now()}
	if err := ts.Register(context.Background(), theirs, false); err != nil {
		t.Fatal(err)
	}
	feats := auth.DefaultIAMFeatures()
	feats.TrustedKeyRegistrationEnabled = true
	h := account.New(newTestKeyStore(t), ts, nil, feats, auth.OperatorGuard{})
	uc := &spi.UserContext{UserID: "u", UserName: "u", Tenant: spi.Tenant{ID: "tenant-b"}, Roles: []string{"ROLE_ADMIN"}}
	body, _ := json.Marshal(genapi.RegisterTrustedKeyRequestDto{KeyId: "shared", Jwk: rsaJWK(t, "shared")})
	req := httptest.NewRequest("POST", "/", bytes.NewReader(body)).WithContext(spi.WithUserContext(httptest.NewRequest("POST", "/", nil).Context(), uc))
	w := httptest.NewRecorder()
	h.RegisterTrustedKey(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	got, err := ts.Get(context.Background(), "tenant-a", "shared")
	if err != nil || got.PublicKey.N.Cmp(theirs.PublicKey.N) != 0 {
		t.Fatalf("tenant-a's key changed: %+v, %v", got, err)
	}
	if _, err := ts.Get(context.Background(), "tenant-b", "shared"); err != nil {
		t.Fatalf("tenant-b's key not registered: %v", err)
	}
}

func TestListTrustedKeys_TenantScoped(t *testing.T) {
	ts := newTestTrustedStore(t)
	mine := &auth.TrustedKey{KID: "mine", TenantID: spi.TenantID("t1"), PublicKey: mkRSAPub(t), Active: true, ValidFrom: time.Now(), JWK: map[string]any{"kty": "RSA", "kid": "mine"}}
	theirs := &auth.TrustedKey{KID: "theirs", TenantID: spi.TenantID("other"), PublicKey: mkRSAPub(t), Active: true, ValidFrom: time.Now(), JWK: map[string]any{"kty": "RSA", "kid": "theirs"}}
	_ = ts.Register(context.Background(), mine, false)
	_ = ts.Register(context.Background(), theirs, false)
	feats := auth.DefaultIAMFeatures()
	feats.TrustedKeyRegistrationEnabled = true
	h := account.New(newTestKeyStore(t), ts, nil, feats, auth.OperatorGuard{})
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
	ts := newTestTrustedStore(t)
	tk := &auth.TrustedKey{KID: "k", TenantID: spi.TenantID("other"), PublicKey: mkRSAPub(t), Active: true, ValidFrom: time.Now()}
	_ = ts.Register(context.Background(), tk, false)
	feats := auth.DefaultIAMFeatures()
	feats.TrustedKeyRegistrationEnabled = true
	h := account.New(newTestKeyStore(t), ts, nil, feats, auth.OperatorGuard{})
	w := httptest.NewRecorder()
	h.DeleteTrustedKey(w, adminReq(t, "DELETE", "/", nil), "k")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d", w.Code)
	}
}

// Invalidation ends the key at once, and the request has no body: a stray
// gracePeriodSec is not read.
func TestInvalidateTrustedKey_EndsAtOnce(t *testing.T) {
	for name, body := range map[string][]byte{"no-body": nil, "stray-grace": []byte(`{"gracePeriodSec":3600}`)} {
		t.Run(name, func(t *testing.T) {
			ts := newTestTrustedStore(t)
			tk := &auth.TrustedKey{KID: "k", TenantID: spi.TenantID("t1"), PublicKey: mkRSAPub(t), Active: true, ValidFrom: time.Now().Add(-time.Minute), JWK: map[string]any{"kty": "RSA", "kid": "k"}}
			if err := ts.Register(context.Background(), tk, false); err != nil {
				t.Fatal(err)
			}
			feats := auth.DefaultIAMFeatures()
			feats.TrustedKeyRegistrationEnabled = true
			h := account.New(newTestKeyStore(t), ts, nil, feats, auth.OperatorGuard{})
			w := httptest.NewRecorder()
			h.InvalidateTrustedKey(w, adminReq(t, "POST", "/", body), "k")
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if _, err := ts.GetForVerification(context.Background(), spi.TenantID("t1"), "k"); !errors.Is(err, auth.ErrTrustedKeyNotFound) {
				t.Errorf("invalidated key still verifies: err = %v", err)
			}
		})
	}
}

func TestReactivateTrustedKey_RequiresValidTo(t *testing.T) {
	ts := newTestTrustedStore(t)
	past := time.Now().Add(-1 * time.Hour)
	tk := &auth.TrustedKey{KID: "k", TenantID: spi.TenantID("t1"), PublicKey: mkRSAPub(t), Active: false, ValidFrom: past, ValidTo: &past, JWK: map[string]any{"kty": "RSA", "kid": "k"}}
	_ = ts.Register(context.Background(), tk, false)
	feats := auth.DefaultIAMFeatures()
	feats.TrustedKeyRegistrationEnabled = true
	h := account.New(newTestKeyStore(t), ts, nil, feats, auth.OperatorGuard{})
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

func TestRegisterTrustedKey_ResponseIncludesActiveTrue(t *testing.T) {
	h := enabledHandler(t)
	body, _ := json.Marshal(genapi.RegisterTrustedKeyRequestDto{KeyId: "k1", Jwk: rsaJWK(t, "k1")})
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
	ts := newTestTrustedStore(t)
	tk := &auth.TrustedKey{
		KID: "k", TenantID: spi.TenantID("t1"), PublicKey: mkRSAPub(t), Active: true, ValidFrom: time.Now(),
		JWK: map[string]any{"kty": "RSA", "kid": "k"},
	}
	_ = ts.Register(context.Background(), tk, false)
	_ = ts.Invalidate(context.Background(), spi.TenantID("t1"), "k")
	feats := auth.DefaultIAMFeatures()
	feats.TrustedKeyRegistrationEnabled = true
	h := account.New(newTestKeyStore(t), ts, nil, feats, auth.OperatorGuard{})
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
	ts := newTestTrustedStore(t)
	past := time.Now().Add(-1 * time.Hour)
	tk := &auth.TrustedKey{
		KID: "k", TenantID: spi.TenantID("t1"), PublicKey: mkRSAPub(t), Active: false, ValidFrom: past, ValidTo: &past,
		JWK: map[string]any{"kty": "RSA", "kid": "k"},
	}
	_ = ts.Register(context.Background(), tk, false)
	feats := auth.DefaultIAMFeatures()
	feats.TrustedKeyRegistrationEnabled = true
	h := account.New(newTestKeyStore(t), ts, nil, feats, auth.OperatorGuard{})
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

// Reactivating a trusted key at the per-tenant cap is 400
// TRUSTED_KEY_CAP_REACHED, the same answer registration gives — not a 5xx.
func TestReactivateTrustedKey_AtCap_400(t *testing.T) {
	ts := auth.NewKVTrustedKeyStore(newMemoryKV(t), 1)
	past := time.Now().Add(-1 * time.Hour)
	future := time.Now().Add(time.Hour)
	_ = ts.Register(context.Background(), &auth.TrustedKey{
		KID: "old", TenantID: spi.TenantID("t1"), PublicKey: mkRSAPub(t), Active: false, ValidFrom: past.Add(-time.Hour), ValidTo: &past,
	}, false)
	_ = ts.Register(context.Background(), &auth.TrustedKey{
		KID: "live", TenantID: spi.TenantID("t1"), PublicKey: mkRSAPub(t), Active: true, ValidFrom: past, ValidTo: &future,
	}, false)
	feats := auth.DefaultIAMFeatures()
	feats.TrustedKeyRegistrationEnabled = true
	h := account.New(newTestKeyStore(t), ts, nil, feats, auth.OperatorGuard{})

	body, _ := json.Marshal(genapi.ReactivateKeyRequestDto{ValidTo: time.Now().Add(24 * time.Hour)})
	w := httptest.NewRecorder()
	h.ReactivateTrustedKey(w, adminReq(t, "POST", "/", body), "old")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400; body=%s", w.Code, w.Body.String())
	}
	commontest.ExpectErrorCode(t, resultResp(w), "TRUSTED_KEY_CAP_REACHED")
}

// Regression-lock: nil trustedKeyStore (mock IAM mode wiring) must return 501
// NOT_IMPLEMENTED, never panic. The feature flag is enabled to bypass
// FEATURE_DISABLED and reach the nil-store guard. All 5 trusted-key handlers
// are covered so an accidental removal of the requireTrustedKeyStore guard is
// caught per-handler.
func TestTrustedAdapter_NilStoreReturns501_AllHandlers(t *testing.T) {
	feats := auth.DefaultIAMFeatures()
	feats.TrustedKeyRegistrationEnabled = true // bypass FEATURE_DISABLED so we hit the nil-store guard
	h := account.New(nil, nil, nil, feats, auth.OperatorGuard{})
	cases := []struct {
		name string
		call func(w http.ResponseWriter)
	}{
		{"RegisterTrustedKey", func(w http.ResponseWriter) {
			body, _ := json.Marshal(genapi.RegisterTrustedKeyRequestDto{KeyId: "k", Jwk: rsaJWK(t, "k")})
			h.RegisterTrustedKey(w, adminReq(t, "POST", "/", body))
		}},
		{"ListTrustedKeys", func(w http.ResponseWriter) {
			h.ListTrustedKeys(w, adminReq(t, "GET", "/", nil))
		}},
		{"DeleteTrustedKey", func(w http.ResponseWriter) {
			h.DeleteTrustedKey(w, adminReq(t, "DELETE", "/", nil), "k")
		}},
		{"InvalidateTrustedKey", func(w http.ResponseWriter) {
			h.InvalidateTrustedKey(w, adminReq(t, "POST", "/", []byte(`{}`)), "k")
		}},
		{"ReactivateTrustedKey", func(w http.ResponseWriter) {
			body, _ := json.Marshal(genapi.ReactivateKeyRequestDto{ValidTo: time.Now().Add(24 * time.Hour)})
			h.ReactivateTrustedKey(w, adminReq(t, "POST", "/", body), "k")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			tc.call(w)
			if w.Code != http.StatusNotImplemented {
				t.Errorf("status=%d want 501 NOT_IMPLEMENTED", w.Code)
			}
		})
	}
}

// Trusted keys have no audience: the response does not carry one.
func TestRegisterTrustedKey_ResponseHasNoAudience(t *testing.T) {
	h := enabledHandler(t)
	body, _ := json.Marshal(genapi.RegisterTrustedKeyRequestDto{KeyId: "k1", Jwk: rsaJWK(t, "k1")})
	w := httptest.NewRecorder()
	h.RegisterTrustedKey(w, adminReq(t, "POST", "/", body))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if _, has := resp["audience"]; has {
		t.Errorf("response carries an audience: %s", w.Body.String())
	}
}

// Regression-lock test: same-tenant re-register with same keyId is a silent
// upsert (200, no error). Cloud does atomic delete-and-replace transactionally;
// cyoda-go preserves the existing silent-upsert behaviour in KVTrustedKeyStore.Register.
// Spec §3.2 #8.
func TestRegression_SameTenantSilentUpsert(t *testing.T) {
	h := enabledHandler(t)
	for i := 0; i < 2; i++ {
		body, _ := json.Marshal(genapi.RegisterTrustedKeyRequestDto{KeyId: "k", Jwk: rsaJWK(t, "k")})
		w := httptest.NewRecorder()
		h.RegisterTrustedKey(w, adminReq(t, "POST", "/", body))
		if w.Code != http.StatusOK {
			t.Errorf("iteration %d: status=%d", i, w.Code)
		}
	}
}

// 401/403 coverage for trusted-key endpoints.
// Mirrors the keypair-adapter pattern: RequireAdmin must reject missing
// UserContext (401) and missing ROLE_ADMIN (403) on every handler.

func TestRegisterTrustedKey_401_NoAuth(t *testing.T) {
	h := enabledHandler(t)
	body, _ := json.Marshal(genapi.RegisterTrustedKeyRequestDto{KeyId: "k", Jwk: rsaJWK(t, "k")})
	req := httptest.NewRequest("POST", "/oauth/keys/trusted", bytes.NewReader(body))
	// No UserContext attached — must return 401.
	w := httptest.NewRecorder()
	h.RegisterTrustedKey(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status=%d want 401", w.Code)
	}
}

func TestRegisterTrustedKey_403_NonAdmin(t *testing.T) {
	h := enabledHandler(t)
	body, _ := json.Marshal(genapi.RegisterTrustedKeyRequestDto{KeyId: "k", Jwk: rsaJWK(t, "k")})
	uc := &spi.UserContext{UserID: "u", UserName: "u", Tenant: spi.Tenant{ID: "t1"}, Roles: []string{"ROLE_M2M"}}
	req := httptest.NewRequest("POST", "/oauth/keys/trusted", bytes.NewReader(body))
	req = req.WithContext(spi.WithUserContext(req.Context(), uc))
	w := httptest.NewRecorder()
	h.RegisterTrustedKey(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("status=%d want 403", w.Code)
	}
}

func TestListTrustedKeys_401_NoAuth(t *testing.T) {
	h := enabledHandler(t)
	req := httptest.NewRequest("GET", "/oauth/keys/trusted", nil)
	w := httptest.NewRecorder()
	h.ListTrustedKeys(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status=%d want 401", w.Code)
	}
}

func TestListTrustedKeys_403_NonAdmin(t *testing.T) {
	h := enabledHandler(t)
	uc := &spi.UserContext{UserID: "u", UserName: "u", Tenant: spi.Tenant{ID: "t1"}, Roles: []string{"ROLE_M2M"}}
	req := httptest.NewRequest("GET", "/oauth/keys/trusted", nil)
	req = req.WithContext(spi.WithUserContext(req.Context(), uc))
	w := httptest.NewRecorder()
	h.ListTrustedKeys(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("status=%d want 403", w.Code)
	}
}

func TestRegisterTrustedKey_UnstorableTime_400(t *testing.T) {
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	jwk, _ := json.Marshal(rsaJWK(t, "k1"))
	for _, c := range []struct{ name, window, field string }{
		{"validTo year 10000", `"validTo":"` + year10000JSON + `"`, "validTo"},
		{"validFrom year -1", `"validFrom":"` + yearMinusJSON + `","validTo":"` + future + `"`, "validFrom"},
		{"default validTo year 10000", `"validFrom":"` + lastDayJSON + `"`, "validTo"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ts := newTestTrustedStore(t)
			feats := auth.DefaultIAMFeatures()
			feats.TrustedKeyRegistrationEnabled = true
			h := account.New(newTestKeyStore(t), ts, nil, feats, auth.OperatorGuard{})
			body := `{"keyId":"k1","jwk":` + string(jwk) + `,` + c.window + `}`
			w := httptest.NewRecorder()
			h.RegisterTrustedKey(w, adminReq(t, "POST", "/oauth/keys/trusted", []byte(body)))
			expectOutOfRange(t, w, c.field)
			if got, _ := ts.List(context.Background(), spi.TenantID("t1")); len(got) != 0 {
				t.Fatalf("%d keys registered", len(got))
			}
		})
	}
}

func TestReactivateTrustedKey_UnstorableTime_400(t *testing.T) {
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	for _, c := range []struct{ name, body, field string }{
		{"validTo year 10000", `{"validTo":"` + year10000JSON + `"}`, "validTo"},
		{"validFrom year -1", `{"validFrom":"` + yearMinusJSON + `","validTo":"` + future + `"}`, "validFrom"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ts := newTestTrustedStore(t)
			tk := &auth.TrustedKey{KID: "k", TenantID: spi.TenantID("t1"), PublicKey: mkRSAPub(t), Active: true, ValidFrom: time.Now(), JWK: map[string]any{"kty": "RSA", "kid": "k"}}
			if err := ts.Register(context.Background(), tk, false); err != nil {
				t.Fatal(err)
			}
			feats := auth.DefaultIAMFeatures()
			feats.TrustedKeyRegistrationEnabled = true
			h := account.New(newTestKeyStore(t), ts, nil, feats, auth.OperatorGuard{})
			w := httptest.NewRecorder()
			h.ReactivateTrustedKey(w, adminReq(t, "POST", "/", []byte(c.body)), "k")
			expectOutOfRange(t, w, c.field)
		})
	}
}
