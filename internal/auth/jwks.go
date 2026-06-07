package auth

import (
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
)

// jwkEntry represents a single JWK entry in the JWKS response.
type jwkEntry struct {
	Kty string `json:"kty"`
	KID string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
}

// jwksResponse represents the JWKS endpoint response.
type jwksResponse struct {
	Keys []jwkEntry `json:"keys"`
}

// JWKSHandler serves the /.well-known/jwks.json endpoint.
type JWKSHandler struct {
	keyStore KeyStore
}

// NewJWKSHandler creates a new JWKSHandler.
func NewJWKSHandler(keyStore KeyStore) *JWKSHandler {
	return &JWKSHandler{keyStore: keyStore}
}

// ServeHTTP handles GET requests and returns the JWKS JSON.
func (h *JWKSHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	allKeys := h.keyStore.ListForVerification()

	entries := make([]jwkEntry, 0, len(allKeys))
	for _, kp := range allKeys {
		if !kp.Active {
			continue
		}
		entries = append(entries, jwkEntry{
			Kty: "RSA",
			KID: kp.KID,
			Use: "sig",
			Alg: "RS256",
			N:   base64.RawURLEncoding.EncodeToString(kp.PublicKey.N.Bytes()),
			E:   encodeExponent(kp.PublicKey.E),
		})
	}

	resp := jwksResponse{Keys: entries}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		http.Error(w, "failed to encode response", http.StatusInternalServerError)
		return
	}
}

// encodeExponent encodes an RSA public key exponent as base64url.
func encodeExponent(e int) string {
	b := big.NewInt(int64(e)).Bytes()
	return base64.RawURLEncoding.EncodeToString(b)
}
