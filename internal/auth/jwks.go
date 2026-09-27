package auth

import (
	"encoding/base64"
	"encoding/json"
	"math"
	"math/big"
	"net/http"
	"strconv"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/common"
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
	keyStore   KeyStore
	retryAfter time.Duration
}

// NewJWKSHandler serves the key set; while the store is stale it answers 503
// with Retry-After instead of an empty set, which would make external
// verifiers drop their cached keys.
func NewJWKSHandler(keyStore KeyStore, retryAfter time.Duration) *JWKSHandler {
	return &JWKSHandler{keyStore: keyStore, retryAfter: retryAfter}
}

// ServeHTTP handles GET requests and returns the JWKS JSON.
func (h *JWKSHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	allKeys, err := h.keyStore.Published()
	if err != nil {
		// Whole seconds, rounded up so a sub-second interval never says 0.
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(h.retryAfter.Seconds()))))
		common.WriteError(w, r, common.Internal("jwks", err))
		return
	}

	// Published already leaves out key pairs whose window has ended. It does
	// NOT filter by Active: grace-period keys (Active=false, ValidTo
	// in the future) are intentionally published so external verifiers can
	// validate tokens that were signed before a rotation (spec §3.2 #1).
	entries := make([]jwkEntry, 0, len(allKeys))
	for _, kp := range allKeys {
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
