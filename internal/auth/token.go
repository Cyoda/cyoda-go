package auth

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

// tokenHandler implements the POST /oauth/token endpoint.
type tokenHandler struct {
	keyStore        KeyStore
	trustedKeyStore TrustedKeyStore
	m2mStore        M2MClientStore
	issuer          string
	expirySeconds   int
}

// NewTokenHandler creates a new token endpoint handler.
func NewTokenHandler(
	keyStore KeyStore,
	trustedKeyStore TrustedKeyStore,
	m2mStore M2MClientStore,
	issuer string,
	expirySeconds int,
) http.Handler {
	return &tokenHandler{
		keyStore:        keyStore,
		trustedKeyStore: trustedKeyStore,
		m2mStore:        m2mStore,
		issuer:          issuer,
		expirySeconds:   expirySeconds,
	}
}

func (h *tokenHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeTokenError(w, http.StatusMethodNotAllowed, "method_not_allowed", "")
		return
	}

	// Limit request body to 1MB to prevent abuse.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	clientID, secret, ok := parseBasicAuth(r)
	if !ok {
		writeTokenError(w, http.StatusUnauthorized, "invalid_client", "")
		return
	}

	valid, err := h.m2mStore.VerifySecret(clientID, secret)
	if err != nil || !valid {
		writeTokenError(w, http.StatusUnauthorized, "invalid_client", "")
		return
	}

	grantType := r.FormValue("grant_type")
	switch grantType {
	case "client_credentials":
		h.handleClientCredentials(w, clientID)
	case "urn:ietf:params:oauth:grant-type:token-exchange":
		h.handleTokenExchange(w, r, clientID)
	default:
		writeTokenError(w, http.StatusBadRequest, "unsupported_grant_type", "")
	}
}

func (h *tokenHandler) handleClientCredentials(w http.ResponseWriter, clientID string) {
	client, err := h.m2mStore.Get(clientID)
	if err != nil {
		writeTokenError(w, http.StatusUnauthorized, "invalid_client", "")
		return
	}

	kp, err := h.keyStore.GetActive("client")
	if err != nil {
		writeTokenError(w, http.StatusInternalServerError, "server_error", "")
		return
	}

	now := time.Now()
	claims := map[string]any{
		"sub":          clientID,
		"iss":          h.issuer,
		"caas_user_id": client.UserID,
		"caas_org_id":  client.TenantID,
		"scopes":       client.Roles,
		"caas_tier":    "unlimited",
		"exp":          now.Add(time.Duration(h.expirySeconds) * time.Second).Unix(),
		"iat":          now.Unix(),
		"jti":          uuid.NewString(),
	}

	token, err := Sign(claims, kp.PrivateKey, kp.KID)
	if err != nil {
		writeTokenError(w, http.StatusInternalServerError, "server_error", "")
		return
	}

	writeTokenResponse(w, http.StatusOK, map[string]any{
		"access_token": token,
		"token_type":   "Bearer",
		"expires_in":   h.expirySeconds,
	})
}

