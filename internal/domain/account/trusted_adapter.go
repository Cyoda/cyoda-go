package account

import (
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

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
	var req genapi.RegisterTrustedKeyRequestDto
	if err := boundedJSONDecode(w, r, 1<<20, &req); err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid request body"))
		return
	}
	if !auth.MatchesTrustedKIDPattern(req.KeyId) {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid keyId format"))
		return
	}
	pub, errCode, jwkErr := parseTrustedJWK(req.Jwk, req.KeyId, h.iam.TrustedKeyMaxJWKProperties)
	if jwkErr != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, errCode, jwkErr.Error()))
		return
	}
	if !isValidKeyPairAudience(string(req.Audience)) {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid audience"))
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
	if !validTo.After(validFrom) {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "validTo must be > validFrom"))
		return
	}
	var grace int64
	if req.InvalidateGracePeriodSec != nil {
		grace = *req.InvalidateGracePeriodSec
		if grace < 0 {
			common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "gracePeriodSec must be >= 0"))
			return
		}
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
	tk := &auth.TrustedKey{
		KID: req.KeyId, TenantID: tenantFromCtx(r), JWK: req.Jwk, PublicKey: pub,
		Audience: string(req.Audience), Issuers: issuers,
		Active: true, ValidFrom: validFrom, ValidTo: &vt,
	}
	if err := h.trustedKeyStore.Register(tk, auth.RotateOptions{Invalidate: invalidate, GracePeriodSec: grace}); err != nil {
		var ae *common.AppError
		if errors.As(err, &ae) {
			common.WriteError(w, r, ae)
			return
		}
		common.WriteError(w, r, common.Internal("trustedKeyStore.Register", err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(toTrustedKeyResponse(tk))
}

func parseTrustedJWK(jwk map[string]any, keyId string, maxProps int) (pub *rsa.PublicKey, code string, err error) {
	if len(jwk) > maxProps {
		return nil, common.ErrCodeBadRequest, fmt.Errorf("jwk has too many properties (%d > %d)", len(jwk), maxProps)
	}
	ktyAny, ok := jwk["kty"]
	if !ok {
		return nil, common.ErrCodeBadRequest, fmt.Errorf("jwk missing kty")
	}
	kty, _ := ktyAny.(string)
	if kty == "" {
		return nil, common.ErrCodeBadRequest, fmt.Errorf("jwk kty must be a string")
	}
	if kty != "RSA" {
		return nil, common.ErrCodeUnsupportedKeyType, fmt.Errorf("only RSA JWKs supported (v0.8.0)")
	}
	if rawKid, ok := jwk["kid"]; ok {
		s, _ := rawKid.(string)
		if s != keyId {
			return nil, common.ErrCodeBadRequest, fmt.Errorf("jwk.kid (%q) must equal keyId (%q)", s, keyId)
		}
	}
	raw, err := json.Marshal(jwk)
	if err != nil {
		return nil, common.ErrCodeBadRequest, fmt.Errorf("re-marshal jwk: %w", err)
	}
	pubKey, err := auth.ParseRSAPublicKeyFromJWK(raw)
	if err != nil {
		return nil, common.ErrCodeBadRequest, fmt.Errorf("invalid jwk: %w", err)
	}
	return pubKey, "", nil
}

func toTrustedKeyResponse(tk *auth.TrustedKey) genapi.TrustedKeyResponseDto {
	resp := genapi.TrustedKeyResponseDto{
		KeyId: tk.KID, LegalEntityId: string(tk.TenantID),
		Jwk:       tk.JWK,
		Audience:  genapi.TrustedKeyResponseDtoAudience(tk.Audience),
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
	tID := tenantFromCtx(r)
	keys := h.trustedKeyStore.List(tID)
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
	if !auth.MatchesTrustedKIDPattern(keyId) {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid keyId format"))
		return
	}
	tID := tenantFromCtx(r)
	if err := h.trustedKeyStore.Delete(tID, keyId); err != nil {
		common.WriteError(w, r, common.Operational(http.StatusNotFound, common.ErrCodeTrustedKeyNotFound, "trusted key not found"))
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) InvalidateTrustedKey(w http.ResponseWriter, r *http.Request, keyId string) {
	if !auth.RequireAdmin(w, r) {
		return
	}
	if !h.gateTrustedKeyFeature(w, r) {
		return
	}
	if !auth.MatchesTrustedKIDPattern(keyId) {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid keyId format"))
		return
	}
	var grace int64
	if r.ContentLength != 0 {
		var req genapi.InvalidateKeyRequestDto
		if err := boundedJSONDecode(w, r, 1<<20, &req); err != nil {
			common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid request body"))
			return
		}
		if req.GracePeriodSec != nil {
			grace = *req.GracePeriodSec
			if grace < 0 {
				common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "gracePeriodSec must be >= 0"))
				return
			}
		}
	}
	tID := tenantFromCtx(r)
	if err := h.trustedKeyStore.Invalidate(tID, keyId, grace); err != nil {
		common.WriteError(w, r, common.Operational(http.StatusNotFound, common.ErrCodeTrustedKeyNotFound, "trusted key not found"))
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) ReactivateTrustedKey(w http.ResponseWriter, r *http.Request, keyId string) {
	if !auth.RequireAdmin(w, r) {
		return
	}
	if !h.gateTrustedKeyFeature(w, r) {
		return
	}
	if !auth.MatchesTrustedKIDPattern(keyId) {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid keyId format"))
		return
	}
	var req genapi.ReactivateKeyRequestDto
	if err := boundedJSONDecode(w, r, 1<<20, &req); err != nil {
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
	if !validTo.After(time.Now()) {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "validTo must be in the future"))
		return
	}
	if !validTo.After(validFrom) {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "validTo must be > validFrom"))
		return
	}
	tID := tenantFromCtx(r)
	if err := h.trustedKeyStore.Reactivate(tID, keyId, validFrom, validTo); err != nil {
		common.WriteError(w, r, common.Operational(http.StatusNotFound, common.ErrCodeTrustedKeyNotFound, "trusted key not found"))
		return
	}
	tk, err := h.trustedKeyStore.Get(tID, keyId)
	if err != nil {
		common.WriteError(w, r, common.Internal("trustedKeyStore.Get after Reactivate", err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(toTrustedKeyResponse(tk))
}
