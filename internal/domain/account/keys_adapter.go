package account

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
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
	if !storableWindow(w, r, validFrom, validTo) {
		return
	}
	if !validTo.After(validFrom) {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "validTo must be > validFrom"))
		return
	}
	// A key pair whose window has already ended could never sign.
	if !validTo.After(now) {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "validTo must be in the future"))
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
	// Invalidating the current key stops it signing at once (it may still
	// verify through its grace period, but that is not signing), while a key
	// issued ahead of time cannot sign until its validFrom: the audience
	// would have no signing key in between. Issue ahead of time without
	// invalidating, then invalidate the old key once the new one's window
	// has opened.
	if invalidate && validFrom.After(now) {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest,
			"invalidateCurrent cannot be combined with a validFrom in the future"))
		return
	}
	kp, err := h.keyStore.Issue(r.Context(), auth.IssueRequest{
		Audience: string(req.Audience), ValidFrom: validFrom, ValidTo: validTo,
		Invalidate: invalidate, GracePeriodSec: grace,
	})
	if err != nil {
		common.WriteError(w, r, keyPairError(err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(toJwtKeyPairResponse(kp))
}

func isValidKeyPairAudience(s string) bool { return s == "human" || s == "client" }

// validKeyPairID writes 400 BAD_REQUEST and returns false if keyId does not
// have the form of a key-pair KID (auth.MatchesKeyPairIDPattern).
func validKeyPairID(w http.ResponseWriter, r *http.Request, keyId string) bool {
	if !auth.MatchesKeyPairIDPattern(keyId) {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid keyId format"))
		return false
	}
	return true
}

// storableWindow writes 400 BAD_REQUEST and returns false if the key store
// could not hold validFrom or validTo (auth.StorableTime): a UTC year outside
// 1..9999, which an offset timestamp such as 9999-12-31T23:59:59-05:00 reaches.
func storableWindow(w http.ResponseWriter, r *http.Request, validFrom, validTo time.Time) bool {
	for _, f := range []struct {
		name string
		t    time.Time
	}{{"validFrom", validFrom}, {"validTo", validTo}} {
		if !auth.StorableTime(f.t) {
			common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest,
				f.name+" out of range: its UTC year must be between 1 and 9999"))
			return false
		}
	}
	return true
}

// keyPairError: not found keeps 404 KEYPAIR_NOT_FOUND; every other failure
// goes through common.Internal (500 with a ticket, or 503 when storage is
// unavailable or the node's copy is stale).
func keyPairError(err error) *common.AppError {
	if errors.Is(err, auth.ErrKeyPairNotFound) {
		return common.Operational(http.StatusNotFound, common.ErrCodeKeypairNotFound, "key pair not found")
	}
	return common.Internal("key-pair store", err)
}

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
	kp, err := h.keyStore.Current(string(params.Audience))
	if errors.Is(err, auth.ErrKeyPairNotFound) {
		common.WriteError(w, r, common.Operational(http.StatusNotFound, common.ErrCodeKeypairNotFound, "no active key pair for audience"))
		return
	}
	if err != nil {
		common.WriteError(w, r, keyPairError(err))
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
	if !validKeyPairID(w, r, keyId) {
		return
	}
	if err := h.keyStore.Delete(r.Context(), keyId); err != nil {
		common.WriteError(w, r, keyPairError(err))
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
	if !validKeyPairID(w, r, keyId) {
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
	if err := h.keyStore.Invalidate(r.Context(), keyId, grace); err != nil {
		common.WriteError(w, r, keyPairError(err))
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
	if !validKeyPairID(w, r, keyId) {
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
	now := time.Now()
	validFrom := now
	if req.ValidFrom != nil {
		validFrom = *req.ValidFrom
	}
	if !storableWindow(w, r, validFrom, req.ValidTo) {
		return
	}
	// A future validFrom would put the key pair outside its own window at
	// once; for the key signing now, that leaves the audience with no signing
	// key. Issue a new key pair ahead of time instead.
	if validFrom.After(now) {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "validFrom cannot be in the future"))
		return
	}
	validTo := req.ValidTo
	if !validTo.After(now) {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "validTo must be in the future"))
		return
	}
	if !validTo.After(validFrom) {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "validTo must be > validFrom"))
		return
	}
	kp, err := h.keyStore.Reactivate(r.Context(), keyId, validFrom, validTo)
	if err != nil {
		common.WriteError(w, r, keyPairError(err))
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
