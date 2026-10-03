package account

import (
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// requireTrustedKeyStore returns false after writing 501 NOT_IMPLEMENTED if the
// trusted-key store wasn't wired (e.g. mock IAM mode). All 5 trusted-key adapters call this.
func (h *Handler) requireTrustedKeyStore(w http.ResponseWriter, r *http.Request) bool {
	if h.trustedKeyStore == nil {
		common.WriteError(w, r, common.Operational(http.StatusNotImplemented,
			common.ErrCodeNotImplemented, "trusted-key management requires JWT IAM mode"))
		return false
	}
	return true
}

// trustedKeyStoreError maps a Register / Get / List / Delete / Invalidate /
// Reactivate failure to a response. A KID that is not registered for this
// tenant keeps 404 TRUSTED_KEY_NOT_FOUND; every other failure routes through
// common.Internal, so a KV write or read that failed because storage was
// unavailable surfaces as a retryable 503 with its cause logged rather than
// as "the key does not exist" — an answer that reads as a completed lookup
// and stops the caller retrying.
func trustedKeyStoreError(err error) *common.AppError {
	if errors.Is(err, auth.ErrTrustedKeyNotFound) {
		return common.Operational(http.StatusNotFound, common.ErrCodeTrustedKeyNotFound, "trusted key not found")
	}
	// A refusal the store itself classified (the per-tenant cap) keeps its
	// status and code.
	var appErr *common.AppError
	if errors.As(err, &appErr) && appErr.Level == common.LevelOperational {
		return appErr
	}
	return common.Internal("trusted-key store failed", err)
}

// logTrustedKeyChange writes the INFO line of a trusted-key change: tenant,
// kid, and the attributed principal and executor of the request.
func logTrustedKeyChange(r *http.Request, msg string, tID spi.TenantID, kid string) {
	att, exe := spi.AttributionFor(r.Context())
	slog.Info(msg, "pkg", "account", "tenant", string(tID), "kid", kid,
		"attributedId", att.ID, "attributedKind", string(att.Kind), "executorId", exe.ID, "executorKind", string(exe.Kind))
}

func (h *Handler) gateTrustedKeyFeature(w http.ResponseWriter, r *http.Request) bool {
	if !h.iam.TrustedKeyRegistrationEnabled {
		common.WriteError(w, r, common.Operational(http.StatusNotFound, common.ErrCodeFeatureDisabled, "trusted-key registration is disabled"))
		return false
	}
	return true
}

func (h *Handler) RegisterTrustedKey(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireAdmin(w, r) {
		return
	}
	if !h.gateTrustedKeyFeature(w, r) {
		return
	}
	if !h.requireTrustedKeyStore(w, r) {
		return
	}
	var req genapi.RegisterTrustedKeyRequestDto
	if err := common.DecodeBoundedJSON(w, r, 1<<20, &req); err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid request body"))
		return
	}
	if !auth.MatchesTrustedKIDPattern(req.KeyId) {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid keyId format"))
		return
	}
	pub, publicJWK, errCode, jwkErr := parseTrustedJWK(req.Jwk, req.KeyId, h.iam.TrustedKeyMaxJWKProperties)
	if jwkErr != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, errCode, jwkErr.Error()))
		return
	}
	now := time.Now()
	validFrom := now
	if req.ValidFrom != nil {
		validFrom = *req.ValidFrom
	}
	validTo := validFrom.Add(time.Duration(h.iam.TrustedKeyMaxValidityDays) * 24 * time.Hour)
	if req.ValidTo != nil {
		validTo = *req.ValidTo
	}
	if !storableWindow(w, r, validFrom, validTo) {
		return
	}
	if !validTo.After(validFrom) {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "validTo must be > validFrom"))
		return
	}
	invalidate := false
	if req.InvalidatePrevious != nil {
		invalidate = *req.InvalidatePrevious
	}
	var issuers []string
	if req.Issuers != nil {
		issuers = *req.Issuers
	}
	vt := validTo
	tID := tenantFromCtx(r)
	tk := &auth.TrustedKey{
		KID: req.KeyId, TenantID: tID, JWK: publicJWK, PublicKey: pub,
		Issuers: issuers, Active: true, ValidFrom: validFrom, ValidTo: &vt,
	}
	if err := h.trustedKeyStore.Register(r.Context(), tk, invalidate); err != nil {
		common.WriteError(w, r, trustedKeyStoreError(err))
		return
	}
	logTrustedKeyChange(r, "trusted key registered", tID, tk.KID)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(toTrustedKeyResponse(tk))
}

