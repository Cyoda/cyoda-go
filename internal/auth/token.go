package auth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/tenantroute"
)

// The token-exchange grant (RFC 8693) and the only subject token type it
// accepts.
const (
	grantTokenExchange = "urn:ietf:params:oauth:grant-type:token-exchange"
	tokenTypeJWT       = "urn:ietf:params:oauth:token-type:jwt"
)

// formMediaType is the only request media type the endpoint accepts.
const formMediaType = "application/x-www-form-urlencoded"

// Bounds on a user assertion presented to the token exchange.
const (
	// assertionMaxLifetime is the longest exp − iat an assertion may carry.
	assertionMaxLifetime = 300
	// assertionClockSkew is the tolerance on every time claim.
	assertionClockSkew = 30
)

// exchangeForbiddenParams are the RFC 8693 parameters the exchange does not
// support. A request that carries any of them, even empty, is refused rather
// than served with the parameter ignored.
var exchangeForbiddenParams = []string{
	"actor_token", "actor_token_type", "resource", "audience", "scope", "requested_token_type",
}

// tokenHandler implements the POST /tenants/{tenant}/oauth/token endpoint.
type tokenHandler struct {
	keyStore        KeyStore
	trustedKeyStore TrustedKeyStore
	m2mStore        M2MClientStore
	issuer          string
	audience        string // empty: no aud claim
	expirySeconds   int
	buckets         *clientBuckets // per-client limit on this node
}

// NewTokenHandler creates the token endpoint handler. audience, when not
// empty, is set as the aud claim of every issued token: a server that checks
// the audience (CYODA_JWT_AUDIENCE) must accept its own tokens.
// requestsPerMinute limits each authenticated client on this node, across
// both grants; <= 0: no limit.
func NewTokenHandler(
	keyStore KeyStore,
	trustedKeyStore TrustedKeyStore,
	m2mStore M2MClientStore,
	issuer, audience string,
	expirySeconds, requestsPerMinute int,
) http.Handler {
	return &tokenHandler{
		keyStore:        keyStore,
		trustedKeyStore: trustedKeyStore,
		m2mStore:        m2mStore,
		issuer:          issuer,
		audience:        audience,
		expirySeconds:   expirySeconds,
		buckets:         newClientBuckets(requestsPerMinute),
	}
}

// withAudience sets aud on claims when an audience is configured.
func (h *tokenHandler) withAudience(claims map[string]any) map[string]any {
	if h.audience != "" {
		claims["aud"] = h.audience
	}
	return claims
}

// ServeHTTP authenticates the client, then serves the grant it asks for. A
// plain or admin client may only use client_credentials, an on-behalf-of
// client only the token exchange.
//
// Order: group refusal (400, by the tenant route group before the handler) →
// no addressed tenant (500) → method (405) → Content-Type (400) → client
// authentication (401, a client of another tenant included) → body. The
// checks before the body read headers only, so a body that is not a form is
// never read, and no body is read before the client has authenticated.
func (h *tokenHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tenant, ok := tenantroute.Addressed(r.Context())
	if !ok {
		writeTokenServerError(w, "tenantroute.Addressed", errors.New("token handler reached outside the tenant route group"))
		return
	}

	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeTokenError(w, http.StatusMethodNotAllowed, "method_not_allowed", "")
		return
	}

	// RFC 6749 §3.2: the request body is application/x-www-form-urlencoded.
	// Any other media type, or none, is refused without reading the body.
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != formMediaType {
		writeTokenError(w, http.StatusBadRequest, "invalid_request", "the request body must be application/x-www-form-urlencoded")
		return
	}

	// Limit request body to 1MB to prevent abuse.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	clientID, secret, ok := parseBasicAuth(r)
	if !ok {
		writeTokenError(w, http.StatusUnauthorized, "invalid_client", "client authentication failed")
		return
	}

	// A store failure is the server failing, not the credentials being
	// wrong: it never answers 401. Neither does a node with no free
	// secret-check slot: that is a retryable 503.
	client, err := h.m2mStore.Authenticate(r.Context(), clientID, secret)
	if errors.Is(err, ErrInvalidClient) {
		writeTokenError(w, http.StatusUnauthorized, "invalid_client", "client authentication failed")
		return
	}
	if errors.Is(err, ErrSecretCheckBusy) {
		writeTokenRetry(w, http.StatusServiceUnavailable, "temporarily_unavailable", 1)
		return
	}
	if err != nil {
		writeTokenStoreError(w, "m2mStore.Authenticate", err)
		return
	}
	// A client of another tenant is no client of this one.
	if client.TenantID != tenant {
		writeTokenError(w, http.StatusUnauthorized, "invalid_client", "client authentication failed")
		return
	}

	if err := r.ParseForm(); err != nil {
		writeTokenError(w, http.StatusBadRequest, "invalid_request", "malformed request body")
		return
	}

	switch r.PostForm.Get("grant_type") {
	case "client_credentials":
		h.handleClientCredentials(w, r, client)
	case grantTokenExchange:
		h.handleTokenExchange(w, r, client)
	default:
		writeTokenError(w, http.StatusBadRequest, "unsupported_grant_type", "")
	}
}

