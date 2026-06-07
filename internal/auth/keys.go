package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// KeysHandler handles HTTP requests for key pair management.
type KeysHandler struct {
	keyStore KeyStore
}

// NewKeysHandler creates a new KeysHandler.
func NewKeysHandler(keyStore KeyStore) *KeysHandler {
	return &KeysHandler{keyStore: keyStore}
}

// keysInfoResponse is the JSON response for key pair info.
type keysInfoResponse struct {
	KID       string `json:"kid"`
	Active    bool   `json:"active"`
	CreatedAt string `json:"createdAt"`
}

// ServeHTTP routes key pair management requests. Requires ROLE_ADMIN.
func (h *KeysHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	const basePath = "/oauth/keys/keypair"

	path := r.URL.Path
	if !strings.HasPrefix(path, basePath) {
		http.NotFound(w, r)
		return
	}

	// Strip base path to get the remainder
	remainder := strings.TrimPrefix(path, basePath)
	remainder = strings.TrimPrefix(remainder, "/")

	// Route: POST /oauth/keys/keypair (no remainder)
	if remainder == "" && r.Method == http.MethodPost {
		h.issueKeyPair(w, r)
		return
	}

	// Route: GET /oauth/keys/keypair/current
	if remainder == "current" && r.Method == http.MethodGet {
		h.getCurrent(w, r)
		return
	}

	// Routes with keyId: {keyId}, {keyId}/invalidate, {keyId}/reactivate
	parts := strings.SplitN(remainder, "/", 2)
	keyID := parts[0]
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}

	if keyID == "" {
		http.NotFound(w, r)
		return
	}

	switch {
	case action == "" && r.Method == http.MethodDelete:
		h.deleteKeyPair(w, r, keyID)
	case action == "invalidate" && r.Method == http.MethodPost:
		h.invalidateKeyPair(w, r, keyID)
	case action == "reactivate" && r.Method == http.MethodPost:
		h.reactivateKeyPair(w, r, keyID)
	default:
		http.NotFound(w, r)
	}
}

func (h *KeysHandler) issueKeyPair(w http.ResponseWriter, r *http.Request) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		common.WriteError(w, r, common.Internal("failed to generate RSA key", err))
		return
	}

	kidBytes := make([]byte, 16)
	if _, err := rand.Read(kidBytes); err != nil {
		common.WriteError(w, r, common.Internal("failed to read random bytes for KID", err))
		return
	}
	kid := hex.EncodeToString(kidBytes)

	now := time.Now().UTC()
	kp := &KeyPair{
		KID:        kid,
		Audience:   "client",
		Algorithm:  "RS256",
		PublicKey:  &privateKey.PublicKey,
		PrivateKey: privateKey,
		Active:     true,
		ValidFrom:  now,
	}

	if err := h.keyStore.Save(kp, RotateOptions{}); err != nil {
		common.WriteError(w, r, common.Internal("failed to save key pair", err))
		return
	}

	resp := keysInfoResponse{
		KID:       kid,
		Active:    true,
		CreatedAt: now.Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(resp)
}

func (h *KeysHandler) getCurrent(w http.ResponseWriter, r *http.Request) {
	kp, err := h.keyStore.GetActive("client")
	if err != nil {
		common.WriteError(w, r, common.Operational(
			http.StatusNotFound, common.ErrCodeNotFound, "no active key pair"))
		return
	}

	resp := keysInfoResponse{
		KID:       kp.KID,
		Active:    kp.Active,
		CreatedAt: kp.ValidFrom.Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (h *KeysHandler) deleteKeyPair(w http.ResponseWriter, r *http.Request, keyID string) {
	if err := h.keyStore.Delete(keyID); err != nil {
		// Do not echo attacker-controllable keyID in the response body —
		// log it server-side at INFO instead so operators can correlate.
		slog.Info("key pair delete: not found", "pkg", "auth", "kid", keyID)
		common.WriteError(w, r, common.Operational(
			http.StatusNotFound, common.ErrCodeNotFound, "key pair not found"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *KeysHandler) invalidateKeyPair(w http.ResponseWriter, r *http.Request, keyID string) {
	// gracePeriodSec=0 means immediate expiry; Task 6 will expose this as a request param.
	if err := h.keyStore.Invalidate(keyID, 0); err != nil {
		slog.Info("key pair invalidate: not found", "pkg", "auth", "kid", keyID)
		common.WriteError(w, r, common.Operational(
			http.StatusNotFound, common.ErrCodeNotFound, "key pair not found"))
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *KeysHandler) reactivateKeyPair(w http.ResponseWriter, r *http.Request, keyID string) {
	// Task 6 will expose validFrom/validTo as request params; default to now + 365 days.
	now := time.Now().UTC()
	if err := h.keyStore.Reactivate(keyID, now, now.Add(365*24*time.Hour)); err != nil {
		slog.Info("key pair reactivate: not found", "pkg", "auth", "kid", keyID)
		common.WriteError(w, r, common.Operational(
			http.StatusNotFound, common.ErrCodeNotFound, "key pair not found"))
		return
	}
	w.WriteHeader(http.StatusOK)
}