// privateJWKMembers are the RSA private-key members of a JWK (RFC 7518
// §6.3.2). A trusted key is a public key: a JWK carrying any of them is
// refused, so the server never holds the private half.
var privateJWKMembers = []string{"d", "p", "q", "dp", "dq", "qi", "oth"}

// parseTrustedJWK validates a trusted-key JWK and returns its public key and
// the JWK to store and return: kty, kid (the keyId), n and e, plus alg and use
// when given. Every other member of the request is dropped.
func parseTrustedJWK(jwk map[string]any, keyId string, maxProps int) (pub *rsa.PublicKey, public map[string]any, code string, err error) {
	if len(jwk) > maxProps {
		return nil, nil, common.ErrCodeBadRequest, fmt.Errorf("jwk has too many properties (%d > %d)", len(jwk), maxProps)
	}
	for _, m := range privateJWKMembers {
		if _, ok := jwk[m]; ok {
			return nil, nil, common.ErrCodeBadRequest, fmt.Errorf("jwk must not contain the private member %q", m)
		}
	}
	ktyAny, ok := jwk["kty"]
	if !ok {
		return nil, nil, common.ErrCodeBadRequest, fmt.Errorf("jwk missing kty")
	}
	kty, _ := ktyAny.(string)
	if kty == "" {
		return nil, nil, common.ErrCodeBadRequest, fmt.Errorf("jwk kty must be a string")
	}
	if kty != "RSA" {
		return nil, nil, common.ErrCodeUnsupportedKeyType, fmt.Errorf("only RSA JWKs supported")
	}
	if rawKid, ok := jwk["kid"]; ok {
		s, _ := rawKid.(string)
		if s != keyId {
			return nil, nil, common.ErrCodeBadRequest, fmt.Errorf("jwk.kid (%q) must equal keyId (%q)", s, keyId)
		}
	}
	raw, err := json.Marshal(jwk)
	if err != nil {
		return nil, nil, common.ErrCodeBadRequest, fmt.Errorf("re-marshal jwk: %w", err)
	}
	pubKey, err := auth.ParseRSAPublicKeyFromJWK(raw)
	if err != nil {
		return nil, nil, common.ErrCodeBadRequest, fmt.Errorf("invalid jwk: %w", err)
	}
	// ParseRSAPublicKeyFromJWK accepted n and e, so both are strings.
	public = map[string]any{"kty": kty, "kid": keyId, "n": jwk["n"], "e": jwk["e"]}
	for _, m := range []string{"alg", "use"} {
		v, ok := jwk[m]
		if !ok {
			continue
		}
		s, isString := v.(string)
		if !isString {
			return nil, nil, common.ErrCodeBadRequest, fmt.Errorf("jwk %s must be a string", m)
		}
		public[m] = s
	}
	return pubKey, public, "", nil
}

func toTrustedKeyResponse(tk *auth.TrustedKey) genapi.TrustedKeyResponseDto {
	resp := genapi.TrustedKeyResponseDto{
		KeyId: tk.KID, LegalEntityId: string(tk.TenantID),
		Jwk:       tk.JWK,
		Active:    tk.Active,
		ValidFrom: tk.ValidFrom,
	}
	if tk.Issuers != nil {
		s := tk.Issuers
		resp.Issuers = &s
	}
	if tk.ValidTo != nil {
		vt := *tk.ValidTo
		resp.ValidTo = &vt
	}
	return resp
}