// takeToken takes one request from client's bucket on this node. An empty
// bucket answers 429 slow_down with Retry-After: the whole seconds until a
// token is there, at least one; the caller then stops.
func (h *tokenHandler) takeToken(w http.ResponseWriter, client *M2MClient) bool {
	ok, wait := h.buckets.allow(clientKey{client.TenantID, client.ClientID}, time.Now())
	if !ok {
		writeTokenRetry(w, http.StatusTooManyRequests, "slow_down", int(math.Ceil(wait.Seconds())))
	}
	return ok
}

// handleClientCredentials issues a client its own token (§4.2).
func (h *tokenHandler) handleClientCredentials(w http.ResponseWriter, r *http.Request, client *M2MClient) {
	if client.OnBehalfOf {
		writeTokenError(w, http.StatusBadRequest, "unauthorized_client", "this client may only exchange user assertions")
		return
	}
	if !h.takeToken(w, client) {
		return
	}

	now := time.Now()
	claims := map[string]any{
		"sub":          client.ClientID,
		"caas_user_id": client.UserID,
		"caas_org_id":  client.TenantID,
		"scopes":       client.Roles,
		// The secret generation the token was issued under: a stream opened
		// with it ends when the client's secret is reset.
		"cgen": client.SecretGen,
	}
	h.mintAndRespond(w, r, claims, now, now.Add(time.Duration(h.expirySeconds)*time.Second), false)
}

