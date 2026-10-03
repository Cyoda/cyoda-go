package account

import (
	"net/http"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

type Handler struct {
	keyStore        auth.KeyStore
	trustedKeyStore auth.TrustedKeyStore
	m2mClientStore  auth.M2MClientStore
	iam             auth.IAMFeatures
	operator        auth.OperatorGuard
}

func New(keyStore auth.KeyStore, trustedKeyStore auth.TrustedKeyStore, m2mClientStore auth.M2MClientStore,
	iam auth.IAMFeatures, operator auth.OperatorGuard) *Handler {
	return &Handler{
		keyStore:        keyStore,
		trustedKeyStore: trustedKeyStore,
		m2mClientStore:  m2mClientStore,
		iam:             iam,
		operator:        operator,
	}
}

func (h *Handler) stub(w http.ResponseWriter, r *http.Request) {
	common.WriteError(w, r, common.Operational(http.StatusNotImplemented, common.ErrCodeNotImplemented, "not yet implemented"))
}

// writeRequiresJWTMode answers 501 NOT_IMPLEMENTED for a feature that exists
// only in JWT IAM mode. In mock IAM mode its store is not wired, and the
// feature names what is missing ("token issuance", "key management", …).
func writeRequiresJWTMode(w http.ResponseWriter, r *http.Request, feature string) {
	common.WriteError(w, r, common.Operational(http.StatusNotImplemented,
		common.ErrCodeNotImplemented, feature+" requires JWT IAM mode"))
}

func (h *Handler) AccountGet(w http.ResponseWriter, r *http.Request) {
	uc := spi.GetUserContext(r.Context())
	if uc == nil {
		common.WriteError(w, r, common.Operational(http.StatusUnauthorized, common.ErrCodeUnauthorized, "not authenticated"))
		return
	}

	roles := make([]map[string]string, len(uc.Roles))
	for i, role := range uc.Roles {
		roles[i] = map[string]string{"id": role}
	}

	resp := map[string]any{
		"userAccountInfo": map[string]any{
			"userId":   uc.UserID,
			"userName": uc.UserName,
			"legalEntity": map[string]string{
				"id":   string(uc.Tenant.ID),
				"name": uc.Tenant.Name,
			},
			"roles": roles,
			"currentSubscription": map[string]any{
				"id":            "unlimited",
				"legalEntityId": string(uc.Tenant.ID),
				"status":        "ACTIVE",
				"tierName":      "unlimited",
				"periodFrom":    "2020-01-01T00:00:00Z",
				"limits":        []any{},
			},
		},
	}
	common.WriteJSON(w, http.StatusOK, resp)
}

func (h *Handler) AccountSubscriptionsGet(w http.ResponseWriter, r *http.Request) {
	h.stub(w, r)
}

// GetTechnicalUserToken is the generated router's POST /oauth/token. In JWT
// IAM mode the auth-service token handler on the public mux (app/app.go, the
// /oauth/token entry, every method) takes the path before the generated
// router sees it, so this method runs only in mock IAM mode, which issues no
// token.
func (h *Handler) GetTechnicalUserToken(w http.ResponseWriter, r *http.Request, params genapi.GetTechnicalUserTokenParams) {
	writeRequiresJWTMode(w, r, "token issuance")
}