func (h *Handler) ListTrustedKeys(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireAdmin(w, r) {
		return
	}
	if !h.gateTrustedKeyFeature(w, r) {
		return
	}
	if !h.requireTrustedKeyStore(w, r) {
		return
	}
	keys, err := h.trustedKeyStore.List(r.Context(), tenantFromCtx(r))
	if err != nil {
		common.WriteError(w, r, trustedKeyStoreError(err))
		return
	}
	out := make([]genapi.TrustedKeyResponseDto, 0, len(keys))
	for _, k := range keys {
		out = append(out, toTrustedKeyResponse(k))
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (h *Handler) DeleteTrustedKey(w http.ResponseWriter, r *http.Request, keyId string) {
	if !auth.RequireAdmin(w, r) {
		return
	}
	if !h.gateTrustedKeyFeature(w, r) {
		return
	}
	if !h.requireTrustedKeyStore(w, r) {
		return
	}
	if !auth.MatchesTrustedKIDPattern(keyId) {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid keyId format"))
		return
	}
	tID := tenantFromCtx(r)
	if err := h.trustedKeyStore.Delete(r.Context(), tID, keyId); err != nil {
		common.WriteError(w, r, trustedKeyStoreError(err))
		return
	}
	logTrustedKeyChange(r, "trusted key deleted", tID, keyId)
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) InvalidateTrustedKey(w http.ResponseWriter, r *http.Request, keyId string) {
	if !auth.RequireAdmin(w, r) {
		return
	}
	if !h.gateTrustedKeyFeature(w, r) {
		return
	}
	if !h.requireTrustedKeyStore(w, r) {
		return
	}
	if !auth.MatchesTrustedKIDPattern(keyId) {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid keyId format"))
		return
	}
	// The request has no body: trusted keys have no grace period, and
	// invalidation ends the key at once.
	tID := tenantFromCtx(r)
	if err := h.trustedKeyStore.Invalidate(r.Context(), tID, keyId); err != nil {
		common.WriteError(w, r, trustedKeyStoreError(err))
		return
	}
	logTrustedKeyChange(r, "trusted key invalidated", tID, keyId)
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) ReactivateTrustedKey(w http.ResponseWriter, r *http.Request, keyId string) {
	if !auth.RequireAdmin(w, r) {
		return
	}
	if !h.gateTrustedKeyFeature(w, r) {
		return
	}
	if !h.requireTrustedKeyStore(w, r) {
		return
	}
	if !auth.MatchesTrustedKIDPattern(keyId) {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid keyId format"))
		return
	}
	var req genapi.ReactivateKeyRequestDto
	if err := common.DecodeBoundedJSON(w, r, 1<<20, &req); err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid request body"))
		return
	}
	if req.ValidTo.IsZero() {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "validTo required"))
		return
	}
	validFrom := time.Now()
	if req.ValidFrom != nil {
		validFrom = *req.ValidFrom
	}
	validTo := req.ValidTo
	if !storableWindow(w, r, validFrom, validTo) {
		return
	}
	if !validTo.After(time.Now()) {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "validTo must be in the future"))
		return
	}
	if !validTo.After(validFrom) {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "validTo must be > validFrom"))
		return
	}
	tID := tenantFromCtx(r)
	if err := h.trustedKeyStore.Reactivate(r.Context(), tID, keyId, validFrom, validTo); err != nil {
		common.WriteError(w, r, trustedKeyStoreError(err))
		return
	}
	logTrustedKeyChange(r, "trusted key reactivated", tID, keyId)
	tk, err := h.trustedKeyStore.Get(r.Context(), tID, keyId)
	if err != nil {
		common.WriteError(w, r, trustedKeyStoreError(err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(toTrustedKeyResponse(tk))
}