// handleTokenExchange issues an on-behalf-of client a token for the user its
// assertion names (§4.3, §4.4). Every refusal carries a fixed description:
// none echoes the subject, the tenant or the key id.
func (h *tokenHandler) handleTokenExchange(w http.ResponseWriter, r *http.Request, client *M2MClient) {
	// Refused before the assertion is read.
	if !client.OnBehalfOf {
		writeTokenError(w, http.StatusBadRequest, "unauthorized_client", "this client may not exchange user assertions")
		return
	}
	if !h.takeToken(w, client) {
		return
	}

	// r.Form holds the body and the query string; a parameter in either is
	// refused, even when empty.
	for _, p := range exchangeForbiddenParams {
		if _, present := r.Form[p]; present {
			writeTokenError(w, http.StatusBadRequest, "invalid_request", "unsupported parameter")
			return
		}
	}
	if r.PostForm.Get("subject_token_type") != tokenTypeJWT {
		writeTokenError(w, http.StatusBadRequest, "invalid_request", "unsupported subject_token_type")
		return
	}

	parsed, err := Parse(r.PostForm.Get("subject_token"))
	if err != nil || EnsureAlgRS256(parsed.Header) != nil {
		writeTokenError(w, http.StatusBadRequest, "invalid_request", "invalid subject token")
		return
	}
	kid, _ := parsed.Header["kid"].(string)
	if kid == "" {
		writeTokenError(w, http.StatusBadRequest, "invalid_request", "invalid subject token")
		return
	}

	// The key is read from the store in the exchanging client's own tenant,
	// on every exchange. A key's tenant is the tenant that registered it; the
	// assertion's caas_org_id is written by the key holder and cannot bind a
	// key to a tenant, so a key registered elsewhere is not found here. The
	// store refuses a key that is absent, inactive or outside its window. A
	// store that cannot answer fails the exchange: it is never read as "no
	// such key".
	trustedKey, err := h.trustedKeyStore.GetForVerification(r.Context(), client.TenantID, kid)
	if errors.Is(err, ErrTrustedKeyNotFound) {
		writeTokenError(w, http.StatusBadRequest, "invalid_request", "unknown or inactive trusted key")
		return
	}
	if err != nil {
		writeTokenStoreError(w, "trustedKeyStore.GetForVerification", err)
		return
	}

	if err := Verify(parsed.SigningInput, parsed.Signature, trustedKey.PublicKey); err != nil ||
		!issuerListed(trustedKey.Issuers, parsed.Claims["iss"]) {
		writeTokenError(w, http.StatusBadRequest, "invalid_request", "subject token signature or issuer rejected")
		return
	}

	now := time.Now()
	assertionExp, ok := h.assertionClaimsAcceptable(parsed.Claims, now)
	if !ok {
		writeTokenError(w, http.StatusBadRequest, "invalid_request", "subject token claims rejected")
		return
	}

	// The asserted user must be a user of the client's tenant. Domain-type
	// tenant compared against the raw claim at the security boundary.
	if orgID, _ := parsed.Claims["caas_org_id"].(string); orgID != string(client.TenantID) {
		writeTokenError(w, http.StatusForbidden, "access_denied", "tenant mismatch")
		return
	}

	// sub becomes the issued token's user id, so it passes the check every
	// door applies to one.
	sub, _ := parsed.Claims["sub"].(string)
	if common.ValidateUserID(sub) != nil {
		writeTokenError(w, http.StatusBadRequest, "invalid_request", "subject token sub rejected")
		return
	}

	// exp = min(assertion exp, now + expiry), in whole seconds. The time
	// claims bound the assertion's exp to now + 330 s, so the comparison is
	// in range.
	exp := now.Unix() + int64(h.expirySeconds)
	if assertionExp < float64(exp) {
		exp = int64(math.Floor(assertionExp))
	}
	if exp <= now.Unix() {
		writeTokenError(w, http.StatusBadRequest, "invalid_request", "subject token has expired")
		return
	}

	claims := map[string]any{
		"sub":          sub,
		"caas_user_id": sub,
		"caas_org_id":  client.TenantID,
		// The client's roles; the assertion's are ignored.
		"scopes": client.Roles,
		"act":    map[string]any{"sub": client.ClientID},
	}
	h.mintAndRespond(w, r, claims, now, time.Unix(exp, 0), true)
}

// assertionClaimsAcceptable applies the time and audience rules to a
// verified assertion and returns its exp: aud contains the cyoda issuer; exp
// and iat are numbers with exp − iat at most assertionMaxLifetime; iat is
// not in the future, exp not past and nbf, when present, not in the future,
// each within assertionClockSkew.
func (h *tokenHandler) assertionClaimsAcceptable(claims map[string]any, now time.Time) (float64, bool) {
	if checkAudience(claims["aud"], h.issuer) != nil {
		return 0, false
	}
	exp, okExp := claims["exp"].(float64)
	iat, okIat := claims["iat"].(float64)
	if !okExp || !okIat {
		return 0, false
	}
	t := float64(now.Unix())
	if exp-iat > assertionMaxLifetime || iat > t+assertionClockSkew || exp <= t-assertionClockSkew {
		return 0, false
	}
	if raw, present := claims["nbf"]; present {
		nbf, ok := raw.(float64)
		if !ok || nbf > t+assertionClockSkew {
			return 0, false
		}
	}
	return exp, true
}

// issuerListed reports whether iss is one of issuers; a key that lists no
// issuers accepts any.
func issuerListed(issuers []string, iss any) bool {
	if len(issuers) == 0 {
		return true
	}
	s, ok := iss.(string)
	if !ok {
		return false
	}
	for _, allowed := range issuers {
		if s == allowed {
			return true
		}
	}
	return false
}

// mintAndRespond completes claims with the fields every issued token
// carries, signs it and writes the token response. expires_in is the token's
// remaining life, exp − now in whole seconds. exchanged adds the RFC 8693
// issued_token_type.
func (h *tokenHandler) mintAndRespond(w http.ResponseWriter, r *http.Request, claims map[string]any, now, exp time.Time, exchanged bool) {
	token, err := h.mint(r, claims, now, exp)
	if err != nil {
		op := "sign"
		var mf signerFailure
		if errors.As(err, &mf) {
			op = "keyStore.Signer"
		}
		writeTokenServerError(w, op, err)
		return
	}
	body := map[string]any{
		"access_token": token,
		"token_type":   "Bearer",
		"expires_in":   exp.Unix() - now.Unix(),
	}
	if exchanged {
		body["issued_token_type"] = tokenTypeJWT
	}
	writeTokenResponse(w, http.StatusOK, body)
}

