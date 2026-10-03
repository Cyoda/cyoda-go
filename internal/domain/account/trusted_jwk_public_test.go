package account_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
	"github.com/cyoda-platform/cyoda-go/internal/domain/account"
)

// problemDetail decodes the detail of a problem response.
func problemDetail(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var pd struct {
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &pd); err != nil {
		t.Fatalf("decode problem: %v; body=%s", err, w.Body.String())
	}
	return pd.Detail
}

// A JWK that carries a private member is refused, and the detail names the
// member: a trusted key is a public key, and the server must not hold the
// private half.
func TestRegisterTrustedKey_PrivateMember_400(t *testing.T) {
	for _, member := range []string{"d", "p", "q", "dp", "dq", "qi", "oth"} {
		t.Run(member, func(t *testing.T) {
			ts := newTestTrustedStore(t)
			feats := auth.DefaultIAMFeatures()
			feats.TrustedKeyRegistrationEnabled = true
			h := account.New(newTestKeyStore(t), ts, nil, feats, auth.OperatorGuard{})
			jwk := rsaJWK(t, "k1")
			jwk[member] = "AQAB"
			body, _ := json.Marshal(genapi.RegisterTrustedKeyRequestDto{KeyId: "k1", Jwk: jwk})
			w := httptest.NewRecorder()
			h.RegisterTrustedKey(w, adminReq(t, "POST", "/", body))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status=%d want 400; body=%s", w.Code, w.Body.String())
			}
			if d := problemDetail(t, w); !strings.Contains(d, `"`+member+`"`) {
				t.Errorf("detail %q does not name the member %q", d, member)
			}
			commontest.ExpectErrorCode(t, resultResp(w), "BAD_REQUEST")
			if _, err := ts.Get(context.Background(), spi.TenantID("t1"), "k1"); err == nil {
				t.Error("a refused key was stored")
			}
		})
	}
}