func (h *tokenHandler) handleTokenExchange(w http.ResponseWriter, r *http.Request, clientID string) {
	subjectToken := r.FormValue("subject_token")
	subjectTokenType := r.FormValue("subject_token_type")

	if subjectTokenType != "urn:ietf:params:oauth:token-type:jwt" {
		writeTokenError(w, http.StatusBadRequest, "invalid_grant", "unsupported subject_token_type")
		return
	}

	parsed, err := Parse(subjectToken)
	if err != nil {
		writeTokenError(w, http.StatusBadRequest, "invalid_grant", "invalid subject token")
		return
	}

	if err := EnsureAlgRS256(parsed.Header); err != nil {
		writeTokenError(w, http.StatusBadRequest, "invalid_grant", "unsupported token algorithm")
		return
	}

	kid, _ := parsed.Header["kid"].(string)
	if kid == "" {
		writeTokenError(w, http.StatusBadRequest, "invalid_grant", "missing kid in subject token")
		return
	}

	trustedKey, err := getTrustedKeyByKID(h.trustedKeyStore, kid)
	if err != nil {
		writeTokenError(w, http.StatusBadRequest, "invalid_grant", "unknown trusted key")
		return
	}

	// Verify trusted key is active and within validity window.
	if !trustedKey.Active {
		writeTokenError(w, http.StatusBadRequest, "invalid_grant", "trusted key is inactive")
		return
	}
	now := time.Now()
	if now.Before(trustedKey.ValidFrom) {
		writeTokenError(w, http.StatusBadRequest, "invalid_grant", "trusted key not yet valid")
		return
	}
	if trustedKey.ValidTo != nil && now.After(*trustedKey.ValidTo) {
		writeTokenError(w, http.StatusBadRequest, "invalid_grant", "trusted key expired")
		return
	}

	if err := Verify(parsed.SigningInput, parsed.Signature, trustedKey.PublicKey); err != nil {
		writeTokenError(w, http.StatusBadRequest, "invalid_grant", "signature verification failed")
		return
	}

	// Validate issuer if the trusted key has issuers configured.
	if len(trustedKey.Issuers) > 0 {
		iss, _ := parsed.Claims["iss"].(string)
		issuerMatch := false
		for _, allowed := range trustedKey.Issuers {
			if iss == allowed {
				issuerMatch = true
				break
			}
		}
		if !issuerMatch {
			writeTokenError(w, http.StatusBadRequest, "invalid_grant", "untrusted token issuer")
			return
		}
	}

	if err := ValidateClaims(parsed.Claims, 30*time.Second); err != nil {
		writeTokenError(w, http.StatusBadRequest, "invalid_grant", err.Error())
		return
	}

	// Extract claims from subject token.
	// The sub claim identifies the user on whose behalf we're acting.
	subjectSub, _ := parsed.Claims["sub"].(string)
	if subjectSub == "" {
		writeTokenError(w, http.StatusBadRequest, "invalid_grant", "subject token missing sub claim")
		return
	}
	subOrgID, _ := parsed.Claims["caas_org_id"].(string)
	subRoles := parsed.Claims["user_roles"]
	if subRoles == nil {
		subRoles = parsed.Claims["roles"]
	}

	// Tenant boundary check.
	client, err := h.m2mStore.Get(clientID)
	if err != nil {
		writeTokenError(w, http.StatusUnauthorized, "invalid_client", "")
		return
	}
	if client.TenantID != subOrgID {
		writeTokenError(w, http.StatusForbidden, "access_denied", "tenant mismatch")
		return
	}

	kp, err := h.keyStore.GetActive("client")
	if err != nil {
		writeTokenError(w, http.StatusInternalServerError, "server_error", "")
		return
	}

	oboNow := time.Now()
	claims := map[string]any{
		"sub":          subjectSub,
		"iss":          h.issuer,
		"caas_user_id": subjectSub,
		"caas_org_id":  subOrgID,
		"user_roles":   subRoles,
		"act":          map[string]any{"sub": clientID},
		"caas_tier":    "unlimited",
		"exp":          oboNow.Add(time.Duration(h.expirySeconds) * time.Second).Unix(),
		"iat":          oboNow.Unix(),
		"jti":          uuid.NewString(),
	}

	token, err := Sign(claims, kp.PrivateKey, kp.KID)
	if err != nil {
		writeTokenError(w, http.StatusInternalServerError, "server_error", "")
		return
	}

	writeTokenResponse(w, http.StatusOK, map[string]any{
		"access_token":      token,
		"token_type":        "Bearer",
		"expires_in":        h.expirySeconds,
		"issued_token_type": "urn:ietf:params:oauth:token-type:jwt",
	})
}

// parseBasicAuth extracts client_id and client_secret from the Authorization header.
func parseBasicAuth(r *http.Request) (clientID, secret string, ok bool) {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return "", "", false
	}
	if !strings.HasPrefix(authHeader, "Basic ") {
		return "", "", false
	}
	decoded, err := base64.StdEncoding.DecodeString(authHeader[6:])
	if err != nil {
		return "", "", false
	}
	parts := strings.SplitN(string(decoded), ":", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	id, err := url.QueryUnescape(parts[0])
	if err != nil {
		return "", "", false
	}
	sec, err := url.QueryUnescape(parts[1])
	if err != nil {
		return "", "", false
	}
	return id, sec, true
}

func writeTokenError(w http.ResponseWriter, status int, errCode, description string) {
	resp := map[string]string{"error": errCode}
	if description != "" {
		resp["error_description"] = description
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(resp)
}

func writeTokenResponse(w http.ResponseWriter, status int, body map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}
