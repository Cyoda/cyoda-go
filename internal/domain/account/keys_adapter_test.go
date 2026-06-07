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
