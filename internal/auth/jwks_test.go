package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestJWKS_EmptyKeyStore(t *testing.T) {
	store := NewInMemoryKeyStore()
	handler := NewJWKSHandler(store)

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

	if len(resp.Keys) != 0 {
		t.Fatalf("expected 0 keys, got %d", len(resp.Keys))
	}
}

func TestJWKS_OneActiveKey(t *testing.T) {
	store := NewInMemoryKeyStore()
	handler := NewJWKSHandler(store)

	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate RSA key: %v", err)
	}

	kp := &KeyPair{
		KID:        "key-1",
		Audience:   "client",
		Algorithm:  "RS256",
		PublicKey:  &privKey.PublicKey,
		PrivateKey: privKey,
		Active:     true,
		ValidFrom:  time.Now(),
	}
	if err := store.Save(kp, RotateOptions{}); err != nil {
		t.Fatalf("failed to save key pair: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/.well-known/jwks.json", nil)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rr.Code)
	}

	var resp jwksResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp.Keys) != 1 {
		t.Fatalf("expected 1 key, got %d", len(resp.Keys))
	}

	entry := resp.Keys[0]

	if entry.Kty != "RSA" {
		t.Errorf("expected kty RSA, got %s", entry.Kty)
	}
	if entry.KID != "key-1" {
		t.Errorf("expected kid key-1, got %s", entry.KID)
	}
	if entry.Use != "sig" {
		t.Errorf("expected use sig, got %s", entry.Use)
	}
	if entry.Alg != "RS256" {
		t.Errorf("expected alg RS256, got %s", entry.Alg)
	}

	expectedN := base64.RawURLEncoding.EncodeToString(privKey.PublicKey.N.Bytes())
	if entry.N != expectedN {
		t.Errorf("modulus mismatch")
	}

	// Standard exponent 65537 → big-endian bytes [1, 0, 1] → base64url "AQAB"
	expectedE := base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1})
	if entry.E != expectedE {
		t.Errorf("exponent mismatch: expected %s, got %s", expectedE, entry.E)
	}
}

func TestJWKS_InvalidatedKeyNotIncluded(t *testing.T) {
	store := NewInMemoryKeyStore()
	handler := NewJWKSHandler(store)

	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate RSA key: %v", err)
	}

	kp := &KeyPair{
		KID:        "key-inactive",
		Audience:   "client",
		Algorithm:  "RS256",
		PublicKey:  &privKey.PublicKey,
		PrivateKey: privKey,
		Active:     true,
		ValidFrom:  time.Now(),
	}
	if err := store.Save(kp, RotateOptions{}); err != nil {
		t.Fatalf("failed to save key pair: %v", err)
	}

	if err := store.Invalidate("key-inactive", 0); err != nil {
		t.Fatalf("failed to invalidate key: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/.well-known/jwks.json", nil)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rr.Code)
	}

	var resp jwksResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp.Keys) != 0 {
		t.Fatalf("expected 0 keys after invalidation, got %d", len(resp.Keys))
	}
}

func TestJWKS_MultipleKeys_OnlyActiveIncluded(t *testing.T) {
	store := NewInMemoryKeyStore()
	handler := NewJWKSHandler(store)

	for _, tc := range []struct {
		kid    string
		active bool
	}{
		{"active-1", true},
		{"active-2", true},
		{"inactive-1", false},
	} {
		privKey, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("failed to generate RSA key: %v", err)
		}

		kp := &KeyPair{
			KID:        tc.kid,
			Audience:   "client",
			Algorithm:  "RS256",
			PublicKey:  &privKey.PublicKey,
			PrivateKey: privKey,
			Active:     tc.active,
			ValidFrom:  time.Now(),
		}
		if err := store.Save(kp, RotateOptions{}); err != nil {
			t.Fatalf("failed to save key pair %s: %v", tc.kid, err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/.well-known/jwks.json", nil)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rr.Code)
	}

	var resp jwksResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp.Keys) != 2 {
		t.Fatalf("expected 2 active keys, got %d", len(resp.Keys))
	}

	kids := make(map[string]bool)
	for _, entry := range resp.Keys {
		kids[entry.KID] = true
	}

	if !kids["active-1"] {
		t.Error("expected active-1 in JWKS response")
	}
	if !kids["active-2"] {
		t.Error("expected active-2 in JWKS response")
	}
	if kids["inactive-1"] {
		t.Error("inactive-1 should not be in JWKS response")
	}
}
