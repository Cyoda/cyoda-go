package auth

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// serveJWKS answers one GET on a JWKS handler over ks and decodes the body.
func serveJWKS(t *testing.T, ks KeyStore) jwksResponse {
	t.Helper()
	handler := NewJWKSHandler(ks, time.Minute)
	req := httptest.NewRequest(http.MethodGet, "/.well-known/jwks.json", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("expected Content-Type application/json, got %s", ct)
	}
	var resp jwksResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	return resp
}

// A store whose bootstrap key is deleted and that has no issued key pair
// publishes an empty set.
func TestJWKS_EmptyKeyStore(t *testing.T) {
	store := newTestKeyStore(t, loadFixtureKey(t))
	if err := store.Delete(replicaSystemCtx(), store.boot.kid); err != nil {
		t.Fatal(err)
	}
	if resp := serveJWKS(t, store); len(resp.Keys) != 0 {
		t.Fatalf("expected 0 keys, got %d", len(resp.Keys))
	}
}

func TestJWKS_OneActiveKey(t *testing.T) {
	boot := loadFixtureKey(t)
	store := newTestKeyStore(t, boot)

	resp := serveJWKS(t, store)
	if len(resp.Keys) != 1 {
		t.Fatalf("expected 1 key, got %d", len(resp.Keys))
	}
	entry := resp.Keys[0]
	if entry.Kty != "RSA" {
		t.Errorf("expected kty RSA, got %s", entry.Kty)
	}
	if entry.KID != store.boot.kid {
		t.Errorf("expected kid %s, got %s", store.boot.kid, entry.KID)
	}
	if entry.Use != "sig" {
		t.Errorf("expected use sig, got %s", entry.Use)
	}
	if entry.Alg != "RS256" {
		t.Errorf("expected alg RS256, got %s", entry.Alg)
	}
	expectedN := base64.RawURLEncoding.EncodeToString(boot.PublicKey.N.Bytes())
	if entry.N != expectedN {
		t.Errorf("modulus mismatch")
	}
	// Standard exponent 65537 → big-endian bytes [1, 0, 1] → base64url "AQAB"
	expectedE := base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1})
	if entry.E != expectedE {
		t.Errorf("exponent mismatch: expected %s, got %s", expectedE, entry.E)
	}
}

// A key pair invalidated with no grace period has ended, so it is not
// published.
func TestJWKS_InvalidatedKeyNotIncluded(t *testing.T) {
	ctx := replicaSystemCtx()
	store := newTestKeyStore(t, loadFixtureKey(t))
	kp, err := store.Issue(ctx, IssueRequest{Audience: "client", ValidFrom: time.Now(), ValidTo: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Invalidate(ctx, kp.KID, 0); err != nil {
		t.Fatalf("failed to invalidate key: %v", err)
	}
	for _, entry := range serveJWKS(t, store).Keys {
		if entry.KID == kp.KID {
			t.Fatalf("invalidated key %s is still published", kp.KID)
		}
	}
}

// TestJWKS_GracePeriodKeyIncluded verifies that a grace-period key
// (Active=false but ValidTo in the future) IS published in JWKS, so that
// external verifiers can validate tokens signed before rotation. Only keys
// whose ValidTo is in the past are excluded.
func TestJWKS_GracePeriodKeyIncluded(t *testing.T) {
	ctx := replicaSystemCtx()
	kv := newReplicaKV(t)
	boot := loadFixtureKey(t)
	bootKID, err := DeriveKID(&boot.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	v, err := NewWrappedVault(boot, bootKID)
	if err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(30 * time.Second)
	past := time.Now().Add(-time.Second)
	from := time.Now().Add(-time.Minute)
	for _, tc := range []struct {
		kid     string
		active  bool
		validTo *time.Time
	}{
		{"active-1", true, nil},     // active, no expiry — always published
		{"grace-1", false, &future}, // grace period — published
		{"expired-1", false, &past}, // window ended — excluded
	} {
		rec := issuedRecordFull(t, v, tc.kid, "client", tc.active, from, tc.validTo)
		if err := kv.Put(ctx, signingKeysNamespace, tc.kid, rec); err != nil {
			t.Fatal(err)
		}
	}
	store, err := NewKVKeyStore(ctx, kv, KVKeyStoreConfig{Bootstrap: boot, BootstrapAudience: "client"})
	if err != nil {
		t.Fatal(err)
	}

	kids := make(map[string]bool)
	for _, entry := range serveJWKS(t, store).Keys {
		kids[entry.KID] = true
	}
	if !kids["active-1"] {
		t.Error("expected active-1 in JWKS (active, no expiry)")
	}
	if !kids["grace-1"] {
		t.Error("expected grace-1 in JWKS (grace-period key, ValidTo in future)")
	}
	if kids["expired-1"] {
		t.Error("expired-1 must not be in JWKS (ValidTo in past)")
	}
}