// An RSA modulus under 2048 bits is refused.
func TestRegisterTrustedKey_SmallModulus_400(t *testing.T) {
	h := enabledHandler(t)
	n := make([]byte, 255) // 2040 bits
	for i := range n {
		n[i] = 0xFF
	}
	jwk := map[string]any{
		"kty": "RSA", "kid": "k1",
		"n": base64.RawURLEncoding.EncodeToString(n),
		"e": base64.RawURLEncoding.EncodeToString([]byte{0x01, 0x00, 0x01}),
	}
	body, _ := json.Marshal(genapi.RegisterTrustedKeyRequestDto{KeyId: "k1", Jwk: jwk})
	w := httptest.NewRecorder()
	h.RegisterTrustedKey(w, adminReq(t, "POST", "/", body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400; body=%s", w.Code, w.Body.String())
	}
	commontest.ExpectErrorCode(t, resultResp(w), "BAD_REQUEST")
}

func jwkMembers(jwk map[string]any) string {
	out := make([]string, 0, len(jwk))
	for k := range jwk {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}

// Register stores and returns only the public members kty, kid, n, e, and
// alg and use when given; every other member of the request is dropped.
// List returns the same members.
func TestRegisterTrustedKey_StoresOnlyPublicMembers(t *testing.T) {
	ts := newTestTrustedStore(t)
	feats := auth.DefaultIAMFeatures()
	feats.TrustedKeyRegistrationEnabled = true
	h := account.New(newTestKeyStore(t), ts, nil, feats, auth.OperatorGuard{})
	jwk := rsaJWK(t, "k1")
	jwk["alg"] = "RS256"
	jwk["use"] = "sig"
	jwk["x5c"] = []any{"MIIB"}
	jwk["ext"] = true
	body, _ := json.Marshal(genapi.RegisterTrustedKeyRequestDto{KeyId: "k1", Jwk: jwk})
	w := httptest.NewRecorder()
	h.RegisterTrustedKey(w, adminReq(t, "POST", "/", body))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	const want = "alg e kid kty n use"
	var reg genapi.TrustedKeyResponseDto
	if err := json.Unmarshal(w.Body.Bytes(), &reg); err != nil {
		t.Fatal(err)
	}
	if got := jwkMembers(reg.Jwk); got != want {
		t.Errorf("register response jwk members = [%s], want [%s]", got, want)
	}
	if reg.Jwk["n"] != jwk["n"] || reg.Jwk["e"] != jwk["e"] || reg.Jwk["alg"] != "RS256" || reg.Jwk["use"] != "sig" {
		t.Errorf("register response jwk values changed: %+v", reg.Jwk)
	}
	stored, err := ts.Get(context.Background(), spi.TenantID("t1"), "k1")
	if err != nil {
		t.Fatal(err)
	}
	if got := jwkMembers(stored.JWK); got != want {
		t.Errorf("stored jwk members = [%s], want [%s]", got, want)
	}
	lw := httptest.NewRecorder()
	h.ListTrustedKeys(lw, adminReq(t, "GET", "/", nil))
	var list []genapi.TrustedKeyResponseDto
	if err := json.Unmarshal(lw.Body.Bytes(), &list); err != nil || len(list) != 1 {
		t.Fatalf("list: %v %s", err, lw.Body.String())
	}
	if got := jwkMembers(list[0].Jwk); got != want {
		t.Errorf("list jwk members = [%s], want [%s]", got, want)
	}
}

// Without alg and use the stored JWK is kty, kid, n, e; kid is the keyId.
func TestRegisterTrustedKey_MinimalPublicMembers(t *testing.T) {
	h := enabledHandler(t)
	jwk := rsaJWK(t, "k1")
	delete(jwk, "kid")
	body, _ := json.Marshal(genapi.RegisterTrustedKeyRequestDto{KeyId: "k1", Jwk: jwk})
	w := httptest.NewRecorder()
	h.RegisterTrustedKey(w, adminReq(t, "POST", "/", body))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var reg genapi.TrustedKeyResponseDto
	_ = json.Unmarshal(w.Body.Bytes(), &reg)
	if got := jwkMembers(reg.Jwk); got != "e kid kty n" || reg.Jwk["kid"] != "k1" {
		t.Errorf("jwk = %+v, want members e kid kty n with kid k1", reg.Jwk)
	}
}

// The stored and returned n and e are the ones the key verifies with, even
// when the request spells the members in another letter case: the JWK parser
// matches member names without regard to case, so copying the request's
// exact-case "n" and "e" would list nulls beside a working key.
func TestRegisterTrustedKey_ListsTheModulusItVerifiesWith(t *testing.T) {
	h := enabledHandler(t)
	jwk := rsaJWK(t, "k1")
	n, e := jwk["n"], jwk["e"]
	delete(jwk, "n")
	delete(jwk, "e")
	jwk["N"], jwk["E"] = n, e
	body, _ := json.Marshal(genapi.RegisterTrustedKeyRequestDto{KeyId: "k1", Jwk: jwk})
	w := httptest.NewRecorder()
	h.RegisterTrustedKey(w, adminReq(t, "POST", "/", body))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var reg genapi.TrustedKeyResponseDto
	_ = json.Unmarshal(w.Body.Bytes(), &reg)
	if reg.Jwk["n"] != n || reg.Jwk["e"] != e {
		t.Errorf("jwk n=%v e=%v, want the verifying modulus and exponent", reg.Jwk["n"], reg.Jwk["e"])
	}
	if got := jwkMembers(reg.Jwk); got != "e kid kty n" {
		t.Errorf("jwk members = %q, want e kid kty n", got)
	}
}

// alg and use, when present, are strings.
func TestRegisterTrustedKey_NonStringAlgOrUse_400(t *testing.T) {
	for _, member := range []string{"alg", "use"} {
		t.Run(member, func(t *testing.T) {
			h := enabledHandler(t)
			jwk := rsaJWK(t, "k1")
			jwk[member] = map[string]any{"x": 1}
			body, _ := json.Marshal(genapi.RegisterTrustedKeyRequestDto{KeyId: "k1", Jwk: jwk})
			w := httptest.NewRecorder()
			h.RegisterTrustedKey(w, adminReq(t, "POST", "/", body))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status=%d want 400; body=%s", w.Code, w.Body.String())
			}
			commontest.ExpectErrorCode(t, resultResp(w), "BAD_REQUEST")
		})
	}
}
