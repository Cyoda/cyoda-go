package account

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// requireKeyStore returns false after writing 501 NOT_IMPLEMENTED if the
// store wasn't wired (e.g. mock IAM mode). All 5 keypair adapters call this.
func (h *Handler) requireKeyStore(w http.ResponseWriter, r *http.Request) bool {
	if h.keyStore == nil {
		common.WriteError(w, r, common.Operational(http.StatusNotImplemented,
			common.ErrCodeNotImplemented, "key management requires JWT IAM mode"))
		return false
	}
	return true
}

func (h *Handler) IssueJwtKeyPair(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireAdmin(w, r) {
		return
	}
	if !h.requireKeyStore(w, r) {
		return
	}
	var req genapi.IssueJwtKeyPairRequestDto
	if err := boundedJSONDecode(w, r, 1<<20, &req); err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid request body"))
		return
	}
	if string(req.Algorithm) != "RS256" {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeUnsupportedAlgorithm, "only RS256 supported in this version"))
		return
	}
	if !isValidKeyPairAudience(string(req.Audience)) {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid audience"))
		return
	}
	now := time.Now().UTC()
	validFrom := now
	if req.ValidFrom != nil {
		validFrom = *req.ValidFrom
	}
	validTo := validFrom.Add(time.Duration(h.iam.KeypairDefaultValidityDays) * 24 * time.Hour)
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
		if grace > MaxGracePeriodSec {
			common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest,
				fmt.Sprintf("gracePeriodSec must be <= %d (366 days = 1 leap year)", MaxGracePeriodSec)))
			return
		}
	}
	invalidate := false
	if req.InvalidateCurrent != nil {
		invalidate = *req.InvalidateCurrent
	}
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		common.WriteError(w, r, common.Internal("rsa.GenerateKey", err))
		return
	}
	kidBytes := make([]byte, 16)
	if _, err := rand.Read(kidBytes); err != nil {
		common.WriteError(w, r, common.Internal("rand.Read", err))
		return
	}
	kid := hex.EncodeToString(kidBytes)
	vt := validTo
	kp := &auth.KeyPair{
		KID: kid, Audience: string(req.Audience), Algorithm: "RS256",
		PublicKey: &priv.PublicKey, PrivateKey: priv,
		Active: true, ValidFrom: validFrom, ValidTo: &vt,
	}
	if err := h.keyStore.Save(kp, auth.RotateOptions{Invalidate: invalidate, GracePeriodSec: grace}); err != nil {
		common.WriteError(w, r, common.Internal("keyStore.Save", err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(toJwtKeyPairResponse(kp))
}

func isValidKeyPairAudience(s string) bool { return s == "human" || s == "client" }

func toJwtKeyPairResponse(kp *auth.KeyPair) genapi.JwtKeyPairResponseDto {
	der, _ := x509.MarshalPKIXPublicKey(kp.PublicKey)
	resp := genapi.JwtKeyPairResponseDto{
		KeyId:     kp.KID,
		Algorithm: genapi.JwtKeyPairResponseDtoAlgorithm(kp.Algorithm),
		PublicKey: base64.StdEncoding.EncodeToString(der),
		Active:    kp.Active,
		ValidFrom: kp.ValidFrom,
	}
	if kp.ValidTo != nil {
		vt := *kp.ValidTo
		resp.ValidTo = &vt
	}
	return resp
}

func (h *Handler) GetCurrentJwtKeyPair(w http.ResponseWriter, r *http.Request, params genapi.GetCurrentJwtKeyPairParams) {
	if !auth.RequireAdmin(w, r) {
		return
	}
	if !h.requireKeyStore(w, r) {
		return
	}
	if !isValidKeyPairAudience(string(params.Audience)) {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid audience"))
		return
	}
	kp, err := h.keyStore.GetActive(string(params.Audience))
	if err != nil {
		common.WriteError(w, r, common.Operational(http.StatusNotFound, common.ErrCodeKeypairNotFound, "no active key pair for audience"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(toJwtKeyPairResponse(kp))
}

func (h *Handler) DeleteJwtKeyPair(w http.ResponseWriter, r *http.Request, keyId string) {
	if !auth.RequireAdmin(w, r) {
		return
	}
	if !h.requireKeyStore(w, r) {
		return
	}
	if err := h.keyStore.Delete(keyId); err != nil {
		common.WriteError(w, r, common.Operational(http.StatusNotFound, common.ErrCodeKeypairNotFound, "key pair not found"))
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) InvalidateJwtKeyPair(w http.ResponseWriter, r *http.Request, keyId string) {
	if !auth.RequireAdmin(w, r) {
		return
	}
	if !h.requireKeyStore(w, r) {
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
			if grace > MaxGracePeriodSec {
				common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest,
					fmt.Sprintf("gracePeriodSec must be <= %d (366 days = 1 leap year)", MaxGracePeriodSec)))
				return
			}
		}
	}
	if err := h.keyStore.Invalidate(keyId, grace); err != nil {
		common.WriteError(w, r, common.Operational(http.StatusNotFound, common.ErrCodeKeypairNotFound, "key pair not found"))
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) ReactivateJwtKeyPair(w http.ResponseWriter, r *http.Request, keyId string) {
	if !auth.RequireAdmin(w, r) {
		return
	}
	if !h.requireKeyStore(w, r) {
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
	if err := h.keyStore.Reactivate(keyId, validFrom, validTo); err != nil {
		common.WriteError(w, r, common.Operational(http.StatusNotFound, common.ErrCodeKeypairNotFound, "key pair not found"))
		return
	}
	kp, err := h.keyStore.Get(keyId)
	if err != nil {
		common.WriteError(w, r, common.Internal("keyStore.Get after Reactivate", err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(toJwtKeyPairResponse(kp))
}

func tenantFromCtx(r *http.Request) spi.TenantID {
	uc := spi.GetUserContext(r.Context())
	if uc == nil {
		return ""
	}
	return uc.Tenant.ID
}