// signerFailure marks a mint failure as the key store failing to select a
// signer, as opposed to the signer failing to sign; the 500's log names
// which.
type signerFailure struct{ err error }

func (f signerFailure) Error() string { return "failed to select a signing key: " + f.err.Error() }
func (f signerFailure) Unwrap() error { return f.err }

// mint signs claims with the current signing key, after setting iss, iat,
// exp, jti, caas_tier and, when configured, aud. A failure to select the key
// is a signerFailure.
func (h *tokenHandler) mint(r *http.Request, claims map[string]any, now time.Time, exp time.Time) (string, error) {
	kp, signer, err := h.keyStore.Signer()
	if err != nil {
		return "", signerFailure{err}
	}
	claims["iss"], claims["iat"], claims["exp"], claims["jti"], claims["caas_tier"] =
		h.issuer, now.Unix(), exp.Unix(), uuid.NewString(), "unlimited"
	return Sign(r.Context(), h.withAudience(claims), signer, kp.KID)
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

// writeTokenError writes an OAuth-shaped error (RFC 6749 §5.2). A 401
// carries WWW-Authenticate: Basic realm="cyoda", the scheme the endpoint
// authenticates clients with (RFC 7617 §2 requires the realm).
func writeTokenError(w http.ResponseWriter, status int, errCode, description string) {
	resp := map[string]string{"error": errCode}
	if description == "" {
		// OpenAPI declares error_description as a required field on the 401
		// response. Fall back to the OAuth2 error code so the wire shape always
		// satisfies the spec without leaking internal state.
		description = errCode
	}
	resp["error_description"] = description
	SetNoStore(w.Header())
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Basic realm="cyoda"`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(resp)
}

// writeTokenServerError answers a 500 on the OAuth2-shaped token endpoint.
//
// Gate 3 requires every 5xx to carry a generic message plus a ticket UUID and
// no internals. RFC 6749 §5.2 fixes this endpoint's body shape, which has no
// dedicated field, so the ticket rides in error_description — already a
// declared string in the schema. The cause goes to the log under the same
// ticket and never into the response, matching the LevelInternal rendering in
// internal/common/errors.go.
func writeTokenServerError(w http.ResponseWriter, op string, cause error) {
	ticket := uuid.NewString()
	slog.Error("internal error",
		"pkg", "auth",
		"ticket", ticket,
		"op", op,
		"cause", cause,
	)
	writeTokenError(w, http.StatusInternalServerError, "server_error",
		fmt.Sprintf("server_error [ticket: %s]", ticket))
}

// writeTokenStoreError answers a failed store read: a storage-unavailable
// error is a retryable 503 temporarily_unavailable, any other a ticketed
// 500. The cause goes to the log, never into the response.
func writeTokenStoreError(w http.ResponseWriter, op string, cause error) {
	if common.StorageUnavailable(cause) == nil {
		writeTokenServerError(w, op, cause)
		return
	}
	slog.Warn("token request refused: store unavailable",
		"pkg", "auth",
		"op", op,
		"cause", cause,
	)
	writeTokenRetry(w, http.StatusServiceUnavailable, "temporarily_unavailable", 1)
}

// writeTokenRetry answers a refusal the client may retry —
// temporarily_unavailable with the meaning of RFC 6749 §4.1.2.1, or
// slow_down — with Retry-After: seconds, at least 1.
func writeTokenRetry(w http.ResponseWriter, status int, code string, seconds int) {
	w.Header().Set("Retry-After", strconv.Itoa(max(seconds, 1)))
	writeTokenError(w, status, code, "")
}

// SetNoStore marks a response as never to be stored by a cache (RFC 6749
// §5.1): every token endpoint response, success or error, and every response
// carrying a plaintext client secret. It must be called before the header is
// written.
func SetNoStore(h http.Header) {
	h.Set("Cache-Control", "no-store")
	h.Set("Pragma", "no-cache")
}

func writeTokenResponse(w http.ResponseWriter, status int, body map[string]any) {
	SetNoStore(w.Header())
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}
